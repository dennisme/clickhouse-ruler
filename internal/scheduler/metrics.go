package scheduler

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/dennisme/clickhouse-ruler/internal/buildinfo"
)

// Metrics are the scheduler's own operational signals (spec 8.2).
//
// Everything here is labelled by rule_group and rule only, never by alert
// instance: a rule returning ten thousand rows must still produce exactly
// one metric series per rule, or the ruler becomes the cardinality problem
// it exists to fix (spec 8.3).
//
// clickhouse_ruler_problem is the exception to that and the only metric here
// aimed at somebody other than the operator: a rule that broke while running is
// fixed by whoever owns the query, so it carries whose rule it is and where to
// edit (spec 8.2, 10.4).
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
	Problem        *prometheus.GaugeVec
	SourceProblem  *prometheus.GaugeVec

	BuildInfo *prometheus.GaugeVec

	ConfigLastReloadSuccessful prometheus.Gauge
	ConfigLastReloadTimestamp  prometheus.Gauge
	ConfigInfo                 *prometheus.GaugeVec

	QueryReadRowsTotal  *prometheus.CounterVec
	QueryReadBytesTotal *prometheus.CounterVec
	QueryMemoryUsage    *prometheus.HistogramVec
	QueryDuration       *prometheus.HistogramVec
	QueryQueueWait      *prometheus.HistogramVec

	QueryConcurrencyWait *prometheus.HistogramVec
	QueryConcurrency     prometheus.Gauge
	QueriesInFlight      prometheus.Gauge
}

// NewMetrics registers every scheduler metric against reg. A nil reg uses
// the default registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)

	// One series per process, fixed at 1, carrying the build as labels. The
	// convention the ecosystem already reads: prometheus_build_info and
	// cortex_build_info are the same shape, so an operator's existing version
	// panel works by changing the metric name (spec 8.2).
	//
	// Dirty state is deliberately not a label. A released binary is never
	// dirty, so it would only ever distinguish one developer's laptop build
	// from another's.
	build := buildinfo.Get()
	buildInfo := f.NewGaugeVec(prometheus.GaugeOpts{
		Name: "clickhouse_ruler_build_info",
		Help: "Always 1. The version, commit and Go version of the running binary, as labels.",
	}, []string{"version", "revision", "goversion"})
	buildInfo.WithLabelValues(build.Version, build.Commit, build.GoVersion).Set(1)

	return &Metrics{
		BuildInfo: buildInfo,

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

		// The one signal here addressed to a rule's owner rather than to the
		// operator, so its labels have to say whose rule it is and where to
		// edit it: `team` from the rule's effective labels, empty when nobody
		// claimed it for the same reason the cost metrics leave it empty, and
		// `file` so a finding names a path rather than a rule somebody then has
		// to grep for. `check` is what makes it actionable at all, because
		// every check has a page and every finding links to it (spec 7.8), so
		// an alert built on this gauge lands its owner on an explanation rather
		// than on our dashboard.
		//
		// `source` is the cluster the finding was found against, and empty on a
		// finding about the rule rather than one of its clusters, which is
		// rule/source-schema comparing two of them. Without it a rule broken on
		// one of four clusters reads like a rule broken on all four, and a pass
		// that reached one cluster would clear what another raised: the label is
		// what makes a source-scoped answer expressible at all (spec 10.4).
		//
		// Cardinality is rules times checks times the sources each rule matched,
		// bounded by the rules loaded, and it is rebuilt per pass rather than
		// incremented: a finding that went away has to stop being a series or
		// the alert never clears (spec 8.2).
		Problem: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "clickhouse_ruler_problem",
			Help: "Rules that broke while running or loaded with a finding that should have blocked the merge, " +
				"by check and by the cluster it was found against. " +
				"Fixed by whoever owns the rule, not by the operator.",
		}, []string{"rule", "check", "severity", "team", "file", "source"}),

		// The same idea for the other audience, and a second gauge rather than
		// a label on the one above.
		//
		// A source whose user does not meet the contract in 6.7.2 is the
		// operator's to fix, where everything on Problem is the rule owner's, so
		// the two are routed to different people and an alert on one has no
		// business matching the other. They also carry different labels, keep
		// different lifecycles, and the union would give every series of both
		// two permanently empty dimensions: a rule finding names the source
		// inside its own text, and a contract finding has no rule to name
		// because nothing about a rule is broken by a missing grant.
		//
		// Cardinality is sources times contract assertions, bounded by the
		// sources file, and it is rebuilt on each load rather than incremented,
		// for the reason Problem is: a grant an operator added has to stop being
		// a series or the alert outlives the fix (spec 8.2, 6.7.3).
		SourceProblem: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "clickhouse_ruler_source_problem",
			Help: "Sources whose ClickHouse user does not meet the contract the checks rely on, by check. " +
				"Fixed by the operator.",
		}, []string{"source", "check", "severity", "file"}),

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

		// Which configuration is running, the shape build_info already uses:
		// always 1, one series, the labels are the whole payload. The pair of
		// questions during a rollout is which binary each replica runs and
		// which rules each replica loaded, and the second had no answer
		// (spec 8.2).
		//
		// `revision` is a hash of the files the ruler read rather than a
		// commit, because the ruler fetches nothing and nothing hands it a
		// sha. It is also the stronger answer to whether a fleet agrees: two
		// replicas on one commit whose volumes disagree carry the same sha and
		// different rules.
		//
		// `rules_root` is the resolved root they were read from, which is where
		// a commit rides along when a deployment named one: under git-sync the
		// root is a worktree named after the commit (spec 10.2), so the sha is
		// on the metric without the ruler claiming to know that is what it is.
		// A label here rather than a dimension of every series, because it
		// changes on every sync whether the rules did or not.
		ConfigInfo: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "clickhouse_ruler_config_info",
			Help: "Always 1. The revision of the rules this ruler is evaluating and the root they were read from, as labels.",
		}, []string{"revision", "rules_root"}),

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
			Help: "Total rows ClickHouse read evaluating a rule, by the cluster it read from. Empty team means the rule carries no team label.",
		}, []string{"rule", "team", "source"}),

		QueryReadBytesTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "clickhouse_ruler_query_read_bytes_total",
			Help: "Total bytes ClickHouse read evaluating a rule, by the cluster it read from. Empty team means the rule carries no team label.",
		}, []string{"rule", "team", "source"}),

		// Bucketed in powers of eight from a megabyte, because the cap this
		// is read against is measured in gigabytes and a linear scale over
		// that range says nothing about the rules below it.
		//
		// The one cost metric that stays on `rule` alone: peak memory is a
		// property of the query rather than of the cluster it ran on, and the
		// cap it is read against is the same wherever the rule evaluates
		// (spec 8.8).
		QueryMemoryUsage: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "clickhouse_ruler_query_memory_usage_bytes",
			Help:    "Peak memory one evaluation's query reached on the server.",
			Buckets: prometheus.ExponentialBuckets(1<<20, 8, 6),
		}, []string{"rule"}),

		// Carries `source` because a rule evaluates against every cluster its
		// selector matches, so without it one histogram folds them all together
		// and cannot say which cluster is slow. It joins to queue wait on that
		// label and on nothing else. `rule_group` because the group is the
		// scheduling unit, so its query latency against its interval is the
		// arithmetic behind a missed iteration, and `team` so chargeback can say
		// what an owner made a cluster spend time on and not only what they
		// read. Neither adds series: a rule belongs to one group and carries one
		// team, so both are determined by `rule` (spec 8.8).
		QueryDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "clickhouse_ruler_query_duration_seconds",
			Help:    "Time one evaluation's query took, measured by the ruler from sending it to the last row arriving.",
			Buckets: prometheus.DefBuckets,
		}, []string{"rule", "rule_group", "team", "source"}),

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

		// The other gate, and the one that makes a group late while every
		// cluster it reads is fast: a group's rules all fire at once, so a
		// group with more rules than the ruler-wide cap has slots queues
		// against itself and nothing per query reports it. Labelled by
		// rule_group because the question arrives as "why was my group late",
		// and not by source as well, because that is the reading queue wait
		// already gives and a histogram pays the fanout on every bucket
		// (spec 8.8).
		//
		// Zeros belong here, unlike on queue wait: this cap is on unless it is
		// turned off, so a query that found a slot waiting is the reading that
		// says the ruler is running below it.
		QueryConcurrencyWait: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "clickhouse_ruler_query_concurrency_wait_seconds",
			Help:    "Time a query waited for a slot against the ruler-wide query concurrency cap.",
			Buckets: prometheus.DefBuckets,
		}, []string{"rule_group"}),

		// The cap the wait above is read against. Configuration rather than
		// measurement, exposed for the same reason the wait is: nine seconds of
		// queueing says nothing without the number of slots it queued for, and
		// an operator who has to hardcode it reads every expression against a
		// flag somebody else can change. Zero means unbounded, as the flag
		// does (spec 8.8).
		QueryConcurrency: f.NewGauge(prometheus.GaugeOpts{
			Name: "clickhouse_ruler_query_concurrency",
			Help: "How many rule queries this ruler allows in flight at once across every group. Zero means unbounded.",
		}),

		// Read against the cap above, which is saturation without waiting for
		// the wait histogram to fill. Counts queries waiting for a slot as
		// well as running ones, because a query the ruler is trying to send is
		// load whether or not it got through. The pair is
		// prometheus_engine_queries and prometheus_engine_queries_concurrent_max
		// by another prefix, so the reading carries over (spec 8.8).
		QueriesInFlight: f.NewGauge(prometheus.GaugeOpts{
			Name: "clickhouse_ruler_queries_in_flight",
			Help: "Rule queries currently running or waiting for a slot.",
		}),
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

	// Query duration and the concurrency wait carry the group too, so they go
	// with the group like everything else labelled rule_group (spec 8.2). The
	// other cost series carry the rule alone and wait for deleteRuleName,
	// because an alert name may repeat across groups (spec 7.6).
	m.QueryDuration.DeletePartialMatch(labels)
	m.QueryConcurrencyWait.DeletePartialMatch(labels)
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

// deleteProblem removes the findings raised against one rule at one path, which
// is how both runtime feeds address the gauge: a pass clears its own series by
// the rule and the file together, so a pair no configuration holds any more is a
// pair no pass can reach (spec 8.2).
//
// Separate from deleteRuleName, which is about the cost series. Those carry the
// rule alone and so wait until no group holds a rule by that name, where a
// finding is addressed to an owner at a path and goes the moment that path stops
// being one of this rule's.
func (m *Metrics) deleteProblem(rule, file string) {
	m.Problem.DeletePartialMatch(prometheus.Labels{"rule": rule, "file": file})
}

// deleteSource removes the series of a source no rule reaches any more. Left
// behind, a histogram of waits against a source this ruler no longer connects
// to reads as a concurrency limit that is still being hit, and the cost series
// read as a cluster this ruler still bills for and still measures (spec 8.8).
func (m *Metrics) deleteSource(source string) {
	labels := prometheus.Labels{"source": source}

	m.QueryQueueWait.DeletePartialMatch(labels)
	m.QueryReadRowsTotal.DeletePartialMatch(labels)
	m.QueryReadBytesTotal.DeletePartialMatch(labels)
	m.QueryDuration.DeletePartialMatch(labels)

	// A rule finding about a cluster this ruler no longer reads is a finding
	// nothing can ever clear, because clearing it takes a pass against that
	// cluster (spec 8.2).
	m.Problem.DeletePartialMatch(labels)
}
