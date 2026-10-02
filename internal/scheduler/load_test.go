package scheduler

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

func loadFixture(t *testing.T) (*Metrics, *ruleset.Set) {
	t.Helper()

	set := &ruleset.Set{Rules: []ruleset.Rule{{
		Rule:    rule.Rule{Alert: "HighLatency"},
		File:    "payments/latency.yaml",
		Path:    "payments/latency.yaml",
		Group:   testGroup("latency", time.Minute),
		Labels:  map[string]string{"team": "payments"},
		Sources: []source.Source{{Name: "src1"}},
	}}}
	return NewMetrics(prometheus.NewRegistry()), set
}

func loadProblem(check string, sev lint.Severity) lint.Problem {
	p := lint.NewProblem("payments/latency.yaml", 4, check, sev, "finding")
	p.Subject = "HighLatency"
	return p
}

// A rule that merged past the checker runs, so something has to say so. The
// load feed is the only surface that names the team and the file and clears
// when somebody fixes it (spec 7.6, 10.4).
func TestLoadFeedRaisesAFindingThatBlockedAMerge(t *testing.T) {
	m, set := loadFixture(t)

	ReportLoadFindings(m, nil, set, []lint.Problem{loadProblem(lint.CheckRuleExpr, lint.SeverityError)})

	got := testutil.ToFloat64(m.Problem.WithLabelValues(
		"HighLatency", lint.CheckRuleExpr, "error", "payments", "payments/latency.yaml", ""))
	if got != 1 {
		t.Errorf("clickhouse_ruler_problem for rule/expr = %v, want 1", got)
	}
}

// Rebuilt per reading, so fixing the file and reloading clears it. A finding
// left standing after its cause is gone is an alert nobody can resolve.
func TestLoadFeedClearsAFindingThatIsFixed(t *testing.T) {
	m, set := loadFixture(t)

	ReportLoadFindings(m, nil, set, []lint.Problem{loadProblem(lint.CheckRuleExpr, lint.SeverityError)})
	ReportLoadFindings(m, nil, set, nil)

	if got := testutil.CollectAndCount(m.Problem); got != 0 {
		t.Errorf("series after a clean reading = %d, want 0", got)
	}
}

// The load feed owns the fixed checks and nothing else, so it cannot blank a
// finding the evaluation feed raised on its own clock (spec 10.4).
func TestLoadFeedLeavesTheEvaluationFeedsFindingsAlone(t *testing.T) {
	m, set := loadFixture(t)

	// What an evaluation raises: a template that would not render.
	m.Problem.WithLabelValues(
		"HighLatency", lint.CheckAnnotationsTemplate, "warning", "payments", "payments/latency.yaml", "src1").Set(1)

	ReportLoadFindings(m, nil, set, nil)

	got := testutil.ToFloat64(m.Problem.WithLabelValues(
		"HighLatency", lint.CheckAnnotationsTemplate, "warning", "payments", "payments/latency.yaml", "src1"))
	if got != 1 {
		t.Errorf("annotations/template after a load pass = %v, want 1: the load feed blanked another feed's finding", got)
	}
}

// A warning did not block a merge, so nothing merged past anything and there
// is nothing for an operator to be told about.
func TestLoadFeedIgnoresAWarning(t *testing.T) {
	m, set := loadFixture(t)

	ReportLoadFindings(m, nil, set, []lint.Problem{loadProblem(lint.CheckRuleSourceMatch, lint.SeverityWarning)})

	if got := testutil.CollectAndCount(m.Problem); got != 0 {
		t.Errorf("series from a warning = %d, want 0", got)
	}
}
