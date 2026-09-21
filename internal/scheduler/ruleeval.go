// Package scheduler runs the evaluation loop around the pieces slices 1
// through 6 already built: it ticks rule groups on their interval, keeps one
// alert.State per rule and source for the process lifetime, and hands the
// result to a notify.Cadence.
package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
)

// Querier runs one rule and returns a sample per matched row. query.Querier
// satisfies this; the interface exists so a test can stand in for ClickHouse.
type Querier interface {
	Run(ctx context.Context, r rule.Rule, now time.Time) ([]alert.Sample, error)
}

// Result reports what one evaluation of a rule did, for the caller to fold
// into metrics.
type Result struct {
	// QueryErrors counts sources whose query failed this evaluation. The
	// source's alert.State is left untouched, so its `for` timer survives.
	QueryErrors int

	// SendError is set when Cadence failed to reach Alertmanager. The alert
	// state has already been advanced regardless: a notification failure is
	// a delivery problem, not an evaluation problem (spec 6.5).
	SendError error

	// Pending and Firing are counts of currently tracked instances across
	// every matched source, for the ruler_alerts_active gauge. Counts only:
	// labelling that gauge by alert instance would turn the ruler into the
	// cardinality problem it exists to avoid (spec 8.3).
	Pending int
	Firing  int
}

// RuleEval evaluates one rule against every source it matched.
//
// It owns one alert.State per source for its entire lifetime (spec 6.10.1):
// creating it fresh on every evaluation would reset every `for` and
// keep_firing_for timer, which is the one thing the state exists to avoid.
type RuleEval struct {
	rule     ruleset.Rule
	queriers map[string]Querier
	cadence  *notify.Cadence
	states   map[string]*alert.State

	// queries bounds how many of this rule's sources are queried at once,
	// shared with every other rule so the limit is the ruler's, not one
	// rule's.
	queries semaphore
}

// NewRuleEval builds the per-source state up front, from the sources the
// rule already matched at load time (spec 6.10).
func NewRuleEval(r ruleset.Rule, queriers map[string]Querier, cadence *notify.Cadence, queries semaphore) *RuleEval {
	states := make(map[string]*alert.State, len(r.Sources))
	for _, src := range r.Sources {
		states[src.Name] = alert.New(r.Rule, r.Labels, src)
	}
	return &RuleEval{rule: r, queriers: queriers, cadence: cadence, states: states, queries: queries}
}

// Evaluate runs the rule against every matched source and sends whatever
// fired or resolved. A source whose query fails is skipped for this tick
// only; its state is untouched and the next evaluation picks up where the
// last successful one left off.
//
// Sources are queried concurrently, bounded by the shared semaphore. A rule
// spanning an estate would otherwise cost the sum of every cluster's latency
// on every tick, and a group's tick is the sum of its rules, so the slowest
// cluster sets the pace for everything behind it.
//
// Each source has its own alert.State and they share nothing (spec 6.10.1),
// so evaluating them in parallel needs no lock. Results are collected by
// index and merged in source order, because a batch whose ordering changed
// from tick to tick would be needlessly hard to read in a log or a diff.
func (e *RuleEval) Evaluate(ctx context.Context, now time.Time) Result {
	var res Result

	type sourceResult struct {
		alerts []alert.Alert
		failed bool
	}
	results := make([]sourceResult, len(e.rule.Sources))

	var wg sync.WaitGroup
	for i, src := range e.rule.Sources {
		q, ok := e.queriers[src.Name]
		if !ok {
			results[i].failed = true
			continue
		}

		wg.Add(1)
		go func(i int, name string, q Querier) {
			defer wg.Done()

			release, ok := e.queries.acquire(ctx)
			if !ok {
				// Shutdown arrived while this query was still queued behind
				// the limit. Leaving state untouched is the same outcome as
				// a failed query, and the timers survive either way.
				results[i].failed = true
				return
			}
			samples, err := q.Run(ctx, e.rule.Rule, now)
			release()

			if err != nil {
				results[i].failed = true
				return
			}
			// State.Eval returns every instance still tracked, pending and
			// firing, plus anything that resolved this tick.
			results[i].alerts = e.states[name].Eval(now, samples)
		}(i, src.Name, q)
	}
	wg.Wait()

	var current []alert.Alert
	for _, r := range results {
		if r.failed {
			res.QueryErrors++
			continue
		}
		current = append(current, r.alerts...)
	}

	for _, a := range current {
		switch a.Phase {
		case alert.PhasePending:
			res.Pending++
		case alert.PhaseFiring:
			res.Firing++
		}
	}

	if len(current) == 0 {
		return res
	}
	res.SendError = e.cadence.Send(ctx, now, current, e.rule.Annotations)
	return res
}
