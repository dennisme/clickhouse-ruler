package scheduler

import (
	"fmt"
	"strings"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// evaluated is one source's successful evaluation, as much of it as the drift
// comparison reads: what the result looked like and what the query cost. The
// rows themselves are deliberately absent (spec 6.3.2).
type evaluated struct {
	source source.Source
	shape  []query.Column
	usage  query.Usage
}

// drift compares each evaluation of one rule against the last one, so a rule
// that was correct when it merged and has been broken by a schema change since
// is reported without CI running and without anything reloading (spec 6.3.2).
//
// Three things it can find, all of them out of an evaluation that already
// happened: a column dropped, renamed or retyped under the query, two sources
// that stopped agreeing on what the rule returns, and a query whose measured
// cost crossed the ceilings its prediction was checked against at authoring
// time.
//
// What it never compares is how many rows came back. Zero rows is the healthy
// state of most alert rules, so a row count comparison would fire on every
// resolve, which is the loudest possible false positive.
//
// It reports and never refuses. The rule keeps evaluating, keeps its alert
// state and keeps paging; refusing a rule belongs to reload alone (spec 7.6),
// because a ruler that unloaded a rule on drift would resolve that rule's
// alerts and stop paging for the condition on the strength of a schema change
// nobody reviewed.
type drift struct {
	rule ruleset.Rule

	// prev is the shape each source's last successful evaluation returned.
	//
	// Memory only. Two evaluations establish a baseline, so a restart delays
	// the comparison by one tick rather than disabling it, and a baseline that
	// outlived the process would have to be reconciled against a rule that
	// changed while it was down. A reload starts fresh for the same reason in
	// reverse: the edit that changed a rule's columns is a change somebody
	// reviewed, and reporting it as drift would name the author's own commit.
	prev map[string][]query.Column
}

func newDrift(r ruleset.Rule) *drift {
	return &drift{rule: r, prev: make(map[string][]query.Column, len(r.Sources))}
}

// inspect compares this evaluation against the last one and returns what to
// report, in source order. Only the sources that answered are passed in: a
// cluster the query failed against has not drifted, and its baseline is left
// standing so the comparison resumes against the last result that was real.
func (d *drift) inspect(evals []evaluated, now time.Time) []lint.Problem {
	var problems []lint.Problem

	for _, e := range evals {
		prev, seen := d.prev[e.source.Name]
		d.prev[e.source.Name] = e.shape

		if seen {
			if diff := query.Differences(prev, e.shape,
				"the last evaluation", "this one"); len(diff) > 0 {
				problems = append(problems, d.problem(e.source, now, query.Finding{
					Check: lint.CheckRuleColumns,
					Detail: "what this rule returns changed without the file changing, so it no " +
						"longer means what it was reviewed as: " + strings.Join(diff, "; "),
				})...)
			}
		}
		if over := d.overCost(e); over != "" {
			problems = append(problems, d.problem(e.source, now, query.Finding{
				Check:  lint.CheckRuleCost,
				Detail: over,
			})...)
		}
	}

	return append(problems, d.disagreements(evals)...)
}

// disagreements reports the sources of one rule that no longer return the same
// result, which is observable from the evaluations already happening because a
// rule evaluates against every source its selector matched on every tick
// (spec 6.10.1).
//
// One problem however many sources differed, at the rule's own severity and
// exempted by nobody, for the reasons the same check gives at authoring time:
// a finding naming two clusters has no one source to take a setting from.
func (d *drift) disagreements(evals []evaluated) []lint.Problem {
	if len(evals) < 2 {
		return nil
	}

	setting := d.rule.Policy.For(lint.CheckRuleSourceSchema)
	if setting.Severity == lint.SeverityOff {
		return nil
	}

	reference := evals[0]
	var found []string
	for _, other := range evals[1:] {
		found = append(found, query.Differences(reference.shape, other.shape,
			"source "+reference.source.Name, "source "+other.source.Name)...)
	}
	if len(found) == 0 {
		return nil
	}

	p := lint.NewProblem(d.rule.File, d.rule.Line(), lint.CheckRuleSourceSchema, setting.Severity,
		"the sources this rule matched do not agree on what it returns, so it cannot mean the same "+
			"thing on all of them: "+strings.Join(found, "; "))
	p.Subject = d.rule.Alert
	p.PolicyFile, p.PolicyLine = setting.File, setting.Line

	return []lint.Problem{p}
}

// overCost describes what this evaluation read against the ceilings the rule's
// predicted cost was checked against, and returns empty when it is inside them
// or none are configured.
//
// Measured rather than predicted, which is the difference from the same check
// at authoring time: rule/cost reads what the optimiser guessed, and this reads
// what the driver counted as the query ran (spec 8.2).
func (d *drift) overCost(e evaluated) string {
	limit := query.CostFromPolicy(policy.Merge(d.rule.Policy, e.source.Policy))
	if limit == nil {
		return ""
	}

	interval := d.rule.Group.Interval
	rate := query.RowsPerSecond(e.usage.ReadRows, interval)

	var over []string
	if e.usage.ReadRows > limit.MaxRows {
		over = append(over, fmt.Sprintf("%d rows against a ceiling of %d",
			e.usage.ReadRows, limit.MaxRows))
	}
	if rate > limit.MaxRowsPerSecond {
		over = append(over, fmt.Sprintf("%.0f rows a second against a ceiling of %.0f",
			rate, limit.MaxRowsPerSecond))
	}
	if len(over) == 0 {
		return ""
	}

	detail := "this evaluation read " + strings.Join(over, " and ")
	if interval > 0 {
		detail += fmt.Sprintf(", and the group evaluates every %s", interval)
	}
	return detail
}

// problem resolves a finding's severity the way the online pass does, so a
// check an operator turned down reports the same while the ruler is running as
// it does in CI, and a source holding an unexpired exemption reports nothing
// (spec 7.7).
func (d *drift) problem(src source.Source, now time.Time, f query.Finding) []lint.Problem {
	severity := lint.SeverityError
	var origin policy.Setting

	if lint.Configurable(f.Check) {
		origin = policy.Merge(d.rule.Policy, src.Policy).For(f.Check)
		severity = origin.Severity

		if severity == lint.SeverityOff || src.Exempts(f.Check, now) {
			return nil
		}
	}

	p := lint.NewProblem(d.rule.File, d.rule.Line(), f.Check, severity,
		fmt.Sprintf("against source %s: %s", src.Name, f.Detail))
	p.Subject = d.rule.Alert
	p.PolicyFile, p.PolicyLine = origin.File, origin.Line

	return []lint.Problem{p}
}
