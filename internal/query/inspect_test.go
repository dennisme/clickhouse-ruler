package query

import (
	"errors"
	"fmt"
	"net"
	"testing"

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
	got, ok := findingFor(inspectTree(readAST(t, "ast_select_star.txt"), checksFor()), CheckSelectStar)
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
	got, ok := findingFor(inspectTree(readAST(t, "ast_table_function_in_cte.txt"), checksFor()), CheckTableFunction)
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

	if _, ok := findingFor(findings, CheckTableFunction); ok {
		t.Error("merge() was reported although the allowlist permits it")
	}
}

func TestInspectTreeAllowlistIsCaseInsensitive(t *testing.T) {
	findings := inspectTree(readAST(t, "ast_table_function_in_cte.txt"), checksFor("MERGE"))

	if _, ok := findingFor(findings, CheckTableFunction); ok {
		t.Error("MERGE in the allowlist did not permit merge()")
	}
}

func TestInspectTreeReportsNondeterministicFunctions(t *testing.T) {
	got, ok := findingFor(inspectTree(readAST(t, "ast_nondeterministic.txt"), checksFor()), CheckNondeterministic)
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

func TestClassifySyntax(t *testing.T) {
	syntaxErr := func(msg string) error {
		return &clickhouse.Exception{Code: codeSyntaxError, Message: msg}
	}

	tests := []struct {
		name         string
		err          error
		wantFinding  bool
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
			name:    "some other server error",
			err:     &clickhouse.Exception{Code: 241, Message: "Memory limit exceeded"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := classifySyntax(tt.err)

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
		})
	}
}

func TestClassifySyntaxUnwraps(t *testing.T) {
	wrapped := fmt.Errorf("checking syntax: %w", &clickhouse.Exception{Code: codeSyntaxError, Message: "Syntax error"})
	if _, err := classifySyntax(wrapped); err != nil {
		t.Errorf("err = %v, want the wrapped exception to be recognised", err)
	}
}
