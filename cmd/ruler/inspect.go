package main

import (
	"context"
	"fmt"

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
func inspectRules(ctx context.Context, set *ruleset.Set, root *policy.Policy) []lint.Problem {
	var problems []lint.Problem

	queriers := map[string]*query.Querier{}
	defer func() {
		for _, q := range queriers {
			_ = q.Close()
		}
	}()

	for _, r := range set.Rules {
		for _, src := range r.Sources {
			merged := policy.Merge(root, src.Policy)

			checks := checksFromPolicy(merged)

			q, err := querierFor(queriers, src)
			if err != nil {
				problems = append(problems, inspectionFailed(r, src.Name, err))
				continue
			}

			findings, err := q.Inspect(ctx, r.Rule, checks)
			if err != nil {
				problems = append(problems, inspectionFailed(r, src.Name, err))
				continue
			}
			problems = append(problems,
				inspectionProblems(r.File, r.Alert, r.Line(), src.Name, merged, findings)...)
		}
	}
	return problems
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
func checksFromPolicy(p *policy.Policy) query.Checks {
	var c query.Checks

	if tf := p.For(policy.CheckRuleTableFunction); tf.Severity != lint.SeverityOff {
		c.AllowedTableFunctions = tf.Keys
	}
	if nd := p.For(policy.CheckRuleNondeterministic); nd.Severity != lint.SeverityOff {
		c.Nondeterministic = nd.Keys
	}
	return c
}

// inspectionProblems resolves each finding's severity from policy.
//
// The source is named in every one of them: a rule can match several and fail
// against only one, and "which cluster said so" is the first thing an author
// asks.
func inspectionProblems(
	file, alert string,
	line int,
	sourceName string,
	merged *policy.Policy,
	findings []query.Finding,
) []lint.Problem {
	var problems []lint.Problem

	for _, f := range findings {
		severity := lint.SeverityError
		var origin policy.Setting

		if policy.Configurable(f.Check) {
			origin = merged.For(f.Check)
			severity = origin.Severity
		}
		if severity == lint.SeverityOff {
			continue
		}

		problems = append(problems, lint.Problem{
			File:       file,
			Line:       line,
			Subject:    alert,
			Check:      f.Check,
			Severity:   severity,
			Text:       fmt.Sprintf("against source %s: %s", sourceName, f.Detail),
			PolicyFile: origin.File,
			PolicyLine: origin.Line,
		})
	}
	return problems
}

// inspectionFailed reports that the ruler could not ask, which is never a
// finding about the rule. It is a warning for the same reason an inconclusive
// contract assertion is: a cluster that did not answer says nothing about the
// SQL, and blocking on it would let a network blip fail a deploy.
func inspectionFailed(r ruleset.Rule, sourceName string, err error) lint.Problem {
	return lint.Problem{
		File:     r.File,
		Line:     r.Line(),
		Subject:  r.Alert,
		Check:    query.CheckInspect,
		Severity: lint.SeverityWarning,
		Text:     fmt.Sprintf("against source %s: could not inspect the query: %s", sourceName, err),
	}
}
