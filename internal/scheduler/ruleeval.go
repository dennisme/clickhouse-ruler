// Package scheduler runs the evaluation loop around the pieces slices 1
// through 6 already built: it ticks rule groups on their interval, keeps one
// alert.State per rule and source for the process lifetime, and hands the
// result to a notify.Cadence.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// Querier is what one source's connection does for a running ruler: it
// evaluates a rule, and it samples recent data for the re-check pass
// (spec 10.4). query.Querier satisfies this; the interface exists so a test can
// stand in for ClickHouse.
type Querier interface {
	Run(ctx context.Context, r rule.Rule, who query.Attribution, now time.Time) (query.Evaluation, error)
	Sample(ctx context.Context, r rule.Rule, who query.Attribution, c query.SampleChecks, now time.Time) ([]query.Finding, error)
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
// carries a marker where its annotation should be, with the template error in
// ErrorAnnotation beside it. It is reported so an author learns their summary
// reads as a marker on somebody's page.
//
// What this pass itself found, which is what the counter counts: once per rule,
// source and annotation per evaluation. The finding that says the rule is still
// broken is in Problems, and outlives a pass with nothing to render (spec 8.2).
type AnnotationError struct {
	Source     string
	Annotation string
	Err        error
}

// Finding is one thing to report about a rule, with the error behind it where
// there is one to carry.
//
// Err is set only on an annotations/template finding, whose Go template error
// has nowhere else to go: the finding's text names the annotation and the key
// the alert did not carry, and the error itself would otherwise reach nobody
// but whoever reads the alert's ruler_error annotation (spec 6.5). The drift
// findings are read out of a result rather than raised by an error, so they
// have none.
type Finding struct {
	lint.Problem
	Err error

	// Source is the cluster this was found against, and empty when the finding
	// is about the rule rather than one of its sources: rule/source-schema
	// compares two clusters and belongs to neither (spec 8.2).
	//
	// A label on the gauge, so a rule broken on one of four clusters does not
	// read like a rule broken on all four, and so the pass that reaches a
	// cluster again clears only what that cluster raised.
	Source string
}

// Answer is a check this pass can rebuild the gauge for, and the source it can
// rebuild it for. An empty Source is an answer about the rule as a whole, which
// clears every source's series for that check.
//
// Per source because that is the grain the evidence arrives at: one cluster
// answering says nothing about another, and a pass that reached three of four
// clusters knows three quarters of the truth rather than none of it (spec 10.4).
type Answer struct {
	Check  string
	Source string
}

// annotationFindings pairs each finding with the template error that produced
// it, in the order the annotations were read.
func annotationFindings(r ruleset.Rule, src source.Source, now time.Time, a alert.AnnotationError) []Finding {
	var out []Finding
	for _, p := range runtimeProblem(r, src, now, query.Finding{
		Check:  lint.CheckAnnotationsTemplate,
		Detail: renderDetail(a),
	}) {
		out = append(out, Finding{Problem: p, Err: a.Err, Source: src.Name})
	}
	return out
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

	// ConcurrencyWaits holds one entry per query this evaluation sent, with
	// the time it spent queued at the ruler-wide cap. Empty when that cap is
	// off. Zeros are kept, because the cap is on unless it is turned off and a
	// query that found a slot waiting is the reading that says so: this is the
	// term that makes a group late while every cluster it reads is fast, which
	// nothing else reports (spec 8.8).
	ConcurrencyWaits []time.Duration

	// SendError is set when Cadence failed to reach Alertmanager. The alert
	// state has already been advanced regardless: a notification failure is
	// a delivery problem, not an evaluation problem (spec 6.5).
	SendError error

	// Problems holds what this evaluation found about the rule itself: a
	// result the schema moved under, sources that stopped agreeing, a query
	// whose measured cost crossed its ceilings, or a query that failed
	// (spec 6.3.2). They are reported and never acted on, so a rule appearing
	// here has kept its alert state and is still paging.
	//
	// Empty means nothing was found, for the checks Answered names. A finding
	// for a check outside it is still this rule's finding and still reported:
	// a source that replied is evidence about that source whatever the rule's
	// other clusters did.
	Problems []Finding

	// Answered is the checks this pass has a current answer for, so the gauge
	// series this rule holds for each of them can be rebuilt from Problems. A
	// check absent here is one the pass could not ask about: blanking its
	// series then would resolve the finding and read as a schema somebody
	// fixed, so the previous answer is left standing (spec 8.2).
	//
	// Per check rather than per pass, because the two things a pass can fail
	// to answer are different. A source that did not reply says nothing about
	// its result's shape or its cost, and a source the ruler holds no
	// connection for says nothing about whether the query runs.
	Answered []Answer

	// Pending and Firing are counts of currently tracked instances across
	// every matched source, for the clickhouse_ruler_alerts_active gauge. Counts only:
	// labelling that gauge by alert instance would turn the ruler into the
	// cardinality problem it exists to avoid (spec 8.3).
	Pending int
	Firing  int
}

// sourceResult is what one source's half of an evaluation produced, before the
// halves are merged in source order.
type sourceResult struct {
	alerts      []alert.Alert
	err         error
	annotations []alert.AnnotationError

	// shape and usage are what the drift comparison reads out of an
	// evaluation that succeeded (spec 6.3.2).
	shape []query.Column
	usage query.Usage

	// queryErr is set when the query itself failed, which is a finding
	// about the rule. err covers that and more: a source with no
	// connection open, a query abandoned at shutdown, and rows that could
	// not be turned into alerts are all reported to the operator and none
	// of them says the rule's query stopped running.
	queryErr error

	// rendered says this source's evaluation had an instance to render
	// annotations for, which is what answers annotations/template at
	// runtime. A pass that returned no rows rendered nothing and so learned
	// nothing about the templates (spec 6.5).
	rendered bool

	// acquired says the query got through both gates, so wait holds real
	// measurements rather than the zero values of a query that never ran.
	acquired bool
	wait     waits
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

	// drift compares each evaluation against the last one. Per rule, and per
	// source inside it, which is the grain the comparison is at (spec 6.3.2).
	drift *drift

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
	return &RuleEval{
		rule:     r,
		queriers: queriers,
		cadence:  cadence,
		states:   states,
		limits:   limits,
		drift:    newDrift(r),
	}
}

// sources is how many evaluations one tick of this rule makes, one per cluster
// its selector matched. It is the denominator the failure counter is read
// against, so both have to count the same unit (spec 8.2).
func (e *RuleEval) sources() int { return len(e.rule.Sources) }

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

			release, w, ok := e.limits.acquire(ctx, name)
			if !ok {
				// Shutdown arrived while this query was still queued behind
				// the limit. Leaving state untouched is the same outcome as
				// a failed query, and the timers survive either way.
				results[i].err = errQueueAbandoned
				return
			}
			results[i].acquired, results[i].wait = true, w
			evaluation, err := q.Run(ctx, e.rule.Rule, e.attribution(), now)
			release()

			if err != nil {
				results[i].err, results[i].queryErr = err, err
				return
			}
			results[i].shape, results[i].usage = evaluation.Shape, evaluation.Usage
			// State.Eval returns every instance still tracked, pending and
			// firing, plus anything that resolved this tick. It fails when two
			// rows reached one identity, which leaves its state untouched and
			// so reports exactly like a failed query (spec 6.3).
			alerts, annotations, err := e.states[name].Eval(now, evaluation.Samples)
			if err != nil {
				results[i].err = err
				return
			}
			results[i].alerts = alerts
			results[i].rendered = len(evaluation.Samples) > 0
			// An annotation that would not render is reported without failing
			// anything: the alert is intact and still worth sending.
			results[i].annotations = annotations
		}(i, src.Name, q)
	}
	wg.Wait()

	// The two halves of the pass, in source order. A cluster the query failed
	// against has not drifted, so leaving its baseline standing means the next
	// successful evaluation is compared against the last real result rather
	// than against nothing; the failure is reported on its own.
	var answered []evaluated
	var failures []failed

	// The one finding here that is not drift: an annotation the author wrote
	// that will not render against a real alert. Reported under the name the
	// pull request would have used, so an operator who raised that check to
	// block a merge has already said what they think of it (spec 6.5).
	var annotationProblems []Finding

	var current []alert.Alert
	for i, r := range results {
		name := e.rule.Sources[i].Name
		if r.acquired && e.limits.boundsSource(name) {
			res.QueueWaits = append(res.QueueWaits, SourceWait{Source: name, Wait: r.wait.source})
		}
		if r.acquired && e.limits.boundsRuler() {
			res.ConcurrencyWaits = append(res.ConcurrencyWaits, r.wait.ruler)
		}
		for _, a := range r.annotations {
			res.AnnotationErrors = append(res.AnnotationErrors,
				AnnotationError{Source: name, Annotation: a.Annotation, Err: a.Err})
			annotationProblems = append(annotationProblems,
				annotationFindings(e.rule, e.rule.Sources[i], now, a)...)
		}
		if r.err != nil {
			res.SourceErrors = append(res.SourceErrors, SourceError{Source: name, Err: r.err})
			if r.queryErr != nil {
				failures = append(failures, failed{source: e.rule.Sources[i], err: r.queryErr})
			}
			continue
		}
		answered = append(answered, evaluated{
			source: e.rule.Sources[i],
			shape:  r.shape,
			usage:  r.usage,
		})
		current = append(current, r.alerts...)
	}

	res.Problems = append(e.drift.inspect(answered, failures, now), annotationProblems...)
	res.Answered = answeredChecks(e.rule.Sources, results)

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

// answeredChecks says which checks a pass can rebuild the gauge for, and for
// which source.
//
// Per source, because that is the grain the evidence arrives at (spec 10.4). A
// cluster that replied says what its result's shape is and what its query cost;
// a cluster that was reached at all says whether its query runs, whether it
// answered or refused; and a cluster with rows says whether the annotations
// render against them. None of the three says anything about the cluster beside
// it, so a pass that reached three of four clusters rebuilds three quarters of
// this rule's series and leaves the fourth standing.
//
// The exception is the comparison between sources, which belongs to no single one
// of them: a rule whose clusters stopped agreeing has one finding, answered for
// the whole rule, and answered only by a pass that has two results to compare.
func answeredChecks(sources []source.Source, results []sourceResult) []Answer {
	var out []Answer
	replied := 0

	for i, src := range sources {
		r := results[i]

		if r.err == nil {
			replied++
			out = append(out,
				Answer{Check: lint.CheckRuleColumns, Source: src.Name},
				Answer{Check: lint.CheckRuleCost, Source: src.Name})
		}

		// Reached its cluster and came back, with a result or with the
		// database's refusal. A source with no connection open, or a query
		// abandoned at shutdown, is one nothing was learned about.
		if r.err == nil || r.queryErr != nil {
			out = append(out, Answer{Check: lint.CheckRuleExecution, Source: src.Name})
		}

		// An alert to render the templates against. A pass that returned no
		// rows rendered nothing and so learned nothing: clearing on it would
		// let a rule that broke and then stopped firing clear the finding
		// saying so (spec 6.5).
		if r.rendered {
			out = append(out, Answer{Check: lint.CheckAnnotationsTemplate, Source: src.Name})
		}
	}

	// Two replies, because one reply compares with nothing: a pass where the
	// second cluster refused the query cannot say the two still agree, and
	// clearing on it would read as somebody having reconciled them. A rule left
	// matching one source is the other case, where the comparison can never be
	// raised again and a finding from when it matched two has to go.
	if replied >= 2 || (replied > 0 && len(sources) < 2) {
		out = append(out, Answer{Check: lint.CheckRuleSourceSchema})
	}
	return out
}

// renderDetail says what an author has to change, which is the annotation and
// the keys their template read that the alert did not carry. The template error
// itself stays on the page and in the log rather than being copied onto the
// gauge (spec 6.5).
//
// Every missing key is named, where the marker on the alert stops at
// alert.markerKeys and counts the rest. The asymmetry is deliberate: this is a
// log field read by somebody already debugging one rule, and the marker is a
// notification field that becomes a PagerDuty title. Length costs nothing here
// and costs a responder their first line there.
func renderDetail(a alert.AnnotationError) string {
	if len(a.MissingKeys) == 0 {
		return fmt.Sprintf("annotation %q did not render, and the template error is in the log beside this",
			a.Annotation)
	}

	quoted := make([]string, 0, len(a.MissingKeys))
	for _, key := range a.MissingKeys {
		quoted = append(quoted, strconv.Quote(key))
	}
	return fmt.Sprintf("annotation %q reads %s, which this alert does not carry",
		a.Annotation, strings.Join(quoted, " and "))
}

// carry takes over the alert state prev accumulated for the same rule, so a
// reload does not restart the `for` timer of an alert that is already pending
// (spec.md open question 2).
//
// Per source, because state is per rule per source (spec 6.10.1): a rule whose
// selector reached a second cluster on this reload carries the first cluster's
// instances and starts fresh on the new one, which is what the source labels on
// the alerts already say. Whether the reloaded definition is the same alert as
// the one prev was tracking is alert.State's decision, and a state that refuses
// leaves this evaluator with the fresh one NewRuleEval already built.
//
// resolvedRetention is passed rather than read from the state because it is
// derived from the reloaded group's interval, which an author can change in the
// same edit.
func (e *RuleEval) carry(prev *RuleEval, resolvedRetention time.Duration) {
	for _, src := range e.rule.Sources {
		state, ok := prev.states[src.Name]
		if !ok {
			continue
		}
		if state.Adopt(e.rule.Rule, e.rule.Labels, src, resolvedRetention) {
			e.states[src.Name] = state
		}
	}
}
