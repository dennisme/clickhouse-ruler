package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// ownedRuleSet is a rule somebody claimed, because the gauge this exercises
// exists to reach the team that owns the query rather than the operator
// (spec 8.2).
func ownedRuleSet(sources ...source.Source) *ruleset.Set {
	return &ruleset.Set{Rules: []ruleset.Rule{{
		Rule:    rule.Rule{Alert: "SlowCheckout"},
		File:    "rules/payments.yaml",
		Group:   testGroup("payments", time.Minute),
		Labels:  map[string]string{"team": "payments"},
		Sources: sources,
	}}}
}

// problemGauge is what the metric carries for one finding about the rule
// ownedRuleSet builds, so a test asserts on the labels an owner reads rather
// than on a count.
func problemGauge(t *testing.T, m *Metrics) float64 {
	t.Helper()
	return checkGauge(t, m, lint.CheckRuleColumns, lint.SeverityError)
}

// checkGauge is the same reading for one named check at one severity, because
// the two feeds into this gauge own a check each and a test has to say which one
// it means.
func checkGauge(t *testing.T, m *Metrics, check string, severity lint.Severity) float64 {
	t.Helper()

	g, err := m.Problem.GetMetricWithLabelValues(
		"SlowCheckout", check, severity.String(), "payments", "rules/payments.yaml")
	if err != nil {
		t.Fatalf("reading the gauge: %v", err)
	}
	return testutil.ToFloat64(g)
}

// A rule the schema moved under is reported into clickhouse_ruler_problem and
// named in a log line addressed to whoever owns it (spec 6.3.2, 8.2).
func TestEvalGroupReportsDrift(t *testing.T) {
	log, buf := logBuffer()
	q := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	m := NewMetrics(prometheus.NewRegistry())

	sched := New(ownedRuleSet(source.Source{Name: "payments_prod"}),
		map[string]Querier{"payments_prod": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	if got := problemGauge(t, m); got != 0 {
		t.Fatalf("the first evaluation raised the gauge to %v, want 0: it has nothing to compare against", got)
	}

	q.shape = []query.Column{{Name: "value", Type: "Int64"}}
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))

	if got := problemGauge(t, m); got != 1 {
		t.Fatalf("gauge is %v, want 1 for the rule whose result was retyped", got)
	}

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %v", len(lines), lines)
	}
	wantFields(t, lines[0], map[string]string{
		"level":      "WARN",
		"rule_group": "rules/payments.yaml:payments",
		"rule":       "SlowCheckout",
		"check":      lint.CheckRuleColumns,
		"team":       "payments",
		"file":       "rules/payments.yaml",
	})
	if got, _ := lines[0]["problem"].(string); !strings.Contains(got, "Float64") {
		t.Errorf("problem field = %q, want it to say what changed", got)
	}
}

// A finding that went away has to stop being a series, or the alert built on
// the gauge never clears (spec 8.2).
func TestEvalGroupClearsAFindingThatWentAway(t *testing.T) {
	log, _ := logBuffer()
	q := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	m := NewMetrics(prometheus.NewRegistry())

	sched := New(ownedRuleSet(source.Source{Name: "payments_prod"}),
		map[string]Querier{"payments_prod": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	q.shape = []query.Column{{Name: "value", Type: "Int64"}}
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))
	if got := problemGauge(t, m); got != 1 {
		t.Fatalf("gauge is %v, want 1 before the schema is fixed", got)
	}

	// Nothing changed this time, so the rule is healthy and the series has to
	// go rather than stay raised at its last value.
	sched.groups[0].Eval(context.Background(), time.Unix(120, 0))
	if got := testutil.CollectAndCount(m.Problem); got != 0 {
		t.Fatalf("%d series left, want none once the rule stopped drifting", got)
	}
}

// Blanking the gauge because the ruler could not ask would resolve every
// finding at once and read as a schema somebody fixed, so a pass that failed
// leaves the previous answer standing (spec 8.2).
func TestEvalGroupKeepsFindingsWhenAPassCouldNotAsk(t *testing.T) {
	log, _ := logBuffer()
	q := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	m := NewMetrics(prometheus.NewRegistry())

	sched := New(ownedRuleSet(source.Source{Name: "payments_prod"}),
		map[string]Querier{"payments_prod": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	q.shape = []query.Column{{Name: "value", Type: "Int64"}}
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))

	q.err = errors.New("connection refused")
	sched.groups[0].Eval(context.Background(), time.Unix(120, 0))

	if got := problemGauge(t, m); got != 1 {
		t.Fatalf("gauge is %v, want the previous answer left standing at 1", got)
	}
}

// A reload starts the comparison over. The edit that changed what a rule
// returns is a change somebody reviewed, so reporting it as drift would name
// the author's own commit as the schema moving under them (spec 6.3.2).
func TestReloadStartsTheComparisonOver(t *testing.T) {
	q := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	queriers := map[string]Querier{"payments_prod": q}

	set := ownedRuleSet(source.Source{Name: "payments_prod"})
	sched, m, _, _ := reloadSched(t, set, queriers)

	evalAll(sched, time.Unix(0, 0))

	// The reloaded rule returns something else, which is what the edit did.
	q.shape = []query.Column{{Name: "value", Type: "Int64"}}
	sched.Reload(ownedRuleSet(source.Source{Name: "payments_prod"}), queriers)
	evalAll(sched, time.Unix(60, 0))

	if got := testutil.CollectAndCount(m.Problem); got != 0 {
		t.Fatalf("%d series raised, want none: the first evaluation after a reload has no baseline", got)
	}
}

// A query that stopped running, raised under rule/execution and cleared by the
// evaluation that works again (spec 6.3.2).
func TestEvalGroupReportsAQueryThatFailed(t *testing.T) {
	log, _ := logBuffer()
	q := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	m := NewMetrics(prometheus.NewRegistry())

	sched := New(ownedRuleSet(source.Source{Name: "payments_prod"}),
		map[string]Querier{"payments_prod": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	if got := checkGauge(t, m, lint.CheckRuleExecution, lint.SeverityError); got != 0 {
		t.Fatalf("gauge is %v, want 0 while the query runs", got)
	}

	q.err = errors.New("Code: 47. Unknown expression identifier 'status_code'")
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))
	if got := checkGauge(t, m, lint.CheckRuleExecution, lint.SeverityError); got != 1 {
		t.Fatalf("gauge is %v, want 1 for the rule whose query failed", got)
	}

	q.err = nil
	sched.groups[0].Eval(context.Background(), time.Unix(120, 0))
	if got := testutil.CollectAndCount(m.Problem); got != 0 {
		t.Fatalf("%d series left, want none once the query runs again", got)
	}
}

// The two things this gauge carries are answered on different clocks, so a pass
// that could not compare shapes must still be able to report that the query
// failed, and must not blank the shape finding while it does (spec 10.4).
func TestEvalGroupKeepsAShapeFindingWhileTheQueryFails(t *testing.T) {
	log, _ := logBuffer()
	q := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	m := NewMetrics(prometheus.NewRegistry())

	sched := New(ownedRuleSet(source.Source{Name: "payments_prod"}),
		map[string]Querier{"payments_prod": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	q.shape = []query.Column{{Name: "value", Type: "Int64"}}
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))

	q.err = errors.New("Code: 60. Table does not exist")
	sched.groups[0].Eval(context.Background(), time.Unix(120, 0))

	if got := problemGauge(t, m); got != 1 {
		t.Errorf("rule/columns is %v, want the previous answer left standing at 1", got)
	}
	if got := checkGauge(t, m, lint.CheckRuleExecution, lint.SeverityError); got != 1 {
		t.Errorf("rule/execution is %v, want 1: the pass knows the query failed", got)
	}
}

// A rule whose second cluster has no connection open. Nothing was asked there,
// so the pass cannot say the query runs and the finding it raised before stays
// standing (spec 8.2).
func TestEvalGroupKeepsAFailureWhenASourceWasNotAsked(t *testing.T) {
	log, _ := logBuffer()
	q := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	m := NewMetrics(prometheus.NewRegistry())
	prod := source.Source{Name: "payments_prod"}
	eu := source.Source{Name: "payments_eu"}

	sched := New(ownedRuleSet(prod, eu), map[string]Querier{"payments_prod": q, "payments_eu": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	q.err = errors.New("Code: 60. Table does not exist")
	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	if got := checkGauge(t, m, lint.CheckRuleExecution, lint.SeverityError); got != 1 {
		t.Fatalf("gauge is %v, want 1 while both clusters refuse the query", got)
	}

	// The reload drops the connection to one of them without dropping the rule.
	sched.Reload(ownedRuleSet(prod, eu), map[string]Querier{"payments_prod": q})
	q.err = nil
	evalAll(sched, time.Unix(60, 0))

	if got := checkGauge(t, m, lint.CheckRuleExecution, lint.SeverityError); got != 1 {
		t.Errorf("gauge is %v, want the previous answer left standing: one cluster was never asked", got)
	}
}

// A source that replied is evidence about that source, whatever the rule's other
// clusters did, so the failure is raised even though this pass cannot rebuild the
// check for the whole rule (spec 6.3.2).
func TestEvalGroupReportsAFailureWhileAnotherSourceWasNotAsked(t *testing.T) {
	log, _ := logBuffer()
	q := &fakeQuerier{err: errors.New("Code: 60. Table does not exist")}
	m := NewMetrics(prometheus.NewRegistry())
	prod := source.Source{Name: "payments_prod"}
	eu := source.Source{Name: "payments_eu"}

	sched := New(ownedRuleSet(prod, eu), map[string]Querier{"payments_prod": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))

	if got := checkGauge(t, m, lint.CheckRuleExecution, lint.SeverityError); got != 1 {
		t.Fatalf("gauge is %v, want 1 for the cluster that refused the query", got)
	}
}

// brokenAnnotationRuleSet is a rule whose summary reads a label its query does
// not return, which is the runtime half of annotations/template: it parses in a
// pull request and fails only against a real alert (spec 6.5).
func brokenAnnotationRuleSet(annotations map[string]string, sources ...source.Source) *ruleset.Set {
	return &ruleset.Set{Rules: []ruleset.Rule{{
		Rule:    rule.Rule{Alert: "SlowCheckout", Annotations: annotations},
		File:    "rules/payments.yaml",
		Group:   testGroup("payments", time.Minute),
		Labels:  map[string]string{"team": "payments"},
		Sources: sources,
	}}}
}

// renderGauge is annotations/template as the running ruler raises it, which is
// the same name a pull request uses and a different severity from the errors the
// drift checks carry.
func renderGauge(t *testing.T, m *Metrics) float64 {
	t.Helper()
	return checkGauge(t, m, lint.CheckAnnotationsTemplate, lint.SeverityWarning)
}

// A template that will not render reaches its author on the gauge that names the
// team and the file, because the page reaches whoever is on call and the log line
// reaches whoever ships logs, and neither of those is the person who can fix it
// (spec 6.5, 8.2).
func TestEvalGroupReportsAnAnnotationThatWouldNotRender(t *testing.T) {
	log, buf := logBuffer()
	q := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	m := NewMetrics(prometheus.NewRegistry())

	set := brokenAnnotationRuleSet(
		map[string]string{"summary": "{{ .ServiceName }} p99 is {{ .p99 }}ms"},
		source.Source{Name: "payments_prod"})
	sched := New(set, map[string]Querier{"payments_prod": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))

	if got := renderGauge(t, m); got != 1 {
		t.Fatalf("gauge is %v, want 1 for the rule whose summary would not render", got)
	}

	// The finding names the annotation and the key the alert did not carry, and
	// not the Go error, which is already on the page and in the log.
	var finding map[string]any
	for _, line := range logLines(t, buf) {
		if line["check"] == lint.CheckAnnotationsTemplate {
			finding = line
		}
	}
	if finding == nil {
		t.Fatalf("no log line carried the finding: %v", logLines(t, buf))
	}
	wantFields(t, finding, map[string]string{
		"level":    "WARN",
		"rule":     "SlowCheckout",
		"check":    lint.CheckAnnotationsTemplate,
		"severity": lint.SeverityWarning.String(),
		"team":     "payments",
		"file":     "rules/payments.yaml",
	})
	problem, _ := finding["problem"].(string)
	if !strings.Contains(problem, "summary") || !strings.Contains(problem, "p99") {
		t.Errorf("problem field = %q, want it to name the annotation and the missing key", problem)
	}
	if strings.Contains(problem, "map has no entry") {
		t.Errorf("problem field = %q, want the Go error left in the annotation and the log line", problem)
	}
}

// The gauge's labels do not name the annotation, so a rule with two broken
// templates is one series and cardinality stays rules times checks (spec 8.2).
func TestEvalGroupReportsTwoBrokenAnnotationsAsOneSeries(t *testing.T) {
	log, _ := logBuffer()
	q := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	m := NewMetrics(prometheus.NewRegistry())

	set := brokenAnnotationRuleSet(map[string]string{
		"summary":     "p99 is {{ .p99 }}ms",
		"description": "in {{ .region }}",
	}, source.Source{Name: "payments_prod"})
	sched := New(set, map[string]Querier{"payments_prod": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))

	if got := testutil.CollectAndCount(m.Problem); got != 1 {
		t.Fatalf("%d series raised, want 1 for a rule with two broken annotations", got)
	}
	if got := renderGauge(t, m); got != 1 {
		t.Fatalf("gauge is %v, want 1", got)
	}
}

// Fixed by the author, so the series has to go or the alert built on it never
// clears (spec 8.2).
func TestEvalGroupClearsAnAnnotationFindingWhenItRenders(t *testing.T) {
	log, _ := logBuffer()
	q := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	m := NewMetrics(prometheus.NewRegistry())

	broken := map[string]string{"summary": "p99 is {{ .p99 }}ms"}
	set := brokenAnnotationRuleSet(broken, source.Source{Name: "payments_prod"})
	sched := New(set, map[string]Querier{"payments_prod": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	if got := renderGauge(t, m); got != 1 {
		t.Fatalf("gauge is %v, want 1 before the template is fixed", got)
	}

	fixed := brokenAnnotationRuleSet(
		map[string]string{"summary": "{{ .ServiceName }} is slow"},
		source.Source{Name: "payments_prod"})
	sched.Reload(fixed, map[string]Querier{"payments_prod": q})
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))

	if got := testutil.CollectAndCount(m.Problem); got != 0 {
		t.Fatalf("%d series left, want none once every annotation renders", got)
	}
}

// An evaluation that produced no alerts rendered no annotations, which is not
// evidence that the template works. Clearing on it would let a rule that broke
// and then stopped firing clear the finding saying it is broken (spec 6.5).
func TestEvalGroupKeepsAnAnnotationFindingWhenNothingRendered(t *testing.T) {
	log, _ := logBuffer()
	q := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	m := NewMetrics(prometheus.NewRegistry())

	set := brokenAnnotationRuleSet(
		map[string]string{"summary": "p99 is {{ .p99 }}ms"},
		source.Source{Name: "payments_prod"})
	sched := New(set, map[string]Querier{"payments_prod": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	if got := renderGauge(t, m); got != 1 {
		t.Fatalf("gauge is %v, want 1 before the rule stops firing", got)
	}

	// The condition went away, so there is nothing to render and nothing was
	// learned about the template.
	q.samples = nil
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))

	if got := renderGauge(t, m); got != 1 {
		t.Fatalf("gauge is %v, want the finding left standing at 1", got)
	}
}
