package scheduler

import (
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
	Handler(reg).ServeHTTP(rec, req)

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

// Spec 8.1 fixes the surface at three endpoints. Health and readiness exist so
// a supervisor can tell a process that is up from one that is ready.
func TestHealthEndpoints(t *testing.T) {
	handler := Handler(prometheus.NewRegistry())

	for _, path := range []string{"/-/healthy", "/-/ready"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		if got := rec.Body.String(); got != "ok" {
			t.Errorf("GET %s body = %q, want %q", path, got, "ok")
		}
	}
}
