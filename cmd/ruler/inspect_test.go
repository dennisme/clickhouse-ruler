package main

import (
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

func TestInspectionProblemsCarrySeverityFromPolicy(t *testing.T) {
	merged := policy.Merge(&policy.Policy{
		File: "ruler.yaml",
		Checks: map[string]policy.Setting{
			lint.CheckRuleSelectStar: {Severity: lint.SeverityError, File: "ruler.yaml", Line: 3},
		},
	})

	findings := []query.Finding{
		{Check: lint.CheckRuleSelectStar, Detail: "the query selects *"},
		{Check: lint.CheckRuleNondeterministic, Detail: "the query calls now"},
	}

	src := source.Source{Name: "payments_prod"}
	problems := inspectionProblems("rules/payments.yaml", "CheckoutIsSlow", 12, src, merged, findings, time.Now())
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

	if got := bySeverity[lint.CheckRuleSelectStar]; got != lint.SeverityError {
		t.Errorf("select-star severity = %v, want the configured error", got)
	}
	if got := bySeverity[lint.CheckRuleNondeterministic]; got != lint.SeverityWarning {
		t.Errorf("nondeterministic severity = %v, want the default warning", got)
	}
}

// A check turned off produces no finding, so nothing it noticed is reported.
func TestInspectionProblemsDropsChecksTurnedOff(t *testing.T) {
	merged := policy.Merge(&policy.Policy{Checks: map[string]policy.Setting{
		lint.CheckRuleNondeterministic: {Severity: lint.SeverityOff},
	}})

	findings := []query.Finding{{Check: lint.CheckRuleNondeterministic, Detail: "the query calls now"}}

	src := source.Source{Name: "s"}
	if problems := inspectionProblems("f.yaml", "A", 1, src, merged, findings, time.Now()); len(problems) != 0 {
		t.Errorf("got %v, want nothing at severity off", problems)
	}
}

// rule/syntax is a correctness check: a rule whose SQL will not parse cannot
// run, so there is no severity to resolve and it always blocks.
func TestInspectionProblemsAlwaysBlocksOnSyntax(t *testing.T) {
	merged := policy.Merge()
	findings := []query.Finding{{Check: lint.CheckRuleSyntax, Detail: "Syntax error at position 18"}}

	src := source.Source{Name: "s"}
	problems := inspectionProblems("f.yaml", "A", 1, src, merged, findings, time.Now())
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
		lint.CheckRuleTableFunction:    {Severity: lint.SeverityError, Keys: []string{"merge"}},
		lint.CheckRuleNondeterministic: {Severity: lint.SeverityOff},
	}})

	got := checksFromPolicy(merged, "otel")
	if len(got.AllowedTableFunctions) != 1 || got.AllowedTableFunctions[0] != "merge" {
		t.Errorf("allowed = %v, want the configured allowlist", got.AllowedTableFunctions)
	}
	if len(got.Nondeterministic) != 0 {
		t.Errorf("nondeterministic = %v, want none: the check is off", got.Nondeterministic)
	}
}

// A foreign table is one outside the source's own database, so the source
// the rule matched is what the check is measured against.
func TestChecksFromPolicyCarriesTheSourceDatabase(t *testing.T) {
	if got := checksFromPolicy(policy.Merge(), "otel"); got.Database != "otel" {
		t.Errorf("database = %q, want otel", got.Database)
	}

	off := policy.Merge(&policy.Policy{Checks: map[string]policy.Setting{
		lint.CheckRuleForeignTable: {Severity: lint.SeverityOff},
	}})
	if got := checksFromPolicy(off, "otel"); got.Database != "" {
		t.Errorf("database = %q, want none: the check is off", got.Database)
	}
}

func TestChecksFromPolicyReadsTheComplexityCeilings(t *testing.T) {
	got := checksFromPolicy(policy.Merge(&policy.Policy{Checks: map[string]policy.Setting{
		lint.CheckRuleComplexity: {
			Severity: lint.SeverityWarning,
			Keys:     []string{lint.LimitJoins + ":1", lint.LimitSubqueries + ":4"},
		},
	}}), "otel")

	if got.Complexity == nil {
		t.Fatal("no ceilings, want the configured ones")
	}
	if got.Complexity.MaxJoins != 1 || got.Complexity.MaxSubqueries != 4 {
		t.Errorf("ceilings = %+v, want 1 join and 4 subqueries", *got.Complexity)
	}

	off := policy.Merge(&policy.Policy{Checks: map[string]policy.Setting{
		lint.CheckRuleComplexity: {Severity: lint.SeverityOff},
	}})
	if got := checksFromPolicy(off, "otel"); got.Complexity != nil {
		t.Errorf("ceilings = %+v, want none: the check is off", *got.Complexity)
	}
}

// An exemption drops the finding for one check on one source, so the same
// rule still reports it against every other cluster it matched.
func TestInspectionProblemsHonoursAnExemption(t *testing.T) {
	src := source.Source{
		Name: "otel_shared",
		Exemptions: []source.Exemption{{
			Check:  lint.CheckRuleForeignTable,
			Reason: "system.parts is granted here on purpose",
			Until:  time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		}},
	}
	findings := []query.Finding{
		{Check: lint.CheckRuleForeignTable, Detail: "the query reads system.parts"},
		{Check: lint.CheckRuleSelectStar, Detail: "the query selects *"},
	}

	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	got := inspectionProblems("f.yaml", "A", 1, src, policy.Merge(), findings, now)

	if len(got) != 1 {
		t.Fatalf("got %v, want the exempted finding dropped and the other kept", got)
	}
	if got[0].Check != lint.CheckRuleSelectStar {
		t.Errorf("kept %q, want the check nobody exempted", got[0].Check)
	}
}

// An expired exemption stops dropping anything. The finding comes back on its
// own, alongside the expiry problem the sources file reports separately.
func TestInspectionProblemsIgnoresAnExpiredExemption(t *testing.T) {
	src := source.Source{
		Name: "otel_shared",
		Exemptions: []source.Exemption{{
			Check:  lint.CheckRuleForeignTable,
			Reason: "system.parts is granted here on purpose",
			Until:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}},
	}
	findings := []query.Finding{{Check: lint.CheckRuleForeignTable, Detail: "the query reads system.parts"}}

	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	if got := inspectionProblems("f.yaml", "A", 1, src, policy.Merge(), findings, now); len(got) != 1 {
		t.Errorf("got %v, want the finding back once the exemption expired", got)
	}
}

// A correctness check is refused as an exemption when the sources file is
// parsed. If one ever reached here it would still not apply: nothing that
// blocks can be dropped by a source.
func TestInspectionProblemsNeverExemptsAFixedCheck(t *testing.T) {
	src := source.Source{
		Name: "otel_shared",
		Exemptions: []source.Exemption{{
			Check: lint.CheckRuleSettings,
			Until: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		}},
	}
	findings := []query.Finding{{Check: lint.CheckRuleSettings, Detail: "the query sets its own SETTINGS"}}

	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	if got := inspectionProblems("f.yaml", "A", 1, src, policy.Merge(), findings, now); len(got) != 1 {
		t.Errorf("got %v, want the correctness finding kept", got)
	}
}
