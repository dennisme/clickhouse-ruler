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

// SELECT * is reported twice, by two checks answering different questions:
// its columns are whatever the table has today, and none of them is named
// value, so the rule also cannot fire.
func TestInspectReportsSelectStar(t *testing.T) {
	expr := `SELECT * FROM otel.otel_traces WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}`

	got := checkNames(inspect(t, expr, describeChecks()))
	want := map[string]bool{lint.CheckRuleSelectStar: true, lint.CheckRuleColumns: true}

	if len(got) != len(want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("unexpected finding %s in %v", name, got)
		}
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

// Nothing here reads a row, and the two round trips need different things
// from the cluster. EXPLAIN AST only parses, so it is happy with a table that
// does not exist; DESCRIBE has to resolve the query, which is what makes the
// missing table a finding at all.
func TestInspectParsesWithoutResolvingButStillResolves(t *testing.T) {
	expr := `SELECT 1 AS value FROM otel.no_such_table WHERE {{ .From }} <= {{ .To }}`

	c := describeChecks()

	// The parse half says nothing: no syntax finding, and every tree check
	// passed on a query naming a table nobody has.
	got := inspect(t, expr, c)
	if len(got) != 1 || got[0].Check != lint.CheckRuleColumns {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleColumns)
	}
	if !strings.Contains(got[0].Detail, "no_such_table") {
		t.Errorf("detail = %q, want it to name the table", got[0].Detail)
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

	c := describeChecks()
	c.Database = "otel"

	// Two findings, and the pair is the point: reading outside the source's
	// database is early feedback, and the grant refusing it is what actually
	// stops the rule (spec 6.7.1).
	got := checkNames(inspect(t, expr, c))
	want := map[string]bool{lint.CheckRuleForeignTable: true, lint.CheckRuleTableAccess: true}

	if len(got) != len(want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("unexpected finding %s in %v", name, got)
		}
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

// describeChecks is what the result checks need from the caller: the labels
// that will exist at evaluation time, and the ones a query may not produce.
func describeChecks() Checks {
	c := defaultChecks()
	c.KnownLabels = []string{"alertname", "severity", "source", "team"}
	c.ProtectedLabels = []string{"alertname", "source", "team"}
	return c
}

// The case a parse tree cannot see: the column is gone from the table, and
// only resolving the query against this cluster says so.
func TestInspectReportsAColumnThatIsNotThere(t *testing.T) {
	expr := `
SELECT ServiceName, max(Duration_milliseconds) AS value
FROM otel.otel_traces
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
GROUP BY ServiceName`

	got := inspect(t, expr, describeChecks())
	if len(got) != 1 || got[0].Check != lint.CheckRuleColumns {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleColumns)
	}
	if !strings.Contains(got[0].Detail, "Duration_milliseconds") {
		t.Errorf("detail = %q, want it to name what did not resolve", got[0].Detail)
	}
}

func TestInspectReportsAnUnknownTable(t *testing.T) {
	expr := `SELECT 1 AS value FROM otel.no_such_table WHERE {{ .From }} <= {{ .To }}`

	got := inspect(t, expr, describeChecks())
	if len(got) != 1 || got[0].Check != lint.CheckRuleColumns {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleColumns)
	}
}

// A rule that returns rows and produces no alert, which otherwise fails at
// evaluation time in front of nobody.
func TestInspectReportsAMissingValueColumn(t *testing.T) {
	expr := `
SELECT ServiceName, count() AS total
FROM otel.otel_traces
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
GROUP BY ServiceName`

	got := inspect(t, expr, describeChecks())
	if len(got) != 1 || got[0].Check != lint.CheckRuleColumns {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleColumns)
	}
	for _, want := range []string{"value", "total"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("detail = %q, want it to carry %q", got[0].Detail, want)
		}
	}
}

// The gap the tier 0 text check documents: no literal `AS team` appears
// anywhere in this rule, and the column is produced all the same.
func TestInspectReportsAProtectedLabelFromASubquery(t *testing.T) {
	expr := `
SELECT s AS team, count() AS value
FROM (
  SELECT ServiceName AS s
  FROM otel.otel_traces
  WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
)
GROUP BY s`

	got := inspect(t, expr, describeChecks())
	if len(got) != 1 || got[0].Check != lint.CheckRuleProtectedLabel {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleProtectedLabel)
	}
	if !strings.Contains(got[0].Detail, "team") {
		t.Errorf("detail = %q, want it to name the label", got[0].Detail)
	}
}

// Annotation variables resolve against the real output columns, which is the
// claim spec 7.3 makes about this check.
func TestInspectReportsAnAnnotationReadingNothing(t *testing.T) {
	q, err := Open(testSource(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r := rule.Rule{
		Alert: "Probe",
		Expr:  goodExpr,
		Annotations: map[string]string{
			"summary": "{{ .ServiceName }} is at {{ .value }} for {{ .team }}",
			"detail":  "owned by {{ .Squad }}",
		},
	}

	got, err := q.Inspect(ctx, r, describeChecks())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(got) != 1 || got[0].Check != lint.CheckAnnotationsTemplate {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckAnnotationsTemplate)
	}
	for _, want := range []string{"detail", "Squad"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("detail = %q, want it to carry %q", got[0].Detail, want)
		}
	}
}

// A table the source's user cannot read says nothing about the SQL, so it is
// its own check at its own severity rather than a broken rule.
func TestInspectReportsATableItCannotRead(t *testing.T) {
	expr := `
SELECT database AS ServiceName, count() AS value
FROM system.parts
WHERE modification_time >= {{ .From }} AND modification_time < {{ .To }}
GROUP BY database`

	c := describeChecks()

	got := inspect(t, expr, c)
	if len(got) != 1 || got[0].Check != lint.CheckRuleTableAccess {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleTableAccess)
	}
	if !strings.Contains(got[0].Detail, "privileges") {
		t.Errorf("detail = %q, want the server's own reason", got[0].Detail)
	}
}

// The rule the compose stack's fixture uses, with annotations that resolve.
// A check that cannot stay quiet on a working rule is one nobody keeps on.
func TestInspectSaysNothingAboutAWorkingRuleWithAnnotations(t *testing.T) {
	q, err := Open(testSource(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r := rule.Rule{
		Alert:       "Probe",
		Expr:        goodExpr,
		Annotations: map[string]string{"summary": "{{ .ServiceName }} at {{ .value }} on {{ .source }}"},
	}

	got, err := q.Inspect(ctx, r, describeChecks())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("findings = %v, want none", checkNames(got))
	}
}

// costChecks asks the cost question with ceilings a caller chose, against
// whatever the fixture table holds.
func costChecks(maxRows uint64, maxRate float64, interval time.Duration) Checks {
	c := describeChecks()
	c.Cost = &Cost{MaxRows: maxRows, MaxRowsPerSecond: maxRate}
	c.Interval = interval
	return c
}

// The estimate has to come from the real optimiser: a fixture would keep
// parsing long after the server's answer changed shape.
func TestInspectReportsAnExpensiveRule(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seedManySpans(t, q)

	expr := `
SELECT ServiceName, count() AS value
FROM otel.otel_traces
WHERE Duration > 0 AND {{ .From }} <= {{ .To }}
GROUP BY ServiceName`

	got := inspect(t, expr, costChecks(100, 1_000_000, time.Minute))
	if len(got) != 1 || got[0].Check != lint.CheckRuleCost {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleCost)
	}
	if !strings.Contains(got[0].Detail, "estimate") {
		t.Errorf("detail = %q, want it to say the number is predicted", got[0].Detail)
	}
	// This rule has no time bound on the ordering key, so the plan should say
	// the primary key excluded nothing.
	if !strings.Contains(got[0].Detail, "primary key") {
		t.Errorf("detail = %q, want it to explain why the estimate is large", got[0].Detail)
	}
}

// The same rule against a ceiling that permits it. A check that cannot be
// satisfied by raising its ceiling is one an operator turns off instead.
func TestInspectAcceptsAnExpensiveRuleUnderARaisedCeiling(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seedManySpans(t, q)

	expr := `
SELECT ServiceName, count() AS value
FROM otel.otel_traces
WHERE Duration > 0 AND {{ .From }} <= {{ .To }}
GROUP BY ServiceName`

	if got := inspect(t, expr, costChecks(10_000_000, 1_000_000, time.Minute)); len(got) != 0 {
		t.Errorf("findings = %v, want none under a ceiling that permits it", checkNames(got))
	}
}

// The interval is what makes a modest query expensive, which is the whole
// reason the rate ceiling exists.
func TestInspectReportsARuleThatIsOnlyExpensiveBecauseOfItsInterval(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seedManySpans(t, q)

	expr := `
SELECT ServiceName, count() AS value
FROM otel.otel_traces
WHERE Duration > 0 AND {{ .From }} <= {{ .To }}
GROUP BY ServiceName`

	// Row count is permitted; running it every second is not.
	hourly := costChecks(10_000_000, 1000, time.Hour)
	if got := inspect(t, expr, hourly); len(got) != 0 {
		t.Fatalf("findings = %v, want none: hourly, this is affordable", checkNames(got))
	}

	perSecond := costChecks(10_000_000, 1000, time.Second)
	got := inspect(t, expr, perSecond)
	if len(got) != 1 || got[0].Check != lint.CheckRuleCost {
		t.Fatalf("findings = %v, want only %s", checkNames(got), lint.CheckRuleCost)
	}
	if !strings.Contains(got[0].Detail, "rows a second") {
		t.Errorf("detail = %q, want it to report the rate", got[0].Detail)
	}
}

// A rule bounded on the ordering key, under the shipped ceilings. The check
// has to stay quiet here or nobody keeps it on.
func TestInspectSaysNothingAboutAnAffordableRule(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seedManySpans(t, q)

	c := describeChecks()
	c.Cost = &Cost{MaxRows: 100_000_000, MaxRowsPerSecond: 1_000_000}
	c.Interval = time.Minute

	if got := inspect(t, goodExpr, c); len(got) != 0 {
		t.Errorf("findings = %v, want none", checkNames(got))
	}
}

// seedManySpans writes enough rows for an estimate to be worth reading. A
// handful would leave every ceiling untested, since the optimiser reports
// what it would actually open and one part of three rows is one granule.
func seedManySpans(t *testing.T, q *Querier) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn := adminConn(t, q.src.Address)
	if err := conn.Exec(ctx, "TRUNCATE TABLE otel.otel_traces"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Written with the server's own generator rather than a batch from here:
	// the rows exist to give the primary index something to estimate, and
	// their contents do not matter.
	//
	// Placed around anchor rather than a date written into the statement, for
	// the reason anchor itself is read from the clock: the schema TTLs at three
	// days, and rows older than that are dropped on arrival, which leaves every
	// cost estimate at zero and every ceiling satisfied.
	err := conn.Exec(ctx, `
INSERT INTO otel.otel_traces (Timestamp, TraceId, SpanId, ServiceName, SpanName, Duration, StatusCode)
SELECT
  ? - toIntervalSecond(number % 600),
  toString(number), toString(number),
  concat('svc-', toString(number % 5)), 'GET /', number, 'Ok'
FROM numbers(200000)`, anchor)
	if err != nil {
		t.Fatalf("seeding rows: %v", err)
	}
}
