// Package scheduler runs the evaluation loop around the pieces slices 1
// through 6 already built: it ticks rule groups on their interval, keeps one
// alert.State per rule and source for the process lifetime, and hands the
// result to a notify.Cadence.
package scheduler

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
)

// Querier runs one rule and returns a sample per matched row. query.Querier
// satisfies this; the interface exists so a test can stand in for ClickHouse.
type Querier interface {
	Run(ctx context.Context, r rule.Rule, who query.Attribution, now time.Time) ([]alert.Sample, error)
}

// Reasons a source produced no samples that are not the query's own error.
var (
	// errNoQuerier means the rule matched a source the ruler holds no
	// connection for, which is a configuration mismatch rather than an
	// outage.
	errNoQuerier = errors.New("no connection open for this source")

	// errQueueAbandoned means shutdown arrived while the query was still
	// queued behind the concurrency limit, so it never ran.
	errQueueAbandoned = errors.New("shutdown before the query started")
)

// SourceError names a source this evaluation failed against, and why.
type SourceError struct {
	Source string
	Err    error
}

// AnnotationError names an annotation whose template would not render, and the
// source whose evaluation found it.
//
// Not a failure of anything: the alert was evaluated, it is being sent, and it
// carries the template error where its annotation should be. It is reported so
// an author learns their summary reads as an error on somebody's page.
type AnnotationError struct {
	Source     string
	Annotation string
	Err        error
}

// SourceWait is how long one source's query spent queued behind that
// source's own concurrency limit before it ran.
type SourceWait struct {
	Source string
	Wait   time.Duration
}

// Result reports what one evaluation of a rule did, for the caller to fold
// into metrics and logs.
type Result struct {
	// SourceErrors holds one entry per source this evaluation failed against,
	// in source order, whether the query failed or the result could not be
	// turned into alerts. The source's alert.State is left untouched either
	// way, so its `for` timer survives. The error is carried rather than
	// counted because a counter alone tells an operator that something failed
	// without saying which source or what it said.
	SourceErrors []SourceError

	// AnnotationErrors holds one entry per source and annotation whose
	// template would not render, deduplicated by alert.State across however
	// many instances the rule produced (spec 8.3).
	AnnotationErrors []AnnotationError

	// QueueWaits holds one entry per source that has a concurrency limit of
	// its own, in source order, with the time its query spent waiting for a
	// slot. Sources bounded only by the ruler-wide cap are absent: they never
	// queue per source, and a zero wait for them would read as a limit doing
	// something. This is the signal that says a limit is set too low, and
	// without it the knob cannot be sized (spec 6.11).
	QueueWaits []SourceWait

	// SendError is set when Cadence failed to reach Alertmanager. The alert
	// state has already been advanced regardless: a notification failure is
	// a delivery problem, not an evaluation problem (spec 6.5).
	SendError error

	// Pending and Firing are counts of currently tracked instances across
	// every matched source, for the clickhouse_ruler_alerts_active gauge. Counts only:
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

	// limits bound how many of this rule's sources are queried at once,
	// shared with every other rule so the ruler-wide cap is the ruler's and
	// each source's cap is that cluster's, not one rule's.
	limits *queryLimits
}

// NewRuleEval builds the per-source state up front, from the sources the
// rule already matched at load time (spec 6.10).
// resolvedRetention is how long each source's state keeps a resolved instance
// so its notification can be retried, derived from how long delivery can take
// rather than picked (spec 6.5).
func NewRuleEval(r ruleset.Rule, queriers map[string]Querier, cadence *notify.Cadence, limits *queryLimits, resolvedRetention time.Duration) *RuleEval {
	states := make(map[string]*alert.State, len(r.Sources))
	for _, src := range r.Sources {
		states[src.Name] = alert.New(r.Rule, r.Labels, src, resolvedRetention)
	}
	return &RuleEval{rule: r, queriers: queriers, cadence: cadence, states: states, limits: limits}
}

// attribution is what this rule's queries are recorded against: its group,
// which names them in system.query_log, and its team, which their cost is
// billed to (spec 8.5, 8.2).
func (e *RuleEval) attribution() query.Attribution {
	return query.Attribution{Group: e.rule.GroupID(), Team: e.rule.Team()}
}

// Evaluate runs the rule against every matched source and sends whatever
// fired or resolved. A source the evaluation failed against is skipped for
// this tick only, whether its query failed or its rows could not be turned
// into alerts; its state is untouched and the next evaluation picks up where
// the last successful one left off.
//
// Sources are queried concurrently, bounded by the shared limits: the
// ruler-wide cap, and inside it whatever cap the source itself sets. A rule
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
		alerts      []alert.Alert
		err         error
		annotations []alert.AnnotationError

		// acquired says the query got its source slot, so wait is a real
		// measurement rather than the zero value of a query that never ran.
		acquired bool
		wait     time.Duration
	}
	results := make([]sourceResult, len(e.rule.Sources))

	var wg sync.WaitGroup
	for i, src := range e.rule.Sources {
		q, ok := e.queriers[src.Name]
		if !ok {
			results[i].err = errNoQuerier
			continue
		}

		wg.Add(1)
		go func(i int, name string, q Querier) {
			defer wg.Done()

			release, wait, ok := e.limits.acquire(ctx, name)
			if !ok {
				// Shutdown arrived while this query was still queued behind
				// the limit. Leaving state untouched is the same outcome as
				// a failed query, and the timers survive either way.
				results[i].err = errQueueAbandoned
				return
			}
			results[i].acquired, results[i].wait = true, wait
			samples, err := q.Run(ctx, e.rule.Rule, e.attribution(), now)
			release()

			if err != nil {
				results[i].err = err
				return
			}
			// State.Eval returns every instance still tracked, pending and
			// firing, plus anything that resolved this tick. It fails when two
			// rows reached one identity, which leaves its state untouched and
			// so reports exactly like a failed query (spec 6.3).
			alerts, annotations, err := e.states[name].Eval(now, samples)
			if err != nil {
				results[i].err = err
				return
			}
			results[i].alerts = alerts
			// An annotation that would not render is reported without failing
			// anything: the alert is intact and still worth sending.
			results[i].annotations = annotations
		}(i, src.Name, q)
	}
	wg.Wait()

	var current []alert.Alert
	for i, r := range results {
		name := e.rule.Sources[i].Name
		if r.acquired && e.limits.boundsSource(name) {
			res.QueueWaits = append(res.QueueWaits, SourceWait{Source: name, Wait: r.wait})
		}
		for _, a := range r.annotations {
			res.AnnotationErrors = append(res.AnnotationErrors,
				AnnotationError{Source: name, Annotation: a.Annotation, Err: a.Err})
		}
		if r.err != nil {
			res.SourceErrors = append(res.SourceErrors, SourceError{Source: name, Err: r.err})
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
	res.SendError = e.cadence.Send(ctx, now, e.rule.Group.Interval, current)
	return res
}
