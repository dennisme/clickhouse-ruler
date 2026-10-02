package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
)

// cadenceRule is one rule in one group on the interval a case is about.
func cadenceRule(group string, interval time.Duration) ruleset.Rule {
	r := reloadRule(group, "Slow", 0, nil)
	r.Group = testGroup(group, interval)
	return r
}

// tickDelaySum is the seconds of lateness observed for a group so far. Only
// the sum, because a histogram carries no maximum and the sum only grows, so
// a test can wait on it.
func tickDelaySum(t *testing.T, reg *prometheus.Registry, group string) float64 {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "clickhouse_ruler_rule_group_tick_delay_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "rule_group" && l.GetValue() == group {
					return m.GetHistogram().GetSampleSum()
				}
			}
		}
	}
	return 0
}

// Every cadence expression is read against the group's configured interval, so
// the interval has to be a series rather than a number an operator hardcodes
// out of the rule file (spec 8.8).
func TestNewReportsTheConfiguredIntervalOfEachGroup(t *testing.T) {
	set := &ruleset.Set{Rules: []ruleset.Rule{
		cadenceRule("g1", 30*time.Second),
		cadenceRule("g2", 5*time.Minute),
	}}
	queriers := map[string]Querier{"src1": &fakeQuerier{samples: oneSample()}}

	_, metrics, _, _ := reloadSched(t, set, queriers)

	if got := testutil.ToFloat64(metrics.GroupInterval.WithLabelValues("f.yaml:g1")); got != 30 {
		t.Errorf("interval of f.yaml:g1 = %v, want 30", got)
	}
	if got := testutil.ToFloat64(metrics.GroupInterval.WithLabelValues("f.yaml:g2")); got != 300 {
		t.Errorf("interval of f.yaml:g2 = %v, want 300", got)
	}
}

// The gauge is set from the configuration on every load, not once at first
// start, or a group whose interval was edited reports the one it started on and
// every expression read against it is wrong by the size of the edit (spec 8.8).
func TestReloadReportsAChangedInterval(t *testing.T) {
	queriers := map[string]Querier{"src1": &fakeQuerier{samples: oneSample()}}
	set := &ruleset.Set{Rules: []ruleset.Rule{cadenceRule("g1", time.Minute)}}

	sched, metrics, _, _ := reloadSched(t, set, queriers)

	sched.Reload(&ruleset.Set{Rules: []ruleset.Rule{cadenceRule("g1", 4*time.Minute)}}, queriers)

	if got := testutil.ToFloat64(metrics.GroupInterval.WithLabelValues("f.yaml:g1")); got != 240 {
		t.Errorf("interval of f.yaml:g1 after the reload = %v, want 240", got)
	}
}

// A gauge asserting an interval nothing runs is the stuck series every other
// rule_group metric is deleted to avoid (spec 8.2).
func TestReloadDeletesTheCadenceSeriesOfAGroupThatIsGone(t *testing.T) {
	set := &ruleset.Set{Rules: []ruleset.Rule{
		cadenceRule("g1", time.Minute),
		cadenceRule("g2", time.Minute),
	}}
	queriers := map[string]Querier{"src1": &fakeQuerier{samples: oneSample()}}

	sched, metrics, _, _ := reloadSched(t, set, queriers)
	metrics.TickDelay.WithLabelValues("f.yaml:g2").Observe(1)

	if got := testutil.CollectAndCount(metrics.GroupInterval); got != 2 {
		t.Fatalf("interval series before the reload = %d, want 2", got)
	}

	sched.Reload(&ruleset.Set{Rules: []ruleset.Rule{cadenceRule("g1", time.Minute)}}, queriers)

	if got := testutil.CollectAndCount(metrics.GroupInterval); got != 1 {
		t.Errorf("interval series after the reload = %d, want 1: f.yaml:g2 left a gauge behind", got)
	}
	if got := testutil.CollectAndCount(metrics.TickDelay); got != 0 {
		t.Errorf("tick delay series after the reload = %d, want 0", got)
	}
}

// A tick woken on schedule can still start late, waiting on the ruler-wide
// concurrency cap or on the source's own, and nothing else measures that: the
// iterations missed counter reads healthy for the whole of it (spec 8.8).
func TestTickDelayObservesALateStart(t *testing.T) {
	set := &ruleset.Set{Rules: []ruleset.Rule{cadenceRule("g1", time.Minute)}}
	queriers := map[string]Querier{"src1": &fakeQuerier{samples: oneSample()}}

	sched, _, reg, clock := reloadSched(t, set, queriers)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	// Two intervals at a time, so the clock is already a whole interval past
	// the tick time by the moment the group is woken: the lateness a ruler
	// held up behind a query queue has, without a test that sleeps.
	waitFor(t, func() bool {
		clock.Advance(2 * time.Minute)
		return tickDelaySum(t, reg, "f.yaml:g1") >= 60
	})
}
