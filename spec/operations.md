# Operating the ruler

Metrics, logging, the end to end test stack, and how this gets deployed.

Part of the [clickhouse-ruler spec](../spec.md). Section numbers are stable
and are what the code comments cite.

---

## 8. Observability of the ruler itself

### 8.1 HTTP surface

`client_golang` on a single listener. The complete surface is:

- `GET /metrics`
- `GET /-/healthy`
- `GET /-/ready`

There are no rule create, update, or delete endpoints, and there never will be.
That is section 4 restated as an API decision. The absence of a write path is
the security model.

**The two health endpoints answer different questions, and today they do not.**
Both return 200 unconditionally, which makes them the same endpoint written
twice. What each has to mean:

- `/-/healthy` is about the process. It is alive and its listener is serving,
  which is what an unconditional 200 already says. Correct as it stands.
- `/-/ready` is about whether sending traffic here is useful, and that is a
  different question: rules loaded, and at least one source answering. A ruler
  that cannot reach any ClickHouse is running perfectly and evaluating nothing.

The distinction is not decoration, because the two are wired to different
things. A failed `/-/healthy` gets a process restarted; a failed `/-/ready`
takes it out of a load balancer or stops a rollout. A readiness probe that
cannot fail turns a deployment of a ruler that cannot reach its cluster into a
successful one, and 10.2's highly available topology depends on that
distinction: with several rulers behind one deployment, readiness is what stops
a rollout replacing working replicas with broken ones.

Readiness is deliberately not a source-by-source answer. One unreachable
cluster out of twelve is a finding for `source/privileges` and the evaluation
failure counters, not a reason to declare the whole ruler unfit, and a probe
that flaps with any cluster's availability gets disabled by whoever is on call.

### 8.2 Metric names

Names track the Prometheus ruler's own metrics wherever an equivalent exists,
so existing dashboards and existing operator knowledge carry over. That is
about the suffix: `_rule_group_iterations_missed_total` means here what it
means there.

The prefix is deliberately ours. Every name is namespaced
`clickhouse_ruler_`, not `ruler_`, because `ruler` is a component name rather
than a product name and the ecosystem already uses it: Mimir and Cortex expose
`cortex_ruler_*`, Loki exposes `loki_ruler_*`. A series called
`ruler_alerts_sent_total` in a Prometheus scraping more than one thing does not
say whose it is. The `job` label answers that while you are looking at the
series, and stops answering it the moment a name is pasted into an alert
expression, a recording rule, or a screenshot in an incident channel, which is
where a metric name has to speak for itself. Carrying a Prometheus dashboard
over already means rewriting the prefix, so this costs nothing that was free
before.

Go runtime and process collectors come from `client_golang` defaults.

Evaluation:

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

A missed iteration means the evaluation took longer than the group interval.
It is the single most important operational signal here, because alerts are
then silently late.

`clickhouse_ruler_rule_evaluation_failures_total` counts evaluations that did not happen,
which is what the Prometheus metric it is named after counts. An annotation
that would not render is not one of those: the evaluation produced alerts and
they were delivered, carrying the template error where the annotation should be
(6.5). It gets its own counter rather than a label on this one, because a label
would make every carried-over dashboard query read high, and because the two
have different audiences: a failed evaluation is an operator's problem and a
broken template is the rule author's. `annotation` is a label worth having,
since it names what to fix and an annotation is static configuration rather
than anything data can multiply (8.3).

Alert state and delivery:

| Metric | Type | Labels |
| --- | --- | --- |
| `clickhouse_ruler_alerts_active` | gauge | `rule_group`, `rule`, `state` (pending, firing) |
| `clickhouse_ruler_alerts_sent_total` | counter | `alertmanager` |
| `clickhouse_ruler_alerts_send_failures_total` | counter | `alertmanager` |
| `clickhouse_ruler_notification_latency_seconds` | histogram | none |

`clickhouse_ruler_alerts_active` carries the group because an alert name may repeat
across groups (7.6), and without it two same-named rules would report into one
series. It remains a count per rule, never a series per instance (8.3).

`clickhouse_ruler_alerts_sent_total` counts alerts, not batches, so it reads the same way
as the Prometheus metric it is named after. The latency histogram already
carries a count per send, so there is no separate batch counter.
`clickhouse_ruler_alerts_send_failures_total` counts a failed batch once however many
alerts it held, because it delivered none of them.

ClickHouse query cost. Nothing else in this space exposes these, and they are
what make the guard rails in 6.7 observable rather than theoretical:

| Metric | Type | Labels |
| --- | --- | --- |
| `clickhouse_ruler_query_read_rows_total` | counter | `rule`, `team` |
| `clickhouse_ruler_query_read_bytes_total` | counter | `rule`, `team` |
| `clickhouse_ruler_query_memory_usage_bytes` | histogram | `rule` |
| `clickhouse_ruler_query_duration_seconds` | histogram | `rule` |
| `clickhouse_ruler_query_queue_wait_seconds` | histogram | `source` |

Source these from the ClickHouse Go driver's progress callbacks rather than
from `system.query_log`. The driver reports rows and bytes read during the
query itself, so there is no follow up query and no dependency on query log
retention.

These enable two things worth having: alerting on expensive alert rules, and
per team chargeback.

`clickhouse_ruler_query_queue_wait_seconds` is the exception to the `rule` and
`team` labelling above, because it measures the per-source concurrency limit
in 6.11 rather than what a rule cost. Queueing is a property of the cluster
the limit protects: every rule against a saturated source waits, and which
rule happened to wait says nothing about what to change. Only sources that set
`max_concurrent_queries` reach it, so a series here means a limit exists and
is being hit.

Validation and config:

| Metric | Type | Labels |
| --- | --- | --- |
| `clickhouse_ruler_problem` | gauge | `rule`, `check`, `severity` |
| `clickhouse_ruler_rules_unmatched` | gauge | `rule_group` |
| `clickhouse_ruler_config_last_reload_successful` | gauge | none |
| `clickhouse_ruler_config_last_reload_timestamp_seconds` | gauge | none |

`clickhouse_ruler_problem` is the `pint` analog. `clickhouse_ruler_rules_unmatched` counts rules this
ruler loaded that match no source it holds, so it will never evaluate them
(6.10). Expected to be non-zero on a per-datacenter ruler reading a shared
repository, and expected to return to zero after a cluster rollout finishes.
Alerting on it staying raised is how the soft failure in 6.10 stops being
ignored: the check warns at authoring time, this catches the case where nobody
read the warning.

Of these four, only `clickhouse_ruler_problem` is outstanding: it re-validates
loaded rules on a timer, which is what `ruler watch` is. The reload pair exists
because `SIGHUP` reloads the files, and the two deliberately do not say the same
thing. `clickhouse_ruler_config_last_reload_successful` is about the last
attempt, so a refused reload leaves it at 0 until one succeeds, which is the
alert: the rules that are running are valid and nothing about them looks wrong,
so a ruler running last week's rules is invisible otherwise.
`clickhouse_ruler_config_last_reload_timestamp_seconds` is about the
configuration being evaluated, so a refused reload leaves it alone. The query it
exists for is `time() - clickhouse_ruler_config_last_reload_timestamp_seconds`,
read as how old the running rules are, and stamping it on a refusal would answer
that with the moment the ruler declined to change anything.

The query cost table above is read from the driver's callbacks as the query
runs. Progress packets carry what each block read rather than a running
total, so they are added; memory arrives as a per-thread gauge, so the
query's cost is the highest any thread reached. `team` comes from the rule's
effective labels and is empty when the author set none, which is left
visible: chargeback that hides what is unattributed is chargeback nobody can
reconcile.

Cost is recorded whether or not the evaluation succeeded, because a rule that
trips a cap is the one an operator is looking for. What it records is
whatever the server reported before the failure, which is nothing when the
query was refused outright: a result overflow throws before the first
progress packet, so that evaluation reports a duration and no rows. Timeouts
and memory caps, which are the expensive rules, report what they read.

### 8.3 Cardinality rule

Label metrics by rule and group only. **Never by alert instance.**

A rule returning 10,000 rows produces 10,000 alert instances and must still
produce exactly one metric series per rule. Getting this wrong turns the ruler
into the cardinality problem it exists to avoid. The `clickhouse_ruler_alerts_active`
gauge is a count, not a series per instance.

### 8.4 Logging

A counter that moved says something failed. It does not say which rule, which
source, or what the database replied, and those are the three things an
operator needs before they can act. Logs are the other half of this section,
not a duplicate of it.

`log/slog` from the standard library, text output, on stdout. `--log-level`
takes `debug`, `info`, `warn` or `error` and defaults to `info`. An
unparseable level is refused at startup rather than defaulted. There is no
`--log-format` until somebody asks for one.

Two streams, two audiences. Usage errors, lint findings and the refusal to
start are CLI output, unstructured, on stderr: a person ran a command and the
command has something to say about what they typed. Everything the daemon says
once it is running is a log line, structured, on stdout.

What is logged:

| Level | Event | Fields |
| --- | --- | --- |
| info | ruler running | `rules`, `listen` |
| info | shutting down | `timeout` |
| error | rule evaluation failed against a source | `rule_group`, `rule`, `source`, `error` |
| error | sending alerts to alertmanager failed | `rule_group`, `rule`, `error` |
| warn | annotation template failed, the alert carries the error instead | `rule_group`, `rule`, `source`, `annotation`, `error` |
| error | metrics listener stopped | `listen`, `error` |
| warn | shutdown timeout expired with evaluations still running | `timeout` |

A shutdown that gives up is the only signal an operator gets that a query or a
send was cut off part way through, which is why it is logged rather than
returned silently. A shutdown also cancels evaluations already running, and
each cancelled source logs an evaluation failure like any other, because that
is what it is: the counter has always recorded it and the log now says so.

The cardinality rule in 8.3 is written about metrics and the same reasoning
holds for logs: one line per failed source and one per failed send, never one
per alert instance. A rule returning 10,000 rows that cannot be delivered
writes one line, not 10,000.

Credentials never reach a log. A ClickHouse driver error may echo connection
detail, so every error `internal/query` returns goes through `redact` first.
The address and the database survive, because an operator reading the line
needs them; the password does not.

### 8.5 Finding the ruler's queries in ClickHouse

Half of operating this thing happens on the other side of the connection. A
rule that times out, reads more than the estimate predicted, or trips a memory
cap leaves its evidence in `system.query_log` on the cluster, not in anything
the ruler exposes, and 8.2 cannot help: a counter says an evaluation failed,
and the query text, the rows read and the peak memory are all over there.

Today the only way to pick those queries out of `query_log` is the source's
username, which fails exactly when it matters. A username identifies a source,
so it cannot separate one rule from another, and 10.2's ruler-per-team and
ruler-as-a-service topologies deliberately share a user across many rules.

**So every query the ruler sends carries a `log_comment`.** ClickHouse stores
that setting per query in `system.query_log`, which turns "what did this rule
cost last night" into a `WHERE` clause. It carries the rule and its group, not
the SQL and not the alert's labels: the query text is already in the log, and
label values are data, which would put unbounded cardinality into a column
operators group by (8.3 applies here too).

It is a query setting rather than a comment in the SQL, so it cannot change
what runs, and it sits beside the caps in 6.7 that every evaluation already
sends. The sampling and inspection queries carry it as well, since a check that
read more than expected is the same question asked at check time.

### 8.6 Dashboards

Metric names that carry over (8.2) are not the same as somebody having
something to look at. Two dashboards ship in `deploy/grafana/dashboards`,
because there are two audiences and one dashboard for both serves neither.

- **Operations.** Is the ruler doing its job: missed iterations first, because
  that is 8.2's most important signal, then evaluation failures, evaluation
  duration against the group interval, staleness of the last evaluation, send
  failures and notification latency.
- **Alert rules.** Whether a team's own rules work: which of their rules are
  failing to evaluate, which matched no source and will therefore never run,
  what is firing and pending now, and which annotations will not render. The
  distinction from the first one is ownership. A rule author cannot act on
  notification latency and should not be shown it.

They are files in the repository rather than screenshots in a wiki, for the
reason rules are: a dashboard that is provisioned from git is one that can be
reviewed, and one that can be fixed when a metric is renamed.

**A panel querying a metric nobody exposes renders an empty graph, which looks
exactly like a healthy system.** That is the failure this area produces, and it
is the same shape as a check page documenting a default the tool does not have
(7.8), so it gets the same answer: a test reads every expression in both
dashboards and asserts each metric it names is registered. Renaming a metric
then fails the build rather than quietly emptying a panel somebody is on call
with.

Two consequences of shipping them. The dashboards may only use the metrics in
8.2, because anything else is a panel that cannot work; where one would need a
signal that does not exist yet, the gap is named in this spec rather than
papered over with an expression that returns nothing. And they are portable: a
datasource variable rather than a hardcoded UID, since the UID is local to
whoever imports it.

**What that test does not catch is the label.** It asserts that every metric a
panel names is registered, and a metric name is only half of an expression. A
panel grouping by `rule` on a metric labelled `rule_group` alone passes it and
renders exactly the empty graph the test was written to prevent, as does a
matcher on a label whose values are not what the author assumed. Names are
checked without a running system; labels need series. The stack in 9.8 is
where that half is answered, and until it exists this gate is the weaker of
the two claims this section makes.

One further omission worth stating rather than discovering. Duration is shown
against nothing, because the group's interval is configuration and 8.2 exposes
no metric carrying it. A panel cannot draw the line an operator is meant to
read the duration against, so it says so in its description instead. Exposing
the interval as a gauge is the obvious fix and is not free: it is another
series per group, and it is a metric whose only consumer is a dashboard.

### 8.7 The operations page

Everything in 8.1 through 8.6 is a mechanism. What an operator needs at three in
the morning is the reading of it, and that is a page rather than a spec section:
`docs/operations.md`, published with the check pages.

Four things belong on it, and nothing else:

- **What to watch, as expressions they can paste.** A missed iteration, an
  evaluation failure rate, a last evaluation going stale, send failures,
  notification latency. Each with the number that means trouble and what to do
  about it, because a threshold nobody can justify is a threshold somebody
  silences.
- **Every log line, with what it means.** The table in 8.4 says what is logged.
  The page says what to do when a line appears, which is a different table and
  the one people actually need.
- **What refuses to start, and why that is deliberate.** An error-severity
  finding stops the ruler; a source failing the user contract is refused on its
  own while every other source carries on (6.7.3). Both look like an outage to
  somebody who has not read 7.1, and both are the design working.
- **The ClickHouse side.** `system.query_log` queries keyed on the
  `log_comment` from 8.5: what a rule cost, what it read, what timed out.

What does not belong on it is a second description of the checks. Those have
their own pages and their own generated facts (7.8), and an operations page
restating a severity default is one more thing to go stale.

---

## 9. End to end testing

Everything under test is real. No mocked ClickHouse, no mocked Alertmanager, no
stubbed collector.

### 9.1 Compose stack

`compose.yaml`, all image versions pinned. Items 1 and 4 exist today; the rest
arrive with the scheduler.

1. **ClickHouse**, single node today. Schema in `deploy/clickhouse/init`, the
   OpenTelemetry Collector ClickHouse exporter trace table reproduced verbatim
   from `exporter/clickhouseexporter` in `opentelemetry-collector-contrib`:
   same columns, types, codecs, skip indexes, `PARTITION BY` and `ORDER BY`.
   Copying it rather than trimming it means a rule that works in the tests
   works against real collector output, and it keeps `ResourceAttributes`
   available, which is where `deployment.environment`, `service.namespace` and
   the `k8s.*` keys live. Only the engine and the TTL differ, and both are
   local-development concerns. A second node arrives with 6.9.
2. **OpenTelemetry collector**, ClickHouse exporter, batch timeout set low so
   data lands in seconds rather than tens of seconds.
3. **Telemetry generators.** Two of them, see 9.2.
4. **Alertmanager**, real, configured with a webhook receiver pointing at the
   sink.
5. **Ruler**, the code under test.
6. **Webhook sink**, a small HTTP server that records every notification
   payload it receives and exposes them for assertions. Not a container today:
   it runs inside the integration test so assertions can read the payloads
   directly, which is why Alertmanager routes to `host.docker.internal` on a
   fixed port. Moving it into the stack only becomes worthwhile once the ruler
   itself is a container and no test process is left to host it.
7. **Prometheus**, scraping the ruler. Not for the ruler's benefit: it is
   what the dashboards query, and without it 8.6's panels have nothing behind
   them at all. See 9.8.
8. **Grafana**, provisioned from `deploy/grafana`: a datasource pointing at
   item 7, and a dashboard provider pointing at the files 8.6 ships. Nothing
   is configured through its UI, for the reason rules are files: a dashboard
   that exists only in somebody's browser cannot be reviewed.

Items 5, 7 and 8 arrive together, because each is useless without the one
before it. A Prometheus with nothing to scrape holds no series, and a Grafana
with no Prometheus renders the same empty panels the whole exercise is meant
to catch.

### 9.2 Two generators, not one

- `telemetrygen` from `opentelemetry-collector-contrib` for background volume.
  Proves the thing works against a realistically busy table and gives the
  backfill checks something to read.
- **A small custom Go emitter** for deterministic scenarios. This is the one
  that makes assertions possible. `telemetrygen` gives volume but cannot
  express "inject exactly 40 spans over 1000ms on `ServiceName=checkout`
  starting at T+30s". Without that control, no test can assert an exact alert
  count.

The OpenTelemetry Demo was considered and rejected. Around 15 services is too
heavy and too slow for CI, and it is not controllable enough to assert against.

### 9.3 Assertion path

Assert on what the webhook sink received, not on ruler internal state. That
exercises the full path including Alertmanager grouping and routing, which is
the part most likely to be misconfigured.

Shape of a test:

1. Start the stack, wait for ready.
2. Emitter injects a known anomaly.
3. Poll the sink until the expected notification arrives or the deadline
   passes.
4. Assert labels, annotations after templating, and the rendered value.
5. Emitter stops the anomaly. Assert the resolve notification arrives.

### 9.4 Making tests fast without a fake clock

Test rules use `interval: 1s` and `for: 3s`. Real clock, short durations. The
state machine keeps an injectable clock for unit tests, but the end to end
tests do not use it, because a faked clock in an end to end test stops it being
end to end.

`evaluation_delay` from 6.8 is what absorbs ingestion lag here. Set it to a few
seconds in the test config rather than racing the collector.

### 9.5 Historical fixtures

Backfill check tests need 24 hours of history, which cannot be generated in
real time.

Seed those by inserting synthetic rows directly into the ClickHouse OTel tables
with backdated timestamps, bypassing the collector. That is fixture setup, not
the code under test, so skipping the ingest path is legitimate. Tests that
exercise ingest use the collector.

### 9.6 Recipes

Task running is `just`, so the recipe list is the interface and `just` on its
own prints it.

- `just compose-up` brings the stack up and waits for it to be healthy
- `just integration` runs the tagged suite against a running stack
- `just compose-down` tears down, including the named volume
- `just integration-clean` does all three, tearing down even on failure
- `just test` runs unit tests with `-race` and needs no container
- `just check` runs everything CI runs, in the order CI runs it

`compose-down` deleting the volume is deliberate. ClickHouse applies
`deploy/clickhouse/init` only to an empty data directory, so a schema change
silently does nothing if the previous volume survives.

CI runs the compose stack directly in GitHub Actions.

### 9.7 Which ClickHouse versions are tested

7.2 explains why the readers in `internal/query` are coupled to a server
version, and 9.1 pins one. This section is about the rest of them, and about
what may be said in public.

**The axis is long term support releases, not the last N releases.**
ClickHouse ships monthly, so a promise about "the current version and the
three before it" expires every month and describes a set nobody runs. The
releases operators actually stand up are the long term support ones, two a
year, supported for a year after that. Three legs:

| Leg | Server | Required |
| --- | --- | --- |
| pinned | the current long term support release, the version in `compose.yaml` | yes |
| previous | the long term support release before it | yes |
| newest | `latest-alpine`, whatever released most recently | no |

The newest leg stays advisory for the reason 7.2 already gives: a server
changing its output is worth knowing about and is not the problem of whoever
opened the next pull request. The previous long term support leg is required,
because a version somebody is running is not a weather report.

**Tested is reported, never promised.** The table this produces says which
servers the suite passed against and when, and that is the whole claim. It is
not a support matrix, there is no deprecation policy behind it, and a red leg
on an older server is a fact rather than a commitment to fix it. The project
has no external users (10.2 is topologies people could run, not topologies
anyone runs), and promising compatibility to nobody costs the one thing we
have, which is the freedom to change a reader when a server changes its
output.

**The table is generated from the matrix.** A hand written compatibility
table is a screenshot of a build that has already moved, which is the same
failure as a check page stating a default the tool does not have (7.8) and a
panel querying a metric nobody exposes (8.6). It gets the same answer: the
matrix is the source, the published table is output, and a leg that is not in
continuous integration is not in the table.

**The floor is unknown and the slice that builds this finds it.** Every check
that reads server output has some oldest version it still parses:
`EXPLAIN AST`, `EXPLAIN ESTIMATE`, `EXPLAIN PLAN indexes=1`,
`DESCRIBE (SELECT ...)`, the privileges probes in 6.7.2, and the
`system.query_log` columns 8.5 is read back through. Nothing has ever asked
where that floor is. Finding it is a one-off walk backwards through releases,
and the answer belongs in the table as the oldest version tested rather than
as the oldest version supported, which is a different sentence and one this
project is not in a position to write.

None of this exists yet. Today the matrix is the two legs 7.2 describes and
there is no published table.

### 9.8 Dashboards that render

The gate in 8.6 proves a panel names a metric something registers. It cannot
prove the panel draws a line, because a name with the wrong grouping label or
an impossible matcher is still a name. Answering the other half needs series,
which needs a ruler that is running, a Prometheus that scraped it, and rules
that produced something worth plotting.

The stack already has the hard parts: real ClickHouse, real rules, real
evaluations, real alerts. Items 7 and 8 in 9.1 add the two that are missing,
and the assertion is then cheap: run each dashboard's expressions against
Prometheus through its own query API and require a series back.

**Assert against Prometheus, not against Grafana.** Grafana renders; it does
not decide whether an expression matches anything. Driving its API, or worse
its browser, would be a large amount of machinery to learn something the
datasource already knows, and it would fail for reasons that have nothing to
do with the dashboards. Grafana is in the stack so a person can open the
dashboards and see them working, which is worth having on its own and is not
what the test reads.

Two things this cannot promise, and they should not be attempted. A panel
whose expression returns a series is not a panel whose axis, unit or legend is
right, and no test is going to tell us a graph is legible. And a test that
demands every panel be non-empty will fail on the panels that are empty when
the system is healthy, which is most of the alert rules dashboard: a rule that
is firing during the run is a rule the test has to make fire. The assertion is
that the query is answerable, not that the answer is interesting.

---

## 10. Operational modes

Borrowed from `pint`:

- `ruler check ./rules/` runs validation in CI. Only checks rules changed in
  the pull request, and comments inline on the diff.
- `ruler` runs the eval loop and sends to Alertmanager.
- `ruler watch` re-validates live rules continuously and exports a
  `clickhouse_ruler_problem` gauge. Catches rules that *became* broken after a schema
  change, which CI cannot. Alert on your alerts.

Built so far: `ruler check` with configurable policy, tiers 0 through 2 of
section 7 including the checks that read the query through the database and
the one that reads rows, and `ruler run`, which ticks groups on their
intervals, evaluates against every matched source, delivers to Alertmanager,
and reloads all three files on `SIGHUP`. The observability in section 8 is
complete apart from `clickhouse_ruler_problem`.

Hot reload is `SIGHUP` and nothing else: nothing watches the filesystem,
because whoever rolled the files out is the only party that knows when they are
complete. A reload is all or nothing, keeps the pending state of every rule that
is still the same rule, and refuses a reading that fails a correctness check
(7.6). What it does not do is notice a rule that became broken while nothing
changed on disk, which needs re-validation on a timer rather than on a signal.

`ruler watch` does not exist, so loaded rules are not re-validated on a timer
and `clickhouse_ruler_problem` is not exported.

Next: watch mode, which brings that timer and the last metric in 8.2, then tier
3 backfill (7.4).

The validation package is already re-runnable against loaded rules, so watch
mode is a caller rather than a rewrite.

### 10.1 Validation as something other people can use

Worth recording, because section 7 turns out to be the part nobody else has.

The checks are useful to anyone scheduling ClickHouse SQL, not only to this
ruler. Three shapes this could take without becoming a different project:

- A validating admission webhook for the SigNoz operator's `Rule` resources,
  which today validates the shape of the custom resource and not the SQL it
  carries.
- A CI validator over `terraform show -json`, for the Grafana provider, which
  keeps the alert query as opaque `model` JSON that the provider does not
  inspect.
- The `github` output format already emitted by `ruler check`, which needs no
  integration beyond running the binary.

This argues for keeping the check package free of assumptions about where a
rule came from. It already takes parsed rules and a policy rather than a
directory, so the cost of preserving that is low, and it is worth paying
before a fourth caller makes it expensive.

None of this is scheduled. It is written down so the interface does not drift
somewhere that makes it impossible.

### 10.2 Deployment topologies

The label mapping in 6.10 exists so that these are all the same binary with a
different sources file, rather than four products.

- **One ruler, one cluster.** Sources need no labels at all.
- **Ruler per datacenter.** Each holds sources for its own clusters, so data
  is queried locally rather than across a link. A shared rules repository is
  read by all of them, and each evaluates the subset matching its sources.
  This is the topology that makes an unmatched rule normal rather than broken.
- **Central rulers, highly available.** Several rulers with the same sources
  file. Alertmanager already deduplicates identical alerts (6.5), so running
  more than one is mostly safe, and 12.3 is where the remaining sharp edges
  live.
- **Ruler per team.** A team runs its own, points it at its own sources, and
  consumes the central rules repository for the safety checks in section 7.
- **Ruler as a service.** The team operating ClickHouse owns the sources file
  and the clusters, and other teams contribute only rules. This is the split
  in 6.10: adding a cluster or a user is an admin change, writing an alert
  against one is not.

None of these need code that does not already exist. The one exception used to
be a rule matching several sources, which has to evaluate once per source with
its alert instances staying distinct per source; that is settled, because
`source` is part of an alert's label set and therefore of its fingerprint
(6.3).

#### Running more than one ruler

The highly available topology above is worth spelling out, because "mostly
safe" is doing a lot of work in one bullet and an operator deciding between one
ruler and three needs the actual trade.

**It works by duplication, not by coordination.** Two rulers with the same
sources file produce the same alerts: an alert's identity is its final label
set, and every part of that set comes from the files and the query result
rather than from the process, so both rulers arrive at the same fingerprint and
Alertmanager deduplicates them (6.5). That is how Prometheus HA pairs work and
it needs no leader election, no shared state and no new features here.

**Which makes the sources file the thing to get right.** Source `labels`
(6.10.1) land on alerts, so two rulers meant to be replicas of each other must
carry the same ones. A label naming the replica, the obvious thing to reach
for when two processes are emitting the same alert, is precisely what breaks
deduplication: it makes every alert two alerts, and 6.5's route tree then
routes both.

**The cost is query load, and it is linear.** Each replica evaluates every
matched rule against every matched source, so three rulers is three times the
queries and three times the cost caps in 6.7 consumed. That is the argument for
sharding by source rather than replicating for its own sake, and it is why the
per-source concurrency limit in 6.11 matters more here than anywhere else: a
cluster already carrying triple the rule traffic is the one that starts queueing.

**Two rough edges, both known.** A ruler holds `ActiveAt` in memory, so a
replica that restarts, or one added to an existing set, re-serves every pending
alert's full `for` before it will fire (12.2). And each ruler computes its own
window from its own clock, so skew between replicas shifts what each one reads;
`evaluation_delay` absorbs the ordinary case, and 12.3 is where the rest of
that sits, still deferred.

Neither is a reason to run one ruler instead of three. They are reasons the
deduplicated alerts can disagree about *when*, never about *what*, and an
operator should know that before they are paged about it.

### 10.3 Checking a pull request

Two decisions, both of which look like implementation detail and are not.

#### Changed-file filtering belongs in the binary

"Only checks rules changed in the pull request" is what this section has
always said, and it is easy to read as "the action passes the changed paths
to the checker". That is wrong, because the changed set of *files* is not the
affected set of *rules*:

- A change to `sources.yaml` can move a source's labels, so a rule in an
  untouched file stops matching, starts matching, or matches a different
  cluster (6.10).
- A change to `ruler.yaml`, or to a `checks:` block on a source, can raise a
  check, so a rule that warned yesterday blocks today (7.7).
- A source's `database`, `table` or caps changing alters what the tier 1
  checks conclude about rules nobody edited (7.3).

Computing which rules a change affects needs the ruleset binding and the
policy merge. Both live in the validation package, and reimplementing either
inside an action is the second validation path 7.1 exists to prevent. So the
expansion is the binary's job, and the action supplies only the base
reference and a checkout deep enough to reach the merge base.

It is also the reason this is not a CI-only feature. `ruler check
--changed-since origin/main ./rules/` answers the same question on a laptop,
before anything is pushed.

Three rules it has to follow:

- **The base is the merge base**, not the previous commit. A branch with
  several commits, or one that has been rebased, gets the wrong answer from
  `HEAD~1`.
- **Some paths force a full run.** A change to the sources file or to any
  policy file affects rules the diff does not name, so the filter widens to
  everything rather than trying to be clever about which rules a label change
  reached.
- **An unresolvable base checks everything and says so.** A shallow checkout
  with no merge base is a reason to do more work, never less. Filtering that
  silently checks nothing is the failure mode that makes a green build
  meaningless.

Filtering is off unless asked for. `ruler check` with no flag checks the whole
directory, because that is what the loader does at startup, and a CI run whose
scope quietly differs from the loader's is a rule that passes review and fails
to load.

#### The action is composite, and owns nothing but the wiring

7.1 chose a composite action in `action/`, consumed as
`dennisme/clickhouse-ruler/action@v1`. Holding to that, with two additions
that belong in the spec rather than in whoever writes it:

- **The download is verified.** A composite action that fetches a release
  binary and executes it is a supply chain step. Releases publish a checksums
  file; the action checks it before running anything.
- **Tags are disciplined.** The action version should equal the tool version,
  which means a release moves the floating major tag. Without that, everyone
  pinned to `@v1` runs whatever the tag pointed at the day they wrote it.

Not a Node action: it would add npm, a committed bundle, and a second
dependency tree to a repository whose entire dependency list is three Go
modules, and it would buy nothing that shell cannot do. Not a Docker action
either: an image pull per job for no isolation benefit, since what runs is a
static binary.

**What the binary does not do is talk to GitHub.** Inline annotations need no
API at all: `--format=github` writes workflow commands to stdout and GitHub
renders them on the diff. The summary comment in 7.10 does need the API, a
`pull-requests: write` token, and logic to update one comment in place rather
than appending one per push. That belongs in the action, which already runs
inside GitHub's own environment, and it keeps a GitHub client out of a service
whose dependencies are otherwise ClickHouse and Alertmanager.

The split that makes it possible: the binary can emit its findings as JSON,
and the action reads that to build the comment. The same output serves anyone
integrating the checks elsewhere (10.1), which is an argument for it existing
independent of the comment.

#### What the action exposes

Inputs are the flags `ruler check` already has — the rules path, the sources
file, the policy file, the output format, whether to run the online checks —
plus the base reference for filtering and whether to post a summary. Nothing
is invented for the action that the binary does not already support, so
anything achievable in CI is achievable by hand.

Permissions are worth documenting on the action rather than left to be
discovered: `contents: read` is enough for the offline checks and inline
annotations, and the summary comment additionally needs
`pull-requests: write`. A pull request from a fork gets neither a writable
token nor secrets, so it cannot run the online checks or post a comment. That
is correct behaviour, and 7.10 explains why `pull_request_target` is not the
way around it.
