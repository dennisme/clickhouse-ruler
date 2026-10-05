package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// amTLSServer answers probes over TLS with a privately signed certificate, and
// hands back the PEM bundle that verifies it, which is what ca_file resolves to.
func amTLSServer(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return srv, ca
}

// unrelatedCA is a valid PEM bundle that signed nothing this test reaches,
// which is the bundle an operator has half way through a CA rotation.
//
// Generated rather than taken from a second httptest server, because every
// httptest TLS server presents the same built-in certificate, so two of them
// would compare equal and prove nothing.
func unrelatedCA(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// The material the operator's file named is what the client posts over, which
// is the whole of what this wiring owns: internal/source read it and
// internal/notify built the transport.
func TestAlertmanagerEndpointsUseTheConfiguredTLS(t *testing.T) {
	srv, ca := amTLSServer(t)

	set := source.Alertmanager{URLs: []string{srv.URL}, TLS: &source.TLS{CA: ca}}
	endpoints := alertmanagerEndpoints(set)
	if len(endpoints) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(endpoints))
	}
	if err := endpoints[0].client.Probe(context.Background()); err != nil {
		t.Fatalf("Probe = %v, want nil over the private CA", err)
	}
}

// A set with no material reaches a privately signed Alertmanager no better
// than curl does, so a passing test above is evidence of verification.
func TestAlertmanagerEndpointsWithoutTLSCannotReachOne(t *testing.T) {
	srv, _ := amTLSServer(t)

	endpoints := alertmanagerEndpoints(source.Alertmanager{URLs: []string{srv.URL}})
	if err := endpoints[0].client.Probe(context.Background()); err == nil {
		t.Fatal("Probe = nil, want a verification failure")
	}
}

// What a reload compares, which is what decides whether anything is rebuilt.
// Rebuilding on every signal would drop the pooled connection the probe timer
// keeps warm, for a bundle nobody edited.
func TestAReloadSeesWhichTLSMaterialChanged(t *testing.T) {
	srv, ca := amTLSServer(t)

	set := source.Alertmanager{URLs: []string{srv.URL}, TLS: &source.TLS{CA: ca}}
	endpoints := alertmanagerEndpoints(set)

	if !sameAlertmanagerTLS(endpoints, set) {
		t.Error("the set that built the endpoints read as a changed one")
	}

	replaced := source.Alertmanager{URLs: []string{srv.URL}, TLS: &source.TLS{CA: []byte("-----BEGIN CERTIFICATE-----\n")}}
	if sameAlertmanagerTLS(endpoints, replaced) {
		t.Error("a replaced CA read as unchanged")
	}

	// Dropping the block entirely is a change too: the client would stop
	// verifying against the bundle it was built with.
	if sameAlertmanagerTLS(endpoints, source.Alertmanager{URLs: []string{srv.URL}}) {
		t.Error("a removed tls_config read as unchanged")
	}

	// A rotated client pair is not a change: the paths are the same and each
	// handshake reads them again.
	withPair := source.Alertmanager{URLs: []string{srv.URL}, TLS: &source.TLS{
		CA: ca, CertFile: "/run/secrets/cert.pem", KeyFile: "/run/secrets/key.pem"}}
	paired := alertmanagerEndpoints(withPair)
	if !sameAlertmanagerTLS(paired, withPair) {
		t.Error("the same client pair read as a changed one")
	}
}

// A bundle that now trusts the Alertmanager is applied, so restoring a CA on
// disk costs a signal. A restart would make every pending alert serve its
// `for` again (spec 6.5, 12.2).
func TestAReloadAppliesChangedTLSMaterial(t *testing.T) {
	srv, ca := amTLSServer(t)

	// Built against a CA that signed something else, which is the half-done
	// rotation an operator is reloading to get out of.
	other := unrelatedCA(t)
	stale := source.Alertmanager{URLs: []string{srv.URL}, TLS: &source.TLS{CA: other}}

	var logs bytes.Buffer
	r := &runner{
		configPath: "ruler.yaml",
		log:        slog.New(slog.NewTextHandler(&logs, nil)),
		endpoints:  alertmanagerEndpoints(stale),
	}
	if err := r.endpoints[0].client.Probe(context.Background()); err == nil {
		t.Fatal("Probe = nil before the reload, want a verification failure")
	}

	fixed := source.Alertmanager{URLs: []string{srv.URL}, TLS: &source.TLS{CA: ca}}
	r.rotateAlertmanagers(&config{alertmanagers: []source.Alertmanager{fixed}})

	if err := r.endpoints[0].client.Probe(context.Background()); err != nil {
		t.Fatalf("Probe = %v after the reload, want nil\n%s", err, logs.String())
	}

	// The endpoint now holds what is running, so the next reload of an
	// unedited file compares equal and rebuilds nothing.
	if !sameAlertmanagerTLS(r.endpoints, fixed) {
		t.Error("the endpoint still holds the material it was built with")
	}
}

// And the other direction, which is the one that says verification is real: a
// bundle that no longer names the server's CA stops the sends. A pooled
// connection that outlived the swap would keep working on roots the file does
// not name any more.
func TestAReloadStopsTrustingAWithdrawnCA(t *testing.T) {
	srv, ca := amTLSServer(t)
	other := unrelatedCA(t)

	r := &runner{
		configPath: "ruler.yaml",
		log:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		endpoints:  alertmanagerEndpoints(source.Alertmanager{URLs: []string{srv.URL}, TLS: &source.TLS{CA: ca}}),
	}
	if err := r.endpoints[0].client.Probe(context.Background()); err != nil {
		t.Fatalf("Probe = %v before the reload, want nil", err)
	}

	withdrawn := source.Alertmanager{URLs: []string{srv.URL}, TLS: &source.TLS{CA: other}}
	r.rotateAlertmanagers(&config{alertmanagers: []source.Alertmanager{withdrawn}})

	if err := r.endpoints[0].client.Probe(context.Background()); err == nil {
		t.Fatal("Probe = nil after the reload, want a verification failure")
	}
}

// The log line is what an operator has to go on, so it names the file and says
// the material was applied rather than only that something changed.
func TestAReloadLogsThatItAppliedTLSMaterial(t *testing.T) {
	srv, ca := amTLSServer(t)
	other := unrelatedCA(t)

	var logs bytes.Buffer
	r := &runner{
		configPath: "ruler.yaml",
		log:        slog.New(slog.NewTextHandler(&logs, nil)),
		endpoints:  alertmanagerEndpoints(source.Alertmanager{URLs: []string{srv.URL}, TLS: &source.TLS{CA: ca}}),
	}

	replaced := source.Alertmanager{URLs: []string{srv.URL}, TLS: &source.TLS{CA: other}}
	r.rotateAlertmanagers(&config{alertmanagers: []source.Alertmanager{replaced}})

	if !strings.Contains(logs.String(), "tls_config") {
		t.Errorf("the reload did not name what it applied:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "restart") {
		t.Errorf("the reload still asks for a restart:\n%s", logs.String())
	}

	// A second signal over an unedited file is silent, so the line means
	// something moved.
	logs.Reset()
	r.rotateAlertmanagers(&config{alertmanagers: []source.Alertmanager{replaced}})
	if strings.Contains(logs.String(), "tls_config") {
		t.Errorf("an unchanged tls_config was reported as applied:\n%s", logs.String())
	}
}

// Verification turned off is a finding and not a refusal to start, which is
// the one place an error-severity alertmanager finding does not stop the ruler.
// The path works and what is missing is the server's identity, so an expiry
// that bricked the next restart would take an estate's alerting down over a
// calendar entry (spec 6.5).
func TestAnInsecureAlertmanagerStillStarts(t *testing.T) {
	cfg := &config{
		alertmanagers: []source.Alertmanager{{URLs: []string{"https://am:9093"}}},
		problems: []lint.Problem{{
			Check:    lint.CheckAlertmanagerTLSInsecure,
			Severity: lint.SeverityError,
			Text:     "insecure_skip_verify turns certificate verification off",
		}},
	}
	if err := cfg.deliverable(); err != nil {
		t.Errorf("deliverable = %v, want nil: the delivery path works", err)
	}
}

// Every other alertmanager error still refuses, because a path the ruler
// cannot address or authenticate to is every alert in the checkout.
func TestAnUnusableAlertmanagerStillRefuses(t *testing.T) {
	cfg := &config{
		alertmanagers: []source.Alertmanager{{URLs: []string{"https://am:9093"}}},
		problems: []lint.Problem{{
			Check:    lint.CheckAlertmanagerTLS,
			Severity: lint.SeverityError,
			Text:     `cannot read ca_file "/run/secrets/ca.pem"`,
		}},
	}
	if err := cfg.deliverable(); err == nil {
		t.Error("deliverable = nil, want a refusal over unreadable TLS material")
	}
}
