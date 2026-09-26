package scheduler

import (
	"context"
	"time"

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
}

// newQueryLimits builds the limits from the ruler-wide cap and whatever the
// sources ask for. Semaphores are per source rather than per rule, because
// the cluster sees every rule's query on the same connection pool.
func newQueryLimits(global int, sources []source.Source) *queryLimits {
	l := &queryLimits{global: newSemaphore(global)}
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

// acquire takes the global slot and then the source's, and returns a release
// that gives both back. wait is how long the source's slot alone took, which
// is the number that says a per-source cap is set too low.
//
// The order is global first so the ruler-wide cap stays the ceiling: a source
// slot held while waiting for the global one would let the sources' caps add
// up past it. Both gates abandon on ctx, so shutdown drops a query that is
// still queued rather than running it after the ruler has stopped.
func (l *queryLimits) acquire(ctx context.Context, name string) (release func(), wait time.Duration, ok bool) {
	releaseGlobal, ok := l.global.acquire(ctx)
	if !ok {
		return func() {}, 0, false
	}

	start := time.Now()
	releaseSource, ok := l.bySource[name].acquire(ctx)
	if !ok {
		releaseGlobal()
		return func() {}, 0, false
	}

	return func() {
		releaseSource()
		releaseGlobal()
	}, time.Since(start), true
}
