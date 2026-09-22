package rule

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"text/template"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
)

const (
	checkRuleName            = "rule/name"
	checkRuleGroupName       = "rule/group-name"
	checkRuleExpr            = "rule/expr"
	checkLabelsRequired      = "labels/required"
	checkAnnotationsRequired = "annotations/required"
	checkAnnotationsRunbook  = "annotations/runbook"
	checkAnnotationsTemplate = "annotations/template"
	checkRuleFor             = "rule/for"
	checkRuleWindow          = "rule/window"
)

// timeBoundVars are the template actions a query must use to receive its
// evaluation window. The pattern tolerates the whitespace and trim markers Go
// templates allow, because {{.From}} and {{- .From }} render identically and
// rejecting them would be a false positive.
var timeBoundVars = []struct {
	name    string
	pattern *regexp.Regexp
	reason  string
}{
	{"{{ .From }}", regexp.MustCompile(`\{\{-?\s*\.From\s*-?\}\}`), "lower time bound"},
	{"{{ .To }}", regexp.MustCompile(`\{\{-?\s*\.To\s*-?\}\}`), "upper time bound"},
}

// Validate runs every offline check against a parsed rule file and returns all
// findings. It never stops at the first, because a tool that surfaces one
// error per CI run makes authors iterate once per problem.
// policyFor resolves the effective policy for one rule. Rules in the same file
// can name different sources, and a source carries its own policy, so this
// cannot be a single value for the whole file (spec 7.7).
func Validate(f *RuleFile, policyFor func(Rule) *policy.Policy) []lint.Problem {
	if policyFor == nil {
		policyFor = func(Rule) *policy.Policy { return policy.Defaults() }
	}
	v := &validator{file: f.File, policyFor: policyFor}

	// A group's identity is (file, name): that is what the scheduler keys a
	// group by and what the rule_group metric label carries, so two groups
	// sharing a name in one file become one series with two goroutines
	// reporting into it. The same name in a different file is fine, which
	// is that identity working rather than a gap.
	groupAt := map[string]int{}

	for _, g := range f.Groups {
		v.groupName(g, groupAt)
		v.group(g)
	}
	return v.problems
}

func (v *validator) groupName(g Group, groupAt map[string]int) {
	line := g.lineOf("name")

	if g.Name == "" {
		v.addGroup(g, line, checkRuleGroupName, "group name is empty")
		return
	}
	if first, ok := groupAt[g.Name]; ok {
		v.addGroup(g, line, checkRuleGroupName,
			"duplicate group name %q, first defined on line %d", g.Name, first)
		return
	}
	groupAt[g.Name] = line
}

type validator struct {
	file      string
	policyFor func(Rule) *policy.Policy
	problems  []lint.Problem
}

// add reports a correctness failure. These are always errors: the rule cannot
// do its job, so there is no severity for an operator to choose (spec 7.6).
func (v *validator) add(r Rule, line int, check string, format string, args ...any) {
	v.problems = append(v.problems, lint.Problem{
		File:     v.file,
		Line:     line,
		Subject:  r.Alert,
		Check:    check,
		Severity: lint.SeverityError,
		Text:     fmt.Sprintf(format, args...),
	})
}

// addGroup reports a correctness failure that belongs to a group rather than
// to one of its rules, so the subject is the group's own name.
func (v *validator) addGroup(g Group, line int, check string, format string, args ...any) {
	v.problems = append(v.problems, lint.Problem{
		File:     v.file,
		Line:     line,
		Subject:  g.Name,
		Check:    check,
		Severity: lint.SeverityError,
		Text:     fmt.Sprintf(format, args...),
	})
}

// addPolicy reports a convention failure at whatever severity policy gives it,
// and says nothing at all when the check is off. The finding carries where its
// severity came from so `--explain` can answer "why is this an error".
func (v *validator) addPolicy(r Rule, line int, check string, format string, args ...any) {
	setting := v.policyFor(r).For(check)
	if setting.Severity == lint.SeverityOff {
		return
	}
	v.problems = append(v.problems, lint.Problem{
		File:       v.file,
		Line:       line,
		Subject:    r.Alert,
		Check:      check,
		Severity:   setting.Severity,
		Text:       fmt.Sprintf(format, args...),
		PolicyFile: setting.File,
		PolicyLine: setting.Line,
	})
}

func (v *validator) group(g Group) {
	// An alert's identity is its full label set, not its name, so the same
	// name in another group is a different alert: group labels are part of
	// that set. Uniqueness is therefore scoped to the group rather than to
	// the file or the tree (spec 6.3, 7.6).
	namedAt := map[string]int{}

	for _, r := range g.Rules {
		v.ruleName(g, r, namedAt)
		v.ruleExpr(r)
		v.requiredKeys(r, "labels", "label", checkLabelsRequired, g.EffectiveLabels(r))
		v.requiredKeys(r, "annotations", "annotation", checkAnnotationsRequired, r.Annotations)
		v.annotationsRunbook(r)
		v.annotationsTemplate(r)
		v.ruleFor(g, r)
		v.ruleWindow(g, r)
	}
}

func (v *validator) ruleName(g Group, r Rule, namedAt map[string]int) {
	line := r.LineOf("alert")

	if r.Alert == "" {
		v.add(r, line, checkRuleName, "alert name is empty")
		return
	}
	if first, ok := namedAt[r.Alert]; ok {
		v.add(r, line, checkRuleName,
			"duplicate alert name %q in group %q, first defined on line %d",
			r.Alert, g.Name, first)
		return
	}
	namedAt[r.Alert] = line
}

// ruleExpr enforces the lint half of the time bound decision: the ruler
// supplies the evaluation window through template variables, and a query that
// omits them would scan without bound on every evaluation.
func (v *validator) ruleExpr(r Rule) {
	line := r.LineOf("expr")

	if r.Expr == "" {
		v.add(r, line, checkRuleExpr, "expr is empty")
		return
	}
	for _, tv := range timeBoundVars {
		if !tv.pattern.MatchString(r.Expr) {
			v.add(r, line, checkRuleExpr,
				"expr does not reference %s, so the query has no %s", tv.name, tv.reason)
		}
	}
}

// requiredKeys reports keys a block must carry. A finding points at the key
// itself when it exists, at the block when the key is absent, and at the rule
// when the whole block is absent.
func (v *validator) requiredKeys(r Rule, block, noun, check string, have map[string]string) {
	for _, key := range v.policyFor(r).For(check).Keys {
		line := r.LineOf(block+"."+key, block)

		value, present := have[key]
		switch {
		case !present:
			v.addPolicy(r, line, check, "required %s %q is missing", noun, key)
		case value == "":
			v.addPolicy(r, line, check, "required %s %q is empty", noun, key)
		}
	}
}

// annotationsRunbook rejects a runbook that will not open from a pager, which
// is anything without a scheme and host. An absent runbook is left to
// annotations/required so the two checks do not both report it.
func (v *validator) annotationsRunbook(r Rule) {
	raw, ok := r.Annotations["runbook_url"]
	if !ok || raw == "" {
		return
	}

	u, err := url.Parse(raw)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return
	}
	v.addPolicy(r, r.LineOf("annotations.runbook_url", "annotations"), checkAnnotationsRunbook,
		"runbook_url must be an absolute http or https URL, got %q", raw)
}

// annotationsTemplate reports an annotation that is not a parseable template.
// A rule whose summary cannot be parsed still pages, carrying the parse failure
// where the summary should be, so this is a rule an author wants to hear about
// before it reaches an on-call rotation.
//
// Parsing is all this can do offline. Whether a variable names a column the
// query actually returns depends on the result, which is tier 1 (spec 7.3), so
// the finding says "not a valid template" rather than claiming the annotation
// will render.
//
// Annotations are checked in name order so a rule with two broken templates
// reports them the same way every run.
func (v *validator) annotationsTemplate(r Rule) {
	names := make([]string, 0, len(r.Annotations))
	for name := range r.Annotations {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if _, err := template.New(name).Option("missingkey=error").Parse(r.Annotations[name]); err != nil {
			v.addPolicy(r, r.LineOf("annotations."+name, "annotations"), checkAnnotationsTemplate,
				"annotation %q is not a valid template: %s", name, err)
		}
	}
}

// ruleFor checks the pending duration against the interval it is measured in.
// A rule with no for at all is left alone, because firing on the first
// evaluation is a deliberate and common choice.
func (v *validator) ruleFor(g Group, r Rule) {
	if !r.has("for") {
		return
	}
	line := r.LineOf("for")

	if r.For < 0 {
		v.add(r, line, checkRuleFor, "for must not be negative, got %s", r.For)
		return
	}
	if r.For > 0 && g.Interval > 0 && r.For < g.Interval {
		v.addPolicy(r, line, checkRuleFor,
			"for (%s) is shorter than the group interval (%s), so it rounds up to one interval",
			r.For, g.Interval)
	}
}

// ruleWindow checks how much time the query examines. A window shorter than
// the interval leaves the data between evaluations unread by any evaluation,
// which is a silent blind spot rather than a visible failure.
func (v *validator) ruleWindow(g Group, r Rule) {
	line := r.LineOf("window")

	if r.Window < 0 {
		v.add(r, line, checkRuleWindow, "window must not be negative, got %s", r.Window)
		return
	}
	if r.Window > 0 && g.Interval > 0 && r.Window < g.Interval {
		v.addPolicy(r, line, checkRuleWindow,
			"window (%s) is shorter than the group interval (%s), so data between evaluations is never examined",
			r.Window, g.Interval)
	}
}
