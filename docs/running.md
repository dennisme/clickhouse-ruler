# Running it

Every flag the binary takes, and everything it exposes once it is up.

```bash
ruler run --rules ./rules --sources ./rules/sources.yaml \
  --alertmanager http://localhost:9093
```

An error-severity finding refuses to start. A warning is printed and the ruler
runs anyway. A source failing the user contract at error severity is refused
on its own instead: its rules stop evaluating and every other source carries
on.

`ruler check` stays offline unless it is asked not to. `--online` runs the
checks that need a connection, connecting as each source's own user, because
that user is what is being checked. Those read metadata and no rows.

`--sample` additionally runs the checks that read rows, and implies `--online`.
It is separate because a connection is not consent: asking ClickHouse what a
query is costs a parse, while sampling runs statements against the source's
data. The sample is bounded by `max-sample-rows` and reads as the source's own
user, so row policies apply to it.

| Flag | Default | What it does |
| --- | --- | --- |
| `--rules` | required | rules directory |
| `--alertmanager` | required | Alertmanager base URL |
| `--sources` | `sources.yaml` | sources file |
| `--config` | `ruler.yaml` beside `--rules` | policy file |
| `--listen` | `:9090` | address for `/metrics`, `/-/healthy`, `/-/ready` |
| `--query-concurrency` | `8` | rule queries allowed against ClickHouse at once, across every group; `0` is unbounded |
| `--resend-interval` | `100s` | how often a still-firing alert is re-posted |
| `--resend-tolerance` | `4` | how many resend periods a firing alert stays valid for, so how many consecutive failed evaluations or sends it survives, and how long a resolved alert is retried for. `4` is Prometheus' own number. Minimum `2` |
| `--shutdown-timeout` | `30s` | how long an in-flight evaluation gets to finish once shutdown starts |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error` |

## What it exposes

Names track the Prometheus ruler's own metrics, so existing dashboards and
existing operator knowledge carry over. Everything is labelled by rule and
group, never by alert instance: a rule returning 10,000 rows still produces one
series per rule.

| Metric | Type | Labels |
| --- | --- | --- |
| `clickhouse_ruler_rule_evaluations_total` | counter | `rule_group`, `rule` |
| `clickhouse_ruler_rule_evaluation_failures_total` | counter | `rule_group`, `rule` |
| `clickhouse_ruler_annotation_failures_total` | counter | `rule_group`, `rule`, `annotation` |
| `clickhouse_ruler_rule_evaluation_duration_seconds` | histogram | `rule_group` |
| `clickhouse_ruler_rule_group_iterations_total` | counter | `rule_group` |
| `clickhouse_ruler_rule_group_iterations_missed_total` | counter | `rule_group` |
| `clickhouse_ruler_rule_group_last_evaluation_timestamp_seconds` | gauge | `rule_group` |
| `clickhouse_ruler_rule_group_last_duration_seconds` | gauge | `rule_group` |
| `clickhouse_ruler_alerts_active` | gauge | `rule_group`, `rule`, `state` |
| `clickhouse_ruler_alerts_sent_total` | counter | `alertmanager` |
| `clickhouse_ruler_alerts_send_failures_total` | counter | `alertmanager` |
| `clickhouse_ruler_notification_latency_seconds` | histogram | none |
| `clickhouse_ruler_rules_unmatched` | gauge | `rule_group` |
| `clickhouse_ruler_query_read_rows_total` | counter | `rule`, `team` |
| `clickhouse_ruler_query_read_bytes_total` | counter | `rule`, `team` |
| `clickhouse_ruler_query_memory_usage_bytes` | histogram | `rule` |
| `clickhouse_ruler_query_duration_seconds` | histogram | `rule` |

The four query cost metrics come from the ClickHouse driver's own callbacks
as the query runs, so they cost no extra query and do not depend on how long
`system.query_log` is kept. They are recorded whether or not the evaluation
succeeded, because a rule that trips a cap is the one worth finding. `team`
is read from the rule's labels and is empty when the author set none, which
is a rule nobody has claimed rather than one owned by nobody in particular.

`clickhouse_ruler_annotation_failures_total` is separate from the evaluation failures on
purpose: an annotation that will not render still pages, carrying the template
error in place of the annotation, so it is the rule author's bug rather than a
failed evaluation.

Two are worth alerting on. `clickhouse_ruler_rule_group_iterations_missed_total` rising
means an evaluation took longer than its group interval, so alerts are silently
late. `clickhouse_ruler_rules_unmatched` staying above zero means this ruler loaded rules
that match none of its sources and will never evaluate them, which is expected
during a rollout and a problem if it persists.

Logs are `log/slog` text on stdout, `--log-level` deep. A failed query, a
failed send and a shutdown that gave up each write one line naming the rule
group, the rule and the source involved. One line per rule and per source,
never per alert instance. Passwords never reach a log; a ClickHouse driver
error is scrubbed before it is returned, keeping the address and database an
operator needs.

Usage errors and check findings are separate: unstructured, on stderr, because
those are for the person who typed the command.

What to watch, with the number that means trouble, what each log line means,
and the `system.query_log` queries that say what a rule cost are on
[the operations page](operations.md).
