package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// fakeQuerier returns a fixed answer to every call, so a test controls
// exactly what one evaluation sees without a database.
type fakeQuerier struct {
	samples []alert.Sample
	err     error
	calls   int
}

func (q *fakeQuerier) Run(context.Context, rule.Rule, query.Attribution, time.Time) ([]alert.Sample, error) {
	q.calls++
	if q.err != nil {
		return nil, q.err
	}
	return q.samples, nil
}

// recordingSender captures what a Cadence actually posts, without an HTTP
// server or a real Alertmanager. It is guarded because rules in a group are
// evaluated concurrently, so several can post through one sender at once.
type recordingSender struct {
	mu    sync.Mutex
	calls [][]alert.Alert
	err   error
}

func (s *recordingSender) Send(_ context.Context, alerts []alert.Alert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, alerts)
	return s.err
}

func testRule(forDur time.Duration) ruleset.Rule {
	return ruleset.Rule{
		Rule: rule.Rule{
			Alert: "TestRule",
			For:   forDur,
		},
		Labels: map[string]string{},
		Sources: []source.Source{
			{Name: "src1", Labels: map[string]string{"cluster": "dc1"}},
		},
	}
}

func oneSample() []alert.Sample {
	return []alert.Sample{{Labels: map[string]string{"ServiceName": "checkout"}, Value: 1}}
}

// One alert.State per (rule, source) has to live for the process lifetime,
// otherwise `for` never accumulates: rebuilding state on every evaluation
// would leave every instance stuck in pending forever.
func TestRuleEvalKeepsForTimerAcrossEvaluations(t *testing.T) {
	r := testRule(time.Minute)
	q := &fakeQuerier{samples: oneSample()}
	sender := &recordingSender{}
	eval := NewRuleEval(r, map[string]Querier{"src1": q}, notify.NewCadence(sender, 5*time.Minute, notify.DefaultResendTolerance), newQueryLimits(0, nil), testRetention)

	now := time.Now()
	eval.Evaluate(context.Background(), now)
	if len(sender.calls) != 0 {
		t.Fatalf("pending instance must not be sent, got %d calls", len(sender.calls))
	}

	// Same instance, one rule.For later: if state had been rebuilt this
	// would still be pending instead of crossing into firing.
	eval.Evaluate(context.Background(), now.Add(time.Minute))
	if len(sender.calls) != 1 {
		t.Fatalf("got %d calls, want 1: the instance should have started firing", len(sender.calls))
	}
	if sender.calls[0][0].Phase != alert.PhaseFiring {
		t.Fatalf("phase = %v, want firing", sender.calls[0][0].Phase)
	}
}

// A query failure must not touch the alert state: the next evaluation has to
// see the same `for` timer it would have seen had the query never failed,
// otherwise an outage on the ClickHouse side resets every alert's clock.
func TestRuleEvalLeavesStateIntactAcrossAQueryFailure(t *testing.T) {
	r := testRule(time.Minute)
	q := &fakeQuerier{samples: oneSample()}
	sender := &recordingSender{}
	eval := NewRuleEval(r, map[string]Querier{"src1": q}, notify.NewCadence(sender, 5*time.Minute, notify.DefaultResendTolerance), newQueryLimits(0, nil), testRetention)

	now := time.Now()
	eval.Evaluate(context.Background(), now)

	q.err = errors.New("connection refused")
	res := eval.Evaluate(context.Background(), now.Add(30*time.Second))
	if len(res.SourceErrors) != 1 {
		t.Fatalf("SourceErrors = %v, want one entry", res.SourceErrors)
	}

	q.err = nil
	eval.Evaluate(context.Background(), now.Add(time.Minute))
	if len(sender.calls) != 1 {
		t.Fatalf("got %d calls, want 1: the outage tick must not have reset the for timer", len(sender.calls))
	}
	if sender.calls[0][0].Phase != alert.PhaseFiring {
		t.Fatalf("phase = %v, want firing", sender.calls[0][0].Phase)
	}
}

// A notification failure is a delivery problem, not an evaluation problem.
// The instance already fired; it must still be tracked as firing afterward,
// and cadence must retry rather than losing track of it.
func TestRuleEvalSurvivesAnAlertmanagerOutage(t *testing.T) {
	r := testRule(0)
	q := &fakeQuerier{samples: oneSample()}
	sender := &recordingSender{err: errors.New("alertmanager unreachable")}
	eval := NewRuleEval(r, map[string]Querier{"src1": q}, notify.NewCadence(sender, 5*time.Minute, notify.DefaultResendTolerance), newQueryLimits(0, nil), testRetention)

	now := time.Now()
	res := eval.Evaluate(context.Background(), now)
	if res.SendError == nil {
		t.Fatal("want a send error while alertmanager is down")
	}

	sender.err = nil
	eval.Evaluate(context.Background(), now.Add(time.Second))
	if len(sender.calls) != 2 {
		t.Fatalf("got %d calls, want 2: the retry must still include the alert", len(sender.calls))
	}
	if len(sender.calls[1]) != 1 || sender.calls[1][0].Phase != alert.PhaseFiring {
		t.Fatalf("retry call = %v, want the firing alert", sender.calls[1])
	}
}

// A rule matching no source must not be treated as a query error, that
// condition is reported separately as clickhouse_ruler_rules_unmatched (spec 8.2).
func TestRuleEvalWithNoMatchedSourcesDoesNothing(t *testing.T) {
	r := ruleset.Rule{Rule: rule.Rule{Alert: "Unmatched"}, Labels: map[string]string{}}
	sender := &recordingSender{}
	eval := NewRuleEval(r, map[string]Querier{}, notify.NewCadence(sender, time.Minute, notify.DefaultResendTolerance), newQueryLimits(0, nil), testRetention)

	res := eval.Evaluate(context.Background(), time.Now())
	if len(res.SourceErrors) != 0 {
		t.Fatalf("SourceErrors = %v, want none", res.SourceErrors)
	}
	if len(sender.calls) != 0 {
		t.Fatalf("got %d calls, want 0", len(sender.calls))
	}
}

// A counter that moved tells an operator something failed, not what or where.
// The error has to leave Evaluate carrying the source it came from, or no log
// line can name either.
func TestRuleEvalReportsWhichSourceFailedAndWhy(t *testing.T) {
	r := testRule(0)
	r.Sources = append(r.Sources, source.Source{Name: "src2"})
	refused := errors.New("connection refused")

	queriers := map[string]Querier{
		"src1": &fakeQuerier{samples: oneSample()},
		"src2": &fakeQuerier{err: refused},
	}
	eval := NewRuleEval(r, queriers, notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance), newQueryLimits(0, nil), testRetention)

	res := eval.Evaluate(context.Background(), time.Now())
	if len(res.SourceErrors) != 1 {
		t.Fatalf("SourceErrors = %v, want one entry", res.SourceErrors)
	}
	if got := res.SourceErrors[0].Source; got != "src2" {
		t.Errorf("Source = %q, want %q", got, "src2")
	}
	if !errors.Is(res.SourceErrors[0].Err, refused) {
		t.Errorf("Err = %v, want %v", res.SourceErrors[0].Err, refused)
	}
}

// A source with no connection is a configuration mismatch that repeats on
// every tick. It reports like any other failed source so it can be logged.
func TestRuleEvalReportsASourceWithNoQuerier(t *testing.T) {
	eval := NewRuleEval(testRule(0), map[string]Querier{},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance), newQueryLimits(0, nil), testRetention)

	res := eval.Evaluate(context.Background(), time.Now())
	if len(res.SourceErrors) != 1 {
		t.Fatalf("SourceErrors = %v, want one entry", res.SourceErrors)
	}
	if got := res.SourceErrors[0].Source; got != "src1" {
		t.Errorf("Source = %q, want %q", got, "src1")
	}
	if res.SourceErrors[0].Err == nil {
		t.Error("Err is nil, want a reason")
	}
}

// The point of retaining a resolve. A notification that fails used to lose the
// resolve outright, because State returned it once and deleted it in the same
// step: no retry, and Alertmanager left holding a firing alert for something
// that had recovered.
func TestRuleEvalRetriesAResolveAfterAFailedSend(t *testing.T) {
	r := testRule(0)
	q := &fakeQuerier{samples: oneSample()}
	sender := &recordingSender{}
	eval := NewRuleEval(r, map[string]Querier{"src1": q},
		notify.NewCadence(sender, time.Millisecond, notify.DefaultResendTolerance),
		newQueryLimits(0, nil), testRetention)

	now := time.Now()
	eval.Evaluate(context.Background(), now)
	if len(sender.calls) != 1 {
		t.Fatalf("got %d calls, want the firing alert sent", len(sender.calls))
	}

	// The condition recovers, and Alertmanager is unreachable for that tick.
	sender.err = errors.New("alertmanager unreachable")
	q.samples = nil
	res := eval.Evaluate(context.Background(), now.Add(time.Second))
	if res.SendError == nil {
		t.Fatal("want a send error while alertmanager is down")
	}
	// recordingSender records the attempt it failed, so the retry has to be
	// counted from here rather than read off the end of the slice.
	attempted := len(sender.calls)

	// Alertmanager comes back. The resolve has to still be there.
	sender.err = nil
	eval.Evaluate(context.Background(), now.Add(2*time.Second))

	if len(sender.calls) != attempted+1 {
		t.Fatalf("got %d attempts, want one more than %d: the resolve was never retried",
			len(sender.calls), attempted)
	}
	last := sender.calls[len(sender.calls)-1]
	if len(last) != 1 {
		t.Fatalf("last batch = %v, want one alert", last)
	}
	if last[0].Phase != alert.PhaseResolved {
		t.Errorf("phase = %v, want resolved: the resolve was lost", last[0].Phase)
	}
	if !last[0].ResolvedAt.Equal(now.Add(time.Second)) {
		t.Errorf("ResolvedAt = %v, want %v: it resolved then, not on the retry",
			last[0].ResolvedAt, now.Add(time.Second))
	}
}
