package query

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// What ClickHouse says when it cannot resolve a rule's result. Each one is a
// different answer to a different question, so each is classified separately
// rather than collapsed into "the query is wrong" (spec 7.3).
const (
	codeUnknownIdentifier = 47
	codeUnknownTable      = 60
	codeUnknownDatabase   = 81
)

// classifyDescribe separates a rule that cannot run from a source whose user
// cannot look, and both from a ruler that could not ask.
func classifyDescribe(err error) (*Finding, error) {
	if err == nil {
		return nil, nil
	}

	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		message := strings.TrimSpace(ex.Message)

		switch ex.Code {
		case codeUnknownIdentifier, codeUnknownTable, codeUnknownDatabase:
			// The rule names something that is not there. A column dropped by
			// a schema change lands here, which is the case that otherwise
			// surfaces as an evaluation failing in production long after the
			// migration that caused it.
			return &Finding{
				Check: lint.CheckRuleColumns,
				Detail: "the query does not resolve against this cluster, so the rule cannot run: " +
					message,
			}, nil

		case codeAccessDenied:
			// Says nothing about the SQL: the query was never resolved, so no
			// other check on the result could run either. Whether that blocks
			// is the operator's answer per source (spec 7.7, 10.3), which is
			// why it is a configurable check of its own rather than a mode of
			// rule/columns.
			return &Finding{
				Check: lint.CheckRuleTableAccess,
				Detail: "this source's user cannot read what the rule asks for, so its result was " +
					"never checked: " + message,
			}, nil
		}
	}
	return nil, fmt.Errorf("describing the rule's result: %w", err)
}

// Column is one column of a rule's result.
//
// The names are what an alert's labels will be, so they are the rule's
// contract with everything downstream: routing, annotations and an alert's
// identity all read them (spec 6.3).
type Column struct {
	Name string
	Type string
}

// describe asks ClickHouse what the rule's result looks like.
//
// `DESCRIBE (SELECT ...)` resolves the query and returns its output columns
// without reading a row, which is the only way to know what a rule actually
// produces: a subquery alias, a CTE or SELECT * all name columns the file
// never mentions, and spec 7.2 rules out working it out ourselves.
//
// This is a second round trip per rule per source, after EXPLAIN AST. Neither
// reads data, so the cost is a parse and a network hop, and inspectTimeout
// now covers both.
func (q *Querier) describe(ctx context.Context, sql string) ([]Column, error) {
	rows, err := q.conn.Query(ctx, "DESCRIBE ("+sql+")")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// DESCRIBE returns more than a name and a type: defaults, a comment, a
	// codec and a TTL expression. The extra columns are read by position from
	// whatever the server sent rather than by a fixed count, so a release that
	// adds one does not break the scan.
	names := rows.Columns()
	nameAt, typeAt := indexOf(names, "name"), indexOf(names, "type")
	if nameAt < 0 || typeAt < 0 {
		return nil, fmt.Errorf("DESCRIBE returned %v, want a name and a type", names)
	}

	var out []Column
	for rows.Next() {
		cells := make([]string, len(names))
		dest := make([]any, len(names))
		for i := range cells {
			dest[i] = &cells[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("describing the rule's result: %w", err)
		}
		out = append(out, Column{Name: cells[nameAt], Type: cells[typeAt]})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("describing the rule's result: %w", err)
	}
	return out, nil
}

func indexOf(names []string, want string) int {
	for i, name := range names {
		if name == want {
			return i
		}
	}
	return -1
}

// hasValueColumn reports whether the result carries the one column that is not
// a label. Without it there is nothing to compare against a threshold, so the
// rule returns rows and produces no alert.
func hasValueColumn(cols []Column) bool {
	for _, c := range cols {
		if c.Name == valueColumn {
			return true
		}
	}
	return false
}

// producedProtectedLabels returns the protected labels the query produces as
// columns, sorted.
//
// The tier 0 check reads the SQL as text and sees only a literal `AS team`.
// This sees the resolved result, so an alias inside a subquery, a CTE, or a
// column arriving through SELECT * is caught the same as one written plainly
// (spec 6.3.1, 7.3).
func producedProtectedLabels(cols []Column, protected []string) []string {
	blocked := make(map[string]bool, len(protected))
	for _, name := range protected {
		blocked[strings.ToLower(name)] = true
	}

	var out []string
	for _, c := range cols {
		if c.Name != valueColumn && blocked[strings.ToLower(c.Name)] {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)

	return out
}

// Unresolved is one annotation and the fields it reads that nothing will
// produce.
type Unresolved struct {
	Annotation string
	Fields     []string
}

// unresolvedFields returns the annotations reading something no label will
// carry at evaluation time.
//
// An annotation renders over the alert's labels plus its value, and those
// labels come from three places: the result columns, the rule's own labels
// overlaid on its group's, and the source's, plus the two the ruler sets.
// Only the first is discoverable here, so the caller passes the rest.
//
// A template that does not parse is skipped. Tier 0 already reports it as
// annotations/template against the file, and one mistake deserves one finding.
func unresolvedFields(annotations map[string]string, cols []Column, known []string) []Unresolved {
	resolvable := map[string]bool{valueColumn: true}
	for _, c := range cols {
		resolvable[c.Name] = true
	}
	for _, name := range known {
		resolvable[name] = true
	}

	names := make([]string, 0, len(annotations))
	for name := range annotations {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []Unresolved
	for _, name := range names {
		var missing []string
		for _, field := range templateFields(annotations[name]) {
			if !resolvable[field] {
				missing = append(missing, field)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			out = append(out, Unresolved{Annotation: name, Fields: missing})
		}
	}
	return out
}

// templateFields returns the field names an annotation reads, such as
// ServiceName for {{ .ServiceName }}.
//
// Walks the parsed template rather than matching text, for the reason spec 7.2
// gives about the SQL: a regular expression over the template would miss a
// field inside a pipeline or a conditional and invent ones inside a string.
func templateFields(text string) []string {
	t, err := template.New("annotation").Parse(text)
	if err != nil || t.Tree == nil {
		return nil
	}

	seen := map[string]bool{}
	var walk func(parse.Node)
	walk = func(n parse.Node) {
		switch node := n.(type) {
		case nil:
			return
		case *parse.FieldNode:
			// The first identifier is what an alert's labels are keyed by:
			// .Labels.team is not a shape anything here produces.
			if len(node.Ident) > 0 {
				seen[node.Ident[0]] = true
			}
		case *parse.ListNode:
			if node == nil {
				return
			}
			for _, child := range node.Nodes {
				walk(child)
			}
		case *parse.ActionNode:
			walk(node.Pipe)
		case *parse.PipeNode:
			if node == nil {
				return
			}
			for _, cmd := range node.Cmds {
				walk(cmd)
			}
		case *parse.CommandNode:
			for _, arg := range node.Args {
				walk(arg)
			}
		case *parse.IfNode:
			walk(node.Pipe)
			walk(node.List)
			walk(node.ElseList)
		case *parse.RangeNode:
			walk(node.Pipe)
			walk(node.List)
			walk(node.ElseList)
		case *parse.WithNode:
			walk(node.Pipe)
			walk(node.List)
			walk(node.ElseList)
		}
	}
	walk(t.Root)

	out := make([]string, 0, len(seen))
	for field := range seen {
		out = append(out, field)
	}
	sort.Strings(out)

	return out
}
