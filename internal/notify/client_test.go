package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

func firingAlert() alert.Alert {
	return alert.Alert{
		Labels:  map[string]string{"alertname": "HighP99Latency", "team": "payments"},
		Phase:   alert.PhaseFiring,
		FiredAt: payloadAnchor,
	}
}

// This exercises the real HTTP path against a real server. It is not a mocked
// Alertmanager: the end to end test in integration_test.go uses the genuine
// article. This one pins down status handling, which a real Alertmanager will
// not produce on demand.
func TestSendPostsToAlertsAPI(t *testing.T) {
	var body []Alert
	var path string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if err := c.Send(context.Background(), []alert.Alert{firingAlert()}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if path != "/api/v2/alerts" {
		t.Errorf("path = %q, want /api/v2/alerts", path)
	}
	if len(body) != 1 || body[0].Labels["team"] != "payments" {
		t.Errorf("body = %+v, want one alert for team payments", body)
	}
}

// Alertmanager restarts and rolling deploys produce 5xx. Losing a page to one
// is not acceptable, so a retry is worth the delay.
func TestSendRetriesServerErrors(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	c.Backoff = time.Millisecond

	if err := c.Send(context.Background(), []alert.Alert{firingAlert()}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

// A 400 means the payload is wrong. Retrying sends the same wrong payload
// again, so it fails immediately instead.
func TestSendDoesNotRetryClientErrors(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	c.Backoff = time.Millisecond

	if err := c.Send(context.Background(), []alert.Alert{firingAlert()}, nil); err == nil {
		t.Fatal("expected an error for a 400")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestSendGivesUpAfterMaxAttempts(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	c.Backoff = time.Millisecond
	c.MaxAttempts = 2

	if err := c.Send(context.Background(), []alert.Alert{firingAlert()}, nil); err == nil {
		t.Fatal("expected an error once attempts are exhausted")
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// Nothing to send must not produce a request. An empty POST is a wasted round
// trip on every evaluation of every quiet rule, which is almost all of them.
func TestSendSkipsWhenNothingToNotify(t *testing.T) {
	var called atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	pending := alert.Alert{Labels: map[string]string{"alertname": "X"}, Phase: alert.PhasePending}

	if err := c.Send(context.Background(), []alert.Alert{pending}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if called.Load() {
		t.Error("posted a request with nothing to notify about")
	}
}
