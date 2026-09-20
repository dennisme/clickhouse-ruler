package rule

import (
	"fmt"
	"net/url"
	"regexp"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

const (
	checkRuleName            = "rule/name"
	checkRuleSource          = "rule/source"
	checkRuleExpr            = "rule/expr"
	checkLabelsRequired      = "labels/required"
	checkAnnotationsRequired = "annotations/required"
	checkAnnotationsRunbook  = "annotations/runbook"
	checkRuleFor             = "rule/for"
	checkRuleWindow          = "rule/window"
)

// requiredLabels route and triage every alert, so a rule without them cannot
// be paged on correctly.
var requiredLabels = []string{"team", "severity"}

// requiredAnnotations are what the person woken up at 03:00 actually reads.
var requiredAnnotations = []string{"summary", "runbook_url"}

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
func Validate(f *RuleFile) []lint.Problem {
	v := &validator{file: f.File}
	for _, g := range f.Groups {
		v.group(g)
	}
	return v.problems
}

type validator struct {
	file     string
	problems []lint.Problem
}

func (v *validator) add(r Rule, line int, check string, sev lint.Severity, format string, args ...any) {
	v.problems = append(v.problems, lint.Problem{
		File:     v.file,
		Line:     line,
		Subject:  r.Alert,
		Check:    check,
		Severity: sev,
		Text:     fmt.Sprintf(format, args...),
	})
}

func (v *validator) group(g Group) {
	// Alert names only have to be unique within their own group, so the set is
	// scoped here rather than to the file.
	namedAt := map[string]int{}

	for _, r := range g.Rules {
		v.ruleName(g, r, namedAt)
		v.ruleSource(r)
		v.ruleExpr(r)
		v.requiredKeys(r, "labels", "label", checkLabelsRequired, g.EffectiveLabels(r), requiredLabels)
		v.requiredKeys(r, "annotations", "annotation", checkAnnotationsRequired, r.Annotations, requiredAnnotations)
		v.annotationsRunbook(r)
		v.ruleFor(g, r)
		v.ruleWindow(g, r)
	}
}

func (v *validator) ruleName(g Group, r Rule, namedAt map[string]int) {
	line := r.LineOf("alert")

	if r.Alert == "" {
		v.add(r, line, checkRuleName, lint.SeverityError, "alert name is empty")
		return
	}
	if first, ok := namedAt[r.Alert]; ok {
		v.add(r, line, checkRuleName, lint.SeverityError,
			"duplicate alert name %q in group %q, first defined on line %d",
			r.Alert, g.Name, first)
		return
	}
	namedAt[r.Alert] = line
}

func (v *validator) ruleSource(r Rule) {
	if r.Source == "" {
		v.add(r, r.LineOf("source"), checkRuleSource, lint.SeverityError, "source is empty")
	}
}

// ruleExpr enforces the lint half of the time bound decision: the ruler
// supplies the evaluation window through template variables, and a query that
// omits them would scan without bound on every evaluation.
func (v *validator) ruleExpr(r Rule) {
	line := r.LineOf("expr")

	if r.Expr == "" {
		v.add(r, line, checkRuleExpr, lint.SeverityError, "expr is empty")
		return
	}
	for _, tv := range timeBoundVars {
		if !tv.pattern.MatchString(r.Expr) {
			v.add(r, line, checkRuleExpr, lint.SeverityError,
				"expr does not reference %s, so the query has no %s", tv.name, tv.reason)
		}
	}
}

// requiredKeys reports keys a block must carry. A finding points at the key
// itself when it exists, at the block when the key is absent, and at the rule
// when the whole block is absent.
func (v *validator) requiredKeys(r Rule, block, noun, check string, have map[string]string, required []string) {
	for _, key := range required {
		line := r.LineOf(block+"."+key, block)

		value, present := have[key]
		switch {
		case !present:
			v.add(r, line, check, lint.SeverityError, "required %s %q is missing", noun, key)
		case value == "":
			v.add(r, line, check, lint.SeverityError, "required %s %q is empty", noun, key)
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
	v.add(r, r.LineOf("annotations.runbook_url", "annotations"), checkAnnotationsRunbook,
		lint.SeverityError, "runbook_url must be an absolute http or https URL, got %q", raw)
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
		v.add(r, line, checkRuleFor, lint.SeverityError, "for must not be negative, got %s", r.For)
		return
	}
	if r.For > 0 && g.Interval > 0 && r.For < g.Interval {
		v.add(r, line, checkRuleFor, lint.SeverityWarning,
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
		v.add(r, line, checkRuleWindow, lint.SeverityError, "window must not be negative, got %s", r.Window)
		return
	}
	if r.Window > 0 && g.Interval > 0 && r.Window < g.Interval {
		v.add(r, line, checkRuleWindow, lint.SeverityWarning,
			"window (%s) is shorter than the group interval (%s), so data between evaluations is never examined",
			r.Window, g.Interval)
	}
}
