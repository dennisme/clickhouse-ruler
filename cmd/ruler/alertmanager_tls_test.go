package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// A changed CA is reported and not applied, which is the line 6.5 draws: the
// credential rotates in place, the urls and the TLS material need a restart.
func TestAReloadSaysChangedTLSMaterialNeedsARestart(t *testing.T) {
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

// The log line is what an operator has to go on, so it says which file and
// what to do rather than only that something changed.
func TestAReloadLogsThatTLSMaterialNeedsARestart(t *testing.T) {
	srv, ca := amTLSServer(t)

	var logs bytes.Buffer
	r := &runner{
		configPath: "ruler.yaml",
		log:        slog.New(slog.NewTextHandler(&logs, nil)),
		endpoints:  alertmanagerEndpoints(source.Alertmanager{URLs: []string{srv.URL}, TLS: &source.TLS{CA: ca}}),
	}

	replaced := source.Alertmanager{URLs: []string{srv.URL}, TLS: &source.TLS{CA: []byte("other")}}
	r.rotateAlertmanagerCredential(&config{alertmanagers: []source.Alertmanager{replaced}})

	if !strings.Contains(logs.String(), "restart") {
		t.Errorf("the reload said nothing about needing a restart:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "tls_config") {
		t.Errorf("the reload did not name what changed:\n%s", logs.String())
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
