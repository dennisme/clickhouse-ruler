package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
)

// Check names reported by an inspection. They are policy check names, spelled
// here because policy cannot import this package without a cycle; a test in
// the policy package fails if the two ever disagree.
const (
	// CheckSyntax is a correctness check: a rule whose SQL does not parse
	// cannot run, and neither can one carrying a second statement.
	CheckSyntax = "rule/syntax"

	// CheckInspect is not a finding about a rule. It is how the ruler says it
	// could not ask: a cluster that did not answer has reported nothing about
	// the SQL, and silence there would read as a rule that passed.
	CheckInspect = "rule/inspect"

	CheckSelectStar       = "rule/select-star"
	CheckTableFunction    = "rule/table-function"
	CheckNondeterministic = "rule/nondeterministic"

	// CheckSettings is a correctness check: the ruler sends
	// max_execution_time, max_memory_usage and max_rows with every query, and
	// a rule's own SETTINGS clause overrides them (spec 7.3).
	CheckSettings = "rule/settings"

	CheckForeignTable = "rule/foreign-table"
	CheckComplexity   = "rule/complexity"
)

// codeSyntaxError is ClickHouse's error for a query it could not parse,
// including one carrying more than a single statement.
const codeSyntaxError = 62

// codeSettingConstraintViolation is ClickHouse refusing a setting the user's
// profile constrains. EXPLAIN carries the statement inline, so a rule's own
// SETTINGS clause is applied to the parse and a constrained one is refused
// there: the backstop in spec 6.7 answers before the tree is ever read.
const codeSettingConstraintViolation = 452

// inspectTimeout bounds one rule's inspection. Nothing here reads a row, so
// this only covers a cluster that has stopped answering.
const inspectTimeout = 10 * time.Second

// Finding is one thing an inspection noticed. Severity is not decided here:
// the caller resolves it from policy, because the same finding blocks in one
// repository and annotates in another (spec 7.6).
type Finding struct {
	Check  string
	Detail string
}

// Checks is what an inspection should look for, resolved from policy by the
// caller.
type Checks struct {
	// AllowedTableFunctions permits table functions by name. Empty means
	// every one is refused, which is the shipped default: a rule reads its
	// source's table, and an operator permits the one they actually need.
	AllowedTableFunctions []string

	// Nondeterministic is the list of function names that break window
	// alignment. Curated rather than read from the server, which has no
	// column saying which functions are deterministic (spec 6.7.1).
	Nondeterministic []string

	// Database is the source's own database, which is what makes any other
	// one foreign. Empty means the source names none, and a rule is then
	// compared against nothing rather than against the empty string.
	Database string

	// Complexity is the ceiling on joins and subqueries, nil when no ceiling
	// is configured. A pointer rather than two ints because zero is a ceiling
	// an operator can mean: no joins at all.
	Complexity *Complexity
}

// Complexity is how much query a rule may be. A count of joins and
// subqueries is a cost proxy that reads no data, which is the whole reason it
// belongs in tier 1 (spec 7.3).
type Complexity struct {
	MaxJoins      int
	MaxSubqueries int
}

// Inspect reads a rule's SQL through ClickHouse and reports what it finds.
//
// Nothing here executes the rule: `EXPLAIN AST` parses it and returns the
// tree without reading a row, and refuses a second statement itself. An error
// means the ruler could not ask, and is never a finding about the rule.
func (q *Querier) Inspect(ctx context.Context, r rule.Rule, c Checks) ([]Finding, error) {
	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()

	sql, err := renderForCheck(r.Expr)
	if err != nil {
		return nil, err
	}

	root, err := q.explainAST(ctx, sql)

	// A query the server would not explain is the only finding worth making
	// about it, so it is reported alone rather than alongside checks that
	// could not read the tree anyway.
	finding, err := classifyExplain(err)
	if err != nil {
		return nil, err
	}
	if finding != nil {
		return []Finding{*finding}, nil
	}
	return inspectTree(root, c), nil
}

// classifyExplain separates what the server says about the rule from a ruler
// that could not ask. A query that will not parse and a SETTINGS clause the
// profile refuses are both findings; anything else is not.
func classifyExplain(err error) (*Finding, error) {
	if err == nil {
		return nil, nil
	}

	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		switch ex.Code {
		case codeSyntaxError:
			return &Finding{Check: CheckSyntax, Detail: strings.TrimSpace(ex.Message)}, nil
		case codeSettingConstraintViolation:
			// The clause is the finding, and the server has already named the
			// setting and the limit it exceeded.
			return &Finding{
				Check: CheckSettings,
				Detail: "the query sets its own SETTINGS, which the source's profile refused: " +
					strings.TrimSpace(ex.Message),
			}, nil
		}
	}
	return nil, fmt.Errorf("reading the rule's SQL: %w", err)
}

// explainAST parses the rule's SQL and returns the tree.
//
// EXPLAIN carries the statement inline, so a rule's own SETTINGS clause is
// applied to this parse. That costs nothing, because nothing here reads a
// row, and the clause is still in the tree as a `Set` node for rule/settings
// to report (spec 7.3).
func (q *Querier) explainAST(ctx context.Context, sql string) (*Node, error) {
	rows, err := q.conn.Query(ctx, "EXPLAIN AST "+sql)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, fmt.Errorf("explaining the rule's SQL: %w", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("explaining the rule's SQL: %w", err)
	}
	return parseAST(strings.Join(lines, "\n"))
}

// renderForCheck substitutes the time bounds with literal timestamps rather
// than the parameter placeholders an evaluation uses.
//
// The SQL reaches formatQuery as a string argument, and a placeholder inside
// one is left alone: the server substitutes parameters in the query it is
// given, not in the values it is passed. Literals make the checked statement
// the same shape as the evaluated one without needing that to be true.
func renderForCheck(expr string) (string, error) {
	t, err := template.New("expr").Option("missingkey=error").Parse(expr)
	if err != nil {
		return "", fmt.Errorf("parsing expr template: %w", err)
	}

	// Any instant does. Nothing here reads a row, so the window only has to
	// parse and to be the type the column comparison expects.
	bounds := struct{ From, To string }{
		From: "toDateTime64('2026-01-01 00:00:00.000', 3)",
		To:   "toDateTime64('2026-01-01 00:05:00.000', 3)",
	}

	var out strings.Builder
	if err := t.Execute(&out, bounds); err != nil {
		return "", fmt.Errorf("rendering expr: %w", err)
	}
	return out.String(), nil
}

// inspectTree runs every check that reads the parsed query.
func inspectTree(root *Node, c Checks) []Finding {
	var out []Finding

	if selectsEverything(root) {
		out = append(out, Finding{
			Check: CheckSelectStar,
			Detail: "the query selects *, so its result columns are whatever the table has today: " +
				"adding a column changes every instance's identity and refingerprints the alerts",
		})
	}

	if used := disallowedTableFunctions(root, c.AllowedTableFunctions); len(used) > 0 {
		out = append(out, Finding{
			Check: CheckTableFunction,
			Detail: fmt.Sprintf("the query reads through %s, which is not in the allowlist; "+
				"a rule reads its source's table", strings.Join(used, ", ")),
		})
	}

	if used := functionsNamed(root, nameSet(c.Nondeterministic)); len(used) > 0 {
		out = append(out, Finding{
			Check: CheckNondeterministic,
			Detail: fmt.Sprintf("the query calls %s, so it does not read the window the ruler asked for "+
				"and a replay of it cannot agree with itself", strings.Join(used, ", ")),
		})
	}

	if setsSettings(root) {
		out = append(out, Finding{
			Check: CheckSettings,
			Detail: "the query sets its own SETTINGS, which replaces the execution time, memory and row " +
				"limits the ruler sends with every evaluation",
		})
	}

	if used := foreignTables(root, c.Database); len(used) > 0 {
		out = append(out, Finding{
			Check: CheckForeignTable,
			Detail: fmt.Sprintf("the query reads %s, which is outside the source's %s database, "+
				"so the rule runs only where its user happens to be granted that table",
				strings.Join(used, ", "), c.Database),
		})
	}

	if detail := overComplexity(root, c.Complexity); detail != "" {
		out = append(out, Finding{Check: CheckComplexity, Detail: detail})
	}
	return out
}

// overComplexity describes what the query exceeds, or returns empty when it
// is within the ceilings or none are configured.
func overComplexity(root *Node, limit *Complexity) string {
	if limit == nil {
		return ""
	}

	var over []string
	if joins := countKind(root, "TableJoin"); joins > limit.MaxJoins {
		over = append(over, fmt.Sprintf("%s against a ceiling of %d", plural(joins, "join"), limit.MaxJoins))
	}
	if subqueries := countKind(root, "Subquery"); subqueries > limit.MaxSubqueries {
		over = append(over, fmt.Sprintf("%s against a ceiling of %d", plural(subqueries, "subquery"), limit.MaxSubqueries))
	}
	if len(over) == 0 {
		return ""
	}
	return "the query has " + strings.Join(over, " and ") +
		", so each evaluation costs more than the interval it repeats on was sized for"
}

// disallowedTableFunctions returns the table functions the allowlist does not
// permit. An empty allowlist refuses every one of them.
func disallowedTableFunctions(root *Node, allowed []string) []string {
	permitted := nameSet(allowed)

	var out []string
	for _, name := range tableFunctions(root) {
		if !permitted[strings.ToLower(name)] {
			out = append(out, name)
		}
	}
	return out
}

func nameSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[strings.ToLower(name)] = true
	}
	return out
}

// plural counts a thing in words an author reads without stumbling: one join,
// not 1 joins.
func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	if strings.HasSuffix(thing, "y") {
		return fmt.Sprintf("%d %sies", n, strings.TrimSuffix(thing, "y"))
	}
	return fmt.Sprintf("%d %ss", n, thing)
}
