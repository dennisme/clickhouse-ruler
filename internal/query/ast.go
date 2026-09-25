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

// MapKey is one subscript a query performs: a column and the key read from it.
// The column exists whether or not the key does, which is the whole reason spec
// 7.3 asks the data rather than the schema.
type MapKey struct {
	Column string
	Key    string

	// Numeric says the key was written as a number. That is usually an array
	// index and not a map key at all, but `Map(UInt64, String)` exists, so the
	// tree cannot settle it and the column's type does (see resolveKeys).
	Numeric bool
}

// MapKeySkip is a lookup that could not be turned into a question. Reported
// rather than dropped: a check that examined nothing looks exactly like a
// check that passed (spec 7.3).
type MapKeySkip struct {
	Column string
	Reason string
}

// Why a subscript carried no key to ask about.
const skipDynamicKey = "a key that is an expression, so it is only known at evaluation time"

// mapKeys returns every subscript the query performs, and every one whose key
// cannot be read from the file.
//
// Position, never the name. ClickHouse writes `LogAttributes['k']` and `arr[1]`
// as the same `Function arrayElement` over an Identifier and a Literal, so a
// name list could never tell a map lookup from an array index. Tuple access is
// its own function and is a different question. `mapContains(col, 'k')` names a
// key too, and breaks in exactly the same way when the attribute is renamed
// (spec 7.3).
//
// What the tree cannot settle is which of those subscripts is a map lookup: the
// literal's form is a strong hint and not proof, because a `Map(UInt64, String)`
// is keyed by a number. The keys come back tagged and resolveKeys decides from
// the column's type.
//
// A subscript whose base is not an identifier is left alone: in `m['a']['b']`
// the outer one reads the map the inner one returned, and the inner one is
// found on its own.
func mapKeys(root *Node) ([]MapKey, []MapKeySkip) {
	keys := map[MapKey]bool{}
	skips := map[MapKeySkip]bool{}

	for _, n := range find(root, isSubscript) {
		args := n.Children[0].Children
		base, key := args[0], args[1]

		if key.Kind != "Literal" {
			skips[MapKeySkip{Column: base.Detail, Reason: skipDynamicKey}] = true
			continue
		}

		text, quoted := literalString(key.Detail)
		if !quoted {
			text = literalNumber(key.Detail)
		}
		keys[MapKey{Column: base.Detail, Key: text, Numeric: !quoted}] = true
	}

	return sortedKeys(keys), sortedSkips(skips)
}

// isSubscript reports whether a node reads one element out of a column by key,
// with the two arguments that shape has. Tuple access is excluded: a tuple has
// positions rather than keys, so nothing about it could be missing.
//
// A base that is not an identifier is excluded too. The key it names belongs to
// whatever produced that value, and there is no column to ask about.
func isSubscript(n *Node) bool {
	if n.Kind != "Function" {
		return false
	}
	switch strings.ToLower(n.Detail) {
	case "arrayelement", "mapcontains":
	default:
		return false
	}
	if len(n.Children) != 1 || n.Children[0].Kind != "ExpressionList" {
		return false
	}
	args := n.Children[0].Children

	return len(args) == 2 && args[0].Kind == "Identifier"
}

// bareColumn drops the qualifier an identifier carries, so a subscript written
// on a table alias resolves to the column: `t.SpanAttributes` is the table's
// `SpanAttributes`.
//
// Only ever a fallback, because a dot is not always a qualifier. A Nested column
// flattens into real columns whose names contain one, and `Events.Attributes` is
// the whole name rather than `Attributes` on something called `Events`. Which of
// the two a name is cannot be read off the tree, so resolveKeys asks the table
// and tries the full name first.
func bareColumn(identifier string) string {
	if i := strings.LastIndex(identifier, "."); i >= 0 {
		return identifier[i+1:]
	}
	return identifier
}

// literalString decodes a string literal as `EXPLAIN AST` prints one, and
// reports whether it was a string at all. A numeric literal arrives as
// `UInt64_1` and is how an array index is told from a map key.
//
// Two layers, because the output is the SQL literal text escaped once more:
// the key `a'b` is written `'a\'b'` in SQL and printed as `\'a\\\'b\'`. Probing
// for either intermediate form looks for a key nobody wrote.
func literalString(detail string) (string, bool) {
	text := unescape(detail)

	if len(text) < 2 || text[0] != '\'' || text[len(text)-1] != '\'' {
		return "", false
	}

	return unescape(text[1 : len(text)-1]), true
}

// literalNumber reads the value off a numeric literal, which `EXPLAIN AST`
// writes with its type in front: `UInt64_1`, `Float64_0.99`. The type is not
// what a key is compared against, so only the value is kept.
func literalNumber(detail string) string {
	if _, value, found := strings.Cut(detail, "_"); found {
		return value
	}
	return detail
}

// unescape removes one layer of backslash escaping. Beyond the quote and the
// backslash the output escapes, ClickHouse writes the usual control-character
// forms inside a string literal, so they are decoded here rather than left to
// arrive as a bare letter.
func unescape(s string) string {
	var out strings.Builder
	out.Grow(len(s))

	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			out.WriteByte(s[i])
			continue
		}

		i++
		switch s[i] {
		case 'n':
			out.WriteByte('\n')
		case 't':
			out.WriteByte('\t')
		case 'r':
			out.WriteByte('\r')
		case '0':
			out.WriteByte(0)
		default:
			out.WriteByte(s[i])
		}
	}

	return out.String()
}

// sortedKeys orders by column then key, so a finding reads the same way twice
// and a rule reading one key in two places reports it once.
func sortedKeys(set map[MapKey]bool) []MapKey {
	out := make([]MapKey, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Column != out[j].Column {
			return out[i].Column < out[j].Column
		}
		return out[i].Key < out[j].Key
	})

	return out
}

func sortedSkips(set map[MapKeySkip]bool) []MapKeySkip {
	out := make([]MapKeySkip, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Column != out[j].Column {
			return out[i].Column < out[j].Column
		}
		return out[i].Reason < out[j].Reason
	})

	return out
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
