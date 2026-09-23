//go:build integration

package query

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
)

// inspect runs a real inspection against ClickHouse, which is the only way to
// know the AST reader agrees with the server rather than with a fixture that
// was captured once.
func inspect(t *testing.T, expr string, c Checks) []Finding {
	t.Helper()

	q, err := Open(testSource(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := q.Inspect(ctx, rule.Rule{Alert: "Probe", Expr: expr}, c)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return got
}

func checkNames(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Check)
	}
	return out
}

const goodExpr = `
SELECT ServiceName, max(Duration) AS value
FROM otel.otel_traces
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
GROUP BY ServiceName`

func defaultChecks() Checks {
	return Checks{Nondeterministic: []string{"now", "now64", "today", "yesterday", "rand"}}
}

// The rule the compose stack's own fixture uses. A check that cannot stay
// quiet on a working rule is one nobody will keep enabled.
func TestInspectSaysNothingAboutAWorkingRule(t *testing.T) {
	if got := inspect(t, goodExpr, defaultChecks()); len(got) != 0 {
		t.Errorf("findings = %v, want none", checkNames(got))
	}
}

func TestInspectReportsSelectStar(t *testing.T) {
	expr := `SELECT * FROM otel.otel_traces WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}`

	got := inspect(t, expr, defaultChecks())
	if len(got) != 1 || got[0].Check != lint.CheckRuleSelectStar {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleSelectStar)
	}
}

// The tenancy escape, and the reason the allowlist is load bearing: nothing
// in ClickHouse refuses numbers() for the source's user.
func TestInspectReportsATableFunction(t *testing.T) {
	expr := `SELECT number AS value FROM numbers(10) WHERE {{ .From }} <= {{ .To }}`

	got := inspect(t, expr, defaultChecks())
	if len(got) != 1 || got[0].Check != lint.CheckRuleTableFunction {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleTableFunction)
	}
	if !strings.Contains(got[0].Detail, "numbers") {
		t.Errorf("detail = %q, want it to name numbers", got[0].Detail)
	}
}

// Position is what separates the two, against the real parser rather than a
// captured fixture: this query calls a scalar function and reads a table.
func TestInspectDoesNotConfuseAScalarFunctionWithATableFunction(t *testing.T) {
	expr := `
SELECT ServiceName, toUnixTimestamp(max(Timestamp)) AS value
FROM otel.otel_traces
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
GROUP BY ServiceName`

	for _, f := range inspect(t, expr, defaultChecks()) {
		if f.Check == lint.CheckRuleTableFunction {
			t.Errorf("reported a table function: %s", f.Detail)
		}
	}
}

func TestInspectHonoursTheAllowlistAgainstTheServer(t *testing.T) {
	expr := `SELECT number AS value FROM numbers(10) WHERE {{ .From }} <= {{ .To }}`

	c := defaultChecks()
	c.AllowedTableFunctions = []string{"numbers"}

	if got := inspect(t, expr, c); len(got) != 0 {
		t.Errorf("findings = %v, want none: the allowlist permits numbers()", checkNames(got))
	}
}

func TestInspectReportsNondeterminism(t *testing.T) {
	expr := `
SELECT ServiceName, count() AS value
FROM otel.otel_traces
WHERE Timestamp >= now() - 300 AND Timestamp < {{ .To }} AND {{ .From }} <= {{ .To }}
GROUP BY ServiceName`

	got := inspect(t, expr, defaultChecks())
	if len(got) != 1 || got[0].Check != lint.CheckRuleNondeterministic {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleNondeterministic)
	}
}

// Syntax comes first and alone. Three findings about one unparseable
// statement would bury the one that matters.
func TestInspectReportsSyntaxAloneAndFirst(t *testing.T) {
	expr := `SELECT * FROM WHERE {{ .From }} {{ .To }}`

	got := inspect(t, expr, defaultChecks())
	if len(got) != 1 || got[0].Check != lint.CheckRuleSyntax {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleSyntax)
	}
	if !strings.Contains(got[0].Detail, "Syntax error") {
		t.Errorf("detail = %q, want it to say where parsing stopped", got[0].Detail)
	}
}

// A second statement is a second thing nobody reviewed, and ClickHouse
// refuses it for us rather than us hunting semicolons in a string.
func TestInspectReportsASecondStatement(t *testing.T) {
	expr := `SELECT 1 AS value FROM otel.otel_traces WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}; SELECT 2`

	got := inspect(t, expr, defaultChecks())
	if len(got) != 1 || got[0].Check != lint.CheckRuleSyntax {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleSyntax)
	}
	if !strings.Contains(got[0].Detail, "Multi-statements are not allowed") {
		t.Errorf("detail = %q, want it to say why", got[0].Detail)
	}
}

// A rule's SQL carries no parameters when it is checked: the time bounds are
// rendered to literals, so a brace inside a string literal is just text and
// the server has nothing to substitute.
func TestInspectHandlesBracesInALiteral(t *testing.T) {
	expr := `
SELECT ServiceName, count() AS value
FROM otel.otel_traces
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }} AND SpanName != '{not:aParameter}'
GROUP BY ServiceName`

	if got := inspect(t, expr, defaultChecks()); len(got) != 0 {
		t.Errorf("findings = %v, want none", checkNames(got))
	}
}

// Nothing here reads a row or runs the rule, so a query that would be refused
// or would cost something is still inspectable. This one names a table the
// source's user cannot read.
func TestInspectNeitherReadsNorNeedsAGrant(t *testing.T) {
	expr := `SELECT 1 AS value FROM otel.no_such_table WHERE {{ .From }} <= {{ .To }}`

	if got := inspect(t, expr, defaultChecks()); len(got) != 0 {
		t.Errorf("findings = %v, want none: parsing needs no grant", checkNames(got))
	}
}

// A SETTINGS clause is applied to this very parse and still shows in the
// tree, which is the fact the check rests on.
func TestInspectReportsASettingsClause(t *testing.T) {
	expr := `
SELECT ServiceName, count() AS value
FROM otel.otel_traces
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
GROUP BY ServiceName
SETTINGS max_execution_time = 300`

	got := inspect(t, expr, defaultChecks())
	if len(got) != 1 || got[0].Check != lint.CheckRuleSettings {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleSettings)
	}
}

// One level down is the case worth having: a clause inside a subquery applies
// to that read the same as one at the top.
func TestInspectReportsASettingsClauseInASubquery(t *testing.T) {
	expr := `
SELECT s AS ServiceName, count() AS value
FROM (
  SELECT ServiceName AS s
  FROM otel.otel_traces
  WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
  SETTINGS max_rows_to_read = 100000000
)
GROUP BY s`

	got := inspect(t, expr, defaultChecks())
	if len(got) != 1 || got[0].Check != lint.CheckRuleSettings {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleSettings)
	}
}

func TestInspectReportsAForeignTable(t *testing.T) {
	expr := `
SELECT count() AS value, 'parts' AS ServiceName
FROM system.parts
WHERE modification_time >= {{ .From }} AND modification_time < {{ .To }}`

	c := defaultChecks()
	c.Database = "otel"

	got := inspect(t, expr, c)
	if len(got) != 1 || got[0].Check != lint.CheckRuleForeignTable {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleForeignTable)
	}
	if !strings.Contains(got[0].Detail, "system.parts") {
		t.Errorf("detail = %q, want it to name the table", got[0].Detail)
	}
}

// The source's own database is not foreign however the rule spells it, and an
// unqualified name resolves to that same database.
func TestInspectSaysNothingAboutTheSourcesOwnDatabase(t *testing.T) {
	c := defaultChecks()
	c.Database = "otel"

	if got := inspect(t, goodExpr, c); len(got) != 0 {
		t.Errorf("findings = %v, want none", checkNames(got))
	}
}

func TestInspectReportsComplexity(t *testing.T) {
	expr := `
SELECT a.ServiceName, count() AS value
FROM otel.otel_traces AS a
INNER JOIN (
  SELECT ServiceName FROM otel.otel_traces WHERE Timestamp >= {{ .From }}
) AS b ON a.ServiceName = b.ServiceName
WHERE a.Timestamp >= {{ .From }} AND a.Timestamp < {{ .To }}
GROUP BY a.ServiceName`

	c := defaultChecks()
	c.Complexity = &Complexity{MaxJoins: 0, MaxSubqueries: 0}

	got := inspect(t, expr, c)
	if len(got) != 1 || got[0].Check != lint.CheckRuleComplexity {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleComplexity)
	}
	for _, want := range []string{"1 join", "1 subquery"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("detail = %q, want it to carry %q", got[0].Detail, want)
		}
	}

	// The same query under the shipped ceilings is a rule nobody hears about.
	c.Complexity = &Complexity{MaxJoins: 2, MaxSubqueries: 2}
	if got := inspect(t, expr, c); len(got) != 0 {
		t.Errorf("findings = %v, want none under the shipped ceilings", checkNames(got))
	}
}
