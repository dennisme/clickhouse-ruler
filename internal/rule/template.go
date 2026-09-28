package rule

import (
	"sort"
	"text/template"
	"text/template/parse"
)

// TemplateFields returns the field names a template reads, such as ServiceName
// for {{ .ServiceName }}.
//
// Walks the parsed template rather than matching text, for the reason spec 7.2
// gives about the SQL: a regular expression over the template would miss a
// field inside a pipeline or a conditional and invent ones inside a string.
//
// Here rather than beside either caller, because both the check that compares
// an annotation's fields against a query's result columns and the ruler
// rendering that annotation against a real alert need the same answer, and
// neither package can import the other.
func TemplateFields(t *template.Template) []string {
	if t == nil || t.Tree == nil {
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

	if len(seen) == 0 {
		return nil
	}

	out := make([]string, 0, len(seen))
	for field := range seen {
		out = append(out, field)
	}
	sort.Strings(out)

	return out
}
