package scheduler

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// recheckSched is a ruler holding one owned rule, with the re-check timer on.
func recheckSched(t *testing.T, q Querier, src source.Source) (*Scheduler, *Metrics, *bytes.Buffer) {
	t.Helper()

	log, buf := logBuffer()
	m := NewMetrics(prometheus.NewRegistry())
	sched := New(ownedRuleSet(src), map[string]Querier{src.Name: q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, time.Hour)

	return sched, m, buf
}

// The one thing no evaluation can see: the query parses, returns the columns it
// always did, succeeds every tick, and matches nothing forever because the map
// key it reads was renamed (spec 7.3, 10.4).
func TestRecheckReportsAKeyNothingWrites(t *testing.T) {
	q := &fakeQuerier{findings: []query.Finding{{
		Check:  lint.CheckRuleAttributeKey,
		Detail: `SpanAttributes["http.status_code"] is in none of the 5000 rows sampled`,
	}}}
	sched, m, buf := recheckSched(t, q, source.Source{Name: "payments_prod"})

	sched.recheck.Eval(context.Background(), time.Unix(0, 0))

	if got := checkGauge(t, m, lint.CheckRuleAttributeKey, lint.SeverityWarning, prodSource); got != 1 {
		t.Fatalf("gauge is %v, want 1 for the rule whose key nothing writes", got)
	}

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %v", len(lines), lines)
	}
	wantFields(t, lines[0], map[string]string{
		"level": "WARN",
		"rule":  "SlowCheckout",
		"check": lint.CheckRuleAttributeKey,
		"team":  "payments",
		"file":  "rules/payments.yaml",
		"feed":  feedRecheck,
	})
}

// Which feed found a thing is part of the finding: "your result changed shape"
// and "your map key is gone from recent data" are different problems with
// different fixes, arriving on different clocks (spec 10.4).
func TestDriftAndRecheckSayWhichFeedFoundIt(t *testing.T) {
	q := &fakeQuerier{
		samples: oneSample(),
		shape:   []query.Column{{Name: "value", Type: "Float64"}},
		findings: []query.Finding{{
			Check:  lint.CheckRuleAttributeKey,
			Detail: "a key nothing writes",
		}},
	}
	sched, _, buf := recheckSched(t, q, source.Source{Name: "payments_prod"})

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	q.shape = []query.Column{{Name: "value", Type: "Int64"}}
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))
	sched.recheck.Eval(context.Background(), time.Unix(60, 0))

	lines := logLines(t, buf)
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want one per feed: %v", len(lines), lines)
	}
	if got, _ := lines[0]["feed"].(string); got != feedEvaluation {
		t.Errorf("the shape finding says feed %q, want %q", got, feedEvaluation)
	}
	if got, _ := lines[1]["feed"].(string); got != feedRecheck {
		t.Errorf("the sample finding says feed %q, want %q", got, feedRecheck)
	}
}

// Neither feed may blank the other's series, because the gauge is one metric
// answered on two clocks (spec 10.4).
func TestRecheckLeavesTheEvaluatorsFindingsStanding(t *testing.T) {
	q := &fakeQuerier{
		samples: oneSample(),
		shape:   []query.Column{{Name: "value", Type: "Float64"}},
		findings: []query.Finding{{
			Check:  lint.CheckRuleAttributeKey,
			Detail: "a key nothing writes",
		}},
	}
	sched, m, _ := recheckSched(t, q, source.Source{Name: "payments_prod"})

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	q.shape = []query.Column{{Name: "value", Type: "Int64"}}
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))

	sched.recheck.Eval(context.Background(), time.Unix(60, 0))
	if got := problemGauge(t, m); got != 1 {
		t.Errorf("rule/columns is %v, want the evaluator's finding still raised", got)
	}
	if got := checkGauge(t, m, lint.CheckRuleAttributeKey, lint.SeverityWarning, prodSource); got != 1 {
		t.Errorf("rule/attribute-key is %v, want the re-check pass's own finding", got)
	}

	// And the other way round: an evaluation must not take the timer's answer
	// with it when it rebuilds its own.
	sched.groups[0].Eval(context.Background(), time.Unix(120, 0))
	if got := checkGauge(t, m, lint.CheckRuleAttributeKey, lint.SeverityWarning, prodSource); got != 1 {
		t.Errorf("rule/attribute-key is %v, want it left standing by an evaluation", got)
	}
}

// A finding that went away stops being a series, the same way the evaluator
// feed's does (spec 8.2).
func TestRecheckClearsAFindingThatWentAway(t *testing.T) {
	q := &fakeQuerier{findings: []query.Finding{{
		Check:  lint.CheckRuleAttributeKey,
		Detail: "a key nothing writes",
	}}}
	sched, m, _ := recheckSched(t, q, source.Source{Name: "payments_prod"})

	sched.recheck.Eval(context.Background(), time.Unix(0, 0))
	if got := checkGauge(t, m, lint.CheckRuleAttributeKey, lint.SeverityWarning, prodSource); got != 1 {
		t.Fatalf("gauge is %v, want 1 before the rule is fixed", got)
	}

	q.findings = nil
	sched.recheck.Eval(context.Background(), time.Unix(3600, 0))
	if got := testutil.CollectAndCount(m.Problem); got != 0 {
		t.Fatalf("%d series left, want none once the key is back", got)
	}
}

// A pass that could not ask leaves the previous answer standing, because
// reporting "nothing is wrong" when nobody answered reads as a fix (spec 8.2).
func TestRecheckKeepsFindingsWhenAPassCouldNotAsk(t *testing.T) {
	q := &fakeQuerier{findings: []query.Finding{{
		Check:  lint.CheckRuleAttributeKey,
		Detail: "a key nothing writes",
	}}}
	sched, m, _ := recheckSched(t, q, source.Source{Name: "payments_prod"})

	sched.recheck.Eval(context.Background(), time.Unix(0, 0))

	q.sampleErr = errors.New("connection refused")
	sched.recheck.Eval(context.Background(), time.Unix(3600, 0))

	if got := checkGauge(t, m, lint.CheckRuleAttributeKey, lint.SeverityWarning, prodSource); got != 1 {
		t.Fatalf("gauge is %v, want the previous answer left standing at 1", got)
	}
}

// The pass has to say it ran. It is the one feed whose silence reads as good
// news: rule/attribute-key is raised only by sampling recent data, so a pass
// that stopped reports no findings and looks exactly like an estate where no map
// key was ever renamed (spec 8.6).
func TestRecheckRecordsThatThePassRan(t *testing.T) {
	q := &fakeQuerier{}
	clock := newFakeClock(time.Unix(3600, 0))
	log, _ := logBuffer()
	m := NewMetrics(prometheus.NewRegistry())

	src := source.Source{Name: prodSource}
	sched := New(ownedRuleSet(src), map[string]Querier{src.Name: q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, clock, 0, log, testResend, time.Hour)

	sched.recheck.Eval(context.Background(), clock.Now())

	if got := testutil.ToFloat64(m.RecheckLastCompletion); got != 3600 {
		t.Errorf("last completion is %v, want 3600: nothing says the pass ran", got)
	}
	if got := testutil.ToFloat64(m.RecheckLastDuration); got != 0 {
		t.Errorf("last duration is %v, want 0 on a clock that did not advance", got)
	}
}

// A cluster the pass could not sample is the operator's problem and not the
// author's. It has to be counted and said out loud, and it must stay off
// clickhouse_ruler_problem: the ruler could not ask, so it knows nothing about
// the rule, and a finding there would blame an author for an outage (spec 10.4).
func TestRecheckCountsAClusterItCouldNotSample(t *testing.T) {
	q := &fakeQuerier{sampleErr: errors.New("connection refused")}
	sched, m, buf := recheckSched(t, q, source.Source{Name: prodSource})

	sched.recheck.Eval(context.Background(), time.Unix(0, 0))

	if got := testutil.ToFloat64(m.RecheckSampleFailures.WithLabelValues(prodSource)); got != 1 {
		t.Errorf("sample failures for %s = %v, want 1", prodSource, got)
	}
	if got := testutil.CollectAndCount(m.Problem); got != 0 {
		t.Errorf("the problem gauge carries %d series, want none: the pass could not ask", got)
	}

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1 naming the cluster: %v", len(lines), lines)
	}
	wantFields(t, lines[0], map[string]string{
		"level":  "WARN",
		"source": prodSource,
		"error":  "connection refused",
	})
}

// Honouring off after the fact would mean reading rows an operator asked nobody
// to read, so a rule whose check is off is never sampled at all (spec 7.3).
func TestRecheckReadsNothingWhenTheCheckIsOff(t *testing.T) {
	q := &fakeQuerier{findings: []query.Finding{{
		Check:  lint.CheckRuleAttributeKey,
		Detail: "a key nothing writes",
	}}}
	log, _ := logBuffer()
	m := NewMetrics(prometheus.NewRegistry())

	set := ownedRuleSet(source.Source{Name: "payments_prod"})
	set.Rules[0].Policy = &policy.Policy{Checks: map[string]policy.Setting{
		lint.CheckRuleAttributeKey: {Severity: lint.SeverityOff},
	}}

	sched := New(set, map[string]Querier{"payments_prod": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, time.Hour)

	sched.recheck.Eval(context.Background(), time.Unix(0, 0))

	if got := q.samplesTaken.Load(); got != 0 {
		t.Errorf("took %d samples, want none: the check is off", got)
	}
	if got := testutil.CollectAndCount(m.Problem); got != 0 {
		t.Errorf("%d series raised, want none", got)
	}
}

// No interval, no pass. The timer reads real data on every tick, so an operator
// who did not ask for it gets no queries they did not ask for.
func TestRecheckIsOffWithoutAnInterval(t *testing.T) {
	log, _ := logBuffer()
	sched := New(ownedRuleSet(source.Source{Name: "payments_prod"}),
		map[string]Querier{"payments_prod": &fakeQuerier{}},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		NewMetrics(prometheus.NewRegistry()), newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	if sched.recheck != nil {
		t.Error("a re-check pass exists with no interval configured")
	}
}

// The pass runs on its own interval once the ruler is started, and stops when it
// shuts down. Driven by the fake clock, so no test waits an hour.
func TestRecheckRunsOnItsInterval(t *testing.T) {
	q := &fakeQuerier{}
	clock := newFakeClock(time.Unix(0, 0))
	log, _ := logBuffer()

	sched := New(ownedRuleSet(source.Source{Name: "payments_prod"}),
		map[string]Querier{"payments_prod": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		NewMetrics(prometheus.NewRegistry()), clock, 0, log, testResend, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for q.samplesTaken.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("took %d samples, want at least 2", q.samplesTaken.Load())
		}
		clock.Advance(time.Hour)
		time.Sleep(time.Millisecond)
	}

	sched.Shutdown(5 * time.Second)
	took := q.samplesTaken.Load()
	clock.Advance(4 * time.Hour)
	time.Sleep(10 * time.Millisecond)

	if got := q.samplesTaken.Load(); got != took {
		t.Errorf("took %d samples after shutdown, want the %d it had", got, took)
	}
}

// A reload replaces what the pass asks about, because the rules it samples for
// are exactly what the reload replaced.
func TestReloadReplacesWhatTheRecheckPassAsksAbout(t *testing.T) {
	q := &fakeQuerier{}
	log, _ := logBuffer()

	sched := New(ownedRuleSet(source.Source{Name: "payments_prod"}),
		map[string]Querier{"payments_prod": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		NewMetrics(prometheus.NewRegistry()), newFakeClock(time.Unix(0, 0)), 0, log, testResend, time.Hour)

	sched.Reload(&ruleset.Set{}, map[string]Querier{})
	if sched.recheck != nil {
		t.Error("a re-check pass survived a reload that left no rules to check")
	}
}

// Switching the check off clears what it raised, rather than freezing the last
// finding at its value forever: policy saying "do not report this" is an answer.
func TestRecheckClearsAFindingWhenTheCheckIsSwitchedOff(t *testing.T) {
	q := &fakeQuerier{findings: []query.Finding{{
		Check:  lint.CheckRuleAttributeKey,
		Detail: "a key nothing writes",
	}}}
	log, _ := logBuffer()
	m := NewMetrics(prometheus.NewRegistry())
	src := source.Source{Name: "payments_prod"}

	sched := New(ownedRuleSet(src), map[string]Querier{src.Name: q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, time.Hour)

	sched.recheck.Eval(context.Background(), time.Unix(0, 0))
	if got := checkGauge(t, m, lint.CheckRuleAttributeKey, lint.SeverityWarning, prodSource); got != 1 {
		t.Fatalf("gauge is %v, want 1 before the check is switched off", got)
	}

	off := ownedRuleSet(src)
	off.Rules[0].Policy = &policy.Policy{Checks: map[string]policy.Setting{
		lint.CheckRuleAttributeKey: {Severity: lint.SeverityOff},
	}}
	sched.Reload(off, map[string]Querier{src.Name: q})
	sched.recheck.Eval(context.Background(), time.Unix(3600, 0))

	if got := testutil.CollectAndCount(m.Problem); got != 0 {
		t.Errorf("%d series left, want none once nobody asked for the check", got)
	}
}
