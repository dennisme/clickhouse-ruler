package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func serveMetrics(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	Handler(reg, func(context.Context) error { return nil }).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// The exposed names are a public interface: an operator's dashboards and alert
// expressions are written against them, so a prefix nobody meant to change is
// a silent break. Every name this ruler exposes is namespaced to the ruler
// itself, because `ruler` on its own is a component name that Mimir and Loki
// also use, and a series called ruler_alerts_sent_total in a shared Prometheus
// does not say whose (spec 8.2).
func TestMetricsAreNamespacedToThisRuler(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	// Counters and gauges are only exposed once they carry a value, so every
	// family below is touched first.
	m.EvaluationsTotal.WithLabelValues("f.yaml:g1", "R").Inc()
	m.EvaluationFailuresTotal.WithLabelValues("f.yaml:g1", "R").Inc()
	m.AnnotationFailures.WithLabelValues("f.yaml:g1", "R", "summary").Inc()
	m.EvaluationDuration.WithLabelValues("f.yaml:g1").Observe(1)
	m.IterationsTotal.WithLabelValues("f.yaml:g1").Inc()
	m.IterationsMissedTotal.WithLabelValues("f.yaml:g1").Inc()
	m.LastEvaluationTimestamp.WithLabelValues("f.yaml:g1").Set(1)
	m.LastDuration.WithLabelValues("f.yaml:g1").Set(1)
	m.AlertsActive.WithLabelValues("f.yaml:g1", "R", "firing").Set(1)
	m.AlertsSentTotal.WithLabelValues("am").Inc()
	m.AlertsSendFailures.WithLabelValues("am").Inc()
	m.NotificationLatency.Observe(1)
	m.RulesUnmatched.WithLabelValues("f.yaml:g1").Set(0)

	body := serveMetrics(t, reg)

	for _, name := range []string{
		"clickhouse_ruler_rule_evaluations_total",
		"clickhouse_ruler_rule_evaluation_failures_total",
		"clickhouse_ruler_annotation_failures_total",
		"clickhouse_ruler_rule_evaluation_duration_seconds",
		"clickhouse_ruler_rule_group_iterations_total",
		"clickhouse_ruler_rule_group_iterations_missed_total",
		"clickhouse_ruler_rule_group_last_evaluation_timestamp_seconds",
		"clickhouse_ruler_rule_group_last_duration_seconds",
		"clickhouse_ruler_alerts_active",
		"clickhouse_ruler_alerts_sent_total",
		"clickhouse_ruler_alerts_send_failures_total",
		"clickhouse_ruler_notification_latency_seconds",
		"clickhouse_ruler_rules_unmatched",
	} {
		if !strings.Contains(body, name+"{") && !strings.Contains(body, name+" ") {
			t.Errorf("%s is not exposed on /metrics", name)
		}
	}

	// Nothing may go out under the bare prefix, which is what a half-finished
	// rename would leave behind.
	if bare := regexp.MustCompile(`(?m)^ruler_`); bare.MatchString(body) {
		t.Error("a metric is exposed as ruler_*, want clickhouse_ruler_*")
	}
}

func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// Health is about the process: it is alive and its listener is serving, which
// is what an unconditional 200 says. A failed health check gets a process
// restarted, and a ruler that cannot reach a cluster is not a process a
// restart fixes (spec 8.1).
func TestHealthIsUnconditional(t *testing.T) {
	handler := Handler(prometheus.NewRegistry(), func(context.Context) error {
		return errors.New("no source answering")
	})

	rec := get(t, handler, "/-/healthy")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /-/healthy = %d, want 200 even when the ruler is not ready", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("GET /-/healthy body = %q, want %q", got, "ok")
	}
}

// Readiness is about whether sending traffic here is useful, and it has to be
// able to say no. A probe that cannot fail turns a rollout of a ruler that
// reaches no cluster into a successful one (spec 8.1).
func TestReadinessFailsWhenTheRulerCannotDoItsJob(t *testing.T) {
	handler := Handler(prometheus.NewRegistry(), func(context.Context) error {
		return errors.New("no source answering")
	})

	rec := get(t, handler, "/-/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /-/ready = %d, want 503", rec.Code)
	}
	// The reason, because a probe that says only "not ready" sends whoever
	// is rolling out to the logs to find out what the ruler already knows.
	if got := rec.Body.String(); !strings.Contains(got, "no source answering") {
		t.Errorf("GET /-/ready body = %q, want the reason in it", got)
	}
}

func TestReadinessPassesWhenTheRulerCanEvaluate(t *testing.T) {
	handler := Handler(prometheus.NewRegistry(), func(context.Context) error { return nil })

	rec := get(t, handler, "/-/ready")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /-/ready = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("GET /-/ready body = %q, want %q", got, "ok")
	}
}
