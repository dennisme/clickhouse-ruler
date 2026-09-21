package notify

import (
	"context"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

// recordingSender stands in for a Client, so cadence logic can be tested
// without an HTTP server or the real retry path.
type recordingSender struct {
	calls [][]alert.Alert
	err   error
}

func (s *recordingSender) Send(_ context.Context, alerts []alert.Alert, _ map[string]string) error {
	s.calls = append(s.calls, alerts)
	return s.err
}

func firing(fp uint64) alert.Alert {
	return alert.Alert{Fingerprint: fp, Phase: alert.PhaseFiring, FiredAt: payloadAnchor}
}

func resolved(fp uint64) alert.Alert {
	return alert.Alert{Fingerprint: fp, Phase: alert.PhaseResolved, FiredAt: payloadAnchor, ResolvedAt: payloadAnchor}
}

// A firing alert must reach Alertmanager the first time Cadence sees it,
// otherwise nothing would ever fire.
func TestCadenceSendsAFiringAlertTheFirstTime(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute)

	now := time.Now()
	if err := c.Send(context.Background(), now, []alert.Alert{firing(1)}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(s.calls) != 1 || len(s.calls[0]) != 1 {
		t.Fatalf("got %v, want one call with one alert", s.calls)
	}
}

// Re-sending on every evaluation is the traffic problem 6.5 exists to avoid.
// A firing alert seen again before its cadence interval elapses must not be
// re-posted.
func TestCadenceDropsAFiringAlertBeforeItsCadenceElapses(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute)

	now := time.Now()
	if err := c.Send(context.Background(), now, []alert.Alert{firing(1)}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Well inside the one minute cadence.
	if err := c.Send(context.Background(), now.Add(10*time.Second), []alert.Alert{firing(1)}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(s.calls) != 1 {
		t.Fatalf("got %d calls, want 1: the second evaluation should not have posted", len(s.calls))
	}
}

// Alertmanager expires a firing alert after resolve_timeout, so once the
// cadence interval elapses the alert has to go out again to keep it alive.
func TestCadenceResendsAFiringAlertOnceItsCadenceElapses(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute)

	now := time.Now()
	if err := c.Send(context.Background(), now, []alert.Alert{firing(1)}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := c.Send(context.Background(), now.Add(time.Minute), []alert.Alert{firing(1)}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(s.calls) != 2 {
		t.Fatalf("got %d calls, want 2: the alert should have been resent once its cadence elapsed", len(s.calls))
	}
}

// A resolved alert has to reach Alertmanager regardless of cadence: it is
// notified once, by construction of alert.State, and holding it back would
// leave Alertmanager showing an alert that is no longer true.
func TestCadenceAlwaysSendsAResolvedAlert(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute)

	now := time.Now()
	if err := c.Send(context.Background(), now, []alert.Alert{firing(1)}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Resolved arrives on the very next evaluation, well inside the cadence.
	if err := c.Send(context.Background(), now.Add(time.Second), []alert.Alert{resolved(1)}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(s.calls) != 2 {
		t.Fatalf("got %d calls, want 2: the resolve must not be held back by cadence", len(s.calls))
	}
	if len(s.calls[1]) != 1 || s.calls[1][0].Phase != alert.PhaseResolved {
		t.Fatalf("second call = %v, want the resolved alert", s.calls[1])
	}
}

// A send failure must not be recorded as sent, so the outage is retried on
// the next evaluation rather than waiting out a full cadence interval. This
// is what keeps a notification failure from losing the alert (spec 6.5).
func TestCadenceRetriesAfterASendFailureWithoutWaitingForCadence(t *testing.T) {
	s := &recordingSender{err: context.DeadlineExceeded}
	c := NewCadence(s, time.Minute)

	now := time.Now()
	if err := c.Send(context.Background(), now, []alert.Alert{firing(1)}, nil); err == nil {
		t.Fatal("want error from a failing sender")
	}

	s.err = nil
	if err := c.Send(context.Background(), now.Add(time.Second), []alert.Alert{firing(1)}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(s.calls) != 2 || len(s.calls[1]) != 1 {
		t.Fatalf("got %v, want the retry to include the alert", s.calls)
	}
}
