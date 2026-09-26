# Operating the ruler

What to watch, what the logs mean, what refuses to start, what a reload
refuses, and how to find the ruler's queries on the cluster.

The metrics below are the ones the ruler exposes on `/metrics`. Two Grafana
dashboards built from them ship in
[`deploy/grafana/dashboards`](https://github.com/dennisme/clickhouse-ruler/tree/main/deploy/grafana/dashboards):
one for operating the ruler, one for rule authors.

## What to watch

Paste these into Prometheus. Each one has the number that means trouble and
what to do when it is reached.

### Missed iterations

```promql
sum by (rule_group) (rate(clickhouse_ruler_rule_group_iterations_missed_total[5m])) > 0
```

**Trouble at anything above zero, sustained for 15 minutes.** A missed
iteration means the group's evaluation took longer than its interval, so the
alerts in it are silently late. Nothing else reports this: every rule in the
group still evaluates, still succeeds, and still fires, just not when you
think.

Fix it by making the query cheaper or the interval longer. `ruler check
--online` reports what a rule is predicted to read per evaluation, and the
`system.query_log` queries below say what it actually read.

### Evaluation failures

```promql
sum by (rule_group, rule) (rate(clickhouse_ruler_rule_evaluation_failures_total[5m]))
  / sum by (rule_group, rule) (rate(clickhouse_ruler_rule_evaluations_total[5m])) > 0.1
```

**Trouble above 10% for 10 minutes.** A failed evaluation did not happen at
all, so the rule cannot fire while this is raised. Below that threshold it is
usually one cluster in a set being briefly unreachable, which the next
evaluation picks up.

The counter does not say why. The log line `rule evaluation failed against a
source` does, and it names the source and what the database replied.

### Last evaluation going stale

```promql
time() - max by (rule_group) (clickhouse_ruler_rule_group_last_evaluation_timestamp_seconds) > 600
```

**Trouble at more than ten times the group's interval.** This is the one
signal that catches a group that stopped evaluating rather than one that is
failing: no counter moves, nothing errors, and the alerts simply never
arrive. Compare against the interval in the rule file, not against a fixed
number, and raise the threshold for a group that evaluates hourly.

### Send failures

```promql
sum by (alertmanager) (rate(clickhouse_ruler_alerts_send_failures_total[5m])) > 0
```

**Trouble at anything above zero for 5 minutes.** A failure counts the batch
once however many alerts it held, because none of them were delivered. The
alerts are still tracked and are re-posted on the resend interval, so a brief
failure recovers on its own. A sustained one means alerts that have fired are
not reaching anybody, and the ruler cannot tell you that any other way.

Check Alertmanager first: the ruler's state machine is unaffected by a
delivery failure.

### Notification latency

```promql
histogram_quantile(0.99, sum by (le) (rate(clickhouse_ruler_notification_latency_seconds_bucket[5m]))) > 5
```

**Trouble above five seconds at p99.** Sending is in the evaluation path, so
latency here becomes evaluation duration, and evaluation duration becomes the
missed iterations above. Latency climbing alongside send failures is
Alertmanager being overloaded rather than the ruler.

### A rule reading more than it should

```promql
topk(5, sum by (rule, team) (rate(clickhouse_ruler_query_read_bytes_total[5m])))
```

**Trouble when one rule is reading more per second than the rest of the
estate put together.** There is no universal number here, because a rule over
a wide table legitimately reads more than one over a narrow one. The shape to
watch for is a step change: a rule that was reading megabytes and is now
reading gigabytes had its table grow, its window widened, or its primary key
stop being used.

The same number against the cap rather than against its neighbours:

```promql
histogram_quantile(0.99, sum by (rule, le) (rate(clickhouse_ruler_query_memory_usage_bytes_bucket[5m])))
```

**Trouble at 80% of the source's `max_memory_usage`.** Past that the rule is
one data spike away from failing every evaluation, and it fails as an
evaluation error rather than as anything that says "memory".

These come from the driver as the query runs, so they are recorded whether or
not the evaluation succeeded. A query ClickHouse refused outright reports a
duration and no rows, which is the one case where the counters undercount
what a rule is costing. `system.query_log` has the full account, below.

### Queries queueing behind a source limit

```promql
histogram_quantile(0.99, sum by (source, le) (rate(clickhouse_ruler_query_queue_wait_seconds_bucket[5m])))
```

**Trouble when the wait approaches the interval of the groups reading that
source.** A source that sets `max_concurrent_queries` holds its extra queries
in a queue, and that wait is added to every evaluation behind it. Past the
group interval the queue is what makes iterations missed, not the cluster.

A small, steady wait means the limit is doing its job. A wait that grows means
either the limit is set too low for the rules pointed at this source, or the
cluster has slowed down and the limit is now containing that slowness to this
one source, which is what it is for. The query duration above says which:
duration flat and wait climbing is too low a limit, both climbing is the
cluster.

Only sources that set `max_concurrent_queries` appear here. A source that sets
nothing is bounded by `--query-concurrency` alone and never queues per source.

### Rules that will never run

```promql
clickhouse_ruler_rules_unmatched > 0
```

**Trouble when it stays raised for an hour.** These are rules this ruler
loaded whose source selector matched none of the sources it holds, so it will
never evaluate them. Non-zero is normal on a per-datacenter ruler reading a
shared rules repository, and it is expected to return to zero once a cluster
rollout finishes. Staying raised means somebody wrote a rule against a
cluster that does not exist here, and nobody read the warning `ruler check`
already gave them.

### A reload the ruler refused

```promql
clickhouse_ruler_config_last_reload_successful == 0
```

**Trouble immediately.** The ruler re-read its files, found something it will
not run, and kept the version it was already running. Everything looks healthy
from the outside: the rules that are loaded evaluate, they fire, they deliver.
What is wrong is that they are not the rules in your repository, and nothing
else says so.

The reason is on stderr, with the file and line of every finding. Fix the file
and send another `SIGHUP`; the gauge returns to 1 on the first load that
succeeds.

Its pair dates the configuration actually running:

```promql
time() - clickhouse_ruler_config_last_reload_timestamp_seconds
```

This one is only stamped by a load that succeeded, so it answers "how old are
the rules this ruler is evaluating" rather than "when did somebody last try".
On a ruler nobody reloads it climbs from the moment it started, which is
correct and not worth alerting on by itself.

## What the logs mean

Everything the daemon says once it is running is a structured line on stdout.
Usage errors and lint findings are a different stream: unstructured, on
stderr, because a person ran a command and the command has something to say
about what they typed.

| Level | Message | What to do |
| --- | --- | --- |
| info | `ruler running` | Nothing. It carries `rules` and `listen`; a `rules` count lower than you expect means rules were filtered by source matching, not dropped. |
| info | `shutting down` | Nothing. Carries the `timeout` an in-flight evaluation is being given. |
| error | `rule evaluation failed against a source` | Read `source` and `error`: this is the database's own reply, with credentials removed. A timeout or memory cap means the rule is too expensive, and the `system.query_log` queries below say by how much. The rule's alert state is untouched, so its `for` timer survives and the next evaluation continues from where the last successful one left off. |
| error | `sending alerts to alertmanager failed` | Check Alertmanager. The alerts were evaluated and their state has advanced; only delivery failed, and they are re-posted on the resend interval. Repeated failures past `--resend-tolerance` periods let Alertmanager expire an alert that is still firing. |
| warn | `annotation template failed, the alert carries the error instead` | A rule author's problem, not an operator's. The alert was delivered with the template error where its annotation should be, so somebody is reading that error on their page. `annotation` names which one; fix it in the rule file. |
| error | `metrics listener stopped` | The HTTP surface is gone, so metrics and probes are unanswered while the evaluation loop carries on. Usually the `listen` address is already taken. Restart it. |
| warn | `shutdown timeout expired with evaluations still running` | A query or a send was cut off part way through. This is the only signal that says so. If it happens on every restart, raise `--shutdown-timeout` above your slowest evaluation. |
| warn | `refusing a source that failed the user contract` | Deliberate, see below. |
| info | `reloading` / `reloaded` | Nothing. A `SIGHUP` arrived and the files were re-read. `reloaded` carries the `rules` and `sources` count now running, which is the pair to compare against the `ruler running` line. |
| error | `refusing the reload, the previous configuration keeps running` | Read `reason`, then the findings on stderr. The ruler is still evaluating the rules it had before the signal. Nothing is degraded and nothing was applied. |

One line per failed source and one per failed send, never one per alert
instance: a rule returning ten thousand rows that cannot be delivered writes
one line, not ten thousand.

Credentials never reach a log. A ClickHouse driver error may echo connection
detail, so every error is redacted before it is returned: the address and the
database survive, because you need them, and the password does not.

## What refuses to start

Two refusals look like an outage and are the design working.

**An error-severity finding stops the ruler.** `refusing to start: at least
one rule failed a correctness check` on stderr, and exit code 1. The same
checks run in CI and at startup, so a rule that slipped past a pull request
still cannot run. A rule that fails a correctness check cannot do its job: it
will not parse, has no identity, has nothing to query, scans without a time
bound, or breaks routing. Fix the rule, or re-run `ruler check` to see the
finding with its line number. Correctness checks cannot be softened by
policy, which is what makes this refusal trustworthy.

**A source failing the user contract is refused on its own.** `refusing a
source that failed the user contract` in the log, and the ruler carries on.
Every rule that matched only that source stops being evaluated; every other
source is untouched. This is the one case where a check failure is not fatal,
because the alternative is one misconfigured cluster taking down alerting for
eleven healthy ones. The finding names which assertion failed: revoked table
functions, `readonly = 2`, the constraints behind each limit, or the grant on
the source's own table.

Everything else that refuses is a flag: an unparseable `--log-level`, a
non-positive `--resend-interval`, a `--resend-tolerance` below two. All of
them exit 2 and say so on stderr, rather than starting with a value that
would quietly misbehave. A source the ruler cannot connect to at all exits 3,
which is a distinct code so a supervisor can tell "the flags were wrong" from
"the cluster is unreachable".

## What a reload refuses

`SIGHUP` re-reads the rules directory, the sources file and the policy file,
and replaces what is running with them. Nothing watches the filesystem: you say
when the files are complete, because a watcher would read a rules tree half way
through being written.

```bash
kill -HUP $(pidof ruler)
```

A reload is all or nothing. Every way it can fail leaves the ruler evaluating
exactly what it was evaluating before the signal, and raises the refused-reload
gauge above.

**An error-severity finding refuses the whole reading.** Including the files in
it that are fine: a rules tree is loaded as a tree, and half of one is not a
configuration anybody wrote down. This is the same bar `ruler check` and startup
apply, so a rule that would fail CI cannot be reloaded into a running ruler
either. A warning is reported and the reload proceeds.

**A file that cannot be read refuses it too.** A missing sources file, a policy
file that will not parse, a rules directory that has gone: the running
configuration is kept, because a reload is not an opportunity to leave the ruler
evaluating nothing.

**A source failing the user contract is still refused on its own**, exactly as
at startup, and the rest of the reload proceeds. A reload is where a revoked
grant is noticed: the contract is checked at `ruler check`, at startup and here,
and never per evaluation, so between reloads the report is as old as the last
one.

What survives a reload is as important as what it refuses:

- **A pending alert keeps its place.** A rule whose name, labels and source are
  unchanged keeps the instances it is tracking, so an alert part way through its
  `for` does not start again from zero. Change any of those three and it is a
  different alert, which starts fresh. A process restart keeps nothing, which is
  the reason to reload rather than restart.
- **A firing alert keeps its resend cadence.** It is not re-posted to
  Alertmanager because of the reload.
- **A connection is reused.** A source whose definition did not change keeps the
  connection it had; one nothing matches any more is closed once the evaluations
  holding it have finished; one the reload brought in is opened before anything
  evaluates against it.
- **A group's tick schedule restarts.** Groups are re-staggered from the moment
  the reload finishes rather than resuming their old phase, so one evaluation can
  land up to an interval away from where you would have predicted it.

Series for a group or rule the reload dropped are deleted from `/metrics`. A
counter left at its last value reads as a rule that still runs and has gone
quiet, which is the one thing a deleted rule must not look like.

## Finding the ruler's queries in ClickHouse

Half of operating this happens on the other side of the connection. A rule
that timed out, read more than predicted or tripped a memory cap leaves its
evidence in `system.query_log` on the cluster.

Every query the ruler sends carries a `log_comment` naming the rule and its
group. Evaluations, `ruler check --online` and `--sample` all carry it:

```json
{"ruler":"clickhouse-ruler","rule_group":"rules/payments.yaml:latency","rule":"CheckoutSlow"}
```

It is a query setting rather than a comment in the SQL, so it cannot change
what runs. It carries no SQL, because the query text is already in the log,
and no label values, because those are data and would put unbounded
cardinality into a column you group by.

### What one rule cost last night

```sql
SELECT
    event_time,
    query_duration_ms,
    read_rows,
    formatReadableSize(read_bytes) AS read,
    formatReadableSize(memory_usage) AS memory,
    type
FROM system.query_log
WHERE JSONExtractString(log_comment, 'rule') = 'CheckoutSlow'
  AND event_time >= now() - INTERVAL 1 DAY
  AND type != 'QueryStart'
ORDER BY event_time DESC
LIMIT 50
```

### The most expensive rules on this cluster

```sql
SELECT
    JSONExtractString(log_comment, 'rule_group') AS rule_group,
    JSONExtractString(log_comment, 'rule') AS rule,
    count() AS evaluations,
    formatReadableSize(sum(read_bytes)) AS total_read,
    round(avg(query_duration_ms)) AS avg_ms,
    max(read_rows) AS worst_rows
FROM system.query_log
WHERE JSONExtractString(log_comment, 'ruler') = 'clickhouse-ruler'
  AND event_time >= now() - INTERVAL 1 DAY
  AND type = 'QueryFinish'
GROUP BY rule_group, rule
ORDER BY sum(read_bytes) DESC
```

This is the one to run when the missed iterations expression above starts
firing, and the one to hand a team whose rules are the reason.

### What failed, and what the server said

```sql
SELECT
    event_time,
    JSONExtractString(log_comment, 'rule') AS rule,
    exception_code,
    exception
FROM system.query_log
WHERE JSONExtractString(log_comment, 'ruler') = 'clickhouse-ruler'
  AND event_time >= now() - INTERVAL 1 DAY
  AND type IN ('ExceptionBeforeStart', 'ExceptionWhileProcessing')
ORDER BY event_time DESC
LIMIT 50
```

`159` is the execution time cap and `241` the memory cap, both of them the
ruler's own limits doing their job. The rule is reading more than its source
permits: make the query cheaper, or raise `max_execution_time` or
`max_memory_usage` on that source and on the ClickHouse profile that
constrains it.

### Whether a rule is running at all

```sql
SELECT
    JSONExtractString(log_comment, 'rule') AS rule,
    max(event_time) AS last_seen
FROM system.query_log
WHERE JSONExtractString(log_comment, 'rule_group') = 'rules/payments.yaml:latency'
  AND event_time >= now() - INTERVAL 1 DAY
GROUP BY rule
ORDER BY last_seen
```

A rule missing from this list is one the ruler never sent, which is the
cluster-side view of `clickhouse_ruler_rules_unmatched`.
