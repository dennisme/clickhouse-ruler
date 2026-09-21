package scheduler

import (
	"context"
	"time"
)

// GroupSpec is one rule group to tick on its own interval.
type GroupSpec struct {
	Name     string
	Interval time.Duration

	// Start is the first tick, already staggered across the group's peers so
	// that many groups on the same interval do not all fire on the same
	// second (spec 6.10.2, section 2).
	Start time.Time

	// Eval runs one iteration. It may take arbitrarily long; runGroup never
	// calls it again before it returns.
	Eval func(ctx context.Context, tickAt time.Time)
}

// runGroup ticks spec on its own interval until ctx is done.
//
// An evaluation that overruns the interval is never queued: once Eval
// returns, any boundary that has already passed is skipped rather than run
// late, and onMissed is told how many were skipped. That is the difference
// between a ruler that falls behind loudly and one that falls behind
// silently by building a backlog.
func runGroup(ctx context.Context, clock Clock, spec GroupSpec, onIteration func(), onMissed func(n int)) {
	next := spec.Start
	for {
		wait := next.Sub(clock.Now())

		select {
		case <-ctx.Done():
			return
		case <-clock.After(wait):
		}

		spec.Eval(ctx, next)
		if onIteration != nil {
			onIteration()
		}

		now := clock.Now()
		missed := 0
		next = next.Add(spec.Interval)
		for next.Before(now) {
			next = next.Add(spec.Interval)
			missed++
		}
		if missed > 0 && onMissed != nil {
			onMissed(missed)
		}
	}
}
