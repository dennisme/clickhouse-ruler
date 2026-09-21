package scheduler

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

func testGroup(name string, interval time.Duration) rule.Group {
	return rule.Group{Name: name, Interval: interval}
}

// A rule matching no source is not evaluated, and 6.10 says that has to be
// visible rather than silent: it must show up in ruler_rules_unmatched.
func TestNewCountsRulesWithNoMatchedSourceAsUnmatched(t *testing.T) {
	set := &ruleset.Set{
		Rules: []ruleset.Rule{
			{Rule: rule.Rule{Alert: "Matched"}, File: "f.yaml", Group: testGroup("g1", time.Minute),
				Labels: map[string]string{}, Sources: []source.Source{{Name: "src1"}}},
			{Rule: rule.Rule{Alert: "Unmatched"}, File: "f.yaml", Group: testGroup("g1", time.Minute),
				Labels: map[string]string{}},
		},
	}

	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	cadence := notify.NewCadence(&recordingSender{}, time.Minute)
	clock := newFakeClock(time.Unix(0, 0))

	New(set, map[string]Querier{"src1": &fakeQuerier{}}, cadence, metrics, clock, 0)

	got := testutil.ToFloat64(metrics.RulesUnmatched.WithLabelValues("f.yaml:g1"))
	if got != 1 {
		t.Fatalf("ruler_rules_unmatched = %v, want 1", got)
	}
}

// Twenty groups on the same interval must not all land on the same tick, or
// they stampede ClickHouse together. Two distinct groups on the same
// interval have to get different first ticks.
func TestNewStaggersGroupsSharingAnInterval(t *testing.T) {
	set := &ruleset.Set{
		Rules: []ruleset.Rule{
			{Rule: rule.Rule{Alert: "A"}, File: "f.yaml", Group: testGroup("g1", time.Minute),
				Labels: map[string]string{}, Sources: []source.Source{{Name: "src1"}}},
			{Rule: rule.Rule{Alert: "B"}, File: "f.yaml", Group: testGroup("g2", time.Minute),
				Labels: map[string]string{}, Sources: []source.Source{{Name: "src1"}}},
		},
	}

	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	cadence := notify.NewCadence(&recordingSender{}, time.Minute)
	clock := newFakeClock(time.Unix(0, 0))

	sched := New(set, map[string]Querier{"src1": &fakeQuerier{}}, cadence, metrics, clock, 0)

	if len(sched.groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(sched.groups))
	}
	if sched.groups[0].Start.Equal(sched.groups[1].Start) {
		t.Fatalf("both groups start at %v, want staggered starts", sched.groups[0].Start)
	}
	for _, g := range sched.groups {
		if g.Start.Before(clock.Now()) || !g.Start.Before(clock.Now().Add(time.Minute)) {
			t.Fatalf("start %v is not within one interval of now", g.Start)
		}
	}
}

// Shutdown before Start must not panic: a caller that builds a Scheduler and
// decides not to run it should still be able to clean up unconditionally.
func TestShutdownBeforeStartIsANoop(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	cadence := notify.NewCadence(&recordingSender{}, time.Minute)
	clock := newFakeClock(time.Unix(0, 0))

	sched := New(&ruleset.Set{}, map[string]Querier{}, cadence, metrics, clock, 0)
	sched.Shutdown(time.Second)
}
