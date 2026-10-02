package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/lint"
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
		Path:    "rules/payments.yaml",
		Group:   testGroup("payments", time.Minute),
		Labels:  map[string]string{"team": "payments"},
		Sources: sources,
	}}}
}

// The cluster the single-source fixtures here match, which is the source label
// most of these assertions read.
const prodSource = "payments_prod"

// problemGauge is what the metric carries for one finding about the rule
// ownedRuleSet builds, so a test asserts on the labels an owner reads rather
// than on a count.
func problemGauge(t *testing.T, m *Metrics) float64 {
	t.Helper()
	return checkGauge(t, m, lint.CheckRuleColumns, lint.SeverityError, prodSource)
}

// checkGauge is the same reading for one named check at one severity against one
// source, because the two feeds into this gauge own a check each and a finding is
// raised against the cluster it was found on, so a test has to say which of each
// it means. The empty source is a finding about the rule rather than one of its
// clusters, which is rule/source-schema.
func checkGauge(t *testing.T, m *Metrics, check string, severity lint.Severity, src string) float64 {
	t.Helper()

	g, err := m.Problem.GetMetricWithLabelValues(
		"SlowCheckout", check, severity.String(), "payments", "rules/payments.yaml", src)
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
		queueFor(&recordingSender{}, time.Minute),
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
		queueFor(&recordingSender{}, time.Minute),
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
		queueFor(&recordingSender{}, time.Minute),
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
		queueFor(&recordingSender{}, time.Minute),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	if got := checkGauge(t, m, lint.CheckRuleExecution, lint.SeverityError, prodSource); got != 0 {
		t.Fatalf("gauge is %v, want 0 while the query runs", got)
	}

	q.err = errors.New("Code: 47. Unknown expression identifier 'status_code'")
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))
	if got := checkGauge(t, m, lint.CheckRuleExecution, lint.SeverityError, prodSource); got != 1 {
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
		queueFor(&recordingSender{}, time.Minute),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	q.shape = []query.Column{{Name: "value", Type: "Int64"}}
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))

	q.err = errors.New("Code: 60. Table does not exist")
	sched.groups[0].Eval(context.Background(), time.Unix(120, 0))

	if got := problemGauge(t, m); got != 1 {
		t.Errorf("rule/columns is %v, want the previous answer left standing at 1", got)
	}
	if got := checkGauge(t, m, lint.CheckRuleExecution, lint.SeverityError, prodSource); got != 1 {
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
		queueFor(&recordingSender{}, time.Minute),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	q.err = errors.New("Code: 60. Table does not exist")
	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	for _, src := range []string{prodSource, "payments_eu"} {
		if got := checkGauge(t, m, lint.CheckRuleExecution, lint.SeverityError, src); got != 1 {
			t.Fatalf("gauge for %s is %v, want 1 while both clusters refuse the query", src, got)
		}
	}

	// The reload drops the connection to one of them without dropping the rule.
	sched.Reload(ownedRuleSet(prod, eu), map[string]Querier{"payments_prod": q})
	q.err = nil
	evalAll(sched, time.Unix(60, 0))

	// The cluster that answered is answered for, and the one nothing was asked
	// of keeps what it raised: the pass knows half of this rule's truth and says
	// so rather than claiming all of it.
	if got := checkGauge(t, m, lint.CheckRuleExecution, lint.SeverityError, prodSource); got != 0 {
		t.Errorf("gauge for %s is %v, want 0: its query runs again", prodSource, got)
	}
	if got := checkGauge(t, m, lint.CheckRuleExecution, lint.SeverityError, "payments_eu"); got != 1 {
		t.Errorf("gauge for payments_eu is %v, want the previous answer left standing: it was never asked", got)
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
		queueFor(&recordingSender{}, time.Minute),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))

	if got := checkGauge(t, m, lint.CheckRuleExecution, lint.SeverityError, prodSource); got != 1 {
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
		Path:    "rules/payments.yaml",
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
	return checkGauge(t, m, lint.CheckAnnotationsTemplate, lint.SeverityWarning, prodSource)
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
		queueFor(&recordingSender{}, time.Minute),
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
		queueFor(&recordingSender{}, time.Minute),
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
		queueFor(&recordingSender{}, time.Minute),
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
		queueFor(&recordingSender{}, time.Minute),
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

// Each source contributes its own labels, so a summary reading a label only one
// cluster carries renders there and fails on the other. The healthy cluster
// returning rows says nothing about the broken one, so a pass where only it had
// something to render must not clear the finding (spec 6.5).
func TestEvalGroupKeepsAnAnnotationFindingWhileTheBrokenSourceRendersNothing(t *testing.T) {
	log, _ := logBuffer()
	shape := []query.Column{{Name: "value", Type: "Float64"}}

	// staging carries the label the summary reads. prod does not, so prod is
	// the cluster the template breaks on.
	prod := &fakeQuerier{samples: oneSample(), shape: shape}
	staging := &fakeQuerier{
		samples: []alert.Sample{{Labels: map[string]string{"p99": "42"}, Value: 1}},
		shape:   shape,
	}
	m := NewMetrics(prometheus.NewRegistry())

	set := brokenAnnotationRuleSet(
		map[string]string{"summary": "p99 is {{ .p99 }}ms"},
		source.Source{Name: "payments_prod"}, source.Source{Name: "payments_staging"})
	sched := New(set, map[string]Querier{"payments_prod": prod, "payments_staging": staging},
		queueFor(&recordingSender{}, time.Minute),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	if got := renderGauge(t, m); got != 1 {
		t.Fatalf("gauge is %v, want 1 for the cluster the summary would not render against", got)
	}

	// The condition went away on prod alone. staging still fires and still
	// renders cleanly, which is evidence about staging and nothing else.
	prod.samples = nil
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))

	if got := renderGauge(t, m); got != 1 {
		t.Fatalf("gauge is %v, want the finding left standing at 1: prod is still broken", got)
	}
}

// The other half of remembering per source: what a source last found is
// replaced by what it finds on its next pass with something to render,
// including nothing, so a template that starts rendering clears itself without
// waiting for a reload (spec 6.5).
func TestEvalGroupClearsAnAnnotationFindingWhenTheSourceRendersAgain(t *testing.T) {
	log, _ := logBuffer()
	q := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	m := NewMetrics(prometheus.NewRegistry())

	set := brokenAnnotationRuleSet(
		map[string]string{"summary": "p99 is {{ .p99 }}ms"},
		source.Source{Name: "payments_prod"})
	sched := New(set, map[string]Querier{"payments_prod": q},
		queueFor(&recordingSender{}, time.Minute),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	if got := renderGauge(t, m); got != 1 {
		t.Fatalf("gauge is %v, want 1 while the label the summary reads is missing", got)
	}

	// The column the summary reads is back, so the same template renders.
	q.samples = []alert.Sample{{Labels: map[string]string{"p99": "42"}, Value: 1}}
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))

	if got := testutil.CollectAndCount(m.Problem); got != 0 {
		t.Fatalf("%d series left, want none once the source renders cleanly", got)
	}
}

// The series is the memory, so a reload does not resolve a finding whose cluster
// has gone quiet. Every merge to a rules repository is a reload, including merges
// to somebody else's rule, and a finding that a deploy can clear is one nobody
// can trust (spec 8.2).
func TestReloadKeepsAFindingWhoseSourceIsQuiet(t *testing.T) {
	log, _ := logBuffer()
	shape := []query.Column{{Name: "value", Type: "Float64"}}

	// staging carries the label the summary reads, prod does not.
	prod := &fakeQuerier{samples: oneSample(), shape: shape}
	staging := &fakeQuerier{
		samples: []alert.Sample{{Labels: map[string]string{"p99": "42"}, Value: 1}},
		shape:   shape,
	}
	m := NewMetrics(prometheus.NewRegistry())
	broken := map[string]string{"summary": "p99 is {{ .p99 }}ms"}
	queriers := map[string]Querier{"payments_prod": prod, "payments_staging": staging}
	sources := []source.Source{{Name: "payments_prod"}, {Name: "payments_staging"}}

	sched := New(brokenAnnotationRuleSet(broken, sources...), queriers,
		queueFor(&recordingSender{}, time.Minute),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	if got := checkGauge(t, m, lint.CheckAnnotationsTemplate, lint.SeverityWarning, prodSource); got != 1 {
		t.Fatalf("gauge for %s is %v, want 1: its summary would not render", prodSource, got)
	}

	// prod stops firing, and a reload arrives carrying the same broken rule,
	// which is what any merge to the repository looks like from here.
	prod.samples = nil
	sched.Reload(brokenAnnotationRuleSet(broken, sources...), queriers)
	evalAll(sched, time.Unix(60, 0))

	if got := checkGauge(t, m, lint.CheckAnnotationsTemplate, lint.SeverityWarning, prodSource); got != 1 {
		t.Errorf("gauge for %s is %v, want it standing at 1 across the reload", prodSource, got)
	}
}

// A comparison between clusters needs two of them to have answered. One reply
// says nothing about whether the rule still means the same thing everywhere, so a
// pass where the second cluster refused the query must not clear the finding
// saying they disagree (spec 10.4).
func TestEvalGroupKeepsADisagreementWhileOneClusterIsDown(t *testing.T) {
	log, _ := logBuffer()
	prod := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	eu := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Int64"}}}
	m := NewMetrics(prometheus.NewRegistry())

	sched := New(ownedRuleSet(source.Source{Name: "payments_prod"}, source.Source{Name: "payments_eu"}),
		map[string]Querier{"payments_prod": prod, "payments_eu": eu},
		queueFor(&recordingSender{}, time.Minute),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	// The two clusters return different types for value, which is the
	// disagreement. It belongs to neither of them, so it carries no source.
	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	if got := checkGauge(t, m, lint.CheckRuleSourceSchema, lint.SeverityWarning, ""); got != 1 {
		t.Fatalf("gauge is %v, want 1 while the clusters disagree", got)
	}

	eu.err = errors.New("Code: 60. Table does not exist")
	sched.groups[0].Eval(context.Background(), time.Unix(60, 0))

	if got := checkGauge(t, m, lint.CheckRuleSourceSchema, lint.SeverityWarning, ""); got != 1 {
		t.Errorf("gauge is %v, want it standing at 1: one reply compares with nothing", got)
	}
}

// A rule that used to match two clusters and now matches one can never raise the
// comparison again, so its finding has to go or it outlives the rule that could
// produce it.
func TestEvalGroupClearsADisagreementWhenOneSourceIsLeft(t *testing.T) {
	log, _ := logBuffer()
	prod := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Float64"}}}
	eu := &fakeQuerier{samples: oneSample(), shape: []query.Column{{Name: "value", Type: "Int64"}}}
	m := NewMetrics(prometheus.NewRegistry())
	queriers := map[string]Querier{"payments_prod": prod, "payments_eu": eu}

	sched := New(ownedRuleSet(source.Source{Name: "payments_prod"}, source.Source{Name: "payments_eu"}),
		queriers, queueFor(&recordingSender{}, time.Minute),
		m, newFakeClock(time.Unix(0, 0)), 0, log, testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	if got := checkGauge(t, m, lint.CheckRuleSourceSchema, lint.SeverityWarning, ""); got != 1 {
		t.Fatalf("gauge is %v, want 1 while the clusters disagree", got)
	}

	sched.Reload(ownedRuleSet(source.Source{Name: "payments_prod"}), queriers)
	evalAll(sched, time.Unix(60, 0))

	if got := checkGauge(t, m, lint.CheckRuleSourceSchema, lint.SeverityWarning, ""); got != 0 {
		t.Errorf("gauge is %v, want 0: one cluster cannot disagree with itself", got)
	}
}
