package rule

import (
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"gopkg.in/yaml.v3"
)

// RuleFile is one parsed rule file.
type RuleFile struct {
	File   string
	Groups []Group
}

// Group is a set of rules sharing an evaluation interval.
type Group struct {
	Name     string
	Interval time.Duration

	// Labels apply to every rule in the group and are the weakest of the
	// three label sources in spec 6.3.1. A rule's own labels override them.
	Labels map[string]string

	Rules []Rule

	lines lint.Lines
}

// Rule is a single alerting rule.
type Rule struct {
	Alert  string
	Source string
	Expr   string

	// Window is how much time the query examines. It defaults to the group's
	// interval, which is the data produced since the previous evaluation.
	Window        time.Duration
	For           time.Duration
	KeepFiringFor time.Duration
	Labels        map[string]string
	Annotations   map[string]string

	lines lint.Lines
}

// Line is the line the rule starts on.
func (r Rule) Line() int { return r.lines.Start }

// EffectiveLabels is the group's labels overlaid with the rule's own, which is
// levels 1 and 2 of the precedence in spec 6.3.1. Level 3, the result columns,
// is applied per instance at evaluation time and is not known here.
//
// Validation and evaluation must agree on this, otherwise a rule taking its
// team from the group passes one and fails the other.
func (g Group) EffectiveLabels(r Rule) map[string]string {
	out := make(map[string]string, len(g.Labels)+len(r.Labels))
	for k, v := range g.Labels {
		out[k] = v
	}
	for k, v := range r.Labels {
		out[k] = v
	}
	return out
}

// lineOf returns the line the given yaml key appeared on. See lint.Lines.
func (g Group) lineOf(keys ...string) int { return g.lines.Of(keys...) }

// LineOf is the line a key appeared on. Exported because checks outside this
// package still have to point at a line of the diff.
func (r Rule) LineOf(keys ...string) int { return r.lines.Of(keys...) }

// has reports whether a yaml key was present in the file. It separates a
// field that was left out from one explicitly set to a zero value, which for
// a duration are different intentions.
func (r Rule) has(key string) bool { return r.lines.Has(key) }

// Parse decodes a rule file, collecting every problem it finds rather than
// stopping at the first.
func Parse(file string, data []byte) (*RuleFile, []lint.Problem) {
	f := &RuleFile{File: file}
	r := lint.NewReader(file)

	doc, ok := r.Document(data)
	if !ok {
		return f, r.Problems()
	}
	if !r.Mapping(doc, "file") {
		return f, r.Problems()
	}

	for _, e := range lint.Entries(doc) {
		switch e.Key.Value {
		case "groups":
			f.Groups = parseGroups(r, e.Value)
		default:
			r.UnknownField(e.Key, "file")
		}
	}

	return f, r.Problems()
}

func parseGroups(r *lint.Reader, n *yaml.Node) []Group {
	if !r.Sequence(n, "groups") {
		return nil
	}
	groups := make([]Group, 0, len(n.Content))
	for _, item := range n.Content {
		if !r.Mapping(item, "group") {
			continue
		}
		groups = append(groups, parseGroup(r, item))
	}
	return groups
}

func parseGroup(r *lint.Reader, n *yaml.Node) Group {
	g := Group{lines: lint.NewLines(n.Line)}

	for _, e := range lint.Entries(n) {
		g.lines.Set(e.Key.Value, e.Key.Line)
		switch e.Key.Value {
		case "name":
			g.Name, _ = r.Scalar(e.Value, "group name")
		case "interval":
			g.Interval, _ = r.Duration(e.Value, "group interval")
		case "labels":
			g.Labels = r.StringMap(e.Value, "labels", "group labels", g.lines.Keys())
		case "rules":
			g.Rules = parseRules(r, e.Value)
		default:
			r.UnknownField(e.Key, "group")
		}
	}

	// The interval is only known once the whole group has been read, so the
	// window default is applied here rather than while reading each rule.
	for i := range g.Rules {
		if !g.Rules[i].has("window") {
			g.Rules[i].Window = g.Interval
		}
	}

	return g
}

func parseRules(r *lint.Reader, n *yaml.Node) []Rule {
	if !r.Sequence(n, "rules") {
		return nil
	}
	rules := make([]Rule, 0, len(n.Content))
	for _, item := range n.Content {
		if !r.Mapping(item, "rule") {
			continue
		}
		rules = append(rules, parseRule(r, item))
	}
	return rules
}

func parseRule(r *lint.Reader, n *yaml.Node) Rule {
	rl := Rule{lines: lint.NewLines(n.Line)}
	firstProblem := r.Count()

	for _, e := range lint.Entries(n) {
		rl.lines.Set(e.Key.Value, e.Key.Line)
		switch e.Key.Value {
		case "alert":
			rl.Alert, _ = r.Scalar(e.Value, "alert")
		case "source":
			rl.Source, _ = r.Scalar(e.Value, "source")
		case "expr":
			rl.Expr, _ = r.Scalar(e.Value, "expr")
		case "window":
			rl.Window, _ = r.Duration(e.Value, "window")
		case "for":
			rl.For, _ = r.Duration(e.Value, "for")
		case "keep_firing_for":
			rl.KeepFiringFor, _ = r.Duration(e.Value, "keep_firing_for")
		case "labels":
			rl.Labels = r.StringMap(e.Value, "labels", "labels", rl.lines.Keys())
		case "annotations":
			rl.Annotations = r.StringMap(e.Value, "annotations", "annotations", rl.lines.Keys())
		default:
			r.UnknownField(e.Key, "rule")
		}
	}

	r.AttributeFrom(firstProblem, rl.Alert)
	return rl
}
