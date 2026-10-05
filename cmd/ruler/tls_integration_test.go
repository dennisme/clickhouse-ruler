//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// tlsStack reads what the TLS Alertmanager is reached by, and asserts the
// premise every case below rests on: that the server is signed by a CA no
// host trusts. A server a default client could reach would make the verified
// case pass while proving nothing (spec 6.5, 9.1).
func tlsStack(t *testing.T) (amURL, caFile, chAddr string) {
	t.Helper()

	amURL = os.Getenv("RULER_ALERTMANAGER_TLS_URL")
	caFile = os.Getenv("RULER_ALERTMANAGER_CA")
	chAddr = os.Getenv("RULER_CLICKHOUSE_ADDR")
	if amURL == "" || caFile == "" || chAddr == "" {
		t.Fatal("RULER_ALERTMANAGER_TLS_URL, RULER_ALERTMANAGER_CA and RULER_CLICKHOUSE_ADDR " +
			"must be set, run `just integration`")
	}

	// Verifiable with the bundle, which is what the ruler is about to be given.
	// Asserted here so a broken certificate reads as a broken fixture rather
	// than as a broken ruler.
	//
	// This is also the readiness wait for the whole fixture. The container has
	// no healthcheck, because nothing in its image can speak this handshake,
	// so `docker compose up --wait` returns once the process is running rather
	// than once it is listening (spec 9.1).
	pool := caPool(t, caFile)
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
	}}
	ready := strings.TrimSuffix(amURL, "/") + "/-/ready"

	var err error
	for deadline := time.Now().Add(30 * time.Second); ; {
		var resp *http.Response
		resp, err = client.Get(ready) //nolint:noctx // a fixture assertion
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /-/ready on %s with the stack CA: %v", amURL, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Asserted after the wait above rather than before it. A server that is not
	// listening yet fails this request too, which would pass the assertion for
	// the wrong reason and leave the premise unproven.
	if _, err := http.Get(ready); err == nil { //nolint:noctx,bodyclose // the error is the assertion
		t.Fatalf("GET /-/ready on %s with the host's trust store succeeded: this server is not "+
			"privately signed, so nothing here proves verification", amURL)
	}

	return amURL, caFile, chAddr
}

// caPool is the stack's CA as the only root, which is what ca_file resolves to.
func caPool(t *testing.T, path string) *x509.CertPool {
	t.Helper()

	pem, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the stack CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("%s holds no PEM certificate", path)
	}
	return pool
}

// A private CA named in ca_file is verified, and the alert arrives.
func TestRunDeliversOverTLS(t *testing.T) {
	amURL, caFile, chAddr := tlsStack(t)

	s := startSink(t)
	serviceName := "checkout-tls-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seed(t, chAddr, serviceName)

	cfg := tlsConfigFile(t, chAddr, amURL, "    tls_config:\n      ca_file: "+caFile+"\n")

	stdout, stderr := runUntilDelivered(t, cfg, s, serviceName, true)

	// The paths are fine in a log line and the key material is not, so what is
	// asserted is that no PEM body reached the output (spec 8.4).
	for _, stream := range []struct{ name, body string }{
		{"stdout", stdout}, {"stderr", stderr},
	} {
		if strings.Contains(stream.body, "PRIVATE KEY") {
			t.Errorf("%s carries key material:\n%s", stream.name, stream.body)
		}
	}
}

// A server_name the certificate does not carry is refused, even though the CA
// is the right one. The probe is what says so before anything fires, which is
// the signal an operator reads when a page does not arrive (spec 8.2).
func TestRunRefusesTheWrongServerName(t *testing.T) {
	amURL, caFile, chAddr := tlsStack(t)

	s := startSink(t)
	serviceName := "checkout-tls-name-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seed(t, chAddr, serviceName)

	cfg := tlsConfigFile(t, chAddr, amURL,
		"    tls_config:\n      ca_file: "+caFile+"\n      server_name: alertmanager.invalid\n")

	stdout, stderr := runUntilDelivered(t, cfg, s, serviceName, false)

	out := stdout + stderr
	if !strings.Contains(out, "did not answer its probe") {
		t.Errorf("the refused name was not reported:\n%s", out)
	}
	if !strings.Contains(out, "certificate") {
		t.Errorf("the reported reason does not name the certificate:\n%s", out)
	}
}

// insecure_skip_verify is honoured, under the exemption that clears
// alertmanager/tls-insecure. Without the exemption the finding is raised and
// the ruler still starts, which is internal/source's and reload.go's test; what
// this proves is that the downgrade reaches a server no bundle was named for.
func TestRunDeliversWithVerificationOff(t *testing.T) {
	amURL, _, chAddr := tlsStack(t)

	s := startSink(t)
	serviceName := "checkout-tls-insecure-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seed(t, chAddr, serviceName)

	// A date rather than a constant, because an exemption that expired would
	// make this fail on a day nobody changed anything on.
	until := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	cfg := tlsConfigFile(t, chAddr, amURL,
		"    tls_config:\n      insecure_skip_verify: true\n"+
			"    exempt:\n      - check: alertmanager/tls-insecure\n"+
			"        reason: the stack's alertmanager is privately signed and this case is the point\n"+
			"        until: "+until+"\n")

	runUntilDelivered(t, cfg, s, serviceName, true)
}

// runUntilDelivered runs the ruler against one config until the sink has an
// alert for serviceName, or until it is clear that none is coming. It returns
// what the process wrote, because for the refusal cases that is the assertion.
func runUntilDelivered(t *testing.T, cfg string, s *sink, serviceName string, want bool) (stdout, stderr string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var out, errOut bytes.Buffer
	runDone := make(chan int, 1)
	go func() {
		runDone <- runRun(ctx, []string{
			"--rules", filepath.Join("testdata", "rules"),
			"--config", cfg,
			"--listen", ":0",
		}, &out, &errOut)
	}()

	delivered := func(ds []delivery) bool {
		for _, d := range ds {
			for _, a := range d.Alerts {
				if a.Labels["ServiceName"] == serviceName {
					return true
				}
			}
		}
		return false
	}

	// Longer when something is expected than when nothing is: a delivery has a
	// seed, a tick and a group interval in front of it, while the absence of
	// one only has to outlast the probe that reports why.
	window := 30 * time.Second
	if !want {
		window = 15 * time.Second
	}
	got := s.waitForUpTo(t, window, delivered)

	cancel()
	select {
	case code := <-runDone:
		if want && code != exitOK {
			t.Errorf("ruler run exited %d\nstdout: %s\nstderr: %s", code, out.String(), errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ruler run did not shut down within 10s of cancellation")
	}

	if delivered(got) != want {
		t.Errorf("delivered = %v, want %v for %s\nstdout: %s\nstderr: %s",
			!want, want, serviceName, out.String(), errOut.String())
	}
	return out.String(), errOut.String()
}

// tlsConfigFile writes the operator's file pointing at the TLS Alertmanager
// with the block under test, which is how a deployment names a mounted Secret.
func tlsConfigFile(t *testing.T, chAddr, amURL, block string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "ruler.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	body := bytes.ReplaceAll(data, []byte("address: 127.0.0.1:9000"), []byte("address: "+chAddr))
	body = bytes.ReplaceAll(body,
		[]byte("urls: [http://127.0.0.1:9093]"),
		[]byte("urls: ["+amURL+"]\n"+strings.TrimSuffix(block, "\n")))

	out := filepath.Join(t.TempDir(), "ruler.yaml")
	if err := os.WriteFile(out, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}
