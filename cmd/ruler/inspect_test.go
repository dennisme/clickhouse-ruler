package main

import (
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/query"
)

func TestInspectionProblemsCarrySeverityFromPolicy(t *testing.T) {
	merged := policy.Merge(&policy.Policy{
		File: "ruler.yaml",
		Checks: map[string]policy.Setting{
			policy.CheckRuleSelectStar: {Severity: lint.SeverityError, File: "ruler.yaml", Line: 3},
		},
	})

	findings := []query.Finding{
		{Check: query.CheckSelectStar, Detail: "the query selects *"},
		{Check: query.CheckNondeterministic, Detail: "the query calls now"},
	}

	problems := inspectionProblems("rules/payments.yaml", "CheckoutIsSlow", 12, "payments_prod", merged, findings)
	if len(problems) != 2 {
		t.Fatalf("got %d problems, want one per finding: %v", len(problems), problems)
	}

	bySeverity := map[string]lint.Severity{}
	for _, p := range problems {
		bySeverity[p.Check] = p.Severity

		if p.File != "rules/payments.yaml" || p.Line != 12 {
			t.Errorf("location = %s:%d, want the rule's line", p.File, p.Line)
		}
		if p.Subject != "CheckoutIsSlow" {
			t.Errorf("subject = %q, want the alert name", p.Subject)
		}
		// A rule can match several sources and fail against only one of
		// them, so the finding has to say which cluster answered.
		if !strings.Contains(p.Text, "payments_prod") {
			t.Errorf("text = %q, want it to name the source", p.Text)
		}
	}

	if got := bySeverity[policy.CheckRuleSelectStar]; got != lint.SeverityError {
		t.Errorf("select-star severity = %v, want the configured error", got)
	}
	if got := bySeverity[policy.CheckRuleNondeterministic]; got != lint.SeverityWarning {
		t.Errorf("nondeterministic severity = %v, want the default warning", got)
	}
}

// A check turned off produces no finding, so nothing it noticed is reported.
func TestInspectionProblemsDropsChecksTurnedOff(t *testing.T) {
	merged := policy.Merge(&policy.Policy{Checks: map[string]policy.Setting{
		policy.CheckRuleNondeterministic: {Severity: lint.SeverityOff},
	}})

	findings := []query.Finding{{Check: query.CheckNondeterministic, Detail: "the query calls now"}}

	if problems := inspectionProblems("f.yaml", "A", 1, "s", merged, findings); len(problems) != 0 {
		t.Errorf("got %v, want nothing at severity off", problems)
	}
}

// rule/syntax is a correctness check: a rule whose SQL will not parse cannot
// run, so there is no severity to resolve and it always blocks.
func TestInspectionProblemsAlwaysBlocksOnSyntax(t *testing.T) {
	merged := policy.Merge()
	findings := []query.Finding{{Check: query.CheckSyntax, Detail: "Syntax error at position 18"}}

	problems := inspectionProblems("f.yaml", "A", 1, "s", merged, findings)
	if len(problems) != 1 {
		t.Fatalf("got %v, want one problem", problems)
	}
	if problems[0].Severity != lint.SeverityError {
		t.Errorf("severity = %v, want error", problems[0].Severity)
	}
}

// Which checks the inspection is asked to run comes from the resolved policy,
// so a check at severity off is never even looked for.
func TestChecksFromPolicy(t *testing.T) {
	merged := policy.Merge(&policy.Policy{Checks: map[string]policy.Setting{
		policy.CheckRuleTableFunction:    {Severity: lint.SeverityError, Keys: []string{"merge"}},
		policy.CheckRuleNondeterministic: {Severity: lint.SeverityOff},
	}})

	got := checksFromPolicy(merged)
	if len(got.AllowedTableFunctions) != 1 || got.AllowedTableFunctions[0] != "merge" {
		t.Errorf("allowed = %v, want the configured allowlist", got.AllowedTableFunctions)
	}
	if len(got.Nondeterministic) != 0 {
		t.Errorf("nondeterministic = %v, want none: the check is off", got.Nondeterministic)
	}
}
