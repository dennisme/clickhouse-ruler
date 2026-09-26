package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
)

// readColumns reads a captured `DESCRIBE (SELECT ...)` result, name and type
// per line. Two sources answering differently is the whole subject here, so
// each answer is a fixture of its own rather than a literal in a test: what
// makes a case interesting is the shape of a real result, and a real result is
// wider than anything worth typing twice.
func readColumns(t *testing.T, name string) []query.Column {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}

	var out []query.Column
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		field, kind, _ := strings.Cut(line, "\t")
		out = append(out, query.Column{Name: strings.TrimSpace(field), Type: strings.TrimSpace(kind)})
	}
	return out
}

// The ordinary case: an estate whose clusters carry the same table, so the one
// rule that runs on all of them means the same thing everywhere.
func TestSchemaDisagreementsSaysNothingWhenSourcesAgree(t *testing.T) {
	answers := []sourceColumns{
		{source: "payments_dc1", columns: readColumns(t, "columns_traces.txt")},
		{source: "payments_dc2", columns: readColumns(t, "columns_traces.txt")},
	}

	if got := schemaDisagreements(answers); len(got) != 0 {
		t.Errorf("got %v, want nothing: both sources returned the same result", got)
	}
}

// The subtle one. Both clusters resolve the query, so every other tier 1 check
// passes against both, and the same rule produces a value of a different type
// on each.
func TestSchemaDisagreementsReportsAColumnTypeDifference(t *testing.T) {
	answers := []sourceColumns{
		{source: "payments_dc1", columns: readColumns(t, "columns_traces.txt")},
		{source: "payments_dc2", columns: readColumns(t, "columns_traces_float_value.txt")},
	}

	got := schemaDisagreements(answers)
	if len(got) != 1 {
		t.Fatalf("got %v, want one disagreement", got)
	}
	for _, want := range []string{"value", "payments_dc1", "payments_dc2", "UInt64", "Float64"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("%q does not name %q", got[0], want)
		}
	}
}

// A column one cluster returns and another does not is a label that exists on
// half the estate's alerts, which is a different alert identity per cluster.
func TestSchemaDisagreementsReportsAColumnOneSourceDoesNotReturn(t *testing.T) {
	answers := []sourceColumns{
		{source: "payments_dc1", columns: readColumns(t, "columns_traces.txt")},
		{source: "payments_dc2", columns: readColumns(t, "columns_traces_extra_label.txt")},
	}

	got := schemaDisagreements(answers)
	if len(got) != 1 {
		t.Fatalf("got %v, want one disagreement", got)
	}
	if !strings.Contains(got[0], "Environment") || !strings.Contains(got[0], "payments_dc2") {
		t.Errorf("%q does not say which source returned Environment", got[0])
	}
}

// One answer is not a disagreement. A rule matching a single source, and a
// rule whose second source could not be reached, arrive here the same way:
// the unreachable one is already reported as rule/inspect or
// rule/table-access, and saying it again differently is noise.
func TestSchemaDisagreementsNeedsTwoAnswers(t *testing.T) {
	answers := []sourceColumns{
		{source: "payments_dc1", columns: readColumns(t, "columns_traces.txt")},
	}

	if got := schemaDisagreements(answers); len(got) != 0 {
		t.Errorf("got %v, want nothing: only one source answered", got)
	}
	if got := schemaDisagreements(nil); len(got) != 0 {
		t.Errorf("got %v, want nothing: no source answered", got)
	}
}

// Every disagreement is reported, and each one names the column, so an author
// reading the finding knows how many things differ rather than the first.
func TestSchemaDisagreementsReportsEveryColumn(t *testing.T) {
	answers := []sourceColumns{
		{source: "payments_dc1", columns: readColumns(t, "columns_traces_extra_label.txt")},
		{source: "payments_dc2", columns: readColumns(t, "columns_traces_float_value.txt")},
	}

	got := schemaDisagreements(answers)
	if len(got) != 2 {
		t.Fatalf("got %v, want the missing column and the type difference", got)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"Environment", "value"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q does not name %q", joined, want)
		}
	}
}

// The finding is about the rule rather than about either cluster, so there is
// one problem for it however many sources disagreed and however many columns
// they disagreed on.
func TestSchemaProblemReportsOncePerRule(t *testing.T) {
	r := ruleset.Rule{Rule: rule.Rule{Alert: "CheckoutIsSlow"}, File: "rules/payments.yaml"}
	answers := []sourceColumns{
		{source: "payments_dc1", columns: readColumns(t, "columns_traces_extra_label.txt")},
		{source: "payments_dc2", columns: readColumns(t, "columns_traces_float_value.txt")},
	}

	got := schemaProblems(r, answers, policy.Merge())
	if len(got) != 1 {
		t.Fatalf("got %d problems, want one for the rule: %v", len(got), got)
	}
	if got[0].Check != lint.CheckRuleSourceSchema {
		t.Errorf("check = %q, want %q", got[0].Check, lint.CheckRuleSourceSchema)
	}
	if got[0].Severity != lint.SeverityWarning {
		t.Errorf("severity = %v, want the default warning", got[0].Severity)
	}
	if got[0].Subject != "CheckoutIsSlow" || got[0].File != "rules/payments.yaml" {
		t.Errorf("problem = %s in %s, want the rule that matched both sources", got[0].Subject, got[0].File)
	}
	for _, want := range []string{"Environment", "value"} {
		if !strings.Contains(got[0].Text, want) {
			t.Errorf("text = %q, want it to carry %q", got[0].Text, want)
		}
	}
}

// An operator who would rather an estate-wide rule never ship half working
// raises it, and then the same disagreement blocks.
func TestSchemaProblemTakesItsSeverityFromPolicy(t *testing.T) {
	merged := policy.Merge(&policy.Policy{
		File: "ruler.yaml",
		Checks: map[string]policy.Setting{
			lint.CheckRuleSourceSchema: {Severity: lint.SeverityError, File: "ruler.yaml", Line: 4},
		},
	})
	answers := []sourceColumns{
		{source: "payments_dc1", columns: readColumns(t, "columns_traces.txt")},
		{source: "payments_dc2", columns: readColumns(t, "columns_traces_float_value.txt")},
	}

	got := schemaProblems(ruleset.Rule{Rule: rule.Rule{Alert: "A"}}, answers, merged)
	if len(got) != 1 {
		t.Fatalf("got %v, want one problem", got)
	}
	if got[0].Severity != lint.SeverityError {
		t.Errorf("severity = %v, want the configured error", got[0].Severity)
	}
	if got[0].PolicyFile != "ruler.yaml" || got[0].PolicyLine != 4 {
		t.Errorf("origin = %s:%d, want the file that raised it", got[0].PolicyFile, got[0].PolicyLine)
	}
}

// Off means silence, the way it does for every other configurable check.
func TestSchemaProblemHonoursOff(t *testing.T) {
	merged := policy.Merge(&policy.Policy{Checks: map[string]policy.Setting{
		lint.CheckRuleSourceSchema: {Severity: lint.SeverityOff},
	}})
	answers := []sourceColumns{
		{source: "payments_dc1", columns: readColumns(t, "columns_traces.txt")},
		{source: "payments_dc2", columns: readColumns(t, "columns_traces_float_value.txt")},
	}

	if got := schemaProblems(ruleset.Rule{Rule: rule.Rule{Alert: "A"}}, answers, merged); len(got) != 0 {
		t.Errorf("got %v, want nothing at severity off", got)
	}
}
