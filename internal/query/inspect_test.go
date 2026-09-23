package query

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/lint"

	"github.com/ClickHouse/clickhouse-go/v2"
)

func checksFor(allowed ...string) Checks {
	return Checks{
		AllowedTableFunctions: allowed,
		Nondeterministic:      []string{"now", "now64", "today", "rand"},
	}
}

func findingFor(findings []Finding, check string) (Finding, bool) {
	for _, f := range findings {
		if f.Check == check {
			return f, true
		}
	}
	return Finding{}, false
}

// Nothing is wrong with this rule, so nothing is reported. A check that
// cannot stay quiet on a good rule gets ignored on a bad one.
func TestInspectTreeSaysNothingAboutAGoodRule(t *testing.T) {
	if got := inspectTree(readAST(t, "ast_plain.txt"), checksFor()); len(got) != 0 {
		t.Errorf("findings = %v, want none", got)
	}
}

func TestInspectTreeReportsSelectStar(t *testing.T) {
	got, ok := findingFor(inspectTree(readAST(t, "ast_select_star.txt"), checksFor()), lint.CheckRuleSelectStar)
	if !ok {
		t.Fatal("SELECT * was not reported")
	}
	// The reason is the part an author acts on: the rule works today and
	// breaks the next time a column is added.
	if got.Detail == "" {
		t.Error("the finding says nothing about why")
	}
}

func TestInspectTreeReportsATableFunction(t *testing.T) {
	got, ok := findingFor(inspectTree(readAST(t, "ast_table_function_in_cte.txt"), checksFor()), lint.CheckRuleTableFunction)
	if !ok {
		t.Fatal("a table function inside a CTE was not reported")
	}
	if !contains(got.Detail, "merge") {
		t.Errorf("detail = %q, want it to name the function", got.Detail)
	}
}

// The allowlist is what an operator uses to permit the one function they
// actually need, and it ships empty.
func TestInspectTreeHonoursTheAllowlist(t *testing.T) {
	findings := inspectTree(readAST(t, "ast_table_function_in_cte.txt"), checksFor("merge"))

	if _, ok := findingFor(findings, lint.CheckRuleTableFunction); ok {
		t.Error("merge() was reported although the allowlist permits it")
	}
}

func TestInspectTreeAllowlistIsCaseInsensitive(t *testing.T) {
	findings := inspectTree(readAST(t, "ast_table_function_in_cte.txt"), checksFor("MERGE"))

	if _, ok := findingFor(findings, lint.CheckRuleTableFunction); ok {
		t.Error("MERGE in the allowlist did not permit merge()")
	}
}

func TestInspectTreeReportsNondeterministicFunctions(t *testing.T) {
	got, ok := findingFor(inspectTree(readAST(t, "ast_nondeterministic.txt"), checksFor()), lint.CheckRuleNondeterministic)
	if !ok {
		t.Fatal("now() and today() were not reported")
	}
	for _, want := range []string{"now", "today"} {
		if !contains(got.Detail, want) {
			t.Errorf("detail = %q, want it to name %s", got.Detail, want)
		}
	}
}

// An empty list is an operator having turned the list off, and a check with
// nothing to look for reports nothing rather than falling back to a default
// the operator did not ask for.
func TestInspectTreeWithNoNondeterministicList(t *testing.T) {
	c := Checks{}
	if got := inspectTree(readAST(t, "ast_nondeterministic.txt"), c); len(got) != 0 {
		t.Errorf("findings = %v, want none when no list is configured", got)
	}
}

// A SETTINGS clause is refused whatever it sets, so nothing configures this
// one: the ruler's own limits are what it overrides.
func TestInspectTreeReportsASettingsClause(t *testing.T) {
	for _, name := range []string{"ast_settings.txt", "ast_settings_in_subquery.txt"} {
		got, ok := findingFor(inspectTree(readAST(t, name), checksFor()), lint.CheckRuleSettings)
		if !ok {
			t.Fatalf("%s: a SETTINGS clause was not reported", name)
		}
		if got.Detail == "" {
			t.Errorf("%s: the finding says nothing about why", name)
		}
	}
}

func TestInspectTreeReportsAForeignTable(t *testing.T) {
	c := checksFor()
	c.Database = "otel"

	got, ok := findingFor(inspectTree(readAST(t, "ast_foreign_table.txt"), c), lint.CheckRuleForeignTable)
	if !ok {
		t.Fatal("a table outside the source's database was not reported")
	}
	if !contains(got.Detail, "system.parts") {
		t.Errorf("detail = %q, want it to name the table", got.Detail)
	}

	// The source's own table is not foreign, whichever database it is in.
	if _, ok := findingFor(inspectTree(readAST(t, "ast_plain.txt"), c), lint.CheckRuleForeignTable); ok {
		t.Error("the source's own table was reported as foreign")
	}
}

func TestInspectTreeReportsComplexity(t *testing.T) {
	c := checksFor()
	c.Complexity = &Complexity{MaxJoins: 1, MaxSubqueries: 1}

	got, ok := findingFor(inspectTree(readAST(t, "ast_joins.txt"), c), lint.CheckRuleComplexity)
	if !ok {
		t.Fatal("two joins and two subqueries against a ceiling of one were not reported")
	}
	for _, want := range []string{"2 joins", "2 subqueries"} {
		if !contains(got.Detail, want) {
			t.Errorf("detail = %q, want it to carry %q", got.Detail, want)
		}
	}
}

func TestInspectTreeComplexityUnderTheCeiling(t *testing.T) {
	c := checksFor()
	c.Complexity = &Complexity{MaxJoins: 2, MaxSubqueries: 2}

	if _, ok := findingFor(inspectTree(readAST(t, "ast_joins.txt"), c), lint.CheckRuleComplexity); ok {
		t.Error("a query at the ceiling was reported; the ceiling is what is permitted")
	}
}

// Nil is the check switched off, and a ceiling of zero is an operator meaning
// no joins at all. They cannot be the same value.
func TestInspectTreeComplexityOffAndZero(t *testing.T) {
	if _, ok := findingFor(inspectTree(readAST(t, "ast_joins.txt"), checksFor()), lint.CheckRuleComplexity); ok {
		t.Error("complexity was reported although no ceiling is configured")
	}

	c := checksFor()
	c.Complexity = &Complexity{}
	if _, ok := findingFor(inspectTree(readAST(t, "ast_joins.txt"), c), lint.CheckRuleComplexity); !ok {
		t.Error("a ceiling of zero permitted two joins")
	}
}

func TestClassifyExplain(t *testing.T) {
	syntaxErr := func(msg string) error {
		return &clickhouse.Exception{Code: codeSyntaxError, Message: msg}
	}

	tests := []struct {
		name         string
		err          error
		wantFinding  bool
		wantCheck    string
		wantErr      bool
		wantInDetail string
	}{
		{name: "parses", err: nil},
		{
			name:         "broken sql",
			err:          syntaxErr("Syntax error: failed at position 18 (end of query)"),
			wantFinding:  true,
			wantInDetail: "position 18",
		},
		{
			// A second statement is a second thing nobody reviewed, and
			// ClickHouse refuses it for us rather than us hunting semicolons.
			name:         "two statements",
			err:          syntaxErr("Syntax error (Multi-statements are not allowed): failed at position 9"),
			wantFinding:  true,
			wantInDetail: "Multi-statements are not allowed",
		},
		{
			// Not a finding about the rule: the ruler could not ask.
			name:    "connection failed",
			err:     &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			wantErr: true,
		},
		{
			// The profile constraints refuse the clause before the tree is
			// read, and that refusal is about the rule: reporting it as a
			// ruler that could not ask would lose the finding entirely
			// (spec 6.7).
			name: "a setting the profile constrains",
			err: &clickhouse.Exception{
				Code:    codeSettingConstraintViolation,
				Message: "Setting max_execution_time shouldn't be greater than 60.",
			},
			wantFinding:  true,
			wantCheck:    lint.CheckRuleSettings,
			wantInDetail: "max_execution_time",
		},
		{
			name:    "some other server error",
			err:     &clickhouse.Exception{Code: 241, Message: "Memory limit exceeded"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := classifyExplain(tt.err)

			switch {
			case tt.wantErr && err == nil:
				t.Fatalf("err = nil, want one: %v is not the rule's fault", tt.err)
			case !tt.wantErr && err != nil:
				t.Fatalf("err = %v, want none", err)
			}
			if got != nil != tt.wantFinding {
				t.Fatalf("finding = %v, want finding: %v", got, tt.wantFinding)
			}
			if tt.wantFinding && !contains(got.Detail, tt.wantInDetail) {
				t.Errorf("detail = %q, want it to carry %q", got.Detail, tt.wantInDetail)
			}
			if want := tt.wantCheck; tt.wantFinding && want != "" && got.Check != want {
				t.Errorf("check = %q, want %s", got.Check, want)
			}
		})
	}
}

func TestClassifyExplainUnwraps(t *testing.T) {
	wrapped := fmt.Errorf("checking syntax: %w", &clickhouse.Exception{Code: codeSyntaxError, Message: "Syntax error"})
	if _, err := classifyExplain(wrapped); err != nil {
		t.Errorf("err = %v, want the wrapped exception to be recognised", err)
	}
}
