package alert

import (
	"strings"
	"testing"
	"time"
)

// Two rows that reach the same final label set are not one instance. They are
// a rule asking for something it cannot express, and 6.3 says fail the
// evaluation the way Prometheus' ErrDuplicateAlertLabelSet does. Keeping the
// last row silently reports one arbitrary value and discards the rest, during
// an incident, with nothing in the output to say it happened.
//
// The source's labels outrank a result column (6.3.1), which is what makes
// this reachable: a query grouping by a cluster column, against a source whose
// labels already set cluster, collapses every row onto one identity.
func TestEvalFailsWhenTwoRowsReachOneIdentity(t *testing.T) {
	s := New(testRule(0, 0), nil, dc("traces_dc1", "dc1"))

	got, _, err := s.Eval(t0, []Sample{
		{Labels: map[string]string{"ServiceName": "checkout", "cluster": "reported-a"}, Value: 1200},
		{Labels: map[string]string{"ServiceName": "checkout", "cluster": "reported-b"}, Value: 1300},
	})

	if err == nil {
		t.Fatalf("want an error, got alerts: %s", formatAlerts(got))
	}
	if got != nil {
		t.Errorf("want no alerts alongside the error, got %s", formatAlerts(got))
	}

	// The operator has to be able to see which label set collided, or the
	// error names a rule and nothing inside it.
	for _, want := range []string{"ServiceName", "checkout", "cluster", "dc1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// A failed evaluation leaves the state exactly as it was, like a failed query
// does, so a duplicate that appears for one tick cannot reset or half-advance
// anything. Nothing may be tracked from the failed evaluation.
func TestEvalLeavesStateIntactWhenItFails(t *testing.T) {
	s := New(testRule(time.Minute, 0), nil, dc("traces_dc1", "dc1"))

	if _, _, err := s.Eval(t0, []Sample{
		{Labels: map[string]string{"ServiceName": "checkout", "cluster": "reported-a"}, Value: 1200},
		{Labels: map[string]string{"ServiceName": "checkout", "cluster": "reported-b"}, Value: 1300},
	}); err == nil {
		t.Fatal("want an error from two rows reaching one identity")
	}

	// If the failed evaluation had tracked anything, this instance would carry
	// an ActiveAt of t0 and would already be a minute into its `for`.
	later := t0.Add(time.Minute)
	alerts := evalOK(t, s, later, []Sample{sample("checkout", 1200)})

	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1: %s", len(alerts), formatAlerts(alerts))
	}
	if !alerts[0].ActiveAt.Equal(later) {
		t.Errorf("ActiveAt = %v, want %v: the failed evaluation tracked something", alerts[0].ActiveAt, later)
	}
	if alerts[0].Phase != PhasePending {
		t.Errorf("phase = %v, want pending: the failed evaluation advanced a timer", alerts[0].Phase)
	}
}

// 6.3 length prefixes each key and value so no separator inside a ClickHouse
// value can forge a match, which closes the construction of a collision but
// not its arithmetic: the result is 64 bits. Two genuinely different label
// sets that hash alike must stay two alerts, each with its own value and its
// own `for` timer.
//
// A collision cannot be produced by chance, so the hash is replaced for this
// test only. The real fingerprint is untouched.
func TestEvalKeepsCollidingLabelSetsApart(t *testing.T) {
	s := New(testRule(time.Minute, 0), nil, testSource())
	s.hash = func(map[string]string) uint64 { return 1 }

	// checkout starts its `for` timer half a minute before payments does.
	alerts := evalOK(t, s, t0, []Sample{sample("checkout", 1200)})
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1: %s", len(alerts), formatAlerts(alerts))
	}

	alerts = evalOK(t, s, t0.Add(30*time.Second), []Sample{
		sample("checkout", 1200),
		sample("payments", 1300),
	})
	if len(alerts) != 2 {
		t.Fatalf("got %d alerts, want 2, both hashing alike: %s", len(alerts), formatAlerts(alerts))
	}

	// One `for` timer each: checkout has held for a minute, payments for
	// thirty seconds, so only checkout fires.
	alerts = evalOK(t, s, t0.Add(time.Minute), []Sample{
		sample("checkout", 1200),
		sample("payments", 1300),
	})

	phases := map[string]Phase{}
	values := map[string]float64{}
	for _, a := range alerts {
		phases[a.Labels["ServiceName"]] = a.Phase
		values[a.Labels["ServiceName"]] = a.Value
	}
	if len(phases) != 2 {
		t.Fatalf("got %d distinct instances, want 2: %s", len(phases), formatAlerts(alerts))
	}
	if phases["checkout"] != PhaseFiring {
		t.Errorf("checkout phase = %v, want firing", phases["checkout"])
	}
	if phases["payments"] != PhasePending {
		t.Errorf("payments phase = %v, want pending: the timers were merged", phases["payments"])
	}
	if values["checkout"] != 1200 || values["payments"] != 1300 {
		t.Errorf("values = %v, want checkout 1200 and payments 1300", values)
	}
}

// Two rows colliding on the hash are not duplicates, so the duplicate check
// has to compare label sets rather than trust the fingerprint. Without that,
// every collision would be reported as a rule the author has to fix.
func TestEvalDoesNotMistakeACollisionForADuplicate(t *testing.T) {
	s := New(testRule(0, 0), nil, testSource())
	s.hash = func(map[string]string) uint64 { return 1 }

	alerts, _, err := s.Eval(t0, []Sample{sample("checkout", 1200), sample("payments", 1300)})
	if err != nil {
		t.Fatalf("two different label sets sharing a hash are not a duplicate: %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("got %d alerts, want 2: %s", len(alerts), formatAlerts(alerts))
	}
}

// Ordering has to be stable for the same reason it always did: a batch whose
// order changed from tick to tick is needlessly hard to read in a log or a
// diff. Fingerprint order stops being a total order once two instances share
// one, so something has to break the tie.
func TestEvalOrdersCollidingInstancesDeterministically(t *testing.T) {
	forward := New(testRule(0, 0), nil, testSource())
	forward.hash = func(map[string]string) uint64 { return 1 }
	reverse := New(testRule(0, 0), nil, testSource())
	reverse.hash = func(map[string]string) uint64 { return 1 }

	samples := []Sample{sample("checkout", 1200), sample("payments", 1300), sample("cart", 1400)}
	reversed := []Sample{samples[2], samples[1], samples[0]}

	first := evalOK(t, forward, t0, samples)
	second := evalOK(t, reverse, t0, reversed)

	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("got %d and %d alerts, want 3 each", len(first), len(second))
	}
	for i := range first {
		if first[i].Labels["ServiceName"] != second[i].Labels["ServiceName"] {
			t.Fatalf("order depends on the result order:\n%s\nvs\n%s",
				formatAlerts(first), formatAlerts(second))
		}
	}
}
