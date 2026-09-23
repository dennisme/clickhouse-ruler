package query

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Node is one node of a ClickHouse `EXPLAIN AST` tree.
//
// The tree is what makes these checks possible without a SQL parser of our
// own, which spec 7.2 rules out: ClickHouse has already parsed the query, and
// this reads its answer. Searching the query text instead would flag a column
// named dropped_spans and miss the same word reached through a comment or a
// quoted identifier.
type Node struct {
	// Kind is the node type, such as Function, TableExpression or Asterisk.
	Kind string

	// Detail is whatever followed the kind: a function or table name, a
	// literal's value. Empty for nodes that carry nothing.
	Detail string

	Parent   *Node
	Children []*Node
}

// parseAST reads `EXPLAIN AST` output, which nests by one space per level:
//
//	TablesInSelectQuery (children 1)
//	 TablesInSelectQueryElement (children 1)
//	  TableExpression (children 1)
//	   Function numbers (children 1)
//
// Depth is the whole point. A scalar now() and a table function numbers() are
// both Function nodes, and only where they sit tells them apart.
func parseAST(out string) (*Node, error) {
	var root *Node

	// byDepth[d] is the most recent node seen at depth d, which is the parent
	// of the next node at depth d+1.
	var byDepth []*Node

	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		depth := len(line) - len(strings.TrimLeft(line, " "))
		kind, detail := splitNode(strings.TrimSpace(line))
		node := &Node{Kind: kind, Detail: detail}

		switch {
		case root == nil:
			if depth != 0 {
				return nil, fmt.Errorf("EXPLAIN AST starts at depth %d, want a root at depth 0", depth)
			}
			root = node
		case depth == 0:
			return nil, errors.New("EXPLAIN AST has a second root, want one tree")
		case depth > len(byDepth):
			return nil, fmt.Errorf("EXPLAIN AST node %q at depth %d has no parent at depth %d", kind, depth, depth-1)
		default:
			parent := byDepth[depth-1]
			node.Parent = parent
			parent.Children = append(parent.Children, node)
		}

		byDepth = append(byDepth[:depth], node)
	}

	if root == nil {
		return nil, errors.New("EXPLAIN AST returned nothing to read")
	}
	return root, nil
}

// splitNode separates a node's kind from what follows it, dropping the
// "(children N)" suffix and any "(alias x)": the structure already carries the
// children, and an alias is not what any check is asking about.
func splitNode(line string) (kind, detail string) {
	if i := strings.Index(line, " ("); i >= 0 {
		line = line[:i]
	}

	kind, detail, _ = strings.Cut(line, " ")
	return kind, detail
}

// find walks the whole tree, so a node inside a CTE or a subquery is found the
// same as one at the top. A check that only looked at the outer query would
// miss exactly the cases worth hiding something in.
func find(root *Node, match func(*Node) bool) []*Node {
	var out []*Node

	var walk func(*Node)
	walk = func(n *Node) {
		if n == nil {
			return
		}
		if match(n) {
			out = append(out, n)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)

	return out
}

// tableFunctions returns the name of every function in table position.
//
// Position, never the name. ClickHouse writes `Function numbers` for a table
// function and `Function now` for a scalar one, and a name list would have to
// know every table function that exists to tell them apart, which is the
// blocklist spec 7.3 refuses.
func tableFunctions(root *Node) []string {
	var out []string

	for _, n := range find(root, func(n *Node) bool {
		return n.Kind == "Function" && n.Parent != nil && n.Parent.Kind == "TableExpression"
	}) {
		out = append(out, n.Detail)
	}
	sort.Strings(out)

	return out
}

// selectsEverything reports whether any part of the query selects *.
func selectsEverything(root *Node) bool {
	return len(find(root, func(n *Node) bool {
		return n.Kind == "Asterisk" || n.Kind == "QualifiedAsterisk"
	})) > 0
}

// setsSettings reports whether the query carries a SETTINGS clause anywhere.
//
// ClickHouse writes one as a bare `Set` node with no detail, so there is no
// name to read and nothing to reason about per setting. A clause inside a
// subquery counts: it applies to that read the same as one at the top.
func setsSettings(root *Node) bool {
	return len(find(root, func(n *Node) bool { return n.Kind == "Set" })) > 0
}

// foreignTables returns every table named with a database other than the
// given one, sorted and deduplicated.
//
// The name is read as ClickHouse wrote it, which is enough here because this
// is early feedback rather than the control: the source's grants are what
// stop a read outside its own database (spec 6.7.1). An unqualified name
// resolves to the connection's database, which is the source's own, so it is
// not foreign. A source naming no database has nothing to compare against.
func foreignTables(root *Node, database string) []string {
	if database == "" {
		return nil
	}

	seen := map[string]bool{}
	for _, n := range find(root, func(n *Node) bool { return n.Kind == "TableIdentifier" }) {
		db, _, qualified := strings.Cut(n.Detail, ".")
		if qualified && !strings.EqualFold(db, database) {
			seen[n.Detail] = true
		}
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)

	return out
}

// countKind counts the nodes of one kind anywhere in the tree.
func countKind(root *Node, kind string) int {
	return len(find(root, func(n *Node) bool { return n.Kind == kind }))
}

// functionsNamed returns the called functions appearing in the given set,
// sorted and deduplicated, so a rule calling now() twice is reported once.
func functionsNamed(root *Node, names map[string]bool) []string {
	seen := map[string]bool{}

	for _, n := range find(root, func(n *Node) bool { return n.Kind == "Function" }) {
		if name := strings.ToLower(n.Detail); names[name] {
			seen[name] = true
		}
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)

	return out
}
