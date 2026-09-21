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
)

// Scheduler runs every rule group in a loaded ruleset, one goroutine per
// group, each ticking on its own interval.
type Scheduler struct {
	clock   Clock
	groups  []GroupSpec
	metrics *Metrics
	log     *slog.Logger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type namedEval struct {
	rule string
	eval *RuleEval
}

// New builds a Scheduler for set. Every rule with at least one matched
// source gets its own RuleEval sharing queriers; a rule matching none is
// counted in ruler_rules_unmatched and never evaluated (spec 6.10).
//
// Group starts are staggered across their own interval, deterministically by
// group identity, so that many groups on the same interval do not all fire
// on the same tick and stampede ClickHouse.
// Rules within a group are evaluated concurrently and so are each rule's
// sources, with concurrency bounded by queryConcurrency rather than by the
// shape of the configuration. Sequential evaluation made a group's tick cost
// the sum of every query inside it, so a group grew slower simply by having
// more rules added to it, until it began missing iterations.
// A nil log discards every line, so a caller that does not care about output
// does not have to build a handler.
func New(set *ruleset.Set, queriers map[string]Querier, cadence *notify.Cadence, metrics *Metrics, clock Clock, queryConcurrency int, log *slog.Logger) *Scheduler {
	type groupKey struct{ file, name string }

	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	queries := newSemaphore(queryConcurrency)

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

	now := clock.Now()
	specs := make([]GroupSpec, 0, len(order))

	for _, k := range order {
		groupName := k.file + ":" + k.name
		interval := intervalByGroup[k]

		var evals []namedEval
		unmatched := 0
		for _, r := range rulesByGroup[k] {
			if len(r.Sources) == 0 {
				unmatched++
				continue
			}
			evals = append(evals, namedEval{rule: r.Alert, eval: NewRuleEval(r, queriers, cadence, queries)})
		}
		metrics.RulesUnmatched.WithLabelValues(groupName).Set(float64(unmatched))

		offset := staggerOffset(k.file+"|"+k.name, interval)
		specs = append(specs, GroupSpec{
			Name:     groupName,
			Interval: interval,
			Start:    now.Add(offset),
			Eval:     evalGroup(groupName, evals, metrics, log),
		})
	}

	return &Scheduler{clock: clock, groups: specs, metrics: metrics, log: log}
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
// the semaphore inside each RuleEval does, around the query itself, so a
// group with many rules queues against the limit instead of opening a
// connection per rule. Prometheus collectors are safe for concurrent use, so
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
				if len(res.QueryErrors) > 0 {
					m.EvaluationFailuresTotal.WithLabelValues(groupName, ne.rule).Add(float64(len(res.QueryErrors)))
				}
				for _, se := range res.QueryErrors {
					log.Error("rule evaluation failed against a source",
						"rule_group", groupName, "rule", ne.rule,
						"source", se.Source, "error", se.Err.Error())
				}
				if res.SendError != nil {
					log.Error("sending alerts to alertmanager failed",
						"rule_group", groupName, "rule", ne.rule,
						"error", res.SendError.Error())
				}
				m.AlertsActive.WithLabelValues(groupName, ne.rule, "pending").Set(float64(res.Pending))
				m.AlertsActive.WithLabelValues(groupName, ne.rule, "firing").Set(float64(res.Firing))
			}(ne)
		}
		wg.Wait()
	}
}

// Start launches one goroutine per group and returns immediately.
func (s *Scheduler) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
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
// off mid-way through.
func (s *Scheduler) Shutdown(timeout time.Duration) {
	if s.cancel == nil {
		return
	}
	s.cancel()

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
