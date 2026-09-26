package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// barrierQuerier blocks every call until the expected number of calls are in
// flight at the same time. Sequential evaluation can never satisfy it, so a
// test using it fails by timing out rather than by asserting on a duration.
type barrierQuerier struct {
	wg       sync.WaitGroup
	expect   int32
	started  atomic.Int32
	inFlight atomic.Int32
	peak     atomic.Int32
	release  chan struct{}
}

func newBarrierQuerier(expect int) *barrierQuerier {
	b := &barrierQuerier{expect: int32(expect), release: make(chan struct{})}
	b.wg.Add(expect)
	return b
}

func (b *barrierQuerier) Run(ctx context.Context, _ rule.Rule, _ query.Attribution, _ time.Time) ([]alert.Sample, error) {
	n := b.inFlight.Add(1)
	for {
		peak := b.peak.Load()
		if n <= peak || b.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	// More calls than the barrier expects are legitimate when a limit is
	// under test: the extras queue behind it. Only the first `expect` count
	// toward the barrier, or the counter goes negative.
	if b.started.Add(1) <= b.expect {
		b.wg.Done()
	}

	select {
	case <-b.release:
	case <-ctx.Done():
	}
	b.inFlight.Add(-1)
	return oneSample(), nil
}

// waitAllStarted reports whether every expected call was in flight together.
func (b *barrierQuerier) waitAllStarted(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func multiSourceRule(names ...string) ruleset.Rule {
	srcs := make([]source.Source, 0, len(names))
	for _, n := range names {
		srcs = append(srcs, source.Source{Name: n})
	}
	return ruleset.Rule{
		Rule:    rule.Rule{Alert: "TestRule"},
		Labels:  map[string]string{},
		Sources: srcs,
	}
}

// The bottleneck this fixes: a rule matching many sources used to query them
// one at a time, so its tick cost was the sum of every source's latency.
func TestRuleEvalQueriesItsSourcesConcurrently(t *testing.T) {
	const sources = 4

	b := newBarrierQuerier(sources)
	queriers := map[string]Querier{}
	names := []string{"s1", "s2", "s3", "s4"}
	for _, n := range names {
		queriers[n] = b
	}

	eval := NewRuleEval(multiSourceRule(names...), queriers,
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance), newSemaphore(0), testRetention)

	go eval.Evaluate(context.Background(), time.Now())

	if !b.waitAllStarted(5 * time.Second) {
		t.Fatal("sources did not run concurrently: not all queries were in flight together")
	}
	close(b.release)
}

// Concurrency has to stay bounded, or a large group becomes a stampede
// against ClickHouse rather than a staggered load.
func TestRuleEvalRespectsTheQueryConcurrencyLimit(t *testing.T) {
	const limit = 2

	// Only the first `limit` calls are ever in flight together, so the
	// barrier is sized to that rather than to the source count.
	b := newBarrierQuerier(limit)
	names := []string{"s1", "s2", "s3", "s4", "s5", "s6"}
	queriers := map[string]Querier{}
	for _, n := range names {
		queriers[n] = b
	}

	eval := NewRuleEval(multiSourceRule(names...), queriers,
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance), newSemaphore(limit), testRetention)

	done := make(chan Result, 1)
	go func() { done <- eval.Evaluate(context.Background(), time.Now()) }()

	if !b.waitAllStarted(5 * time.Second) {
		t.Fatal("expected the limit to be saturated")
	}
	// Give any unbounded extra goroutine a chance to exceed the limit.
	time.Sleep(100 * time.Millisecond)
	if peak := b.peak.Load(); peak > limit {
		t.Fatalf("peak concurrent queries = %d, want at most %d", peak, limit)
	}

	close(b.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("evaluation did not finish")
	}
}

// Parallel sources must not scramble the alert order, or the same state
// produces a different batch ordering on every tick for no reason.
func TestRuleEvalReturnsSourcesInAStableOrder(t *testing.T) {
	names := []string{"s1", "s2", "s3", "s4"}
	queriers := map[string]Querier{}
	for _, n := range names {
		queriers[n] = &fakeQuerier{samples: oneSample()}
	}

	sender := &recordingSender{}
	eval := NewRuleEval(multiSourceRule(names...), queriers,
		notify.NewCadence(sender, time.Minute, notify.DefaultResendTolerance), newSemaphore(0), testRetention)

	eval.Evaluate(context.Background(), time.Now())

	if len(sender.calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(sender.calls))
	}
	got := sender.calls[0]
	if len(got) != len(names) {
		t.Fatalf("got %d alerts, want one per source", len(got))
	}
	for i, a := range got {
		if want := names[i]; a.Labels["source"] != want {
			t.Errorf("alert %d source = %q, want %q (source order must be stable)", i, a.Labels["source"], want)
		}
	}
}
