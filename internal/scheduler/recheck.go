package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
)

// DefaultRecheckInterval is how often the re-check pass runs when an operator
// asks for it without saying how often.
//
// An hour, because the pass reads real data: one bounded query per rule per
// source, which is affordable hourly and absurd every minute. How often a schema
// moves is a property of the organisation rather than something derivable, so
// this is a default and not a rule (spec 10.4).
const DefaultRecheckInterval = time.Hour

// The two feeds into clickhouse_ruler_problem, as the log says which one found a
// thing. "Your result changed shape" and "your map key is gone from recent data"
// are different problems, with different fixes, arriving on different clocks
// (spec 10.4).
const (
	feedEvaluation = "evaluation"
	feedRecheck    = "re-check"
)

// recheckRule is one loaded rule the pass asks about, with the labels a finding
// is addressed with.
type recheckRule struct {
	rule ruleset.Rule
	team string
	file string
}

// recheckPass re-asks the one question no evaluation answers.
//
// `rule/attribute-key` catches the OTel map key rename: a query that still
// parses, still returns the same columns, still succeeds on every tick, and
// matches nothing forever because the key it reads is written under a different
// name now. The shape and the row count both look healthy, so the only thing
// that answers it is sampling recent data, which is a query the evaluation does
// not make and therefore needs a clock of its own (spec 6.3.2, 10.4).
//
// It does not gate anything. Findings go to clickhouse_ruler_problem beside the
// evaluator feed's, no rule is unloaded, no evaluation is refused and no alert
// is resolved (spec 7.6).
//
// Rules are asked one at a time. The pass shares the ruler-wide and per-source
// query limits with evaluation rather than getting a budget of its own, so a
// re-check waits behind the evaluations already queued there and holds at most
// one slot while it runs: evaluation is the work that cannot wait, and
// re-checking is the work that can (spec 6.11).
func recheckPass(
	rules []recheckRule,
	queriers map[string]Querier,
	limits *queryLimits,
	m *Metrics,
	log *slog.Logger,
) func(context.Context, time.Time) {
	return func(ctx context.Context, now time.Time) {
		for _, rr := range rules {
			problems, asked := recheckRuleOnce(ctx, rr.rule, queriers, limits, now)

			// Per source, because a cluster that answered says nothing about
			// the one beside it: blanking a series for a cluster nobody
			// sampled would read as a key somebody put back (spec 10.4).
			for _, src := range asked {
				m.Problem.DeletePartialMatch(prometheus.Labels{
					"rule": rr.rule.Alert, "file": rr.file,
					"check": lint.CheckRuleAttributeKey, "source": src,
				})
			}
			for _, p := range problems {
				m.Problem.WithLabelValues(
					rr.rule.Alert, p.Check, p.Severity.String(), rr.team, p.File, p.Source).Set(1)

				log.Warn("a rule broke while running",
					"rule_group", rr.rule.GroupID(), "rule", rr.rule.Alert,
					"check", p.Check, "severity", p.Severity.String(),
					"team", rr.team, "file", p.File, "source", p.Source,
					"feed", feedRecheck, "problem", p.Text)
			}
		}
	}
}

// recheckRuleOnce samples one rule against every source it matched, and says
// which of them answered.
//
// A source whose sample failed is not a finding about the rule, the same line
// the online pass draws: the ruler could not ask, and saying nothing is the only
// honest answer. A source the check is off for is not sampled at all, because
// honouring `off` afterwards would mean reading rows an operator asked nobody to
// read (spec 7.3); it still counts as answered, so switching the check off
// clears what it had raised rather than freezing it.
func recheckRuleOnce(
	ctx context.Context,
	r ruleset.Rule,
	queriers map[string]Querier,
	limits *queryLimits,
	now time.Time,
) ([]Finding, []string) {
	var problems []Finding
	var asked []string

	for _, src := range r.Sources {
		q, ok := queriers[src.Name]
		if !ok {
			continue
		}
		checks, wanted := query.SamplingFromPolicy(policy.Merge(r.Policy, src.Policy))
		if !wanted {
			asked = append(asked, src.Name)
			continue
		}

		release, _, ok := limits.acquire(ctx, src.Name)
		if !ok {
			continue
		}
		findings, err := q.Sample(ctx, r.Rule, recheckAttribution(r), checks, now)
		release()
		if err != nil {
			continue
		}
		asked = append(asked, src.Name)

		for _, f := range findings {
			for _, p := range runtimeProblem(r, src, now, f) {
				problems = append(problems, Finding{Problem: p, Source: src.Name})
			}
		}
	}

	return problems, asked
}

// recheckAttribution is what the pass's queries are recorded against: the same
// group and team an evaluation of this rule carries, so an operator picking a
// rule's work out of system.query_log finds all of it under one name
// (spec 8.5).
func recheckAttribution(r ruleset.Rule) query.Attribution {
	return query.Attribution{Group: r.GroupID(), Team: r.Team()}
}
