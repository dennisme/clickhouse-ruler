package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
)

// instrumentedSender wraps the notify.Sender for one endpoint and records what
// spec 8.2 counts per endpoint: how many alerts that Alertmanager took, and how
// many batches it refused.
//
// One of these per endpoint is what gives the `alertmanager` label more than one
// series, and it is also what puts the endpoint in the error, so the one line a
// failed fan-out writes names which members refused it (spec 6.5).
type instrumentedSender struct {
	inner        notify.Sender
	alertmanager string
	metrics      *Metrics
}

func (s *instrumentedSender) Send(ctx context.Context, alerts []alert.Alert) error {
	err := s.inner.Send(ctx, alerts)

	if err != nil {
		s.metrics.AlertsSendFailures.WithLabelValues(s.alertmanager).Inc()
		return fmt.Errorf("%s: %w", s.alertmanager, err)
	}

	s.metrics.AlertsSentTotal.WithLabelValues(s.alertmanager).Add(float64(len(alerts)))
	return nil
}

// timedSender observes how long a delivery took, measured across the fan-out to
// every endpoint rather than per endpoint.
//
// Observed only for a delivery that worked. A failed one measures the retry
// ladder giving up, which is a duration the retry policy decides rather than one
// Alertmanager produced, so folding it in makes the latency alert fire for a
// delivery outage the failure counter already reports, and the latency panel's
// own reading says that cause is Alertmanager being slow (spec 8.2). Batches
// attempted stays available as this histogram's count plus the failure counter.
//
// One observation per fan-out and no `alertmanager` label, because this is a
// term in the lag budget in 8.8: that budget sums its terms to say how long
// after a condition held a page went out, and a page is out once the slowest
// endpoint it was posted to has it (spec 6.5).
type timedSender struct {
	inner   notify.Sender
	metrics *Metrics
	clock   Clock
}

func (s *timedSender) Send(ctx context.Context, alerts []alert.Alert) error {
	start := s.clock.Now()
	if err := s.inner.Send(ctx, alerts); err != nil {
		return err
	}

	s.metrics.NotificationLatency.Observe(s.clock.Now().Sub(start).Seconds())
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

// Endpoint is one Alertmanager alerts are posted to: what posts them, and the
// redacted spelling that labels its series, because a URL may carry userinfo
// and a label is scraped and put on a dashboard (spec 8.4).
type Endpoint struct {
	Sender notify.Sender
	URL    string
}

// NewCadence builds the notify.Cadence a Scheduler sends through, posting to
// every endpoint and recording what spec 8.2 asks for.
//
// The fan-out goes under the Cadence, which is the only seam it fits: one queue,
// one worker, one Cadence throttling what is due, and a Sender that posts what
// is due to every endpoint (spec 6.5).
func NewCadence(endpoints []Endpoint, resend Resend, metrics *Metrics, clock Clock) *notify.Cadence {
	senders := make([]notify.Sender, 0, len(endpoints))
	for _, e := range endpoints {
		senders = append(senders, &instrumentedSender{inner: e.Sender, alertmanager: e.URL, metrics: metrics})
	}

	fanout := notify.NewFanout(senders...)
	timed := &timedSender{inner: fanout, metrics: metrics, clock: clock}
	return notify.NewCadence(timed, resend.Interval, resend.Tolerance)
}
