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
		// Rendered when the alert was evaluated, so Payload passes it through
		// rather than expanding a template here.
		Annotations: map[string]string{"summary": "payments is slow"},
	}

	got := Payload([]alert.Alert{a})
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
	// Without a validity this alert falls back to Alertmanager's own
	// resolve_timeout, which is the behaviour an unset endsAt asks for.
	if p.EndsAt != "" {
		t.Errorf("endsAt = %q, want empty when the alert carries no validity", p.EndsAt)
	}
}

// A firing alert's endsAt is how long Alertmanager should hold it without
// hearing again. Carrying it on the alert is what decouples expiry from
// Alertmanager's resolve_timeout, which the ruler cannot read.
func TestPayloadSendsValidUntilAsEndsAtWhileFiring(t *testing.T) {
	validUntil := payloadAnchor.Add(400 * time.Second)
	a := alert.Alert{
		Labels:     map[string]string{"alertname": "HighP99Latency"},
		Phase:      alert.PhaseFiring,
		FiredAt:    payloadAnchor,
		ValidUntil: validUntil,
	}

	got := Payload([]alert.Alert{a})
	if len(got) != 1 {
		t.Fatalf("got %d payloads, want 1", len(got))
	}
	if got[0].EndsAt != validUntil.Format(time.RFC3339Nano) {
		t.Errorf("endsAt = %q, want %q", got[0].EndsAt, validUntil.Format(time.RFC3339Nano))
	}
}

// A resolved alert ended when it resolved, not when its validity ran out, so
// the validity it was carrying while firing must not override that.
func TestPayloadPrefersResolvedAtOverValidUntil(t *testing.T) {
	resolvedAt := payloadAnchor.Add(time.Minute)
	a := alert.Alert{
		Labels:     map[string]string{"alertname": "HighP99Latency"},
		Phase:      alert.PhaseResolved,
		FiredAt:    payloadAnchor,
		ResolvedAt: resolvedAt,
		ValidUntil: payloadAnchor.Add(400 * time.Second),
	}

	got := Payload([]alert.Alert{a})
	if len(got) != 1 {
		t.Fatalf("got %d payloads, want 1", len(got))
	}
	if got[0].EndsAt != resolvedAt.Format(time.RFC3339Nano) {
		t.Errorf("endsAt = %q, want %q", got[0].EndsAt, resolvedAt.Format(time.RFC3339Nano))
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

	got := Payload([]alert.Alert{a})
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

	got := Payload([]alert.Alert{a})
	if len(got) != 0 {
		t.Fatalf("got %d payloads, want 0 for a pending alert: %+v", len(got), got)
	}
}
