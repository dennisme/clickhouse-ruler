package notify

import (
	"context"
	"sync"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

// Sender posts a batch of alerts. Client satisfies it; Cadence exists to
// decide what belongs in the batch before Client ever sees it.
type Sender interface {
	Send(ctx context.Context, alerts []alert.Alert, annotations map[string]string) error
}

// DefaultResendInterval is how often a firing alert is re-posted unless an
// operator says otherwise. It decides notification traffic rather than
// correctness, because every firing alert carries its own expiry (validity).
const DefaultResendInterval = 100 * time.Second

// validityFactor is how many resend periods a firing alert stays valid for.
// Four means three consecutive failed sends can pass before Alertmanager
// expires an alert that is still firing, which is the same margin Prometheus
// gives itself.
const validityFactor = 4

// Cadence throttles how often a firing alert is re-sent, and stamps each one
// with how long Alertmanager should hold it.
//
// Alertmanager expires a firing alert once its endsAt passes unless it hears
// about it again, so a firing alert has to be re-posted before then. Posting
// on every evaluation is a lot of traffic for a rule on a short interval, so
// a firing alert is only due again once its own interval has elapsed since it
// was last actually sent (spec 6.5).
//
// The expiry is the ruler's to set, not Alertmanager's. An alert posted with
// no endsAt falls back to Alertmanager's resolve_timeout, which lives in a
// config the ruler cannot read and an operator is free to change: too short
// and a firing alert expires between resends, producing a resolved
// notification for something still broken and a re-fire behind it. Sending an
// explicit validity removes the coupling, so the resend interval below sizes
// traffic and nothing else.
// One Cadence is shared by every rule in every group, and the scheduler runs
// each group in its own goroutine, so two groups whose ticks overlap call
// Send at the same time. lastSent is therefore guarded: an unsynchronised map
// here is not a subtle race but a fatal "concurrent map writes" abort of the
// whole ruler, which is the worst possible failure for a process whose job is
// to page people.
type Cadence struct {
	sender   Sender
	interval time.Duration

	mu       sync.Mutex
	lastSent map[uint64]time.Time
}

// NewCadence builds a Cadence that re-sends a firing alert every interval.
func NewCadence(sender Sender, interval time.Duration) *Cadence {
	return &Cadence{
		sender:   sender,
		interval: interval,
		lastSent: map[uint64]time.Time{},
	}
}

// Send posts the alerts due at now: every resolved alert, and every firing
// alert not sent within the last cadence interval. Pending alerts are left
// for Payload to drop.
//
// evalInterval is the interval of the group the alerts came from. It bounds
// the validity from below, because Cadence cannot re-send between two
// evaluations it is never called on: a group ticking slower than the cadence
// is what actually paces the resend, and a validity sized off the cadence
// alone would expire in the gap.
//
// A send failure is not recorded, so the alert is due again on the very next
// evaluation instead of waiting out a full cadence interval. That is what
// keeps a notification outage from also losing the alert once Alertmanager
// comes back.
// The lock is deliberately not held across the POST. Holding it there would
// serialise every group's notifications behind one slow Alertmanager, which
// is the opposite of what running groups concurrently is for. The cost is a
// window where two goroutines could both judge the same fingerprint due and
// post it twice; that is harmless, because Alertmanager deduplicates
// identical alerts (spec 6.5, which relies on the same property to make two
// ruler replicas safe), and in practice unreachable, because a fingerprint
// belongs to one rule and a rule is evaluated by one goroutine at a time.
func (c *Cadence) Send(ctx context.Context, now time.Time, evalInterval time.Duration, alerts []alert.Alert, annotations map[string]string) error {
	due := c.dueAt(now, evalInterval, alerts)
	if len(due) == 0 {
		return nil
	}
	if err := c.sender.Send(ctx, due, annotations); err != nil {
		return err
	}
	c.record(now, due)
	return nil
}

// dueAt selects the alerts worth posting at now and stamps each firing one
// with how long Alertmanager should hold it.
func (c *Cadence) dueAt(now time.Time, evalInterval time.Duration, alerts []alert.Alert) []alert.Alert {
	c.mu.Lock()
	defer c.mu.Unlock()

	validity := c.validity(evalInterval)

	due := make([]alert.Alert, 0, len(alerts))
	for _, a := range alerts {
		switch a.Phase {
		case alert.PhasePending:
			// A `for` duration exists so a condition that has not held long
			// enough does not page; pending is never due.
			continue
		case alert.PhaseFiring:
			if sent, ok := c.lastSent[a.Fingerprint]; ok && now.Sub(sent) < c.interval {
				continue
			}
			// The alert is a copy, so this never reaches alert.State.
			a.ValidUntil = now.Add(validity)
		}
		due = append(due, a)
	}
	return due
}

// validity is how far ahead a firing alert's expiry is set, measured from the
// period it is actually re-sent on: whichever of the cadence and the group's
// interval is longer.
func (c *Cadence) validity(evalInterval time.Duration) time.Duration {
	period := c.interval
	if evalInterval > period {
		period = evalInterval
	}
	return validityFactor * period
}

// record marks what was actually delivered. It runs only after a successful
// send, so a failure leaves the alert due again on the very next evaluation
// rather than waiting out a full cadence interval.
func (c *Cadence) record(now time.Time, due []alert.Alert) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, a := range due {
		switch a.Phase {
		case alert.PhaseFiring:
			c.lastSent[a.Fingerprint] = now
		case alert.PhaseResolved:
			// Eval only ever returns a resolved instance once, so there is
			// nothing left to throttle and holding the entry would leak.
			delete(c.lastSent, a.Fingerprint)
		}
	}
}
