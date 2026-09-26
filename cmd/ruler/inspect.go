package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// inspectRules reads every rule's SQL through each source it matched.
//
// Per source, not once per rule: a rule spanning an estate is checked against
// every cluster it will run on, which is also how a source that disagrees
// with the others shows up at all (spec 6.10).
func inspectRules(ctx context.Context, set *ruleset.Set, root *policy.Policy, sampling bool) []lint.Problem {
	var problems []lint.Problem

	// One instant for the whole pass, so a long run cannot expire an
	// exemption halfway through and report the same rule two ways.
	now := time.Now()

	queriers := map[string]*query.Querier{}
	defer func() {
		for _, q := range queriers {
			_ = q.Close()
		}
	}()

	for _, r := range set.Rules {
		for _, src := range r.Sources {
			merged := policy.Merge(root, src.Policy)

			checks := checksFromPolicy(merged, r, src)

			q, err := querierFor(queriers, src)
			if err != nil {
				problems = append(problems, inspectionFailed(r, src.Name, err))
				continue
			}

			findings, err := q.Inspect(ctx, r.Rule, r.GroupID(), checks)
			if err != nil {
				problems = append(problems, inspectionFailed(r, src.Name, err))
				continue
			}
			problems = append(problems,
				inspectionProblems(r.File, r.Alert, r.Line(), src, merged, findings, now)...)

			if !sampling {
				continue
			}
			sampleChecks, wanted := samplingFromPolicy(merged)
			if !wanted {
				continue
			}

			// The only checks that read rows, so they run when they were asked
			// for and never merely because a connection exists (spec 7.3).
			sampled, err := q.Sample(ctx, r.Rule, r.GroupID(), sampleChecks, now)
			if err != nil {
				problems = append(problems, inspectionFailed(r, src.Name, err))
				continue
			}
			problems = append(problems,
				inspectionProblems(r.File, r.Alert, r.Line(), src, merged, sampled, now)...)
		}
	}
	return problems
}

// samplingFromPolicy translates the resolved policy into what a sample should
// do, and says whether to sample at all.
//
// A check at severity off is not sampled for, rather than sampled and then
// dropped. That is the line checksFromPolicy draws for the metadata checks, and
// it matters more here: honouring `off` after the fact would read rows an
// operator asked nobody to read.
func samplingFromPolicy(p *policy.Policy) (query.SampleChecks, bool) {
	setting := p.For(lint.CheckRuleAttributeKey)
	if setting.Severity == lint.SeverityOff {
		return query.SampleChecks{}, false
	}

	maxRows, _ := setting.Limit(lint.LimitSampleRows)

	return query.SampleChecks{
		MaxRows:     maxRows,
		RequireRows: setting.Flag(lint.FlagRequireRows),
	}, true
}

// querierFor opens one connection per source and reuses it for every rule
// that matched, rather than reconnecting per rule.
func querierFor(open map[string]*query.Querier, src source.Source) (*query.Querier, error) {
	if q, ok := open[src.Name]; ok {
		return q, nil
	}

	q, err := query.Open(src)
	if err != nil {
		return nil, err
	}
	open[src.Name] = q
	return q, nil
}

// checksFromPolicy translates the resolved policy into what to look for. A
// check at severity off is not looked for at all, rather than looked for and
// discarded.
func checksFromPolicy(p *policy.Policy, r ruleset.Rule, src source.Source) query.Checks {
	var c query.Checks

	// The labels an annotation can read that no result column produces, and
	// the ones a query may not produce at all. Both are assembled here because
	// only the loader knows a rule's effective labels and which sources it
	// matched (spec 6.3.1).
	c.KnownLabels = labelNames(r.Labels, src.Labels, alert.LabelAlertname, alert.LabelSource)
	c.ProtectedLabels = labelNames(nil, src.Labels,
		alert.LabelAlertname, alert.LabelSource, "team")

	if tf := p.For(lint.CheckRuleTableFunction); tf.Severity != lint.SeverityOff {
		c.AllowedTableFunctions = tf.Keys
	}
	if nd := p.For(lint.CheckRuleNondeterministic); nd.Severity != lint.SeverityOff {
		c.Nondeterministic = nd.Keys
	}
	if ft := p.For(lint.CheckRuleForeignTable); ft.Severity != lint.SeverityOff {
		c.Database = src.Database
	}
	if cost := p.For(lint.CheckRuleCost); cost.Severity != lint.SeverityOff {
		// The interval is the group's, and it is what turns a row count into
		// a rate. A group that set none leaves it zero, and the rate ceiling
		// then does not apply rather than dividing by zero (spec 7.3).
		c.Interval = r.Group.Interval

		rows, hasRows := cost.Limit(lint.LimitRowsRead)
		rate, hasRate := cost.Limit(lint.LimitRowsPerSecond)

		if hasRows || hasRate {
			d := policy.Defaults().For(lint.CheckRuleCost)
			if !hasRows {
				rows, _ = d.Limit(lint.LimitRowsRead)
			}
			if !hasRate {
				rate, _ = d.Limit(lint.LimitRowsPerSecond)
			}
			// Both are non-negative by construction: Setting.Limit refuses a
			// ceiling that is not a whole number at or above zero. The guard
			// is here so the conversion is provably safe to a reader and to
			// the linter, not because a negative can arrive.
			if rows < 0 {
				rows = 0
			}
			if rate < 0 {
				rate = 0
			}
			c.Cost = &query.Cost{
				MaxRows:          uint64(rows),
				MaxRowsPerSecond: float64(rate),
			}
		}
	}
	if cx := p.For(lint.CheckRuleComplexity); cx.Severity != lint.SeverityOff {
		joins, hasJoins := cx.Limit(lint.LimitJoins)
		subqueries, hasSubqueries := cx.Limit(lint.LimitSubqueries)

		// Either ceiling alone is a check worth running, and a missing one
		// keeps its default rather than becoming zero, which would refuse
		// every join an operator never said anything about.
		if hasJoins || hasSubqueries {
			d := policy.Defaults().For(lint.CheckRuleComplexity)
			if !hasJoins {
				joins, _ = d.Limit(lint.LimitJoins)
			}
			if !hasSubqueries {
				subqueries, _ = d.Limit(lint.LimitSubqueries)
			}
			c.Complexity = &query.Complexity{MaxJoins: joins, MaxSubqueries: subqueries}
		}
	}
	return c
}

// labelNames collects label keys plus any fixed names, sorted and
// deduplicated so a finding listing them reads the same way every run.
func labelNames(labels, srcLabels map[string]string, fixed ...string) []string {
	seen := map[string]bool{}
	for _, m := range []map[string]string{labels, srcLabels} {
		for k := range m {
			seen[k] = true
		}
	}
	for _, name := range fixed {
		seen[name] = true
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// inspectionProblems resolves each finding's severity from policy.
//
// The source is named in every one of them: a rule can match several and fail
// against only one, and "which cluster said so" is the first thing an author
// asks.
func inspectionProblems(
	file, alertName string,
	line int,
	src source.Source,
	merged *policy.Policy,
	findings []query.Finding,
	now time.Time,
) []lint.Problem {
	var problems []lint.Problem

	for _, f := range findings {
		severity := lint.SeverityError
		var origin policy.Setting

		if lint.Configurable(f.Check) {
			origin = merged.For(f.Check)
			severity = origin.Severity
		}
		if severity == lint.SeverityOff {
			continue
		}

		// An exemption is read after the severity, not merged into it, so
		// "the strictest scope wins" stays true of policy. It drops one
		// check on one source, and only a check that could be configured
		// anyway: nothing that blocks can be exempted (spec 7.7).
		if lint.Configurable(f.Check) && src.Exempts(f.Check, now) {
			continue
		}

		p := lint.NewProblem(file, line, f.Check, severity,
			fmt.Sprintf("against source %s: %s", src.Name, f.Detail))
		p.Subject = alertName
		p.PolicyFile, p.PolicyLine = origin.File, origin.Line
		problems = append(problems, p)
	}
	return problems
}

// inspectionFailed reports that the ruler could not ask, which is never a
// finding about the rule. It is a warning for the same reason an inconclusive
// contract assertion is: a cluster that did not answer says nothing about the
// SQL, and blocking on it would let a network blip fail a deploy.
func inspectionFailed(r ruleset.Rule, sourceName string, err error) lint.Problem {
	p := lint.NewProblem(r.File, r.Line(), lint.CheckRuleInspect, lint.SeverityWarning,
		fmt.Sprintf("against source %s: could not inspect the query: %s", sourceName, err))
	p.Subject = r.Alert
	return p
}
