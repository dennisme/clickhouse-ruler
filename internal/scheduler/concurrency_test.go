package scheduler

import (
	"context"
	"errors"
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

// barrierTimeout is how long a test waits for the queries it expects to be
// in flight together. Long enough that a loaded machine does not fail a
// working limit, short enough that a broken one fails the run rather than
// hanging it.
const barrierTimeout = 5 * time.Second

// waitAllStarted reports whether every expected call was in flight together.
func (b *barrierQuerier) waitAllStarted() bool {
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(barrierTimeout):
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
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance), newQueryLimits(0, nil), testRetention)

	go eval.Evaluate(context.Background(), time.Now())

	if !b.waitAllStarted() {
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
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance), newQueryLimits(limit, nil), testRetention)

	done := make(chan Result, 1)
	go func() { done <- eval.Evaluate(context.Background(), time.Now()) }()

	if !b.waitAllStarted() {
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
		notify.NewCadence(sender, time.Minute, notify.DefaultResendTolerance), newQueryLimits(0, nil), testRetention)

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

// The reason the per-source limit exists: a cluster that has gone slow must
// not hold slots that rules against every other cluster then queue behind.
func TestPerSourceLimitBoundsOnlyThatSource(t *testing.T) {
	limits := newQueryLimits(0, []source.Source{{Name: "slow", MaxConcurrentQueries: 1}})

	// The slow source is asked for three queries at once and may only run
	// one; the unbounded source is asked for three and must run all three
	// while the other two are still queued.
	slow := newBarrierQuerier(1)
	fast := newBarrierQuerier(3)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		for name, q := range map[string]*barrierQuerier{"slow": slow, "fast": fast} {
			eval := NewRuleEval(multiSourceRule(name), map[string]Querier{name: q},
				notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
				limits, testRetention)
			wg.Add(1)
			go func() {
				defer wg.Done()
				eval.Evaluate(context.Background(), time.Now())
			}()
		}
	}

	if !fast.waitAllStarted() {
		t.Fatal("the unbounded source queued behind the limited one")
	}
	if !slow.waitAllStarted() {
		t.Fatal("the limited source ran nothing")
	}
	time.Sleep(100 * time.Millisecond)
	if peak := slow.peak.Load(); peak > 1 {
		t.Errorf("peak concurrent queries against the limited source = %d, want at most 1", peak)
	}

	close(slow.release)
	close(fast.release)
	wg.Wait()
}

// Shutdown must abandon a query still queued behind the per-source limit
// rather than run it after the ruler has stopped, exactly as it does for the
// ruler-wide limit.
func TestPerSourceLimitAbandonsAQueuedQueryOnShutdown(t *testing.T) {
	limits := newQueryLimits(0, []source.Source{{Name: "slow", MaxConcurrentQueries: 1}})

	q := newBarrierQuerier(1)
	queriers := map[string]Querier{"slow": q}
	cadence := notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance)

	holder := NewRuleEval(multiSourceRule("slow"), queriers, cadence, limits, testRetention)
	go holder.Evaluate(context.Background(), time.Now())
	if !q.waitAllStarted() {
		t.Fatal("the first query never took the slot")
	}

	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan Result, 1)
	go func() {
		queued <- NewRuleEval(multiSourceRule("slow"), queriers, cadence, limits, testRetention).
			Evaluate(ctx, time.Now())
	}()

	// Let the second evaluation reach the queue before shutdown arrives.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case res := <-queued:
		if len(res.SourceErrors) != 1 || !errors.Is(res.SourceErrors[0].Err, errQueueAbandoned) {
			t.Fatalf("SourceErrors = %v, want the queued query abandoned", res.SourceErrors)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queued query was not abandoned")
	}

	close(q.release)
	if got := q.started.Load(); got != 1 {
		t.Errorf("queries run = %d, want the abandoned one never to have run", got)
	}
}

// Without the wait time nobody can tell a limit that is doing its job from
// one set too low, so a query that queued has to report how long it queued.
func TestQueueWaitIsReportedForALimitedSource(t *testing.T) {
	limits := newQueryLimits(0, []source.Source{{Name: "slow", MaxConcurrentQueries: 1}})

	q := newBarrierQuerier(1)
	queriers := map[string]Querier{"slow": q}
	cadence := notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance)

	holder := NewRuleEval(multiSourceRule("slow"), queriers, cadence, limits, testRetention)
	done := make(chan Result, 1)
	go func() { done <- holder.Evaluate(context.Background(), time.Now()) }()
	if !q.waitAllStarted() {
		t.Fatal("the first query never took the slot")
	}

	queued := make(chan Result, 1)
	go func() {
		queued <- NewRuleEval(multiSourceRule("slow"), queriers, cadence, limits, testRetention).
			Evaluate(context.Background(), time.Now())
	}()

	time.Sleep(100 * time.Millisecond)
	close(q.release)

	res := <-queued
	<-done
	if len(res.QueueWaits) != 1 {
		t.Fatalf("QueueWaits = %v, want one entry", res.QueueWaits)
	}
	if res.QueueWaits[0].Source != "slow" {
		t.Errorf("Source = %q, want %q", res.QueueWaits[0].Source, "slow")
	}
	if res.QueueWaits[0].Wait < 50*time.Millisecond {
		t.Errorf("Wait = %v, want the time spent queued", res.QueueWaits[0].Wait)
	}
}

// A source with no limit of its own is bounded only by the ruler-wide cap, so
// it never queues per source and must not report a wait that would read as a
// limit doing something.
func TestNoQueueWaitForAnUnboundedSource(t *testing.T) {
	limits := newQueryLimits(0, nil)

	eval := NewRuleEval(multiSourceRule("s1"), map[string]Querier{"s1": &fakeQuerier{samples: oneSample()}},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		limits, testRetention)

	if res := eval.Evaluate(context.Background(), time.Now()); len(res.QueueWaits) != 0 {
		t.Errorf("QueueWaits = %v, want none", res.QueueWaits)
	}
}
