package scheduler

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics are the scheduler's own operational signals (spec 8.2).
//
// Everything here is labelled by rule_group and rule only, never by alert
// instance: a rule returning ten thousand rows must still produce exactly
// one metric series per rule, or the ruler becomes the cardinality problem
// it exists to fix (spec 8.3).
//
// ClickHouse query cost (clickhouse_ruler_query_read_rows_total and friends) and the
// watch mode metrics (clickhouse_ruler_problem, clickhouse_ruler_config_last_reload_*) are not
// here: the former needs a driver progress callback inside internal/query,
// and the latter belongs to `ruler watch`, which does not exist yet. Both
// are out of scope for this slice.
type Metrics struct {
	EvaluationsTotal        *prometheus.CounterVec
	EvaluationFailuresTotal *prometheus.CounterVec
	EvaluationDuration      *prometheus.HistogramVec

	IterationsTotal         *prometheus.CounterVec
	IterationsMissedTotal   *prometheus.CounterVec
	LastEvaluationTimestamp *prometheus.GaugeVec
	LastDuration            *prometheus.GaugeVec

	AnnotationFailures *prometheus.CounterVec

	AlertsActive        *prometheus.GaugeVec
	AlertsSentTotal     *prometheus.CounterVec
	AlertsSendFailures  *prometheus.CounterVec
	NotificationLatency prometheus.Histogram

	RulesUnmatched *prometheus.GaugeVec
}

// NewMetrics registers every scheduler metric against reg. A nil reg uses
// the default registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)

	return &Metrics{
		EvaluationsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "clickhouse_ruler_rule_evaluations_total",
			Help: "Total number of rule evaluations.",
		}, []string{"rule_group", "rule"}),

		EvaluationFailuresTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "clickhouse_ruler_rule_evaluation_failures_total",
			Help: "Total number of rule evaluations that failed against a source.",
		}, []string{"rule_group", "rule"}),

		// Deliberately not a label on clickhouse_ruler_rule_evaluation_failures_total.
		// That name tracks Prometheus' own, which counts evaluations that did
		// not happen, and an evaluation that delivered alerts carrying one
		// error string is not one of those: folding it in would make a
		// dashboard carried over from a Prometheus ruler read high (spec 8.2).
		//
		// Labelled by annotation because that names what to fix, and an
		// annotation is static configuration, two or three per rule, rather
		// than anything data can multiply (spec 8.3).
		AnnotationFailures: f.NewCounterVec(prometheus.CounterOpts{
			Name: "clickhouse_ruler_annotation_failures_total",
			Help: "Total number of evaluations where an annotation template would not render, by annotation.",
		}, []string{"rule_group", "rule", "annotation"}),

		EvaluationDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "clickhouse_ruler_rule_evaluation_duration_seconds",
			Help: "Time spent evaluating one rule group.",
		}, []string{"rule_group"}),

		IterationsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "clickhouse_ruler_rule_group_iterations_total",
			Help: "Total number of rule group evaluation iterations.",
		}, []string{"rule_group"}),

		// The single most important operational signal: a missed iteration
		// means alerts are silently late.
		IterationsMissedTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "clickhouse_ruler_rule_group_iterations_missed_total",
			Help: "Total number of iterations skipped because the previous one overran the group interval.",
		}, []string{"rule_group"}),

		LastEvaluationTimestamp: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "clickhouse_ruler_rule_group_last_evaluation_timestamp_seconds",
			Help: "Unix timestamp of the last rule group evaluation.",
		}, []string{"rule_group"}),

		LastDuration: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "clickhouse_ruler_rule_group_last_duration_seconds",
			Help: "Duration of the last rule group evaluation.",
		}, []string{"rule_group"}),

		// Carries the group as well as the rule because an alert name may
		// legitimately repeat across groups (spec 7.6), and without it two
		// same-named rules would report into one series. Still a count per
		// rule, never a series per alert instance (spec 8.3).
		AlertsActive: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "clickhouse_ruler_alerts_active",
			Help: "Number of alert instances currently tracked, by state.",
		}, []string{"rule_group", "rule", "state"}),

		// Counts alerts rather than batches, matching the Prometheus metric
		// this name tracks (spec 8.2). A batch count would make the number
		// unusable for notification volume and would read wrong on a
		// dashboard carried over from a Prometheus ruler.
		AlertsSentTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "clickhouse_ruler_alerts_sent_total",
			Help: "Total number of alerts sent to Alertmanager.",
		}, []string{"alertmanager"}),

		AlertsSendFailures: f.NewCounterVec(prometheus.CounterOpts{
			Name: "clickhouse_ruler_alerts_send_failures_total",
			Help: "Total number of alert batches that failed to send to Alertmanager.",
		}, []string{"alertmanager"}),

		NotificationLatency: f.NewHistogram(prometheus.HistogramOpts{
			Name: "clickhouse_ruler_notification_latency_seconds",
			Help: "Time spent sending an alert batch to Alertmanager.",
		}),

		RulesUnmatched: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "clickhouse_ruler_rules_unmatched",
			Help: "Number of loaded rules that matched no source, so this ruler will never evaluate them.",
		}, []string{"rule_group"}),
	}
}
