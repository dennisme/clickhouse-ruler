package alert

import (
	"testing"
	"time"
)

// A resolve is the one notification with no retry behind it: once the instance
// is gone the ruler cannot say it happened again. So a resolved instance stays
// tracked, and keeps being returned, for as long as delivery could still be
// failing.
func TestResolvedInstanceIsRetainedAndKeptReturned(t *testing.T) {
	s := New(testRule(0, 0), nil, testSource(), 10*time.Minute)

	evalOK(t, s, t0, []Sample{sample("checkout", 1200)})

	resolvedAt := t0.Add(time.Minute)
	first := evalOK(t, s, resolvedAt, nil)
	assertAlerts(t, first, []want{
		{service: "checkout", phase: PhaseResolved, value: 1200,
			activeAt: t0, firedAt: t0, resolvedAt: resolvedAt},
	})

	// Still returned on later evaluations, with the same resolve time: the
	// resolve did not happen again, it is being asserted again.
	for _, after := range []time.Duration{time.Minute, 5 * time.Minute, 9 * time.Minute} {
		again := evalOK(t, s, resolvedAt.Add(after), nil)
		assertAlerts(t, again, []want{
			{service: "checkout", phase: PhaseResolved, value: 1200,
				activeAt: t0, firedAt: t0, resolvedAt: resolvedAt},
		})
	}
}

// Retention is a window, not forever: an instance nobody has queried for
// minutes is not worth memory for the life of the process.
func TestResolvedInstanceLeavesOnceRetentionPasses(t *testing.T) {
	const retention = 10 * time.Minute
	s := New(testRule(0, 0), nil, testSource(), retention)

	evalOK(t, s, t0, []Sample{sample("checkout", 1200)})

	resolvedAt := t0.Add(time.Minute)
	evalOK(t, s, resolvedAt, nil)

	// One tick inside the window, one past it.
	if got := evalOK(t, s, resolvedAt.Add(retention), nil); len(got) != 1 {
		t.Fatalf("got %d alerts at the edge of the window, want 1: %s", len(got), formatAlerts(got))
	}
	if got := evalOK(t, s, resolvedAt.Add(retention+time.Second), nil); got != nil {
		t.Errorf("want nothing once the window passed, got %s", formatAlerts(got))
	}
	if n := s.tracked(); n != 0 {
		t.Errorf("%d instances still tracked, want 0: the window has to free them", n)
	}
}

// A condition that comes back during the window is a new alert, not the old one
// continuing. Its `for` timer starts again, because the condition has to hold
// for that long before it pages a second time.
func TestResolvedInstanceThatRecursStartsFresh(t *testing.T) {
	s := New(testRule(time.Minute, 0), nil, testSource(), 10*time.Minute)

	// Fires after its `for`, then resolves.
	evalOK(t, s, t0, []Sample{sample("checkout", 1200)})
	evalOK(t, s, t0.Add(time.Minute), []Sample{sample("checkout", 1200)})
	resolvedAt := t0.Add(2 * time.Minute)
	evalOK(t, s, resolvedAt, nil)

	// Back again, well inside the retention window.
	recurred := resolvedAt.Add(time.Minute)
	got := evalOK(t, s, recurred, []Sample{sample("checkout", 1300)})

	assertAlerts(t, got, []want{
		{service: "checkout", phase: PhasePending, value: 1300, activeAt: recurred},
	})
}

// A retained resolve is re-sent for minutes, and every one of those sends has
// to say what the alert said when it fired rather than describing a condition
// that has already gone away.
func TestRetainedResolveKeepsItsFiringAnnotations(t *testing.T) {
	s := New(annotatedRule(map[string]string{
		"summary": "p99 is {{ .value }}ms",
	}), nil, testSource(), 10*time.Minute)

	evalOK(t, s, t0, []Sample{sample("checkout", 1200)})

	resolvedAt := t0.Add(time.Minute)
	evalOK(t, s, resolvedAt, nil)

	for _, after := range []time.Duration{time.Minute, 5 * time.Minute} {
		got := evalOK(t, s, resolvedAt.Add(after), nil)
		if len(got) != 1 {
			t.Fatalf("got %d alerts, want 1", len(got))
		}
		if want := "p99 is 1200ms"; got[0].Annotations["summary"] != want {
			t.Errorf("after %s: summary = %q, want %q", after, got[0].Annotations["summary"], want)
		}
	}
}

// A pending instance was never notified, so there is nothing to retry and
// nothing to retain. Keeping it would hold memory for an alert nobody heard
// about.
func TestPendingInstanceIsNotRetained(t *testing.T) {
	s := New(testRule(5*time.Minute, 0), nil, testSource(), 10*time.Minute)

	evalOK(t, s, t0, []Sample{sample("checkout", 1200)})
	if got := evalOK(t, s, t0.Add(time.Minute), nil); got != nil {
		t.Errorf("want nothing, got %s", formatAlerts(got))
	}
	if n := s.tracked(); n != 0 {
		t.Errorf("%d instances tracked, want 0", n)
	}
}
