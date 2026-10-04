package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The question a probe asks is whether Alertmanager would accept an alert now,
// which is what its own readiness endpoint answers (spec 8.2).
func TestProbeAsksAlertmanagerReadiness(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := NewClient(srv.URL).Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if method != http.MethodGet || path != "/-/ready" {
		t.Errorf("probed %s %s, want GET /-/ready", method, path)
	}
}

// An Alertmanager that answers and says it is not ready is one that would
// refuse the alert, so the probe reports it as a failure rather than as a
// socket that opened.
func TestProbeFailsOnANonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := NewClient(srv.URL).Probe(context.Background())
	if err == nil {
		t.Fatal("Probe = nil against 503, want a failure")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q does not say what the status was", err)
	}
}

// The next tick is the retry. A probe that retried would report a 25 second
// outage as healthy (spec 8.2).
func TestProbeDoesNotRetry(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_ = NewClient(srv.URL).Probe(context.Background())
	if attempts != 1 {
		t.Errorf("got %d requests, want 1", attempts)
	}
}
