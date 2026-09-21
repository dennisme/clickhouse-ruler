package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRunGroupTicksAtStartImmediately(t *testing.T) {
	start := time.Unix(0, 0)
	clock := newFakeClock(start)

	var mu sync.Mutex
	var got []time.Time
	done := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spec := GroupSpec{
		Name:     "g",
		Interval: time.Minute,
		Start:    start,
		Eval: func(_ context.Context, tickAt time.Time) {
			mu.Lock()
			got = append(got, tickAt)
			n := len(got)
			mu.Unlock()
			if n == 1 {
				close(done)
			}
		},
	}

	go runGroup(ctx, clock, spec, nil, nil)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || !got[0].Equal(start) {
		t.Fatalf("got %v, want one tick at %v", got, start)
	}
}

// The heart of the missed-tick requirement: an evaluation that overruns its
// interval must not queue the ticks it missed, only skip them and count them.
func TestRunGroupSkipsAndCountsAMissedTickInsteadOfQueueingIt(t *testing.T) {
	start := time.Unix(0, 0)
	interval := time.Minute
	clock := newFakeClock(start)

	started := make(chan time.Time, 8)
	proceed := make(chan struct{})

	var mu sync.Mutex
	var missed []int

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spec := GroupSpec{
		Name:     "g",
		Interval: interval,
		Start:    start,
		Eval: func(_ context.Context, tickAt time.Time) {
			started <- tickAt
			<-proceed
		},
	}

	go runGroup(ctx, clock, spec, nil, func(n int) {
		mu.Lock()
		missed = append(missed, n)
		mu.Unlock()
	})

	first := <-started
	if !first.Equal(start) {
		t.Fatalf("first tick = %v, want %v", first, start)
	}

	// Three intervals pass while the first evaluation is still running.
	clock.Advance(3 * interval)
	close(proceed) // let every evaluation from here on return immediately

	second := <-started
	want := start.Add(3 * interval)
	if !second.Equal(want) {
		t.Fatalf("second tick = %v, want %v (the boundary in progress when the overrun ended)", second, want)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(missed) != 1 || missed[0] != 2 {
		t.Fatalf("missed = %v, want a single report of 2 (the two boundaries skipped mid-evaluation)", missed)
	}
}
