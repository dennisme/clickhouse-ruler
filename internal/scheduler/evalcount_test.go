package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// fourSources is a rule spanning an estate, which is what the per-rule
// counting got wrong: each cluster is its own evaluation with its own alert
// state and its own failure mode (spec 6.10.1).
func fourSources() []source.Source {
	return []source.Source{
		{Name: "src1"}, {Name: "src2"}, {Name: "src3"}, {Name: "src4"},
	}
}

func evalOnce(t *testing.T, queriers map[string]Querier, srcs ...source.Source) *Metrics {
	t.Helper()

	metrics := NewMetrics(prometheus.NewRegistry())
	sched := New(oneRuleSet("Spanning", srcs...), queriers,
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		metrics, newFakeClock(time.Unix(0, 0)), 0, slog.New(slog.DiscardHandler), testResend, 0)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))
	return metrics
}

func counters(t *testing.T, m *Metrics) (evaluations, failures float64) {
	t.Helper()
	return testutil.ToFloat64(m.EvaluationsTotal.WithLabelValues("f.yaml:g1", "Spanning")),
		testutil.ToFloat64(m.EvaluationFailuresTotal.WithLabelValues("f.yaml:g1", "Spanning"))
}

// The ratio this repository ships in docs/operations.md is failures over
// evaluations, read as the share of evaluations that did not happen. One
// evaluation against four failing clusters recorded four failures, so the
// ratio read 4.0 and the alert on it said something impossible (spec 8.2).
func TestAFailingEstateCannotPushTheRatioAboveOne(t *testing.T) {
	queriers := map[string]Querier{}
	for _, src := range fourSources() {
		queriers[src.Name] = &fakeQuerier{err: errors.New("connection refused")}
	}

	evaluations, failures := counters(t, evalOnce(t, queriers, fourSources()...))

	if failures > evaluations {
		t.Errorf("failures = %v against evaluations = %v: the shipped ratio reads %v, which cannot be a share",
			failures, evaluations, failures/evaluations)
	}
	if evaluations != 4 {
		t.Errorf("evaluations = %v, want 4: one per cluster the rule was evaluated against", evaluations)
	}
}

// One bad cluster out of four is a quarter of this rule's evaluations failing,
// and the expression has to read that way or the threshold means nothing.
func TestOneBadClusterIsItsShareOfTheRatio(t *testing.T) {
	queriers := map[string]Querier{
		"src1": &fakeQuerier{err: errors.New("connection refused")},
		"src2": &fakeQuerier{},
		"src3": &fakeQuerier{},
		"src4": &fakeQuerier{},
	}

	evaluations, failures := counters(t, evalOnce(t, queriers, fourSources()...))

	if got := failures / evaluations; got != 0.25 {
		t.Errorf("ratio = %v, want 0.25: one of four clusters failed", got)
	}
}

// The single source case is the common one and must read exactly as it did
// before, which is also what it reads on a Prometheus ruler.
func TestASingleSourceRuleCountsOneEvaluationPerTick(t *testing.T) {
	m := evalOnce(t, map[string]Querier{"src1": &fakeQuerier{}}, source.Source{Name: "src1"})

	if evaluations, failures := counters(t, m); evaluations != 1 || failures != 0 {
		t.Errorf("evaluations = %v, failures = %v, want 1 and 0", evaluations, failures)
	}
}

// A cluster with no open connection never ran the query, which is a failed
// evaluation of that cluster rather than a rule that was not evaluated.
func TestASourceWithNoQuerierIsAFailedEvaluationOfThatSource(t *testing.T) {
	m := evalOnce(t, map[string]Querier{"src1": &fakeQuerier{}},
		source.Source{Name: "src1"}, source.Source{Name: "gone"})

	evaluations, failures := counters(t, m)
	if evaluations != 2 {
		t.Errorf("evaluations = %v, want 2: both clusters were attempted", evaluations)
	}
	if failures != 1 {
		t.Errorf("failures = %v, want 1: only the cluster with no connection failed", failures)
	}
}

// The latency histogram is read as how long Alertmanager takes to accept a
// batch. A failed send measures the retry ladder giving up, which is the
// delivery outage the send-failure counter already reports, and folding it in
// makes the latency alert fire for a cause its own text denies (spec 8.2).
func TestNotificationLatencyExcludesAFailedSend(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	const am = "http://127.0.0.1:9093"
	s := &instrumentedSender{inner: &recordingSender{err: errors.New("unreachable")},
		alertmanager: am, metrics: metrics, clock: newFakeClock(time.Unix(0, 0))}

	if err := s.Send(context.Background(), threeAlerts()); err == nil {
		t.Fatal("want the sender's error back")
	}

	if got := observations(t, metrics); got != 0 {
		t.Errorf("clickhouse_ruler_notification_latency_seconds_count = %v, want 0 after a failed send", got)
	}
}

// A send that worked is the population the threshold is read against.
func TestNotificationLatencyCountsASuccessfulSend(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	const am = "http://127.0.0.1:9093"
	s := &instrumentedSender{inner: &recordingSender{}, alertmanager: am,
		metrics: metrics, clock: newFakeClock(time.Unix(0, 0))}

	if err := s.Send(context.Background(), []alert.Alert{{Fingerprint: 1}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := observations(t, metrics); got != 1 {
		t.Errorf("clickhouse_ruler_notification_latency_seconds_count = %v, want 1", got)
	}
}

// observations is how many sends the latency histogram holds, which is the
// count a dashboard reads as the batch count beside it (spec 8.2).
func observations(t *testing.T, m *Metrics) uint64 {
	t.Helper()

	var metric dto.Metric
	if err := m.NotificationLatency.(prometheus.Metric).Write(&metric); err != nil {
		t.Fatalf("reading the histogram: %v", err)
	}
	return metric.GetHistogram().GetSampleCount()
}
