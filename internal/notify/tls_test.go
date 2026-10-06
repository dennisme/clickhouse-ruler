package notify

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
// quietly remove the bound a send is made under. It survives a rotation too,
// which is where it would be lost: the replacement reads the bound off the
// client it replaces rather than off a default.
func TestSetTLSKeepsTheSendTimeout(t *testing.T) {
	c := NewClient("https://am:9093")
	c.SetTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	c.SetTLS(&tls.Config{MinVersion: tls.VersionTLS12})

	if got := c.httpClient().Timeout; got != DefaultTimeout {
		t.Errorf("timeout = %s, want %s", got, DefaultTimeout)
	}
}

// A CA rotation is what SetTLS is called for after the client is in use, so
// the roots it replaces stop being trusted on the very next send.
//
// The send before the rotation is what makes it a rotation rather than the
// call-once this used to be: the client has a transport with a verified
// connection in its pool, and the next send has to be made against the bundle
// the operator's file now names.
func TestSetTLSStopsTrustingTheRootsItReplaced(t *testing.T) {
	srv := tlsServer(t)

	c := NewClient(srv.URL)
	c.SetTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: trust(t, srv)})

	if err := c.Send(context.Background(), []alert.Alert{firingAlert()}); err != nil {
		t.Fatalf("Send before the rotation = %v, want nil", err)
	}

	// An empty pool rather than no material at all, because the host's trust
	// store is what a nil RootCAs falls back to and this server is signed by
	// neither.
	c.SetTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: x509.NewCertPool()})

	err := c.Send(context.Background(), []alert.Alert{firingAlert()})
	if err == nil {
		t.Fatal("Send after the rotation = nil, want a verification failure")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("error does not name the certificate: %v", err)
	}
}

// And the other direction: material that trusts the server again is applied to
// a client that has been failing, so a bundle restored on disk costs a signal.
func TestSetTLSAppliesRootsThatTrustTheServerAgain(t *testing.T) {
	srv := tlsServer(t)

	c := NewClient(srv.URL)
	c.SetTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: x509.NewCertPool()})

	if err := c.Send(context.Background(), []alert.Alert{firingAlert()}); err == nil {
		t.Fatal("Send before the rotation = nil, want a verification failure")
	}

	c.SetTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: trust(t, srv)})

	if err := c.Send(context.Background(), []alert.Alert{firingAlert()}); err != nil {
		t.Fatalf("Send after the rotation = %v, want nil", err)
	}
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("Probe after the rotation = %v, want nil", err)
	}
}

// The connections the replaced transport pooled are put down.
//
// Dropping the transport does not do it. Each pooled connection has a
// goroutine holding the transport, so nothing collects it, and a ruler that
// probes every thirty seconds always has one idle connection to leak. Asserted
// from the server's side, because the socket staying open is the whole defect
// and the client cannot see it.
func TestSetTLSClosesTheConnectionsItPooled(t *testing.T) {
	closed := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL)
	c.SetTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: trust(t, srv)})

	if err := c.Send(context.Background(), []alert.Alert{firingAlert()}); err != nil {
		t.Fatalf("Send = %v, want nil", err)
	}

	c.SetTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: trust(t, srv)})

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the rotation left the connection it pooled open")
	}
}

// A rotation arrives on the run loop while the delivery worker and this
// endpoint's probe are both using the client, which is the only way it ever
// arrives. Run under -race, where a transport replaced as a plain field is a
// reported data race rather than a test that happens to pass.
func TestSetTLSWhileSendsAndProbesAreInFlight(t *testing.T) {
	srv := tlsServer(t)
	material := func() *tls.Config {
		return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: trust(t, srv)}
	}

	c := NewClient(srv.URL)
	c.SetTLS(material())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, call := range []func() error{
		func() error { return c.Send(ctx, []alert.Alert{firingAlert()}) },
		func() error { return c.Probe(ctx) },
	} {
		wg.Add(1)
		go func(call func() error) {
			defer wg.Done()
			for ctx.Err() == nil {
				// Every rotation here trusts the server, so a failure is the
				// swap losing a request rather than roots that moved.
				if err := call(); err != nil && ctx.Err() == nil {
					errs <- err
					return
				}
			}
		}(call)
	}

	for i := 0; i < 50; i++ {
		c.SetTLS(material())
	}
	cancel()
	wg.Wait()

	close(errs)
	for err := range errs {
		t.Errorf("a request failed across a rotation: %v", err)
	}
}
