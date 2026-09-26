// Package ruleset turns a directory of rule files plus a sources file into a
// set that can actually be evaluated.
//
// Parsing a rule file (internal/rule) answers "is this file well formed".
// Loading answers "can this rule run": its source exists, and it carries the
// team that owns it. Those questions need the directory tree, which a single
// file parse does not have.
package ruleset

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// Rule is one rule with everything needed to evaluate it resolved: the file it
// came from, the team that owns it, its group's interval, and its source.
type Rule struct {
	rule.Rule

	File  string
	Group rule.Group

	// Labels is the rule's file-level label set, group labels overlaid with
	// the rule's own (spec 6.3.1 levels 1 and 2). Result columns and the
	// source's labels overlay this per instance at evaluation.
	Labels map[string]string

	// Sources is every source the rule's labels matched, sorted by name. A
	// rule evaluates once per source, and may match none: on a ruler holding
	// one datacenter's sources, most rules in a shared repository will
	// (spec 6.10, 10.2).
	Sources []source.Source
}

// GroupID names a group the way every metric label, log line and
// system.query_log comment spells it. A group name is unique within its file
// and not across the directory, so the file is part of the identity (spec 7.6).
func GroupID(file, group string) string { return file + ":" + group }

// GroupID is the rule's own group.
func (r Rule) GroupID() string { return GroupID(r.File, r.Group.Name) }

// Team is who owns the rule, read from its effective labels so a group can
// set it once for every rule in the file. Empty when nobody claimed it,
// which is what the cost metrics report rather than inventing an owner
// (spec 8.2).
func (r Rule) Team() string { return r.Labels["team"] }

// Set is every rule found under a directory.
type Set struct {
	Dir   string
	Rules []Rule
}

// Load reads every *.yaml under dir, resolves each rule against sources, and
// returns all problems rather than stopping at the first.
//
// root is the instance-wide policy. Each rule is validated against root merged
// with its own source's policy, so a source that pages on-call can demand more
// than the baseline without every rule in the repository having to (spec 7.7).
// A nil root means the shipped defaults.
func Load(dir string, sources *source.File, root *policy.Policy) (*Set, []lint.Problem) {
	set := &Set{Dir: dir}
	var problems []lint.Problem

	files, err := ruleFiles(dir)
	if err != nil {
		return set, []lint.Problem{
			lint.NewProblem(dir, 0, lint.CheckRulesetDirectory, lint.SeverityError, err.Error()),
		}
	}

	for _, path := range files {
		loaded, found := loadFile(path, sources, root)
		set.Rules = append(set.Rules, loaded...)
		problems = append(problems, found...)
	}
	problems = append(problems, duplicateAlerts(set.Rules, sources, root)...)
	return set, problems
}

// ruleFiles walks dir for rule files, sorted so that problems come back in a
// stable order no matter how the filesystem enumerates.
func ruleFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext == ".yaml" || ext == ".yml" {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// WalkDir already yields lexical order, but sorting is cheap insurance
	// against that guarantee changing underneath deterministic problem lists.
	return out, nil
}

func loadFile(path string, sources *source.File, root *policy.Policy) ([]Rule, []lint.Problem) {
	// The path comes from walking the directory the operator pointed us at.
	// Reading rule files by path is the entire job of this package, so G304
	// has nothing to warn about here.
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, []lint.Problem{
			lint.NewProblem(path, 0, lint.CheckRulesetDirectory, lint.SeverityError, err.Error()),
		}
	}

	parsed, problems := rule.Parse(path, data)
	if parsed == nil {
		return nil, problems
	}

	policyForRule := func(r rule.Rule) *policy.Policy { return policyFor(r, sources, root) }
	problems = append(problems, rule.Validate(parsed, policyForRule)...)

	var out []Rule
	for _, g := range parsed.Groups {
		for _, r := range g.Rules {
			labels := g.EffectiveLabels(r)

			matched := sources.Match(r.Sources)

			loaded := Rule{
				Rule:    r,
				File:    path,
				Group:   g,
				Labels:  labels,
				Sources: matched,
			}
			problems = append(problems, protectedLabels(path, r, matched)...)
			problems = append(problems, sourceMatch(path, r, matched, policyForRule(r))...)

			out = append(out, loaded)
		}
	}
	return out, problems
}

// policyFor resolves the policy a rule is validated against.
//
// Resolved per rule because a rule's labels decide which sources it reaches,
// and each of those can tighten a check. A rule matching several gets the
// strictest any of them asks for (spec 7.7).
func policyFor(r rule.Rule, sources *source.File, root *policy.Policy) *policy.Policy {
	scopes := []*policy.Policy{root}
	for _, src := range sources.Match(r.Sources) {
		scopes = append(scopes, src.Policy)
	}
	return policy.Merge(scopes...)
}

// duplicateAlerts reports two rules whose alerts cannot be told apart.
//
// An alert's identity is its full label set (spec 6.3.1), and both
// Alertmanager and notify.Cadence key on the fingerprint that set produces.
// Two rules that agree on their alert name, their effective labels and the
// sources they reach therefore write into one alert and one cadence entry:
// whichever evaluated last decides what Alertmanager holds, and a resolve
// from either can end the other's page while the condition behind it is
// still true. `rule/name` does not see this, because it scopes uniqueness to
// the group and these rules are in different groups or different files
// (spec 7.6).
//
// The comparison is over the static identity only, so it is not exact.
// Result columns contribute labels that exist only at evaluation time, so a
// pair flagged here may distinguish itself at runtime on a column one of
// them returns and the other does not. The direction of the inexactness is
// what makes it worth flagging anyway: a pair whose files already agree is
// the reachable case, it relies on the queries returning different columns
// to stay apart, and nothing in either file says so. The reverse, two rules
// that look distinct and collide on runtime labels, needs the result and is
// not a tier 0 question.
//
// A rule no source accepts is skipped. It evaluates nowhere on this ruler,
// so it produces no alert to collide with, and a ruler holding one data
// centre's sources would otherwise report every unmatched pair in a shared
// repository against each other (spec 6.10, 10.2).
func duplicateAlerts(rules []Rule, sources *source.File, root *policy.Policy) []lint.Problem {
	var out []lint.Problem

	first := map[string]Rule{}
	for _, r := range rules {
		if len(r.Sources) == 0 {
			continue
		}
		key := alertIdentity(r)
		prev, seen := first[key]
		if !seen {
			first[key] = r
			continue
		}

		// Both rules are on the hook, so both policies apply and the
		// stricter of the two decides. Merging is a maximum, so which of
		// them is reported does not change the severity (spec 7.7).
		setting := policy.Merge(
			policyFor(prev.Rule, sources, root),
			policyFor(r.Rule, sources, root),
		).For(lint.CheckRuleDuplicateAlert)
		if setting.Severity == lint.SeverityOff {
			continue
		}

		// Reported once, against the second rule, naming the first. One
		// finding per rule would read as two unrelated problems and neither
		// would say who the other is, leaving whoever has to fix it grepping
		// the tree for a name they were not shown.
		p := lint.NewProblem(r.File, r.LineOf("alert"), lint.CheckRuleDuplicateAlert, setting.Severity,
			fmt.Sprintf("this rule and the one at %s:%d produce the same alert: same name, same "+
				"labels, and the same sources, so Alertmanager and the resend cadence cannot tell "+
				"them apart and a resolve from either ends the other. Give one of them a label the "+
				"other does not carry, narrow one selector, or delete the rule that is redundant",
				prev.File, prev.LineOf("alert")))
		p.Subject = r.Alert
		p.PolicyFile, p.PolicyLine = setting.File, setting.Line
		out = append(out, p)
	}
	return out
}

// alertIdentity is everything about a rule's alert that the files decide: its
// name, its effective labels, and the sources its selector reached. Two rules
// agreeing on all three produce the same fingerprint.
func alertIdentity(r Rule) string {
	var sb strings.Builder
	sb.WriteString(r.Alert)

	keys := make([]string, 0, len(r.Labels))
	for k := range r.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// A separator no label name or value can contain, so a key ending
		// where the next value begins cannot be read as a different pair
		// that happens to concatenate the same way.
		sb.WriteString("\x00" + k + "\x00" + r.Labels[k])
	}

	for _, src := range r.Sources {
		sb.WriteString("\x00" + src.Name)
	}
	return sb.String()
}

// sourceMatch reports a rule that no source accepts.
//
// This is a warning rather than an error, and it is the one check where "the
// rule cannot run" is not automatically wrong. Deployments are distributed, so
// a ruler in one datacenter legitimately holds its own sources and nothing
// else, and most rules in a shared repository will match nothing on it. A
// rollout has an order too: rules can land before the source for a new cluster
// does, and failing hard would block every unrelated change until someone
// fixed the sequence (spec 6.10).
func sourceMatch(file string, r rule.Rule, matched []source.Source, p *policy.Policy) []lint.Problem {
	if len(matched) > 0 {
		return nil
	}
	setting := p.For(lint.CheckRuleSourceMatch)
	if setting.Severity == lint.SeverityOff {
		return nil
	}
	unmatched := lint.NewProblem(file, r.LineOf("labels"), lint.CheckRuleSourceMatch, setting.Severity,
		"the sources selector matches no source, so this ruler will never "+
			"evaluate the rule. That is expected when the sources for its cluster live "+
			"on a different ruler, or when a cluster is being added and its source has "+
			"not landed yet. An empty selector matches nothing on purpose")
	unmatched.Subject = r.Alert
	unmatched.PolicyFile, unmatched.PolicyLine = setting.File, setting.Line
	return []lint.Problem{unmatched}
}

// aliasPattern matches a SQL column alias, as in `'platform' AS team`.
var aliasPattern = regexp.MustCompile(`(?i)\bAS\s+` + "[`\"]?" + `(\w+)` + "[`\"]?")

// protectedLabels reports a rule that tries to produce team or alertname from
// its query, and one that writes alertname in its labels block.
//
// A rule may still override team in its labels: that value lives in the file,
// shows up in a diff, and can be enumerated when the route tree is generated.
// A result column cannot be, which is the whole distinction (spec 6.3.1).
//
// This is a tier 0 check, so it only sees the SQL as text and only catches a
// literal `AS team`. A column reached through SELECT *, a subquery alias or a
// CTE is caught by the tier 1 half of the same check, which reads the real
// output columns from DESCRIBE (spec 7.3). This is the half that needs no
// connection, so it still runs when nothing else can.
func protectedLabels(file string, r rule.Rule, matched []source.Source) []lint.Problem {
	var out []lint.Problem

	add := func(line int, format string, args ...any) {
		p := lint.NewProblem(file, line, lint.CheckRuleProtectedLabel, lint.SeverityError,
			fmt.Sprintf(format, args...))
		p.Subject = r.Alert
		out = append(out, p)
	}

	// Keys the alert's identity depends on. alertname and source are always
	// protected; a matched source's alert_labels are protected for the rules
	// that reach it, because a query cannot know better than the ruler which
	// cluster it ran on (spec 6.3.1).
	protected := map[string]bool{"alertname": true, "source": true, "team": true}
	for _, src := range matched {
		for k := range src.Labels {
			protected[k] = true
		}
	}

	for _, key := range []string{"alertname", "source"} {
		if _, ok := r.Labels[key]; ok {
			add(r.LineOf("labels."+key, "labels"),
				"labels may not set %q: an alert's identity comes from its name and its source", key)
		}
	}

	for _, m := range aliasPattern.FindAllStringSubmatch(r.Expr, -1) {
		alias := strings.ToLower(m[1])
		if !protected[alias] {
			continue
		}
		add(r.LineOf("expr"),
			"the query may not produce a %q column: the Alertmanager route tree is "+
				"generated from the files, so a value that only exists at query time has no route",
			alias)
	}
	return out
}
