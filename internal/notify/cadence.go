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
	Send(ctx context.Context, alerts []alert.Alert) error
}

// DefaultResendInterval is how often a firing alert is re-posted unless an
// operator says otherwise. It decides notification traffic rather than
// correctness, because every firing alert carries its own expiry (validity).
const DefaultResendInterval = 100 * time.Second

// DefaultResendTolerance is how many resend periods a firing alert stays
// valid for unless an operator says otherwise. Four means three consecutive
// failures can pass before Alertmanager expires an alert that is still
// firing.
//
// Four is Prometheus' number. `sendAlerts` in its rules package stamps
// `ValidUntil = ts.Add(4 * delta)` where delta is `max(interval,
// resendDelay)`, under the comment "Allow for two Eval or Alertmanager send
// failures". Note what that covers: Prometheus spends the same budget on a
// failed evaluation as on a failed send, because an evaluation it could not
// run sends nothing either.
//
// It is the default rather than a constant because Prometheus sized it for a
// local PromQL evaluation, and ours is a query to a separate database over a
// network. That failure is both likelier and longer, and only an operator who
// knows their cluster can say how much longer (spec 6.5).
const DefaultResendTolerance = 4

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
	sender    Sender
	interval  time.Duration
	tolerance int

	mu        sync.Mutex
	lastSent  map[uint64]time.Time
	lastSwept time.Time
}

// NewCadence builds a Cadence that re-sends a firing alert every interval and
// asks Alertmanager to hold it for tolerance of those periods.
func NewCadence(sender Sender, interval time.Duration, tolerance int) *Cadence {
	return &Cadence{
		sender:    sender,
		interval:  interval,
		tolerance: tolerance,
		lastSent:  map[uint64]time.Time{},
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
func (c *Cadence) Send(ctx context.Context, now time.Time, evalInterval time.Duration, alerts []alert.Alert) error {
	due := c.dueAt(now, evalInterval, alerts)
	if len(due) == 0 {
		return nil
	}
	if err := c.sender.Send(ctx, due); err != nil {
		return err
	}
	c.record(now, evalInterval, due)
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
			if !c.due(now, a, a.FiredAt) {
				continue
			}
			// The alert is a copy, so this never reaches alert.State.
			a.ValidUntil = now.Add(validity)
		case alert.PhaseResolved:
			// A resolved instance is returned on every evaluation of its
			// retention window (spec 6.5), so it needs the same throttle a
			// firing alert has or it would post on every tick.
			if !c.due(now, a, a.ResolvedAt) {
				continue
			}
		}
		due = append(due, a)
	}
	return due
}

// due decides whether an alert is worth posting now.
//
// since is when this instance entered the phase it is in: when it fired, or
// when it resolved. An alert is due if nothing has been sent for it since then,
// which is what makes a new phase reach Alertmanager immediately, and otherwise
// once the resend interval has elapsed.
//
// Keying only on the fingerprint would be wrong for both edges. A resolve would
// wait behind the firing alert it replaces, and a condition that recovers and
// comes back would page late, because a recurrence reuses the fingerprint of
// the resolve still being retained.
func (c *Cadence) due(now time.Time, a alert.Alert, since time.Time) bool {
	sent, ok := c.lastSent[a.Fingerprint]
	if !ok {
		return true
	}
	if sent.Before(since) {
		return true
	}
	return now.Sub(sent) >= c.interval
}

// validity is how far ahead a firing alert's expiry is set, measured from the
// period it is actually re-sent on: whichever of the cadence and the group's
// interval is longer.
func (c *Cadence) validity(evalInterval time.Duration) time.Duration {
	period := c.interval
	if evalInterval > period {
		period = evalInterval
	}
	return time.Duration(c.tolerance) * period
}

// record marks what was actually delivered. It runs only after a successful
// send, so a failure leaves the alert due again on the very next evaluation
// rather than waiting out a full cadence interval.
//
// A resolved alert is recorded like a firing one. It used to be deleted here,
// on the reasoning that alert.State returned a resolve once and there was
// nothing left to throttle; retention made that false, and the entry now has to
// survive as long as the instance is being re-asserted.
func (c *Cadence) record(now time.Time, evalInterval time.Duration, due []alert.Alert) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, a := range due {
		switch a.Phase {
		case alert.PhaseFiring, alert.PhaseResolved:
			c.lastSent[a.Fingerprint] = now
		}
	}
	c.prune(now, evalInterval)
}

// prune drops entries for instances that cannot be re-asserted again.
//
// alert.State keeps a resolved instance for as long as delivery could still be
// failing, which is the same span this computes as a validity: the tolerance
// times whichever of the cadence and the group interval is longer (spec 6.5).
// Once nothing has been sent for a fingerprint in longer than that, its instance
// is gone from every State and the entry is dead weight. Without this the map
// would hold one entry per alert the process has ever sent.
//
// Swept at most once per interval rather than on every send, because the sweep
// is O(tracked fingerprints) and a send is not.
func (c *Cadence) prune(now time.Time, evalInterval time.Duration) {
	if now.Sub(c.lastSwept) < c.interval {
		return
	}
	c.lastSwept = now

	// One interval of slack, so an entry is never dropped in the same window it
	// could still be re-sent in.
	cutoff := c.validity(evalInterval) + c.interval
	for fp, sent := range c.lastSent {
		if now.Sub(sent) > cutoff {
			delete(c.lastSent, fp)
		}
	}
}

// entries is how many fingerprints the cadence is throttling. Used by tests to
// prove the pruning above actually happens.
func (c *Cadence) entries() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lastSent)
}
