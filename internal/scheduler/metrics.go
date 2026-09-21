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
// ClickHouse query cost (ruler_query_read_rows_total and friends) and the
// watch mode metrics (ruler_problem, ruler_config_last_reload_*) are not
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
			Name: "ruler_rule_evaluations_total",
			Help: "Total number of rule evaluations.",
		}, []string{"rule_group", "rule"}),

		EvaluationFailuresTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "ruler_rule_evaluation_failures_total",
			Help: "Total number of rule evaluations that failed against a source.",
		}, []string{"rule_group", "rule"}),

		EvaluationDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "ruler_rule_evaluation_duration_seconds",
			Help: "Time spent evaluating one rule group.",
		}, []string{"rule_group"}),

		IterationsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "ruler_rule_group_iterations_total",
			Help: "Total number of rule group evaluation iterations.",
		}, []string{"rule_group"}),

		// The single most important operational signal: a missed iteration
		// means alerts are silently late.
		IterationsMissedTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "ruler_rule_group_iterations_missed_total",
			Help: "Total number of iterations skipped because the previous one overran the group interval.",
		}, []string{"rule_group"}),

		LastEvaluationTimestamp: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ruler_rule_group_last_evaluation_timestamp_seconds",
			Help: "Unix timestamp of the last rule group evaluation.",
		}, []string{"rule_group"}),

		LastDuration: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ruler_rule_group_last_duration_seconds",
			Help: "Duration of the last rule group evaluation.",
		}, []string{"rule_group"}),

		AlertsActive: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ruler_alerts_active",
			Help: "Number of alert instances currently tracked, by state.",
		}, []string{"rule", "state"}),

		AlertsSentTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "ruler_alerts_sent_total",
			Help: "Total number of alert batches sent to Alertmanager.",
		}, []string{"alertmanager"}),

		AlertsSendFailures: f.NewCounterVec(prometheus.CounterOpts{
			Name: "ruler_alerts_send_failures_total",
			Help: "Total number of alert batches that failed to send to Alertmanager.",
		}, []string{"alertmanager"}),

		NotificationLatency: f.NewHistogram(prometheus.HistogramOpts{
			Name: "ruler_notification_latency_seconds",
			Help: "Time spent sending an alert batch to Alertmanager.",
		}),

		RulesUnmatched: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ruler_rules_unmatched",
			Help: "Number of loaded rules that matched no source, so this ruler will never evaluate them.",
		}, []string{"rule_group"}),
	}
}
