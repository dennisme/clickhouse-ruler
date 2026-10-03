package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
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
	s := &instrumentedSender{inner: &recordingSender{}, alertmanager: am, metrics: metrics}

	if err := s.Send(context.Background(), threeAlerts()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := testutil.ToFloat64(metrics.AlertsSentTotal.WithLabelValues(am))
	if got != 3 {
		t.Errorf("clickhouse_ruler_alerts_sent_total = %v, want 3", got)
	}
}

// A failed batch delivered nothing, so it must not count toward alerts sent.
func TestAlertsSentTotalIgnoresAFailedBatch(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	const am = "http://127.0.0.1:9093"
	s := &instrumentedSender{inner: &recordingSender{err: errors.New("unreachable")},
		alertmanager: am, metrics: metrics}

	if err := s.Send(context.Background(), threeAlerts()); err == nil {
		t.Fatal("want the sender's error back")
	}

	if got := testutil.ToFloat64(metrics.AlertsSentTotal.WithLabelValues(am)); got != 0 {
		t.Errorf("clickhouse_ruler_alerts_sent_total = %v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.AlertsSendFailures.WithLabelValues(am)); got != 1 {
		t.Errorf("clickhouse_ruler_alerts_send_failures_total = %v, want 1", got)
	}
}

// A failure is counted against the endpoint that failed, which is what the
// `alertmanager` label was reserved for: an operator reads which member of the
// cluster stopped accepting, not that delivery failed somewhere (spec 6.5).
func TestSendFailuresAreCountedPerEndpoint(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	const first, second = "http://127.0.0.1:9093", "http://127.0.0.2:9093"

	cadence := NewCadence([]Endpoint{
		{Sender: &recordingSender{err: errors.New("connection refused")}, URL: first},
		{Sender: &recordingSender{err: errors.New("alertmanager returned 503 Service Unavailable")}, URL: second},
	}, testResend, metrics, newFakeClock(time.Unix(0, 0)))

	err := cadence.Send(context.Background(), time.Unix(0, 0), time.Second, threeAlerts())
	if err == nil {
		t.Fatal("Send = nil, want a failure: no endpoint accepted the alerts")
	}
	for _, am := range []string{first, second} {
		if got := testutil.ToFloat64(metrics.AlertsSendFailures.WithLabelValues(am)); got != 1 {
			t.Errorf("clickhouse_ruler_alerts_send_failures_total{alertmanager=%q} = %v, want 1", am, got)
		}
		if !strings.Contains(err.Error(), am) {
			t.Errorf("error %q does not name the endpoint %q that failed", err, am)
		}
	}
	if got := observations(t, metrics); got != 0 {
		t.Errorf("clickhouse_ruler_notification_latency_seconds_count = %v, want 0: nothing was delivered", got)
	}
}

// One endpoint accepting is a delivered send, so the alerts count against that
// endpoint and the one that refused them counts a failure.
func TestAlertsSentCountOnlyTheEndpointThatAccepted(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	const down, up = "http://127.0.0.1:9093", "http://127.0.0.2:9093"

	cadence := NewCadence([]Endpoint{
		{Sender: &recordingSender{err: errors.New("connection refused")}, URL: down},
		{Sender: &recordingSender{}, URL: up},
	}, testResend, metrics, newFakeClock(time.Unix(0, 0)))

	if err := cadence.Send(context.Background(), time.Unix(0, 0), time.Second, threeAlerts()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := testutil.ToFloat64(metrics.AlertsSentTotal.WithLabelValues(up)); got != 3 {
		t.Errorf("clickhouse_ruler_alerts_sent_total{alertmanager=%q} = %v, want 3", up, got)
	}
	if got := testutil.ToFloat64(metrics.AlertsSendFailures.WithLabelValues(down)); got != 1 {
		t.Errorf("clickhouse_ruler_alerts_send_failures_total{alertmanager=%q} = %v, want 1", down, got)
	}
}

// The histogram is a term in the lag budget, which sums its terms to say how
// long after a condition held a page went out. A fan-out is one delivery and a
// page is out once its slowest endpoint has it, so it is one observation
// however many endpoints were posted to (spec 6.5, 8.8).
func TestNotificationLatencyObservesOneFanout(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())

	cadence := NewCadence([]Endpoint{
		{Sender: &recordingSender{}, URL: "http://127.0.0.1:9093"},
		{Sender: &recordingSender{}, URL: "http://127.0.0.2:9093"},
	}, testResend, metrics, newFakeClock(time.Unix(0, 0)))

	if err := cadence.Send(context.Background(), time.Unix(0, 0), time.Second, threeAlerts()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := observations(t, metrics); got != 1 {
		t.Errorf("clickhouse_ruler_notification_latency_seconds_count = %v, want 1 per fan-out", got)
	}
}

// oneEndpoint is the single-Alertmanager wiring every test that is not about
// the fan-out uses.
func oneEndpoint(sender notify.Sender) []Endpoint {
	return []Endpoint{{Sender: sender, URL: "am"}}
}
