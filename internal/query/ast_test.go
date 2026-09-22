package query

import (
	"os"
	"path/filepath"
	"testing"
)

func readAST(t *testing.T, name string) *Node {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	root, err := parseAST(string(data))
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return root
}

// The fixtures are real `EXPLAIN AST` output, captured from the server rather
// than written by hand, so the parser is tested against the shape ClickHouse
// actually produces.
func TestParseASTBuildsTheTree(t *testing.T) {
	root := readAST(t, "ast_plain.txt")

	if root.Kind != "SelectWithUnionQuery" {
		t.Errorf("root kind = %q, want SelectWithUnionQuery", root.Kind)
	}
	if len(root.Children) != 1 {
		t.Fatalf("root has %d children, want 1", len(root.Children))
	}

	// Depth is what tells a table function from a scalar one, so it has to
	// survive parsing rather than being flattened away.
	got := find(root, func(n *Node) bool { return n.Kind == "TableIdentifier" })
	if len(got) != 1 {
		t.Fatalf("found %d table identifiers, want 1", len(got))
	}
	if got[0].Detail != "otel.otel_traces" {
		t.Errorf("table = %q, want otel.otel_traces", got[0].Detail)
	}
	if got[0].Parent == nil || got[0].Parent.Kind != "TableExpression" {
		t.Error("a table identifier must know it sits under a TableExpression")
	}
}

func TestParseASTKeepsDetailAndDropsTheChildCount(t *testing.T) {
	root := readAST(t, "ast_nondeterministic.txt")

	for _, n := range find(root, func(n *Node) bool { return n.Kind == "Function" }) {
		if n.Detail == "" {
			t.Fatal("a function node with no name cannot be checked against any list")
		}
		// "Function now (alias t) (children 1)" must not leave the child
		// count or the alias in the name.
		if n.Detail != "now" && n.Detail != "today" && n.Detail != "greaterOrEquals" {
			t.Errorf("function name = %q, want the bare name", n.Detail)
		}
	}
}

// A table function inside a CTE is still a table function. This is the case a
// flat scan of the output would report correctly by accident and a naive
// depth-1 check would miss entirely.
func TestParseASTFindsNestedNodes(t *testing.T) {
	root := readAST(t, "ast_table_function_in_cte.txt")

	got := tableFunctions(root)
	if len(got) != 1 {
		t.Fatalf("found %v, want one table function", got)
	}
	if got[0] != "merge" {
		t.Errorf("table function = %q, want merge", got[0])
	}
}

// The distinction the whole check rests on: both are `Function` nodes, and
// only position separates them.
func TestTableFunctionsIgnoresScalarFunctions(t *testing.T) {
	root := readAST(t, "ast_nondeterministic.txt")

	if got := tableFunctions(root); len(got) != 0 {
		t.Errorf("tableFunctions = %v, want none: now() and today() are scalar", got)
	}
}

func TestSelectsEverything(t *testing.T) {
	if !selectsEverything(readAST(t, "ast_select_star.txt")) {
		t.Error("selectsEverything = false for SELECT *")
	}
	if selectsEverything(readAST(t, "ast_plain.txt")) {
		t.Error("selectsEverything = true for a query naming its columns")
	}
}

func TestFunctionsNamed(t *testing.T) {
	got := functionsNamed(readAST(t, "ast_nondeterministic.txt"), map[string]bool{"now": true, "today": true})

	if len(got) != 2 {
		t.Fatalf("got %v, want now and today", got)
	}
	if got[0] != "now" || got[1] != "today" {
		t.Errorf("got %v, want [now today] sorted", got)
	}
}

// A subquery is another place a check has to reach, and its columns are the
// outer query's, so SELECT * inside one matters as much as outside.
func TestSelectsEverythingReachesIntoASubquery(t *testing.T) {
	root := readAST(t, "ast_subquery.txt")

	if selectsEverything(root) {
		t.Error("selectsEverything = true, but every column in this fixture is named")
	}
	if got := len(find(root, func(n *Node) bool { return n.Kind == "Subquery" })); got != 1 {
		t.Errorf("found %d subqueries, want 1", got)
	}
}

func TestParseASTRejectsUnreadableOutput(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"blank lines only", "\n\n"},
		{"indented root", "  SelectQuery (children 1)"},
		{"a child with no parent at its depth", "SelectQuery\n   Identifier x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseAST(tt.in); err == nil {
				t.Error("parseAST returned no error, want one: an unreadable tree must never read as a clean rule")
			}
		})
	}
}
