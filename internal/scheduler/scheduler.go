package scheduler

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// Scheduler runs every rule group in a loaded ruleset, one goroutine per
// group, each ticking on its own interval.
//
// What a reload replaces is the group set and the per-rule evaluators; what it
// keeps is everything above them, which is why they are separated here. The
// clock, the metrics, the logger and the notify.Cadence outlive any
// configuration: the Cadence in particular holds when each firing alert was
// last posted to Alertmanager, so rebuilding it on a reload would re-post every
// firing alert at once and reset the resend interval of each (spec 6.5).
type Scheduler struct {
	clock       Clock
	metrics     *Metrics
	log         *slog.Logger
	cadence     *notify.Cadence
	concurrency int
	resend      Resend

	// mu guards the loaded configuration and the goroutines running it, so a
	// reload arriving on the signal handler cannot race a shutdown arriving on
	// another signal.
	mu      sync.Mutex
	groups  []GroupSpec
	evals   map[ruleKey]*RuleEval
	loaded  configured
	base    context.Context
	cancel  context.CancelFunc
	stopped bool
	wg      sync.WaitGroup
}

// ruleKey identifies one rule across a reload: its group, which already carries
// the file it came from (GroupID), and its alert name, which rule/name makes
// unique within a group (spec 7.6). It is what the previous configuration's
// evaluators are looked up by, so a rule that is still there finds the state it
// was accumulating. Whether the two are the same alert is then alert.State's
// question, not this key's (spec 6.3).
//
// So a rule moved to another group, or to another file, is looked up under a key
// nothing held and starts with fresh state, even though its labels may be
// untouched. That is a cost, not a claim about identity: the alternative is
// searching every previous evaluator for one whose labels match, which would
// silently hand the state of a rule an author deleted to an unrelated rule that
// happens to agree with it. A move also renames every metric series the rule
// has, so it is already a visible edit rather than a quiet one.
type ruleKey struct {
	group string
	alert string
}

// configured is what the running configuration has put on the metrics
// registry, so a reload can delete the series of everything that is no longer
// loaded. A series left behind at its last value reads as a rule that still
// runs: the gauge holds, the counters stop moving, and both look exactly like a
// rule that has simply gone quiet.
type configured struct {
	groups  map[string]bool
	rules   map[ruleKey]bool
	sources map[string]bool

	// names counts how many groups hold each alert name, because the query cost
	// metrics carry the rule and not its group (spec 8.2). A rule dropped from
	// one group while another group still holds a rule by that name must not
	// take the surviving one's cost series with it.
	names map[string]int
}

type namedEval struct {
	rule string
	eval *RuleEval
}

// New builds a Scheduler for set, which build lays out group by group.
//
// queryConcurrency is the ruler-wide ceiling on queries in flight, and resend
// is the pair every rule's resolved-alert retention is sized from (spec 6.5).
// A nil log discards every line, so a caller that does not care about output
// does not have to build a handler.
func New(set *ruleset.Set, queriers map[string]Querier, cadence *notify.Cadence, metrics *Metrics, clock Clock, queryConcurrency int, log *slog.Logger, resend Resend) *Scheduler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	s := &Scheduler{
		clock:       clock,
		metrics:     metrics,
		log:         log,
		cadence:     cadence,
		concurrency: queryConcurrency,
		resend:      resend,
	}
	s.groups, s.evals = s.build(set, queriers, nil)
	s.loaded = describe(set)
	return s
}

// Reload replaces the running configuration with set and the connections in
// queriers, keeping the alert state of every rule that is still the same rule.
//
// It stops the group goroutines and waits for every evaluation already in
// flight to return before it builds anything. That wait is what makes closing a
// connection safe: the caller opens the queriers a reload needs, calls this,
// and only then closes the ones nothing matches any more. By the time Reload
// returns, no evaluation of the previous configuration is running and no new
// one has started against a querier this reload did not hand over, so there is
// no moment at which a closed connection is still held by something about to
// use it. A reload that skipped the wait would be a use-after-close on whichever
// source an operator happened to remove.
//
// The cost of the wait is that a reload takes as long as the slowest evaluation
// in flight, and that every group's tick schedule restarts: a group is
// re-staggered from the moment the reload finishes rather than resuming its old
// phase. Both are the right trade. A reload is an operator action, not
// something that happens on a timer, and a schedule that drifted by less than
// one interval is invisible next to reading rules that are wrong.
func (s *Scheduler) Reload(set *ruleset.Set, queriers map[string]Querier) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopped {
		// Shutdown has already run. Starting groups again here would evaluate
		// rules after the process announced it was going away.
		return
	}

	running := s.cancel != nil
	if running {
		s.cancel()
		s.wg.Wait()
		s.cancel = nil
	}

	groups, evals := s.build(set, queriers, s.evals)
	next := describe(set)
	s.deleteGoneSeries(s.loaded, next)
	s.groups, s.evals, s.loaded = groups, evals, next

	if running {
		s.startLocked()
	}
}

// build turns a loaded ruleset into one GroupSpec per group and one RuleEval
// per rule, carrying over the alert state of any rule prev already held.
//
// Group starts are staggered across their own interval, deterministically by
// group identity, so that many groups on the same interval do not all fire
// on the same tick and stampede ClickHouse.
// Rules within a group are evaluated concurrently and so are each rule's
// sources, with concurrency bounded by the ruler-wide cap rather than by the
// shape of the configuration. Sequential evaluation made a group's tick cost
// the sum of every query inside it, so a group grew slower simply by having
// more rules added to it, until it began missing iterations.
// Inside the ruler-wide cap a source may set a limit of its own, so one cluster
// going slow cannot hold every slot while rules against healthy clusters queue
// behind it (spec 6.11). The limits are rebuilt per configuration because a
// source's limit is part of the file being reloaded.
// A rule with no matched source is counted in clickhouse_ruler_rules_unmatched
// and never evaluated (spec 6.10).
func (s *Scheduler) build(set *ruleset.Set, queriers map[string]Querier, prev map[ruleKey]*RuleEval) ([]GroupSpec, map[ruleKey]*RuleEval) {
	type groupKey struct{ file, name string }

	limits := newQueryLimits(s.concurrency, matchedSources(set))

	var order []groupKey
	rulesByGroup := map[groupKey][]ruleset.Rule{}
	intervalByGroup := map[groupKey]time.Duration{}

	for _, r := range set.Rules {
		k := groupKey{r.File, r.Group.Name}
		if _, ok := rulesByGroup[k]; !ok {
			order = append(order, k)
			intervalByGroup[k] = r.Group.Interval
		}
		rulesByGroup[k] = append(rulesByGroup[k], r)
	}

	sort.Slice(order, func(i, j int) bool {
		if order[i].file != order[j].file {
			return order[i].file < order[j].file
		}
		return order[i].name < order[j].name
	})

	now := s.clock.Now()
	specs := make([]GroupSpec, 0, len(order))
	evals := make(map[ruleKey]*RuleEval, len(set.Rules))

	for _, k := range order {
		groupName := ruleset.GroupID(k.file, k.name)
		interval := intervalByGroup[k]
		retention := s.resend.retention(interval)

		var named []namedEval
		unmatched := 0
		for _, r := range rulesByGroup[k] {
			if len(r.Sources) == 0 {
				unmatched++
				continue
			}
			eval := NewRuleEval(r, queriers, s.cadence, limits, retention)
			key := ruleKey{group: groupName, alert: r.Alert}
			if p, ok := prev[key]; ok {
				eval.carry(p, retention)
			}
			evals[key] = eval
			named = append(named, namedEval{rule: r.Alert, eval: eval})
		}
		s.metrics.RulesUnmatched.WithLabelValues(groupName).Set(float64(unmatched))

		offset := staggerOffset(k.file+"|"+k.name, interval)
		specs = append(specs, GroupSpec{
			Name:     groupName,
			Interval: interval,
			Start:    now.Add(offset),
			Eval:     evalGroup(groupName, named, s.metrics, s.log),
		})
	}

	return specs, evals
}

// describe records what set puts on the registry.
func describe(set *ruleset.Set) configured {
	c := configured{
		groups:  map[string]bool{},
		rules:   map[ruleKey]bool{},
		sources: map[string]bool{},
		names:   map[string]int{},
	}
	for _, r := range set.Rules {
		group := r.GroupID()
		c.groups[group] = true

		key := ruleKey{group: group, alert: r.Alert}
		if !c.rules[key] {
			c.rules[key] = true
			c.names[r.Alert]++
		}
		for _, src := range r.Sources {
			c.sources[src.Name] = true
		}
	}
	return c
}

// deleteGoneSeries removes the metric series of every group, rule and source
// the reload dropped.
//
// Deleting is not tidying. A counter that stops being incremented keeps its
// last value forever, so a rule deleted from the repository goes on reporting
// the evaluations it once did and the alerts it once had, which is
// indistinguishable on a dashboard from a rule that is loaded and quiet. The
// one signal that would say otherwise, a gauge falling to zero, is exactly
// what a stale series cannot produce.
func (s *Scheduler) deleteGoneSeries(old, next configured) {
	for group := range old.groups {
		if next.groups[group] {
			continue
		}
		s.metrics.deleteGroup(group)
	}

	for key := range old.rules {
		if next.rules[key] {
			continue
		}
		s.metrics.deleteRule(key.group, key.alert)

		// The cost metrics carry the rule alone, so they go only once no group
		// holds a rule by this name any more.
		if next.names[key.alert] == 0 {
			s.metrics.deleteRuleName(key.alert)
		}
	}

	for name := range old.sources {
		if next.sources[name] {
			continue
		}
		s.metrics.deleteSource(name)
	}
}

// matchedSources is every source at least one rule reaches, deduplicated by
// name. A source nothing matched needs no semaphore, because nothing will
// ever queue against it.
func matchedSources(set *ruleset.Set) []source.Source {
	var sources []source.Source
	seen := map[string]bool{}
	for _, r := range set.Rules {
		for _, s := range r.Sources {
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			sources = append(sources, s)
		}
	}
	return sources
}

// staggerOffset spreads a group's first tick across its own interval,
// deterministically by key, so a fleet of groups sharing an interval do not
// all start on the same tick.
func staggerOffset(key string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	// The result of %uint64(interval) is always < interval, and interval is
	// itself a positive int64 Duration, so this always fits back into one.
	return time.Duration(h.Sum64() % uint64(interval)) //nolint:gosec
}

// evalGroup runs every rule in one group and records the per-rule metrics
// spec 8.2 asks for. Labelled by rule_group and rule only (spec 8.3).
//
// It also logs what the metrics cannot say: which source refused a query and
// what it said, and which rule could not be delivered. One line per failed
// source and one per failed send, never one per alert instance, for the same
// reason the metrics carry no instance label (spec 8.3).
//
// Rules run concurrently. The goroutine per rule is not what bounds load:
// the limits inside each RuleEval do, around the query itself, so a group
// with many rules queues against them instead of opening a connection per
// rule. Prometheus collectors are safe for concurrent use, so
// the metric writes below need no coordination.
func evalGroup(groupName string, evals []namedEval, m *Metrics, log *slog.Logger) func(context.Context, time.Time) {
	return func(ctx context.Context, tickAt time.Time) {
		var wg sync.WaitGroup
		for _, ne := range evals {
			wg.Add(1)
			go func(ne namedEval) {
				defer wg.Done()
				res := ne.eval.Evaluate(ctx, tickAt)

				m.EvaluationsTotal.WithLabelValues(groupName, ne.rule).Inc()
				if len(res.SourceErrors) > 0 {
					m.EvaluationFailuresTotal.WithLabelValues(groupName, ne.rule).Add(float64(len(res.SourceErrors)))
				}
				for _, se := range res.SourceErrors {
					log.Error("rule evaluation failed against a source",
						"rule_group", groupName, "rule", ne.rule,
						"source", se.Source, "error", se.Err.Error())
				}
				// One line per broken annotation, not per instance: a template
				// that will not render fails on every row a rule returns
				// (spec 8.3, 8.4). Warn rather than error, because the alert
				// was still delivered.
				for _, ae := range res.AnnotationErrors {
					m.AnnotationFailures.WithLabelValues(groupName, ne.rule, ae.Annotation).Inc()
					log.Warn("annotation template failed, the alert carries the error instead",
						"rule_group", groupName, "rule", ne.rule,
						"source", ae.Source, "annotation", ae.Annotation,
						"error", ae.Err.Error())
				}
				if res.SendError != nil {
					log.Error("sending alerts to alertmanager failed",
						"rule_group", groupName, "rule", ne.rule,
						"error", res.SendError.Error())
				}
				for _, qw := range res.QueueWaits {
					m.QueryQueueWait.WithLabelValues(qw.Source).Observe(qw.Wait.Seconds())
				}
				m.AlertsActive.WithLabelValues(groupName, ne.rule, "pending").Set(float64(res.Pending))
				m.AlertsActive.WithLabelValues(groupName, ne.rule, "firing").Set(float64(res.Firing))
			}(ne)
		}
		wg.Wait()
	}
}

// Start launches one goroutine per group and returns immediately. ctx is the
// scheduler's lifetime: a reload derives each configuration's context from it,
// so cancelling ctx stops the ruler whichever configuration is loaded.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.base = ctx
	s.startLocked()
}

// startLocked launches the loaded groups. Called with mu held, by Start and by
// every reload after it.
func (s *Scheduler) startLocked() {
	ctx, cancel := context.WithCancel(s.base)
	s.cancel = cancel

	for _, spec := range s.groups {
		spec := spec
		groupName := spec.Name
		inner := spec.Eval
		spec.Eval = func(ctx context.Context, tickAt time.Time) {
			start := s.clock.Now()
			inner(ctx, tickAt)
			dur := s.clock.Now().Sub(start)

			s.metrics.EvaluationDuration.WithLabelValues(groupName).Observe(dur.Seconds())
			s.metrics.LastDuration.WithLabelValues(groupName).Set(dur.Seconds())
			s.metrics.LastEvaluationTimestamp.WithLabelValues(groupName).Set(float64(tickAt.Unix()))
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			runGroup(ctx, s.clock, spec,
				func() { s.metrics.IterationsTotal.WithLabelValues(groupName).Inc() },
				func(n int) { s.metrics.IterationsMissedTotal.WithLabelValues(groupName).Add(float64(n)) },
			)
		}()
	}
}

// Shutdown stops every group from ticking again and waits up to timeout for
// evaluations already in flight to finish, so a query or a send is not cut
// off mid-way through. A reload arriving afterwards is ignored.
func (s *Scheduler) Shutdown(timeout time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stopped = true
	if s.cancel == nil {
		return
	}
	s.cancel()
	s.cancel = nil

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		// The only signal an operator gets that a query or a send was cut off
		// part way through.
		s.log.Warn("shutdown timeout expired with evaluations still running",
			"timeout", timeout.String())
	}
}
