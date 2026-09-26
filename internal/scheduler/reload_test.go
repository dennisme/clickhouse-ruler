package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// reloadRule is one rule in one group, with the knobs these tests vary: how
// long it waits before firing and what labels it carries, which is what decides
// whether a reload is looking at the same alert.
func reloadRule(group, alertName string, forDur time.Duration, labels map[string]string) ruleset.Rule {
	if labels == nil {
		labels = map[string]string{}
	}
	return ruleset.Rule{
		Rule:    rule.Rule{Alert: alertName, For: forDur},
		File:    "f.yaml",
		Group:   testGroup(group, time.Minute),
		Labels:  labels,
		Sources: []source.Source{{Name: "src1"}},
	}
}

// reloadSched builds a Scheduler the way run does, with a registry a test can
// read back and a fake clock it can drive.
func reloadSched(t *testing.T, set *ruleset.Set, queriers map[string]Querier) (*Scheduler, *Metrics, *prometheus.Registry, *fakeClock) {
	t.Helper()

	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	clock := newFakeClock(time.Unix(0, 0))
	cadence := notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance)

	return New(set, queriers, cadence, metrics, clock, 0, nil, testResend), metrics, reg, clock
}

// evalAll runs every group once at tickAt, which is what the tests that are
// about state rather than about ticking need.
func evalAll(s *Scheduler, tickAt time.Time) {
	for _, g := range s.groups {
		g.Eval(context.Background(), tickAt)
	}
}

// The whole reason a reload beats a restart: a rule nobody edited keeps the
// instances it is tracking, so an alert part way through its `for` does not
// start again from zero (spec.md open question 2).
func TestReloadKeepsPendingStateForAnUnchangedRule(t *testing.T) {
	set := &ruleset.Set{Rules: []ruleset.Rule{reloadRule("g1", "Slow", 5*time.Minute, nil)}}
	queriers := map[string]Querier{"src1": &fakeQuerier{samples: oneSample()}}

	sched, metrics, _, clock := reloadSched(t, set, queriers)
	evalAll(sched, clock.Now())

	if got := testutil.ToFloat64(metrics.AlertsActive.WithLabelValues("f.yaml:g1", "Slow", "pending")); got != 1 {
		t.Fatalf("pending before the reload = %v, want 1", got)
	}

	// The same rule, re-read from disk: a fresh Rule value that happens to say
	// exactly what the running one says.
	reloaded := &ruleset.Set{Rules: []ruleset.Rule{reloadRule("g1", "Slow", 5*time.Minute, nil)}}
	sched.Reload(reloaded, queriers)

	// One `for` on from the first evaluation. Firing only if ActiveAt survived.
	evalAll(sched, clock.Now().Add(5*time.Minute))

	if got := testutil.ToFloat64(metrics.AlertsActive.WithLabelValues("f.yaml:g1", "Slow", "firing")); got != 1 {
		t.Errorf("firing after the reload = %v, want 1: the instance lost its ActiveAt", got)
	}
}

// The other half of spec 6.3: a rule whose labels changed is a different
// alert, so the instances tracked under the old label set are not carried
// over. They would never be seen again, and an alert nothing resolves is worse
// than an alert that starts its `for` again.
func TestReloadDropsStateWhenTheLabelsChanged(t *testing.T) {
	set := &ruleset.Set{Rules: []ruleset.Rule{reloadRule("g1", "Slow", 5*time.Minute, map[string]string{"team": "payments"})}}
	queriers := map[string]Querier{"src1": &fakeQuerier{samples: oneSample()}}

	sched, metrics, _, clock := reloadSched(t, set, queriers)
	evalAll(sched, clock.Now())

	relabelled := &ruleset.Set{Rules: []ruleset.Rule{reloadRule("g1", "Slow", 5*time.Minute, map[string]string{"team": "checkout"})}}
	sched.Reload(relabelled, queriers)

	evalAll(sched, clock.Now().Add(5*time.Minute))

	if got := testutil.ToFloat64(metrics.AlertsActive.WithLabelValues("f.yaml:g1", "Slow", "pending")); got != 1 {
		t.Errorf("pending after the reload = %v, want 1: a relabelled rule is a new alert", got)
	}
	if got := testutil.ToFloat64(metrics.AlertsActive.WithLabelValues("f.yaml:g1", "Slow", "firing")); got != 0 {
		t.Errorf("firing after the reload = %v, want 0", got)
	}
}

// A series left at its last value reads as a rule that still runs. Nothing
// says otherwise on a dashboard: the gauge holds, the counters stop moving,
// and both look exactly like a rule that has gone quiet.
func TestReloadDeletesTheSeriesOfAGroupThatIsGone(t *testing.T) {
	set := &ruleset.Set{Rules: []ruleset.Rule{
		reloadRule("g1", "Kept", 0, nil),
		reloadRule("g2", "Removed", 0, nil),
	}}
	queriers := map[string]Querier{"src1": &fakeQuerier{samples: oneSample()}}

	sched, _, reg, clock := reloadSched(t, set, queriers)
	evalAll(sched, clock.Now())

	if !hasSeriesFor(t, reg, "rule_group", "f.yaml:g2") {
		t.Fatal("no series for the group about to be removed, so this test proves nothing")
	}

	sched.Reload(&ruleset.Set{Rules: []ruleset.Rule{reloadRule("g1", "Kept", 0, nil)}}, queriers)

	if hasSeriesFor(t, reg, "rule_group", "f.yaml:g2") {
		t.Error("series for f.yaml:g2 survived a reload that removed the group")
	}
	if !hasSeriesFor(t, reg, "rule_group", "f.yaml:g1") {
		t.Error("series for f.yaml:g1 were deleted, but the group is still loaded")
	}
}

// One rule out of a group that stays: the group keeps its own series and the
// rule loses its own, including the query cost series, which carry the rule
// without the group.
func TestReloadDeletesTheSeriesOfARuleThatIsGone(t *testing.T) {
	set := &ruleset.Set{Rules: []ruleset.Rule{
		reloadRule("g1", "Kept", 0, nil),
		reloadRule("g1", "Removed", 0, nil),
	}}
	queriers := map[string]Querier{"src1": &fakeQuerier{samples: oneSample()}}

	sched, metrics, reg, clock := reloadSched(t, set, queriers)
	evalAll(sched, clock.Now())
	metrics.QueryReadRowsTotal.WithLabelValues("Removed", "payments").Add(1)
	metrics.QueryDuration.WithLabelValues("Removed").Observe(0.1)

	sched.Reload(&ruleset.Set{Rules: []ruleset.Rule{reloadRule("g1", "Kept", 0, nil)}}, queriers)

	if hasSeriesFor(t, reg, "rule", "Removed") {
		t.Error("series for the Removed rule survived a reload that dropped it")
	}
	if !hasSeriesFor(t, reg, "rule", "Kept") {
		t.Error("series for the Kept rule were deleted, but the rule is still loaded")
	}
}

// An alert name may repeat across groups (spec 7.6), so a rule dropped from
// one group must not take another group's cost series with it: those carry the
// rule and no group, and the name is all they have to go on.
func TestReloadKeepsTheCostSeriesOfARuleNameStillLoadedElsewhere(t *testing.T) {
	shared := reloadRule("g2", "Shared", 0, nil)
	shared.File = "b.yaml"

	set := &ruleset.Set{Rules: []ruleset.Rule{reloadRule("g1", "Shared", 0, nil), shared}}
	queriers := map[string]Querier{"src1": &fakeQuerier{samples: oneSample()}}

	sched, metrics, reg, clock := reloadSched(t, set, queriers)
	evalAll(sched, clock.Now())
	metrics.QueryDuration.WithLabelValues("Shared").Observe(0.1)

	sched.Reload(&ruleset.Set{Rules: []ruleset.Rule{shared}}, queriers)

	if !hasSeriesFor(t, reg, "rule", "Shared") {
		t.Error("cost series for Shared were deleted, but b.yaml:g2 still holds a rule by that name")
	}
	if hasSeriesFor(t, reg, "rule_group", "f.yaml:g1") {
		t.Error("series for f.yaml:g1 survived a reload that removed the group")
	}
}

// A group the reload added has to start ticking on its own, or the rules in it
// are loaded and never evaluated.
func TestReloadStartsTickingAnAddedGroup(t *testing.T) {
	queriers := map[string]Querier{"src1": &fakeQuerier{samples: oneSample()}}
	sched, metrics, _, clock := reloadSched(t, &ruleset.Set{Rules: []ruleset.Rule{reloadRule("g1", "Kept", 0, nil)}}, queriers)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	sched.Reload(&ruleset.Set{Rules: []ruleset.Rule{
		reloadRule("g1", "Kept", 0, nil),
		reloadRule("g2", "Added", 0, nil),
	}}, queriers)

	// Two intervals covers the added group's stagger offset, which is at most
	// one interval, plus the tick that follows it.
	waitFor(t, func() bool {
		clock.Advance(2 * time.Minute)
		return testutil.ToFloat64(metrics.IterationsTotal.WithLabelValues("f.yaml:g2")) > 0
	})
}

// The hazard a reload has to rule out: a querier closed while an evaluation is
// still using it. Reload is what makes closing safe, so it must not return
// while any evaluation of the old configuration is still running.
func TestReloadWaitsForAnEvaluationInFlight(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	q := &blockingQuerier{started: started, release: release}
	queriers := map[string]Querier{"src1": q}

	set := &ruleset.Set{Rules: []ruleset.Rule{reloadRule("g1", "Slow", 0, nil)}}
	sched, _, _, clock := reloadSched(t, set, queriers)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	// Drive the clock until the group's first tick lands and its query blocks.
	waitFor(t, func() bool {
		clock.Advance(time.Minute)
		select {
		case <-started:
			return true
		default:
			return false
		}
	})

	done := make(chan struct{})
	go func() {
		sched.Reload(set, queriers)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Reload returned while an evaluation still held a querier")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Reload did not return after the evaluation finished")
	}
}

// hasSeriesFor reports whether the registry holds any series carrying label
// name with this value, which is how a test asks whether a group or a rule
// still appears on /metrics at all.
func hasSeriesFor(t *testing.T, reg *prometheus.Registry, name, value string) bool {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == name && l.GetValue() == value {
					return true
				}
			}
		}
	}
	return false
}

// waitFor polls cond until it holds, because the goroutines a Scheduler starts
// reach the fake clock's waiters when the Go scheduler gets to them rather than
// when a test would like them to.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}
