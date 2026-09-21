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
	"strings"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// Check names are namespaced like pint, as in slices 1 and 3.
const (
	checkDirectory   = "ruleset/directory"
	checkSourceMatch = "rule/source-match"
	checkProtected   = "rule/protected-label"
	checkRuleName    = "rule/name"
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
	// one data centre's sources, most rules in a shared repository will
	// (spec 6.10, 10.2).
	Sources []source.Source
}

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
		return set, []lint.Problem{{
			File:     dir,
			Check:    checkDirectory,
			Severity: lint.SeverityError,
			Text:     err.Error(),
		}}
	}

	for _, path := range files {
		loaded, found := loadFile(path, sources, root)
		set.Rules = append(set.Rules, loaded...)
		problems = append(problems, found...)
	}
	problems = append(problems, duplicateNames(set.Rules)...)
	return set, problems
}

// duplicateNames reports an alert name used by more than one rule in the tree.
//
// Within a file this is already caught during validation, so only clashes
// that span files are reported here; catching them needs the whole tree,
// which a single file parse does not have.
//
// It is an error rather than a convention, for the same reason an empty name
// is: the name is the alert's identity. Two rules sharing one collide in the
// per-rule metrics of spec 8.2, which carry the rule name and nothing else,
// so one rule's evaluation counts and alert totals are silently folded into
// the other's. It also makes routing on `alertname` ambiguous, and a page
// naming a rule that could be either of two files is a bad thing to read at
// three in the morning.
func duplicateNames(rules []Rule) []lint.Problem {
	type origin struct {
		file string
		line int
	}
	firstSeen := map[string]origin{}
	var out []lint.Problem

	for _, r := range rules {
		if r.Alert == "" {
			// Already reported as a missing name; there is nothing to clash.
			continue
		}
		first, seen := firstSeen[r.Alert]
		if !seen {
			firstSeen[r.Alert] = origin{file: r.File, line: r.Line()}
			continue
		}
		if first.file == r.File {
			continue
		}
		out = append(out, lint.Problem{
			File:     r.File,
			Line:     r.Line(),
			Subject:  r.Alert,
			Check:    checkRuleName,
			Severity: lint.SeverityError,
			Text: fmt.Sprintf(
				"duplicate alert name %q, already defined at %s:%d. An alert name is its "+
					"identity and the only label the per-rule metrics carry, so two rules "+
					"sharing one report as a single series",
				r.Alert, first.file, first.line),
		})
	}
	return out
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
		return nil, []lint.Problem{{
			File:     path,
			Check:    checkDirectory,
			Severity: lint.SeverityError,
			Text:     err.Error(),
		}}
	}

	parsed, problems := rule.Parse(path, data)
	if parsed == nil {
		return nil, problems
	}

	// Policy is resolved per rule because a rule's labels decide which sources
	// it reaches, and each of those can tighten a check. A rule matching
	// several gets the strictest any of them asks for.
	policyFor := func(r rule.Rule) *policy.Policy {
		scopes := []*policy.Policy{root}
		for _, src := range sources.Match(r.Sources) {
			scopes = append(scopes, src.Policy)
		}
		return policy.Merge(scopes...)
	}
	problems = append(problems, rule.Validate(parsed, policyFor)...)

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
			problems = append(problems, sourceMatch(path, r, matched, policyFor(r))...)

			out = append(out, loaded)
		}
	}
	return out, problems
}

// sourceMatch reports a rule that no source accepts.
//
// This is a warning rather than an error, and it is the one check where "the
// rule cannot run" is not automatically wrong. Deployments are distributed, so
// a ruler in one data centre legitimately holds its own sources and nothing
// else, and most rules in a shared repository will match nothing on it. A
// rollout has an order too: rules can land before the source for a new cluster
// does, and failing hard would block every unrelated change until someone
// fixed the sequence (spec 6.10).
func sourceMatch(file string, r rule.Rule, matched []source.Source, p *policy.Policy) []lint.Problem {
	if len(matched) > 0 {
		return nil
	}
	setting := p.For(checkSourceMatch)
	if setting.Severity == lint.SeverityOff {
		return nil
	}
	return []lint.Problem{{
		File:     file,
		Line:     r.LineOf("labels"),
		Subject:  r.Alert,
		Check:    checkSourceMatch,
		Severity: setting.Severity,
		Text: "the sources selector matches no source, so this ruler will never " +
			"evaluate the rule. That is expected when the sources for its cluster live " +
			"on a different ruler, or when a cluster is being added and its source has " +
			"not landed yet. An empty selector matches nothing on purpose",
		PolicyFile: setting.File,
		PolicyLine: setting.Line,
	}}
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
// literal `AS team`. A query can still produce the column some other way, for
// example through SELECT *, a subquery alias, or a CTE. Catching those needs
// the real output column names from DESCRIBE, which is tier 1 (spec 7.3).
// This check is a cheap first line, not a complete one.
func protectedLabels(file string, r rule.Rule, matched []source.Source) []lint.Problem {
	var out []lint.Problem

	add := func(line int, format string, args ...any) {
		out = append(out, lint.Problem{
			File:     file,
			Line:     line,
			Subject:  r.Alert,
			Check:    checkProtected,
			Severity: lint.SeverityError,
			Text:     fmt.Sprintf(format, args...),
		})
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
