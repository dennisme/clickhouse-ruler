package alert

import (
	"testing"
	"time"
)

// A pending instance has never been notified, so its disappearance must not
// produce a resolve for something nobody was told about.
func TestPendingInstanceDisappearsSilently(t *testing.T) {
	s := New(testRule(5*time.Minute, 0), nil, testSource(), testRetention)

	assertAlerts(t, evalOK(t, s, t0, []Sample{sample("checkout", 1200)}), []want{
		{service: "checkout", phase: PhasePending, value: 1200, activeAt: t0},
	})

	assertAlerts(t, evalOK(t, s, t0.Add(time.Minute), nil), nil)
	assertAlerts(t, evalOK(t, s, t0.Add(2*time.Minute), nil), nil)
}

func TestFiringResolvesWhenSampleDisappears(t *testing.T) {
	s := New(testRule(0, 0), nil, testSource(), testRetention)

	assertAlerts(t, evalOK(t, s, t0, []Sample{sample("checkout", 1200)}), []want{
		{service: "checkout", phase: PhaseFiring, value: 1200, activeAt: t0, firedAt: t0},
	})

	resolved := t0.Add(time.Minute)
	assertAlerts(t, evalOK(t, s, resolved, nil), []want{
		{
			service:    "checkout",
			phase:      PhaseResolved,
			value:      1200,
			activeAt:   t0,
			firedAt:    t0,
			resolvedAt: resolved,
		},
	})

	// The resolve is asserted again while it is retained, so a notification
	// that failed has another attempt. Alertmanager deduplicates an identical
	// alert, and the resolve time does not move, so repeating it says the same
	// thing rather than resolving twice.
	assertAlerts(t, evalOK(t, s, t0.Add(2*time.Minute), nil), []want{
		{
			service:    "checkout",
			phase:      PhaseResolved,
			value:      1200,
			activeAt:   t0,
			firedAt:    t0,
			resolvedAt: resolved,
		},
	})
}

func TestKeepFiringForDelaysResolve(t *testing.T) {
	s := New(testRule(0, 5*time.Minute), nil, testSource(), testRetention)

	assertAlerts(t, evalOK(t, s, t0, []Sample{sample("checkout", 1200)}), []want{
		{service: "checkout", phase: PhaseFiring, value: 1200, activeAt: t0, firedAt: t0},
	})

	// Condition is gone but the grace window is not over, so it keeps firing.
	assertAlerts(t, evalOK(t, s, t0.Add(2*time.Minute), nil), []want{
		{service: "checkout", phase: PhaseFiring, value: 1200, activeAt: t0, firedAt: t0},
	})

	resolved := t0.Add(5 * time.Minute)
	assertAlerts(t, evalOK(t, s, resolved, nil), []want{
		{
			service:    "checkout",
			phase:      PhaseResolved,
			value:      1200,
			activeAt:   t0,
			firedAt:    t0,
			resolvedAt: resolved,
		},
	})

	// Retained, so still asserted on the next evaluation.
	assertAlerts(t, evalOK(t, s, t0.Add(6*time.Minute), nil), []want{
		{
			service:    "checkout",
			phase:      PhaseResolved,
			value:      1200,
			activeAt:   t0,
			firedAt:    t0,
			resolvedAt: resolved,
		},
	})
}

// A flapping condition is what keep_firing_for exists to absorb: the sample
// returning inside the window must restart it, not resolve on the old clock.
func TestKeepFiringForRestartsWhenSampleReturns(t *testing.T) {
	s := New(testRule(0, 5*time.Minute), nil, testSource(), testRetention)

	evalOK(t, s, t0, []Sample{sample("checkout", 1200)})
	evalOK(t, s, t0.Add(2*time.Minute), nil)

	back := t0.Add(4 * time.Minute)
	assertAlerts(t, evalOK(t, s, back, []Sample{sample("checkout", 1600)}), []want{
		{service: "checkout", phase: PhaseFiring, value: 1600, activeAt: t0, firedAt: t0},
	})

	// Without the restart this evaluation would be 6m past the original
	// sighting and would resolve.
	assertAlerts(t, evalOK(t, s, t0.Add(6*time.Minute), nil), []want{
		{service: "checkout", phase: PhaseFiring, value: 1600, activeAt: t0, firedAt: t0},
	})

	resolved := t0.Add(9 * time.Minute)
	assertAlerts(t, evalOK(t, s, resolved, nil), []want{
		{
			service:    "checkout",
			phase:      PhaseResolved,
			value:      1600,
			activeAt:   t0,
			firedAt:    t0,
			resolvedAt: resolved,
		},
	})
}
