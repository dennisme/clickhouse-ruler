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
// clickhouse_ruler_problem is not here: it re-validates loaded rules on a
// timer, which belongs to `ruler watch` and does not exist yet. The config
// reload pair below does, because SIGHUP reloads the files this ruler is
// running (spec 8.2).
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

	ConfigLastReloadSuccessful prometheus.Gauge
	ConfigLastReloadTimestamp  prometheus.Gauge

	QueryReadRowsTotal  *prometheus.CounterVec
	QueryReadBytesTotal *prometheus.CounterVec
	QueryMemoryUsage    *prometheus.HistogramVec
	QueryDuration       *prometheus.HistogramVec
	QueryQueueWait      *prometheus.HistogramVec
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

		// What the two reload gauges say, and deliberately not the same thing.
		//
		// ConfigLastReloadSuccessful is about the last attempt: a reload the
		// ruler refused sets it to 0 and it stays there until one succeeds.
		// That is the alert an operator wants, because a refused reload is
		// silent otherwise. The files on disk say one thing, the ruler is
		// running another, and nothing about the rules that are evaluating
		// looks wrong.
		//
		// ConfigLastReloadTimestamp is about the running configuration, so a
		// refused reload leaves it alone. The query it exists for is
		// `time() - clickhouse_ruler_config_last_reload_timestamp_seconds`,
		// read as "how old is what this ruler is evaluating". Stamping it on a
		// refusal would answer that question with the moment the ruler declined
		// to change anything, which claims the running rules are current when
		// they are precisely not (spec 7.6).
		ConfigLastReloadSuccessful: f.NewGauge(prometheus.GaugeOpts{
			Name: "clickhouse_ruler_config_last_reload_successful",
			Help: "Whether the last attempt to load the rules, sources and policy files succeeded.",
		}),

		ConfigLastReloadTimestamp: f.NewGauge(prometheus.GaugeOpts{
			Name: "clickhouse_ruler_config_last_reload_timestamp_seconds",
			Help: "Unix timestamp of the load that produced the configuration this ruler is evaluating.",
		}),

		// What a rule costs the cluster, read from the driver's callbacks
		// during the query rather than from system.query_log afterwards
		// (spec 8.2). These are what make the caps in 6.7 observable rather
		// than theoretical, and what a team's share of a cluster is billed
		// from.
		//
		// `team` comes from the rule's effective labels and is empty when
		// the author set none, which is a rule nobody has claimed rather
		// than a rule owned by the empty string. Left visible rather than
		// defaulted: the chargeback these exist for needs to show what is
		// unattributed.
		QueryReadRowsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "clickhouse_ruler_query_read_rows_total",
			Help: "Total rows ClickHouse read evaluating a rule. Empty team means the rule carries no team label.",
		}, []string{"rule", "team"}),

		QueryReadBytesTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "clickhouse_ruler_query_read_bytes_total",
			Help: "Total bytes ClickHouse read evaluating a rule. Empty team means the rule carries no team label.",
		}, []string{"rule", "team"}),

		// Bucketed in powers of eight from a megabyte, because the cap this
		// is read against is measured in gigabytes and a linear scale over
		// that range says nothing about the rules below it.
		QueryMemoryUsage: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "clickhouse_ruler_query_memory_usage_bytes",
			Help:    "Peak memory one evaluation's query reached on the server.",
			Buckets: prometheus.ExponentialBuckets(1<<20, 8, 6),
		}, []string{"rule"}),

		QueryDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "clickhouse_ruler_query_duration_seconds",
			Help:    "Time one evaluation's query took, measured by the ruler from sending it to the last row arriving.",
			Buckets: prometheus.DefBuckets,
		}, []string{"rule"}),

		// How long queries wait for a slot against the source's own
		// concurrency limit (spec 6.11), which is what says a limit is set
		// too low: without it the knob cannot be sized and an operator is
		// guessing. Labelled by source rather than by rule or team like the
		// cost metrics above, because queueing is a property of the cluster
		// the limit protects: every rule against a saturated source waits,
		// and which rule happened to wait says nothing about what to change.
		// Sources with no limit of their own never reach this, so a series
		// here means a limit exists and is being hit (spec 8.3).
		QueryQueueWait: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "clickhouse_ruler_query_queue_wait_seconds",
			Help:    "Time a query waited for a slot against its source's concurrent query limit.",
			Buckets: prometheus.DefBuckets,
		}, []string{"source"}),
	}
}

// deleteGroup removes every series a group left behind, which is every metric
// labelled by rule_group whatever else it carries: DeletePartialMatch matches
// on the labels given and ignores the rest.
func (m *Metrics) deleteGroup(group string) {
	labels := prometheus.Labels{"rule_group": group}

	m.EvaluationsTotal.DeletePartialMatch(labels)
	m.EvaluationFailuresTotal.DeletePartialMatch(labels)
	m.AnnotationFailures.DeletePartialMatch(labels)
	m.EvaluationDuration.DeletePartialMatch(labels)
	m.IterationsTotal.DeletePartialMatch(labels)
	m.IterationsMissedTotal.DeletePartialMatch(labels)
	m.LastEvaluationTimestamp.DeletePartialMatch(labels)
	m.LastDuration.DeletePartialMatch(labels)
	m.AlertsActive.DeletePartialMatch(labels)
	m.RulesUnmatched.DeletePartialMatch(labels)
}

// deleteRule removes the series of one rule inside a group that is still
// loaded. Only the metrics carrying both labels: the group's own series,
// its iterations and its evaluation duration, belong to the group and not to
// the rule that left it.
func (m *Metrics) deleteRule(group, rule string) {
	labels := prometheus.Labels{"rule_group": group, "rule": rule}

	m.EvaluationsTotal.DeletePartialMatch(labels)
	m.EvaluationFailuresTotal.DeletePartialMatch(labels)
	m.AnnotationFailures.DeletePartialMatch(labels)
	m.AlertsActive.DeletePartialMatch(labels)
}

// deleteRuleName removes the query cost series of a rule, which carry the rule
// and not its group (spec 8.2). The caller has to be sure no group still holds
// a rule by this name, because an alert name may legitimately repeat across
// groups and these series cannot tell two of them apart.
func (m *Metrics) deleteRuleName(rule string) {
	labels := prometheus.Labels{"rule": rule}

	m.QueryReadRowsTotal.DeletePartialMatch(labels)
	m.QueryReadBytesTotal.DeletePartialMatch(labels)
	m.QueryMemoryUsage.DeletePartialMatch(labels)
	m.QueryDuration.DeletePartialMatch(labels)
}

// deleteSource removes the queue wait series of a source no rule reaches any
// more. Left behind, a histogram of waits against a source this ruler no longer
// connects to reads as a concurrency limit that is still being hit.
func (m *Metrics) deleteSource(source string) {
	m.QueryQueueWait.DeletePartialMatch(prometheus.Labels{"source": source})
}
