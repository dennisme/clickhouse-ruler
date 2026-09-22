package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// countingQuerier is safe to share across the concurrent groups below.
type countingQuerier struct {
	calls atomic.Int64
}

func (q *countingQuerier) Run(context.Context, rule.Rule, time.Time) ([]alert.Sample, error) {
	q.calls.Add(1)
	return oneSample(), nil
}

// Every group shares one Cadence, and each group runs in its own goroutine,
// so overlapping ticks post through that Cadence at the same time. Left
// unsynchronised this is not a subtle race but a fatal "concurrent map
// writes" abort of the whole process, so it is worth a test at the level it
// actually happens rather than only in notify's own package.
func TestConcurrentGroupsShareOneCadenceSafely(t *testing.T) {
	const groups = 6

	var rules []ruleset.Rule
	for i := 0; i < groups; i++ {
		name := string(rune('a' + i))
		rules = append(rules, ruleset.Rule{
			// for is 0, so every tick produces a firing alert and therefore
			// a write into the shared cadence map.
			Rule:    rule.Rule{Alert: "Rule" + name},
			File:    name + ".yaml",
			Group:   testGroup("g"+name, time.Second),
			Labels:  map[string]string{},
			Sources: []source.Source{{Name: "src1"}},
		})
	}

	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	cadence := notify.NewCadence(&recordingSender{}, time.Millisecond, notify.DefaultResendTolerance)
	clock := newFakeClock(time.Unix(0, 0))
	q := &countingQuerier{}

	sched := New(&ruleset.Set{Rules: rules}, map[string]Querier{"src1": q},
		cadence, metrics, clock, DefaultQueryConcurrency, nil, testResend)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	// Drive the clock forward until every group has ticked several times.
	// Advancing when nothing is waiting is harmless, so this needs no
	// coordination with the group goroutines.
	deadline := time.Now().Add(10 * time.Second)
	for q.calls.Load() < groups*3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d evaluations ran, want at least %d", q.calls.Load(), groups*3)
		}
		clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}

	sched.Shutdown(5 * time.Second)
}
