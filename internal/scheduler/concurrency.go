package scheduler

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// DefaultQueryConcurrency bounds how many rule queries one ruler sends at
// once. It exists because a group's tick fans out across its rules and their
// sources simultaneously, and an unbounded fan-out would turn a large group
// into a stampede against ClickHouse: exactly what staggering group starts
// (spec 6.10) is meant to prevent.
const DefaultQueryConcurrency = 8

// semaphore bounds concurrent work. A nil semaphore is unbounded, which is
// what a caller that does not care about the limit gets for free.
type semaphore chan struct{}

func newSemaphore(limit int) semaphore {
	if limit <= 0 {
		return nil
	}
	return make(semaphore, limit)
}

// acquire blocks for a slot and returns a release function, or false if ctx
// ended first. Shutdown cancels the context, so a query still queued behind
// the limit is abandoned rather than run after the ruler has stopped.
func (s semaphore) acquire(ctx context.Context) (release func(), ok bool) {
	if s == nil {
		return func() {}, true
	}
	select {
	case s <- struct{}{}:
		return func() { <-s }, true
	case <-ctx.Done():
		return func() {}, false
	}
}

// waits is how long one query spent queued at each gate before it ran. The two
// are separate because they say different things and are fixed by different
// people: the ruler-wide one is the operator's cap, the source one is the
// cluster's (spec 8.8).
type waits struct {
	ruler  time.Duration
	source time.Duration
}

// queryLimits is the two level bound on queries in flight: the ruler-wide cap
// every query passes through, and inside it an optional cap per source.
//
// The global cap alone is a loose proxy for the thing that actually needs
// protecting, which is each ClickHouse cluster. A cluster that has gone slow
// holds global slots that rules against every other cluster then queue
// behind, so an outage on one source delays evaluation of sources that are
// perfectly healthy. The per-source cap is what confines that damage to the
// cluster causing it (spec 6.11).
//
// A source with no cap of its own has no entry here, and a nil semaphore is
// unbounded, so it passes straight through the second gate.
type queryLimits struct {
	global   semaphore
	bySource map[string]semaphore

	// inFlight counts queries executing or waiting for a slot, which is what
	// the cap is read against without waiting for a histogram to fill
	// (spec 8.8). Nil when nothing is collecting.
	inFlight prometheus.Gauge
}

// newQueryLimits builds the limits from the ruler-wide cap and whatever the
// sources ask for. Semaphores are per source rather than per rule, because
// the cluster sees every rule's query on the same connection pool.
func newQueryLimits(global int, sources []source.Source, m *Metrics) *queryLimits {
	l := &queryLimits{global: newSemaphore(global)}
	if m != nil {
		l.inFlight = m.QueriesInFlight
	}
	for _, s := range sources {
		if s.MaxConcurrentQueries <= 0 {
			continue
		}
		if l.bySource == nil {
			l.bySource = map[string]semaphore{}
		}
		if _, ok := l.bySource[s.Name]; !ok {
			l.bySource[s.Name] = newSemaphore(s.MaxConcurrentQueries)
		}
	}
	return l
}

// boundsSource reports whether this source has a cap of its own, which is
// what decides whether a wait against it is worth recording: a source with no
// cap never queues here, and reporting a zero wait for it would read as a
// limit that is doing something.
func (l *queryLimits) boundsSource(name string) bool {
	_, ok := l.bySource[name]
	return ok
}

// boundsRuler reports whether a ruler-wide cap is set at all. Unlike a source
// cap this one is on unless it is turned off, so a zero wait against it is a
// reading rather than the absence of one: it says the query found a slot
// waiting, which is what a ruler running well below its cap looks like.
func (l *queryLimits) boundsRuler() bool {
	return l.global != nil
}

// acquire takes the global slot and then the source's, and returns a release
// that gives both back, with how long each gate took. The source wait is the
// number that says a per-source cap is set too low; the ruler wait is the one
// that says a group is late while every cluster it reads is fast, which no
// other metric can show (spec 8.8).
//
// The order is global first so the ruler-wide cap stays the ceiling: a source
// slot held while waiting for the global one would let the sources' caps add
// up past it. Both gates abandon on ctx, so shutdown drops a query that is
// still queued rather than running it after the ruler has stopped.
func (l *queryLimits) acquire(ctx context.Context, name string) (release func(), w waits, ok bool) {
	// Counted from here rather than from the far side of the gates, so the
	// gauge reads as queries this ruler is trying to run: a query waiting for
	// a slot is load the operator is carrying.
	l.trackIn()

	start := time.Now()
	releaseGlobal, ok := l.global.acquire(ctx)
	if !ok {
		l.trackOut()
		return func() {}, waits{}, false
	}
	w.ruler = time.Since(start)

	start = time.Now()
	releaseSource, ok := l.bySource[name].acquire(ctx)
	if !ok {
		releaseGlobal()
		l.trackOut()
		return func() {}, waits{}, false
	}
	w.source = time.Since(start)

	return func() {
		releaseSource()
		releaseGlobal()
		l.trackOut()
	}, w, true
}

func (l *queryLimits) trackIn() {
	if l.inFlight != nil {
		l.inFlight.Inc()
	}
}

func (l *queryLimits) trackOut() {
	if l.inFlight != nil {
		l.inFlight.Dec()
	}
}
