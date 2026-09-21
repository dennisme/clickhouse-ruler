package scheduler

import "context"

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
