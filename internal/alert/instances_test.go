package alert

import (
	"testing"
	"time"
)

// assertAlertSet checks alerts by their ServiceName rather than by position,
// so a test about instance behaviour does not also depend on return order.
func assertAlertSet(t *testing.T, got []Alert, wants map[string]want) {
	t.Helper()

	if len(got) != len(wants) {
		t.Fatalf("got %d alerts, want %d: %s", len(got), len(wants), formatAlerts(got))
	}

	byService := make(map[string]Alert, len(got))
	for _, a := range got {
		byService[a.Labels["ServiceName"]] = a
	}

	for service, w := range wants {
		a, ok := byService[service]
		if !ok {
			t.Errorf("no alert for service %q, got: %s", service, formatAlerts(got))
			continue
		}
		if a.Phase != w.phase {
			t.Errorf("%s: phase = %v, want %v", service, a.Phase, w.phase)
		}
		if a.Value != w.value {
			t.Errorf("%s: value = %v, want %v", service, a.Value, w.value)
		}
		if !a.ActiveAt.Equal(w.activeAt) {
			t.Errorf("%s: ActiveAt = %v, want %v", service, a.ActiveAt, w.activeAt)
		}
		if !a.FiredAt.Equal(w.firedAt) {
			t.Errorf("%s: FiredAt = %v, want %v", service, a.FiredAt, w.firedAt)
		}
		if !a.ResolvedAt.Equal(w.resolvedAt) {
			t.Errorf("%s: ResolvedAt = %v, want %v", service, a.ResolvedAt, w.resolvedAt)
		}
	}
}

// One row is one alert instance, so each row carries its own for timer and
// resolves on its own. This is the behaviour the row-per-instance model exists
// for and that a single scalar threshold cannot express.
func TestInstancesAdvanceIndependently(t *testing.T) {
	s := New(testRule(5*time.Minute, 0), nil, testSource())

	assertAlertSet(t, evalOK(t, s, t0, []Sample{sample("checkout", 1200)}), map[string]want{
		"checkout": {phase: PhasePending, value: 1200, activeAt: t0},
	})

	// cart starts its own for timer two minutes behind checkout.
	cartActive := t0.Add(2 * time.Minute)
	assertAlertSet(t, evalOK(t, s, cartActive, []Sample{
		sample("checkout", 1250),
		sample("cart", 1100),
	}), map[string]want{
		"checkout": {phase: PhasePending, value: 1250, activeAt: t0},
		"cart":     {phase: PhasePending, value: 1100, activeAt: cartActive},
	})

	// checkout has waited its full for, cart has not.
	checkoutFired := t0.Add(5 * time.Minute)
	assertAlertSet(t, evalOK(t, s, checkoutFired, []Sample{
		sample("checkout", 1300),
		sample("cart", 1150),
	}), map[string]want{
		"checkout": {phase: PhaseFiring, value: 1300, activeAt: t0, firedAt: checkoutFired},
		"cart":     {phase: PhasePending, value: 1150, activeAt: cartActive},
	})

	// checkout recovers while cart crosses its own threshold. One resolves,
	// the other fires, on the same evaluation.
	cartFired := t0.Add(7 * time.Minute)
	assertAlertSet(t, evalOK(t, s, cartFired, []Sample{sample("cart", 1200)}), map[string]want{
		"checkout": {
			phase:      PhaseResolved,
			value:      1300,
			activeAt:   t0,
			firedAt:    checkoutFired,
			resolvedAt: cartFired,
		},
		"cart": {phase: PhaseFiring, value: 1200, activeAt: cartActive, firedAt: cartFired},
	})

	// Only cart is left, and checkout is not resolved a second time.
	assertAlertSet(t, evalOK(t, s, t0.Add(8*time.Minute), []Sample{sample("cart", 1250)}), map[string]want{
		"cart": {phase: PhaseFiring, value: 1250, activeAt: cartActive, firedAt: cartFired},
	})
}

func TestReturnOrderIsSortedByFingerprint(t *testing.T) {
	s := New(testRule(0, 0), nil, testSource())

	got := evalOK(t, s, t0, []Sample{
		sample("checkout", 1),
		sample("cart", 2),
		sample("search", 3),
		sample("payments", 4),
		sample("shipping", 5),
	})

	if len(got) != 5 {
		t.Fatalf("got %d alerts, want 5", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Fingerprint > got[i].Fingerprint {
			t.Errorf("alert %d fingerprint %d precedes %d, which is out of order",
				i-1, got[i-1].Fingerprint, got[i].Fingerprint)
		}
	}
}

// Two states fed the same samples must agree, including when the samples
// arrive in a different order.
func TestEvalIsDeterministicAcrossStates(t *testing.T) {
	samples := []Sample{
		sample("checkout", 1),
		sample("cart", 2),
		sample("search", 3),
	}
	reversed := []Sample{samples[2], samples[1], samples[0]}

	first := evalOK(t, New(testRule(0, 0), nil, testSource()), t0, samples)
	second := evalOK(t, New(testRule(0, 0), nil, testSource()), t0, reversed)

	if len(first) != len(second) {
		t.Fatalf("lengths differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Fingerprint != second[i].Fingerprint {
			t.Errorf("position %d: %d vs %d", i, first[i].Fingerprint, second[i].Fingerprint)
		}
		if first[i].Labels["ServiceName"] != second[i].Labels["ServiceName"] {
			t.Errorf("position %d: %q vs %q", i,
				first[i].Labels["ServiceName"], second[i].Labels["ServiceName"])
		}
	}
}

// Group labels are the weakest, the rule's own labels override them, and a
// result column overrides both.
func TestLabelPrecedence(t *testing.T) {
	r := testRule(0, 0)
	r.Labels["tier"] = "rule"
	r.Labels["severity"] = "warning"

	s := New(r, map[string]string{"tier": "group", "region": "us-east"}, testSource())

	got := evalOK(t, s, t0, []Sample{{
		Labels: map[string]string{"ServiceName": "checkout", "severity": "critical"},
		Value:  1200,
	}})
	if len(got) != 1 {
		t.Fatalf("got %d alerts, want 1", len(got))
	}

	for key, want := range map[string]string{
		"region":   "us-east",
		"tier":     "rule",
		"severity": "critical",
	} {
		if got[0].Labels[key] != want {
			t.Errorf("label %q = %q, want %q", key, got[0].Labels[key], want)
		}
	}
}
