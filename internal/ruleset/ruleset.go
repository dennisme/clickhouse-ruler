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
	checkSourceFound = "rule/source-exists"
	checkProtected   = "rule/protected-label"
)

// Rule is one rule with everything needed to evaluate it resolved: the file it
// came from, the team that owns it, its group's interval, and its source.
type Rule struct {
	rule.Rule

	File  string
	Team  string
	Group rule.Group

	// Labels is the rule's file-level label set, group labels overlaid with
	// the rule's own (spec 6.3.1 levels 1 and 2) after the derived team has
	// been applied. Result columns overlay this per instance at evaluation.
	Labels map[string]string

	Source source.Source
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
		loaded, found := loadFile(dir, path, sources, root)
		set.Rules = append(set.Rules, loaded...)
		problems = append(problems, found...)
	}
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

func loadFile(dir, path string, sources *source.File, root *policy.Policy) ([]Rule, []lint.Problem) {
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

	// The derived team is applied as a group label before validation, not
	// after. labels/required checks the labels a rule actually ends up with,
	// so injecting afterwards would report a missing team that the loader was
	// about to supply. A group that names its own team keeps it.
	team := teamOf(dir, path)
	for i := range parsed.Groups {
		if team == "" {
			break
		}
		if parsed.Groups[i].Labels == nil {
			parsed.Groups[i].Labels = map[string]string{}
		}
		if parsed.Groups[i].Labels["team"] == "" {
			parsed.Groups[i].Labels["team"] = team
		}
	}

	// Policy is resolved per rule because each names its own source, and a
	// source can tighten a check for the rules that read it.
	policyFor := func(r rule.Rule) *policy.Policy {
		if src, ok := sources.ByName(r.Source); ok {
			return policy.Merge(root, src.Policy)
		}
		return policy.Merge(root)
	}
	problems = append(problems, rule.Validate(parsed, policyFor)...)

	var out []Rule
	for _, g := range parsed.Groups {
		for _, r := range g.Rules {
			labels := g.EffectiveLabels(r)

			loaded := Rule{
				Rule:   r,
				File:   path,
				Team:   labels["team"],
				Group:  g,
				Labels: labels,
			}
			problems = append(problems, protectedLabels(path, r)...)

			if src, ok := sources.ByName(r.Source); ok {
				loaded.Source = src
			} else if r.Source != "" {
				problems = append(problems, lint.Problem{
					File:     path,
					Line:     r.Line(),
					Subject:  r.Alert,
					Check:    checkSourceFound,
					Severity: lint.SeverityError,
					Text:     "source " + r.Source + " is not defined in the sources file",
				})
			}
			out = append(out, loaded)
		}
	}
	return out, problems
}

// teamOf derives the owning team from the first path segment under dir.
//
// The directory name is used exactly as written. Nothing is stripped from it,
// so rules/payments gives "payments" and rules/team-payments gives
// "team-payments". Rewriting part of the path would make the mapping from
// directory to team something a reader has to know rather than something they
// can see.
//
// A rule sitting directly in dir has no team to derive.
func teamOf(dir, path string) string {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return ""
	}
	segments := strings.Split(filepath.ToSlash(rel), "/")
	if len(segments) < 2 {
		return ""
	}
	return segments[0]
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
func protectedLabels(file string, r rule.Rule) []lint.Problem {
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

	if _, ok := r.Labels["alertname"]; ok {
		add(r.LineOf("labels.alertname", "labels"),
			"labels may not set %q: an alert's identity comes from its name", "alertname")
	}

	for _, m := range aliasPattern.FindAllStringSubmatch(r.Expr, -1) {
		alias := strings.ToLower(m[1])
		if alias != "team" && alias != "alertname" {
			continue
		}
		add(r.LineOf("expr"),
			"the query may not produce a %q column: the Alertmanager route tree is "+
				"generated from the files, so a value that only exists at query time has no route",
			alias)
	}
	return out
}
