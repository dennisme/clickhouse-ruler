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

func (s *instrumentedSender) Send(ctx context.Context, alerts []alert.Alert, annotations map[string]string) error {
	start := s.clock.Now()
	err := s.inner.Send(ctx, alerts, annotations)
	s.metrics.NotificationLatency.Observe(s.clock.Now().Sub(start).Seconds())

	if err != nil {
		s.metrics.AlertsSendFailures.WithLabelValues(s.alertmanager).Inc()
		return err
	}
	s.metrics.AlertsSentTotal.WithLabelValues(s.alertmanager).Add(float64(len(alerts)))
	return nil
}

// NewCadence builds the notify.Cadence a Scheduler sends through, wrapping
// sender with the instrumentation spec 8.2 asks for.
func NewCadence(sender notify.Sender, alertmanager string, interval time.Duration, metrics *Metrics, clock Clock) *notify.Cadence {
	instrumented := &instrumentedSender{inner: sender, alertmanager: alertmanager, metrics: metrics, clock: clock}
	return notify.NewCadence(instrumented, interval)
}
