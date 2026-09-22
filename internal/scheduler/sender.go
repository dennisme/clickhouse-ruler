package scheduler

import (
	"context"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
)

// instrumentedSender wraps a notify.Sender and records the delivery metrics
// in spec 8.2: how many alerts were sent, how many batches failed, and how
// long each send took.
type instrumentedSender struct {
	inner        notify.Sender
	alertmanager string
	metrics      *Metrics
	clock        Clock
}

func (s *instrumentedSender) Send(ctx context.Context, alerts []alert.Alert) error {
	start := s.clock.Now()
	err := s.inner.Send(ctx, alerts)
	s.metrics.NotificationLatency.Observe(s.clock.Now().Sub(start).Seconds())

	if err != nil {
		s.metrics.AlertsSendFailures.WithLabelValues(s.alertmanager).Inc()
		return err
	}
	s.metrics.AlertsSentTotal.WithLabelValues(s.alertmanager).Add(float64(len(alerts)))
	return nil
}

// Resend is how often a firing alert is re-posted and how many of those periods
// it stays valid for. The two always travel together, because neither answers
// anything on its own: the interval sizes notification traffic and the tolerance
// decides how much failure that traffic survives.
type Resend struct {
	Interval  time.Duration
	Tolerance int
}

// retention is how long a resolved instance is kept so its notification can be
// retried, for a rule in a group ticking on groupInterval.
//
// It is the same span notify.Cadence stamps as a validity, and deliberately so:
// both answer "how long can delivery still be in progress". A resolve dropped
// before that window closes is a resolve nobody retried; one kept after it is
// memory held for an alert nothing will re-send (spec 6.5).
func (r Resend) retention(groupInterval time.Duration) time.Duration {
	period := r.Interval
	if groupInterval > period {
		period = groupInterval
	}
	return time.Duration(r.Tolerance) * period
}

// NewCadence builds the notify.Cadence a Scheduler sends through, wrapping
// sender with the instrumentation spec 8.2 asks for.
func NewCadence(sender notify.Sender, alertmanager string, resend Resend, metrics *Metrics, clock Clock) *notify.Cadence {
	instrumented := &instrumentedSender{inner: sender, alertmanager: alertmanager, metrics: metrics, clock: clock}
	return notify.NewCadence(instrumented, resend.Interval, resend.Tolerance)
}
