package notify

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

// tlsServer answers alerts and probes over TLS with a certificate no public CA
// signed, which is what a private CA in the operator's file is for.
func tlsServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// trust is the server's own certificate as the only root, which is what
// ca_file resolves to: a bundle that replaces the host's trust store rather
// than adding to it.
func trust(t *testing.T, srv *httptest.Server) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

func TestSendOverTLSVerifiesAgainstThePrivateCA(t *testing.T) {
	srv := tlsServer(t)

	c := NewClient(srv.URL)
	c.SetTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: trust(t, srv)})

	if err := c.Send(context.Background(), []alert.Alert{firingAlert()}); err != nil {
		t.Fatalf("Send = %v, want nil", err)
	}
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("Probe = %v, want nil", err)
	}
}

// A client that was handed no material cannot reach a privately signed
// Alertmanager, which is the failure a ca_file exists to fix. Pinned so that a
// passing test above is evidence of verification rather than of a default that
// trusts everything.
func TestSendOverTLSWithoutTheCAFails(t *testing.T) {
	srv := tlsServer(t)

	err := NewClient(srv.URL).Send(context.Background(), []alert.Alert{firingAlert()})
	if err == nil {
		t.Fatal("Send = nil, want a verification failure")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("error does not name the certificate: %v", err)
	}
}

// server_name is what is verified, so a name the certificate does not carry is
// refused even though the CA is the right one.
func TestSendOverTLSRefusesTheWrongServerName(t *testing.T) {
	srv := tlsServer(t)

	c := NewClient(srv.URL)
	c.SetTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    trust(t, srv),
		ServerName: "alertmanager.invalid",
	})

	err := c.Send(context.Background(), []alert.Alert{firingAlert()})
	if err == nil {
		t.Fatal("Send = nil, want the name to be refused")
	}
}

// The downgrade an operator asked for, which is reported by
// alertmanager/tls-insecure and cleared by a dated exemption (spec 6.5).
func TestSendOverTLSHonoursInsecureSkipVerify(t *testing.T) {
	srv := tlsServer(t)

	c := NewClient(srv.URL)
	c.SetTLS(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}) //nolint:gosec // the point of the test

	if err := c.Send(context.Background(), []alert.Alert{firingAlert()}); err != nil {
		t.Fatalf("Send = %v, want nil", err)
	}
}

// The timeout is the client's, not the transport's, so TLS material does not
// quietly remove the bound a send is made under.
func TestSetTLSKeepsTheSendTimeout(t *testing.T) {
	c := NewClient("https://am:9093")
	c.SetTLS(&tls.Config{MinVersion: tls.VersionTLS12})

	if c.HTTP.Timeout != DefaultTimeout {
		t.Errorf("timeout = %s, want %s", c.HTTP.Timeout, DefaultTimeout)
	}
}
