package scheduler

import "time"

// Clock is time as the scheduler sees it, so a test can drive ticks without
// sleeping for an interval.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// realClock is time.Now and time.After.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewRealClock returns the Clock a production scheduler runs on.
func NewRealClock() Clock { return realClock{} }
