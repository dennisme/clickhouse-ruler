package notify

import (
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

var payloadAnchor = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

func TestPayloadMapsFiringAlert(t *testing.T) {
	a := alert.Alert{
		Labels:  map[string]string{"alertname": "HighP99Latency", "team": "payments"},
		Value:   1234.5,
		Phase:   alert.PhaseFiring,
		FiredAt: payloadAnchor,
	}

	got, err := Payload([]alert.Alert{a}, map[string]string{"summary": "{{ .team }} is slow"})
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d payloads, want 1", len(got))
	}

	p := got[0]
	if p.Labels["team"] != "payments" {
		t.Errorf("labels[team] = %q, want payments", p.Labels["team"])
	}
	if p.Annotations["summary"] != "payments is slow" {
		t.Errorf("summary = %q, want %q", p.Annotations["summary"], "payments is slow")
	}
	if p.StartsAt != payloadAnchor.Format(time.RFC3339Nano) {
		t.Errorf("startsAt = %q, want %q", p.StartsAt, payloadAnchor.Format(time.RFC3339Nano))
	}
	// An endsAt in the future tells Alertmanager the alert is still active.
	// Sending one on a firing alert would resolve it immediately.
	if p.EndsAt != "" {
		t.Errorf("endsAt = %q, want empty while firing", p.EndsAt)
	}
}

func TestPayloadMapsResolvedAlert(t *testing.T) {
	resolved := payloadAnchor.Add(5 * time.Minute)
	a := alert.Alert{
		Labels:     map[string]string{"alertname": "HighP99Latency"},
		Phase:      alert.PhaseResolved,
		FiredAt:    payloadAnchor,
		ResolvedAt: resolved,
	}

	got, err := Payload([]alert.Alert{a}, nil)
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d payloads, want 1", len(got))
	}
	if got[0].EndsAt != resolved.Format(time.RFC3339Nano) {
		t.Errorf("endsAt = %q, want %q", got[0].EndsAt, resolved.Format(time.RFC3339Nano))
	}
}

// Pending is not an alert yet. Prometheus does not notify on it, and sending
// one would page for a condition that has not held for its `for` duration,
// which is the entire point of having a `for`.
func TestPayloadSkipsPending(t *testing.T) {
	a := alert.Alert{
		Labels:   map[string]string{"alertname": "TooSoon"},
		Phase:    alert.PhasePending,
		ActiveAt: payloadAnchor,
	}

	got, err := Payload([]alert.Alert{a}, nil)
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d payloads, want 0 for a pending alert: %+v", len(got), got)
	}
}
