//go:build integration

package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The credential is proven against a server that demands one.
//
// Everything below the wire is already unit tested: internal/notify asserts
// the header goes on a send and on a probe, and internal/source asserts the
// secret is read out of a file and refused twelve ways. None of that can show
// that the header this ruler builds is one Alertmanager accepts, because a
// test asserting on its own request would pass against a credential no server
// would take (spec 6.5, 9.1).
//
// It runs against alertmanager-auth rather than the plain Alertmanager, which
// stays unauthenticated so the anonymous path keeps being covered too.
func TestRunDeliversThroughBasicAuth(t *testing.T) {
	amURL := os.Getenv("RULER_ALERTMANAGER_AUTH_URL")
	chAddr := os.Getenv("RULER_CLICKHOUSE_ADDR")
	if amURL == "" || chAddr == "" {
		t.Fatal("RULER_ALERTMANAGER_AUTH_URL and RULER_CLICKHOUSE_ADDR must be set, run `just integration`")
	}

	// The premise, asserted rather than assumed. An authenticated server that
	// answered an anonymous request would make everything below pass while
	// proving nothing, which is the failure this whole test exists to avoid.
	requireUnauthorized(t, amURL)

	s := startSink(t)

	serviceName := "checkout-auth-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seed(t, chAddr, serviceName)

	rewritten := authConfig(t, chAddr, amURL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	runDone := make(chan int, 1)
	go func() {
		runDone <- runRun(ctx, []string{
			"--rules", filepath.Join("testdata", "rules"),
			"--config", rewritten,
			"--listen", ":0",
		}, &stdout, &stderr)
	}()

	got := s.waitFor(t, 30*time.Second, func(ds []delivery) bool {
		for _, d := range ds {
			for _, a := range d.Alerts {
				if a.Labels["ServiceName"] == serviceName {
					return true
				}
			}
		}
		return false
	})

	cancel()
	select {
	case code := <-runDone:
		if code != exitOK {
			t.Errorf("ruler run exited %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ruler run did not shut down within 10s of cancellation")
	}

	var found bool
	for _, d := range got {
		for _, a := range d.Alerts {
			if a.Labels["ServiceName"] == serviceName {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no alert for %s delivered through basic auth: %+v", serviceName, got)
	}

	// The secret is never logged, on the path that logs the most about where
	// alerts went (spec 8.4).
	for _, stream := range []struct{ name, body string }{
		{"stdout", stdout.String()},
		{"stderr", stderr.String()},
	} {
		if strings.Contains(stream.body, testAlertmanagerPassword) {
			t.Errorf("%s carries the password:\n%s", stream.name, stream.body)
		}
	}
}

// A ruler whose credential is wrong delivers nothing and says so, rather than
// reporting a healthy Alertmanager.
//
// The probe is what makes this visible before anything fires: it authenticates
// too, so a wrong credential reads as an Alertmanager that is not answering
// rather than as one nobody has tried (spec 8.2).
func TestRunReportsAWrongCredential(t *testing.T) {
	amURL := os.Getenv("RULER_ALERTMANAGER_AUTH_URL")
	chAddr := os.Getenv("RULER_CLICKHOUSE_ADDR")
	if amURL == "" || chAddr == "" {
		t.Fatal("RULER_ALERTMANAGER_AUTH_URL and RULER_CLICKHOUSE_ADDR must be set, run `just integration`")
	}

	secret := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(secret, []byte("not-the-password"), 0o600); err != nil {
		t.Fatal(err)
	}
	rewritten := authConfigWithSecret(t, chAddr, amURL, secret)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	runDone := make(chan int, 1)
	go func() {
		runDone <- runRun(ctx, []string{
			"--rules", filepath.Join("testdata", "rules"),
			"--config", rewritten,
			"--listen", ":0",
		}, &stdout, &stderr)
	}()

	// The first probe runs before the first tick, so the warning arrives
	// without waiting out the probe interval.
	deadline := time.Now().Add(30 * time.Second)
	var reported bool
	for time.Now().Before(deadline) {
		if strings.Contains(stdout.String()+stderr.String(), "did not answer its probe") {
			reported = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("ruler run did not shut down within 10s of cancellation")
	}

	if !reported {
		t.Errorf("a wrong credential was not reported\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	if body := stdout.String() + stderr.String(); strings.Contains(body, "not-the-password") {
		t.Errorf("the output carries the password:\n%s", body)
	}
}

// testAlertmanagerPassword is what deploy/alertmanager/web-auth.yml hashes. A
// test credential, checked in on purpose: it authenticates to a container
// bound to localhost and to nothing else.
const testAlertmanagerPassword = "ruler-test-password"

// requireUnauthorized fails the test unless the Alertmanager refuses an
// anonymous request, which is the premise every assertion here rests on.
func requireUnauthorized(t *testing.T, amURL string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(amURL, "/")+"/-/ready", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reaching %s: %v", amURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /-/ready on %s = %s, want 401: this server is not behind auth, "+
			"so nothing here proves a credential", amURL, resp.Status)
	}
}

// authConfig writes the operator's file pointing at the authenticated
// Alertmanager, with the password in a file beside it, which is the posture a
// deployment takes (spec 6.5).
func authConfig(t *testing.T, chAddr, amURL string) string {
	t.Helper()

	secret := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(secret, []byte(testAlertmanagerPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	return authConfigWithSecret(t, chAddr, amURL, secret)
}

func authConfigWithSecret(t *testing.T, chAddr, amURL, secret string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "ruler.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	body := bytes.ReplaceAll(data, []byte("address: 127.0.0.1:9000"), []byte("address: "+chAddr))
	body = bytes.ReplaceAll(body,
		[]byte("urls: [http://127.0.0.1:9093]"),
		[]byte("urls: ["+amURL+"]\n    basic_auth:\n      username: ruler\n      password_file: "+secret))

	out := filepath.Join(t.TempDir(), "ruler.yaml")
	if err := os.WriteFile(out, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}
