package alert

import (
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
)

var t0 = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

func testRule(forDuration, keepFiringFor time.Duration) rule.Rule {
	return rule.Rule{
		Alert:         "HighP99Latency",
		For:           forDuration,
		KeepFiringFor: keepFiringFor,
		Labels: map[string]string{
			"team":     "payments",
			"severity": "warning",
		},
	}
}

func sample(service string, value float64) Sample {
	return Sample{
		Labels: map[string]string{"ServiceName": service},
		Value:  value,
	}
}

// want describes one expected alert. Instances are identified by their
// ServiceName label, which is the only one that varies across the fixtures.
type want struct {
	service    string
	phase      Phase
	value      float64
	activeAt   time.Time
	firedAt    time.Time
	resolvedAt time.Time
}

func assertAlerts(t *testing.T, got []Alert, wants []want) {
	t.Helper()

	if len(got) != len(wants) {
		t.Fatalf("got %d alerts, want %d: %s", len(got), len(wants), formatAlerts(got))
	}
	for i, w := range wants {
		a := got[i]
		if a.Labels["ServiceName"] != w.service {
			t.Errorf("alert %d: ServiceName = %q, want %q", i, a.Labels["ServiceName"], w.service)
		}
		if a.Phase != w.phase {
			t.Errorf("alert %d (%s): phase = %v, want %v", i, w.service, a.Phase, w.phase)
		}
		if a.Value != w.value {
			t.Errorf("alert %d (%s): value = %v, want %v", i, w.service, a.Value, w.value)
		}
		if !a.ActiveAt.Equal(w.activeAt) {
			t.Errorf("alert %d (%s): ActiveAt = %v, want %v", i, w.service, a.ActiveAt, w.activeAt)
		}
		if !a.FiredAt.Equal(w.firedAt) {
			t.Errorf("alert %d (%s): FiredAt = %v, want %v", i, w.service, a.FiredAt, w.firedAt)
		}
		if !a.ResolvedAt.Equal(w.resolvedAt) {
			t.Errorf("alert %d (%s): ResolvedAt = %v, want %v", i, w.service, a.ResolvedAt, w.resolvedAt)
		}
	}
}

func formatAlerts(alerts []Alert) string {
	if len(alerts) == 0 {
		return "[]"
	}
	out := ""
	for _, a := range alerts {
		out += "\n  " + a.Labels["ServiceName"] + " " + a.Phase.String()
	}
	return out
}

func TestPendingUntilForElapses(t *testing.T) {
	s := New(testRule(5*time.Minute, 0), nil, testSource())

	assertAlerts(t, s.Eval(t0, []Sample{sample("checkout", 1200)}), []want{
		{service: "checkout", phase: PhasePending, value: 1200, activeAt: t0},
	})

	assertAlerts(t, s.Eval(t0.Add(2*time.Minute), []Sample{sample("checkout", 1300)}), []want{
		{service: "checkout", phase: PhasePending, value: 1300, activeAt: t0},
	})

	// The sample has now been present for exactly the configured duration.
	fired := t0.Add(5 * time.Minute)
	assertAlerts(t, s.Eval(fired, []Sample{sample("checkout", 1400)}), []want{
		{service: "checkout", phase: PhaseFiring, value: 1400, activeAt: t0, firedAt: fired},
	})

	// FiredAt is the moment it first fired, not the latest evaluation.
	assertAlerts(t, s.Eval(t0.Add(6*time.Minute), []Sample{sample("checkout", 1500)}), []want{
		{service: "checkout", phase: PhaseFiring, value: 1500, activeAt: t0, firedAt: fired},
	})
}

func TestForZeroFiresOnFirstEvaluation(t *testing.T) {
	s := New(testRule(0, 0), nil, testSource())

	assertAlerts(t, s.Eval(t0, []Sample{sample("checkout", 1200)}), []want{
		{service: "checkout", phase: PhaseFiring, value: 1200, activeAt: t0, firedAt: t0},
	})
}

func TestFinalLabelsMergeRuleAndSample(t *testing.T) {
	s := New(testRule(0, 0), nil, testSource())

	got := s.Eval(t0, []Sample{sample("checkout", 1200)})
	if len(got) != 1 {
		t.Fatalf("got %d alerts, want 1", len(got))
	}

	for key, want := range map[string]string{
		"alertname":   "HighP99Latency",
		"team":        "payments",
		"severity":    "warning",
		"ServiceName": "checkout",
	} {
		if got[0].Labels[key] != want {
			t.Errorf("label %q = %q, want %q", key, got[0].Labels[key], want)
		}
	}

	if got[0].Fingerprint != fingerprint(got[0].Labels) {
		t.Error("Fingerprint does not match the alert's own label set")
	}
}
