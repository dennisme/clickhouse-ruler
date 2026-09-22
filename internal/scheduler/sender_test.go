package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

func threeAlerts() []alert.Alert {
	return []alert.Alert{
		{Fingerprint: 1, Phase: alert.PhaseFiring},
		{Fingerprint: 2, Phase: alert.PhaseFiring},
		{Fingerprint: 3, Phase: alert.PhaseFiring},
	}
}

// The Prometheus metric this name tracks (spec 8.2) counts alerts, so a
// dashboard carried over from a Prometheus ruler reads a batch count as a
// notification volume and is wrong by the batch size.
func TestAlertsSentTotalCountsAlertsNotBatches(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	const am = "http://127.0.0.1:9093"
	s := &instrumentedSender{inner: &recordingSender{}, alertmanager: am,
		metrics: metrics, clock: newFakeClock(time.Unix(0, 0))}

	if err := s.Send(context.Background(), threeAlerts()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := testutil.ToFloat64(metrics.AlertsSentTotal.WithLabelValues(am))
	if got != 3 {
		t.Errorf("ruler_alerts_sent_total = %v, want 3", got)
	}
}

// A failed batch delivered nothing, so it must not count toward alerts sent.
func TestAlertsSentTotalIgnoresAFailedBatch(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	const am = "http://127.0.0.1:9093"
	s := &instrumentedSender{inner: &recordingSender{err: errors.New("unreachable")},
		alertmanager: am, metrics: metrics, clock: newFakeClock(time.Unix(0, 0))}

	if err := s.Send(context.Background(), threeAlerts()); err == nil {
		t.Fatal("want the sender's error back")
	}

	if got := testutil.ToFloat64(metrics.AlertsSentTotal.WithLabelValues(am)); got != 0 {
		t.Errorf("ruler_alerts_sent_total = %v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.AlertsSendFailures.WithLabelValues(am)); got != 1 {
		t.Errorf("ruler_alerts_send_failures_total = %v, want 1", got)
	}
}
