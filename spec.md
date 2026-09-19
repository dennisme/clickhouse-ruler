# clickhouse-ruler

Status: research / draft spec
Date: 2026-09-19
Binary: `ruler`

A Go service that evaluates alert rules defined as flat YAML files against a
ClickHouse cluster and sends the resulting alerts to Alertmanager. Prometheus
rule file semantics, ClickHouse SQL instead of PromQL.

---

## 1. Problem

Prometheus alerts are files. They live in git, get reviewed in pull requests,
and cannot be created any other way. That property is what makes alert
standards enforceable at scale.

ClickHouse-backed observability stacks do not have this. SigNoz and ClickStack
both store alerts in an application database and expect users to create them in
a web UI. There is no file format, and no way to make the file the only source
of truth.

We want the Prometheus model on top of ClickHouse.

---

## 2. Research: what exists today

### 2.1 SigNoz

Alerts are created in the UI or through the REST API. They are stored in the
SigNoz application database. There is no rule file format.

SigNoz vendors its own Alertmanager fork internally, alongside its own ruler
package. It does not accept Prometheus rule YAML for ClickHouse-backed queries
and does not route to an external Alertmanager.

### 2.2 ClickStack / HyperDX

ClickStack does have alerting. Same shape as SigNoz.

Two alert types:

- Search alerts. A saved search plus a threshold.
- Chart alerts. A dashboard tile's SQL aggregation plus a threshold.

Thresholds support `>=`, `>`, `<=`, `<`, `=`, `!=`, between, and outside.

Notification targets are Slack webhook, generic webhook, Slack bot token, and
PagerDuty. Slack bot token and PagerDuty are ClickHouse Cloud only. There is no
Alertmanager support.

Alerts are stored in the HyperDX application database. Created in the UI.

### 2.3 Closest thing to alerts as code today

REST APIs exist for both editions:

- Self-hosted OSS: `POST http://<hyperdx>:8000/api/v2/alerts`, Bearer token from
  a personal API access key. Full CRUD.
- ClickHouse Cloud: `https://api.clickhouse.cloud/v1/organizations/<ORG>/services/<SVC>/clickstack/alerts`,
  HTTP basic auth with Cloud API key credentials.

Infrastructure as code wrappers, all Cloud only:

- Official `ClickHouse/clickhouse` Terraform provider, `clickhouse_clickstack_*`
  resources covering alerts, dashboards, saved searches, sources, webhooks.
- `teamlapse/terraform-provider-clickstack`, community, v0.1, roughly 13
  commits, not production ready.
- `justtrackio/provider-clickhouse`, a Crossplane provider generated with Upjet
  from the official Terraform provider. Gives Kubernetes YAML and continuous
  reconcile.

All of these write to the same mutable application database. The file is a
client of the API, not the source of truth.

### 2.4 Grafana plus ClickHouse datasource

The closest existing thing to what we want.

Grafana unified alerting supports file-provisioned alert rules
(`apiVersion: 1`, `groups:`, `rules:`). The official
`grafana-clickhouse-datasource` plugin lets a rule run raw ClickHouse SQL.
Grafana can forward firing alerts to an external Alertmanager.
`grafana-operator` exposes a `GrafanaAlertRuleGroup` CRD for the Kubernetes
flavour of the same thing.

What it gets right: file-provisioned rules are stamped `provenance: file` and
become read only in both the UI and the API. Rules shipped from git cannot
drift.

Why it does not solve the problem at scale:

- Grafana OSS has no custom RBAC. Scoped roles and fine grained alerting
  permissions are Enterprise and Cloud only.
- Access to alert rules in OSS is controlled by folder permissions plus
  datasource query permissions.
- There is no setting for "alert rules may only be created by provisioning".
  Any user with Editor on a folder can create a rule there.

So Grafana OSS prevents drift but not sprawl. In an installation with thousands
of users, one Editor grant handed out for a dashboard also grants alert
creation in that folder. The result is shadow alerts that page on call and were
never reviewed. Closing that hole means setting the default org role to Viewer
and managing folder permissions for every team, which breaks normal dashboard
work.

### 2.5 sql_exporter plus Prometheus

`burningalchemist/sql_exporter` runs SQL against ClickHouse on a schedule and
exposes the results as Prometheus metrics. Normal Prometheus rules and normal
Alertmanager then apply. No new code.

Limits: every alert needs a scrape-interval metric. High cardinality log
queries blow up the metric cardinality. You lose "alert on the raw rows"
semantics, so multi-instance alerts are awkward.

---

## 3. The gap

No tool gives all four of these at once:

1. Rules defined as flat files in git.
2. Raw ClickHouse SQL as the query language.
3. Alertmanager as the notification path.
4. No second write path, so the file is the only way a rule can exist.

Item 4 is the one nobody offers, and it is the reason to build rather than
adopt.

---

## 4. Why a new tool

The argument is not "no YAML format exists". It is that **removing the UI makes
enforcement free**.

Prometheus ruler needs no RBAC because it has no write API. The rule store is a
directory. Git is the access control list. `CODEOWNERS` is the role model. Pull
request review is the approval workflow. Authorization comes from deleting the
write path, not from building a permission system.

That is the property we are copying. Everything else follows from it.

---

## 5. Non-goals

- No UI for creating or editing rules. Ever. This is the whole point.
- No notification routing, grouping, silencing, or inhibition. Alertmanager
  already does all of it.
- No recording rules in v1. Add later only if materialized views are not
  enough.
- No replacement for SigNoz or ClickStack dashboards. This tool alerts, it does
  not visualize.
- No backwards compatibility with SigNoz or HyperDX alert JSON.

---

## 6. Design

### 6.1 Rule file format

Keep the Prometheus rule file schema as close to verbatim as possible so that
migrating an existing Prometheus rule is mechanical. Swap `expr` from PromQL to
ClickHouse SQL.

```yaml
groups:
  - name: api-latency
    interval: 1m
    rules:
      - alert: HighP99Latency
        source: otel_traces
        expr: |
          SELECT
            ServiceName,
            quantile(0.99)(Duration) / 1e6 AS value
          FROM otel_traces
          WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
          GROUP BY ServiceName
          HAVING value > 1000
        window: 5m
        for: 5m
        labels:
          team: payments
          severity: warning
        annotations:
          summary: "{{ .ServiceName }} p99 is {{ .value }}ms"
          runbook_url: https://runbooks.internal/high-p99-latency
```

`window` is how much time the query examines: the distance between
`{{ .From }}` and `{{ .To }}`. It is the one field with no Prometheus
equivalent, because PromQL carries its own lookback in the expression and SQL
does not.

It defaults to the group `interval`, which reads exactly the data produced
since the previous evaluation. A rule wanting a wider view, a p99 that needs
more than a minute of traces to mean anything, sets it explicitly. Above, each
evaluation runs every minute over the last five minutes, so windows overlap.

A window shorter than the interval is a warning rather than an error. It is
valid SQL and it runs, but the gap between the end of one window and the start
of the next is never examined by any evaluation, so an incident living only in
that gap is invisible. Nothing in the query itself reveals this, which is why
it is linted.

### 6.2 Sources

A `source` names a table, its time column, and how to reach ClickHouse. Rules
reference a source by name. Borrowed from the ClickStack sources concept.

Sources live in their own file, never inline in a rule file. Credentials, the
evaluation delay and the cost caps are operator concerns, and a separate file
is what lets CODEOWNERS stop rule authors editing them. See 6.6.

```yaml
sources:
  - name: otel_traces
    address: clickhouse:9000
    database: otel
    username: ruler_payments
    password_file: /run/secrets/ruler/payments
    table: otel_traces
    timestamp_column: Timestamp
    evaluation_delay: 1m
    max_rows: 1000
    max_execution_time: 30s
    max_memory_usage: 1073741824
```

Every field below `timestamp_column` has a default, so the shortest usable
source is name, address, database, username, table and timestamp column.
`evaluation_delay` defaults to 1m (see 6.8), `max_rows` to 1000,
`max_execution_time` to 30s and `max_memory_usage` to 1GiB.

`max_rows` is enforced by the ruler and caps how many alert instances one
evaluation may produce. The other two are sent to ClickHouse as query settings
so the cluster does the enforcing, which is the client side half of 6.7. They
are per source rather than global because a trace source and a log source do
not cost the same, and they sit in the operator's file rather than the author's
for the reason the rest of this section exists.

**Only the password comes from outside the file.** Everything else, the
username included, is written down and reviewable.

That split matters because of 6.6: the ClickHouse user is the tenancy
boundary, and row policies on that user do the isolation. A username hidden in
an environment variable cannot be reviewed, so nothing would stop a change
pointing one team's rules at a user with wider grants. The diff would show
nothing. `username` is therefore required rather than defaulted.

The password comes from `password_file` or `password_env`:

- Setting both is an error, not a precedence rule. If the two differ one is
  stale, and silently choosing either can authenticate with a credential that
  was supposed to have been rotated away.
- A file that cannot be read is an error naming the path, never the contents.
- An empty file is an error. A projected secret volume that has not populated
  yet reads as empty, and sending that as a real password produces a confusing
  auth failure rather than a clear config error.
- A trailing newline is stripped, because `echo secret > file` adds one and it
  is not part of the password.
- Neither set is legal: local development, and mTLS where ClickHouse
  authenticates the client certificate instead.

There is deliberately no DSN field. A DSN carries the password through string
handling, where any error that echoes its input puts the credential in a log.
Connection options are built field by field instead, and `Source.String`
redacts the password so printing one with `%v` cannot leak it.

### 6.3 Evaluation model

**One returned row is one alert instance.** Columns become labels. A column
named `value` becomes the alert value used in templating.

This is the thing that makes multi-instance alerts work, and it is what
sql_exporter handles poorly. A query returning one row per service produces one
alert per service, with no cardinality cost anywhere.

State machine is copied from Prometheus exactly. Inactive, pending, firing.
`for` duration, `keep_firing_for`. Do not invent new semantics here.

Each instance carries its own `for` timer and resolves on its own. Two rows
from the same rule can be in different phases on the same evaluation, and one
resolving does not disturb the other.

Instance identity is a hash of the final label set. Keys are sorted, and each
key and value is length-prefixed rather than joined with a separator. Any
separator byte can appear inside a ClickHouse column value, and a collision
there would silently merge two different alerts into one instance.

### 6.3.1 Label precedence

Three sources contribute labels to an alert, weakest first:

1. **Group labels.** Set once for a whole group.
2. **Rule labels.** The rule's own `labels` block. Overrides group labels.
3. **Result columns.** Every returned column except `value`. Overrides both.

`alertname` is written last and always comes from the rule name. A query that
returns an `alertname` column cannot rename its own alert, because an
identity that query data can set is a routing hazard.

**Open risk.** Everything except `alertname` can be overridden by result
columns, and that includes `team` and `severity`. Those two drive the
Alertmanager route tree (6.5), so as written a query can redirect its own page
by returning a `team` column. That undercuts the tenancy boundary in 6.6,
where the whole point is that a team cannot reach outside its own lane.

Three ways to close it, undecided:

- Make the rule's `labels` block win over result columns, reversing 2 and 3.
  Simple, but then a query can never add a dimension the rule did not
  anticipate, which is the main reason result columns become labels at all.
- Keep the overlay but reserve a protected set (`team`, `severity`,
  `alertname`) that result columns may not touch. A tier 0 check rejects a
  rule whose query aliases a protected name.
- Derive `team` from the rule's directory path rather than from its labels, so
  it is never author-supplied and matches the ClickHouse user already derived
  the same way in 6.6.

The third is the most consistent with 6.6 and is the current lean. Decide
before the notifier is built, because after that the route tree depends on it.

### 6.4 Time window injection

Decided: the author writes complete SQL including `FROM`, and must use the
`{{ .From }}` and `{{ .To }}` template variables, which bind to the source's
time column. Validation rejects any query that omits them.

The SQL is real SQL and can be pasted into `clickhouse-client` with
substitution. Cost of this choice: the time bound is enforced by lint, not by
structure.

Considered and rejected: the ruler builds `FROM` and the time `WHERE` itself
and the author supplies only `SELECT`, `GROUP BY`, `HAVING`. Structurally
unbounded scans become impossible, but the resulting fragment is not valid SQL
and cannot be tested by hand.

The server side settings profile cap is the real guarantee either way. See 6.7.

### 6.5 Alertmanager integration

`POST /api/v2/alerts`. Re-send firing alerts on roughly a third of
`resolve_timeout` so they do not expire.

Alertmanager owns grouping, silences, inhibition, and routing. The ruler does
not.

Generate the Alertmanager route tree from the same repository, keyed on the
`team` label, so eval and routing share one source of truth.

Skip high availability and deduplication in v1. Alertmanager already dedupes
identical alerts, so running two ruler replicas is mostly safe already.

### 6.6 Tenancy and isolation

Derive a ClickHouse user from the rule file's directory path.
`rules/team-payments/*.yaml` runs as the `ruler_payments` user.

Row policies and grants then do the isolation inside ClickHouse. A team cannot
query data it does not own, no matter what SQL it writes. The tool does not
implement authorization, the database does.

`CODEOWNERS` maps the same directories to the same teams, so the git permission
and the database permission come from one fact.

### 6.7 Guard rails

Two layers. Lint predicts, the database enforces. Neither is trusted alone.

Server side, per team settings profile on the derived ClickHouse user:

- `max_execution_time`
- `max_memory_usage`
- `max_concurrent_queries_for_user`
- `readonly = 2`, so a rule physically cannot mutate anything

Client side, at rule load and in CI: see section 7.

A runaway rule hurts one team's quota, not the cluster.

### 6.8 Evaluation delay

OTel data is not in ClickHouse the instant it happens. The collector batches,
and inserts land late. A rule evaluating the window ending at `now()` reads a
partially filled window and produces false resolves and flapping.

Every source carries an `evaluation_delay`. `{{ .To }}` binds to
`now() - evaluation_delay`, not to `now()`. Prometheus calls the same idea
query offset, Mimir calls it evaluation delay.

Default is 1m. Sources with slow batching need more. This is a production
requirement, not a test convenience, and it also makes the end to end tests
deterministic.

### 6.9 Sharded clusters

Not addressed yet. Everything below is a known gap, written down now because
one of the items is a silent correctness bug rather than a missing feature.

The query path itself needs no change. The author writes their own `FROM`, so
on a sharded cluster they name the Distributed table and the ruler never has
to know the difference. What needs work is the connection and the settings.

**`skip_unavailable_shards` must be pinned to `0`.** This is the one that
matters. The ClickHouse default is already `0`, meaning an unreachable shard
fails the query, but it is settable on a user or a profile. If it is ever `1`,
a distributed query with a dead shard *succeeds* and returns only the rows the
surviving shards held. Missing rows are indistinguishable from a recovered
condition: instances disappear from the state machine, their alerts resolve,
and the page that should have fired never does. It goes wrong silently, and
only during an outage, which is exactly when the alerts matter. The ruler
should send it explicitly with the other settings in 6.7 rather than inherit
whatever the profile says. Failing an evaluation loudly is always better than
evaluating a partial result.

**`address` must become a list.** The source schema takes one address and
`query.Open` passes `[]string{src.Address}` to a driver that already accepts
many. Against a sharded cluster that single node is both a point of failure
and the coordinator for every distributed query. Wants an `addresses:` list
and a connection open strategy.

**The cost caps are per node.** `max_execution_time` and `max_memory_usage`
are enforced by each node independently, so on a sharded cluster the real
ceiling is per shard, not per query. 6.2 and 6.7 describe them as a cluster
cap, which is loose. Fanout also means the coordinator merges results, so
`max_result_rows` is the only cap applying to the query as a whole.

**`evaluation_delay` must cover the slowest shard.** Insert lag is per shard,
and the delay has to clear the worst one, not the average. This is a larger
number rather than new configuration.

**What `table:` refers to becomes ambiguous.** It is parsed and validated but
never read by the querier today; it exists for the tier 1 checks in 7.3. On a
sharded cluster it could mean the local table or the Distributed one, and
those have different rows in `system.tables`. Decide this before tier 1 uses
it, not after.

Testing this needs a second ClickHouse node in the compose stack, a
`Distributed` table over both, and a test that stops one node and asserts the
evaluation fails rather than silently returning half the rows. Single-node
testing cannot catch the `skip_unavailable_shards` bug at all, which is the
argument for adding the node rather than reasoning about it on paper.

---

## 7. Validation

Modelled on Cloudflare `pint`, adapted for SQL.

### 7.1 One package, two entry points

`pint` is a separate binary because Cloudflare does not own Prometheus. We do
not have that constraint.

Validation is a single package called from both:

- `ruler check ./rules/` for CI
- the ruler itself at rule load time

A rule that fails validation fails to load, which fails the deploy. A rule that
dodges CI still cannot run. `pint` can only advise; we can enforce.

**A consumable GitHub Action is the third entry point.** Teams keep rules in
their own repositories, and telling each of them to write the download-and-run
YAML themselves guarantees a dozen slightly different versions, some pinned to
a stale release. Ship one:

```yaml
- uses: dennisme/clickhouse-ruler/action@v1
  with:
    path: rules/
```

It lives in this repository under `action/`, not in a repository of its own.
For a linter the action version *should* equal the tool version, and splitting
them makes users pin two things and gives us version skew to debug.
`golangci-lint-action` is separate only because it carries heavy caching and
version-resolution logic; a composite action that fetches a release binary and
runs `ruler check` does not.

The part that belongs in the binary rather than the action is the output
format. `ruler check --format=github` should emit workflow commands:

```text
::error file=rules/payments.yaml,line=12,title=rule/expr::expr must contain {{ .To }}
```

GitHub then renders each finding inline on the diff, which is the whole point
of carrying a file and line on every `Problem` (7.3). With that flag the action
is a few lines of YAML; without it the action has to parse our human-readable
output, which breaks every time the wording changes.

### 7.2 Do not write a SQL parser

Push parsing to ClickHouse: `EXPLAIN SYNTAX`, `EXPLAIN AST`,
`EXPLAIN QUERY TREE`, `EXPLAIN PLAN indexes=1`, `EXPLAIN ESTIMATE`,
`DESCRIBE (SELECT ...)`.

ClickHouse dialect parsers in Go exist but are incomplete, and the SQL surface
is too large to chase. Cost of this decision: there is no meaningful fully
offline mode. That is acceptable, because anyone running this tool already has
a ClickHouse connection by definition.

### 7.3 Check tiers

The split is not offline versus online. It is how much each check reads.

| Tier | Reads | Cost | Default |
|---|---|---|---|
| 0 | nothing | free | on |
| 1 | metadata only | milliseconds | on |
| 2 | bounded data | seconds | on |
| 3 | backfill over history | expensive | opt in |

Tier 0, file only. Checks are namespaced like `pint`, and the name travels
with the finding so it can be silenced or grepped:

| Check | What it rejects |
|---|---|
| `rule/name` | empty alert name, or a duplicate within its group |
| `rule/source` | empty `source` |
| `rule/expr` | empty `expr`, or one missing `{{ .From }}` or `{{ .To }}` |
| `rule/for` | negative `for`; warns when `for` is under the group interval |
| `rule/window` | negative `window`; warns when it is under the group interval |
| `labels/required` | missing or empty `team` or `severity` |
| `annotations/required` | missing `summary` or `runbook_url` |
| `annotations/runbook` | a `runbook_url` that is not an absolute http or https URL |

YAML parsing is strict underneath all of them: an unknown field is an error,
not a warning, because the file is the only way to create a rule and a typo
must never produce one that silently does not alert.

`rule/expr` is the lint half of the time bound decision in 6.4. It needs no
connection, so it belongs here rather than in tier 1.

Tier 1, metadata only, reads no table data:

- `EXPLAIN SYNTAX` for syntax
- table and columns exist, via `system.columns`
- annotation template variables resolve against the real output column names
  from `DESCRIBE (SELECT ...)`. Better than the `pint` equivalent, which has to
  infer labels through aggregations and sometimes gets it wrong.
- `EXPLAIN ESTIMATE` for predicted rows, parts, and marks
- `EXPLAIN PLAN indexes=1`: if granules selected is close to granules total,
  the primary key is not pruning anything
- eval interval against predicted cost. A rule on a 30s interval reading 400GB
  is arithmetic, and it is rejected
- banned constructs: `now()`, `today()`, `rand()` inside rule SQL break window
  alignment and make replays lie. `FINAL`, `clusterAllReplicas`, and `remote()`
  are cost bombs. Statement must be a `SELECT`.

Tier 2, bounded data reads:

- attribute key presence. For OTel map columns such as
  `LogAttributes['payment_id']` the column exists even when the key does not.
  Sample a short recent window and confirm the key is actually present.

  **This is the highest value check in the tool.** Someone renames an OTel
  attribute, the alert silently stops firing forever, and nobody notices until
  the outage. That class of bug is most of the reason `pint` exists.
- single evaluation of the rule as of now, to confirm it runs

Tier 3, backfill. See 7.4.

Configuration:

```yaml
check:
  clickhouse: "clickhouse://ruler_ci@host:9000"   # presence enables tiers 1 and 2
  backfill:
    enabled: true
    window: 24h
```

Per team severity overrides by path or rule name matcher, so the central team
can hard fail while product teams get warnings during rollout. Without this,
nobody adopts.

### 7.4 The "would have fired N times" check

The `pint` `alerts/count` equivalent, and the reason the online checks are
worth the trouble.

`pint` has to issue range queries against Prometheus and rebuild eval windows
on the client. In ClickHouse the eval window is just a `GROUP BY`, so the whole
replay is one query:

```sql
SELECT
  toStartOfInterval(ts, INTERVAL {{ .EvalInterval }}) AS eval_window,
  {{ .LabelCols }},
  {{ .ValueExpr }} AS value
FROM {{ .Table }}
WHERE ts >= now() - INTERVAL {{ .BackfillWindow }}
GROUP BY eval_window, {{ .LabelCols }}
HAVING {{ .Predicate }}
```

Then collapse consecutive windows through `for` and the resolve logic.

**Report two numbers.** Eval hits and real alert instances differ wildly. 4,100
hits with `for: 5m` on one bad host is about 3 pages. Reporting only the large
number trains people to ignore the check.

Target output on a pull request:

```text
would fire: 3 alert instances (4,127 eval hits) over 24h
longest firing streak: 6h12m (never resolved)
top label sets:
  ServiceName=checkout     3,901 hits
  ServiceName=cart           198 hits
```

Top offenders matter as much as the count. "Fires 4,100 times" does not tell
anyone what to fix. "3,900 of them are one service" does.

Known limits, to be surfaced in the output rather than hidden:

- Replay in one query only works for bucketable rules. Window functions, self
  joins, and `argMax` over the eval window do not rewrite generically. Fall
  back to sequential evaluation, sampled and capped, for example 24 windows out
  of 24h rather than 2,880. Mark the result as sampled.
- TTL. If the table TTLs at 3 days, a 7 day backfill quietly under reports.
  Read TTL from `system.tables` and warn when the window exceeds it.
- Schema drift inside the window. A column added 6 hours ago makes a 24 hour
  backfill return nulls or error. Detect and say so rather than reporting zero.

### 7.5 Reuse in watch mode

Running the same backfill weekly against already deployed rules produces an
alert hygiene report for free: "these 5 rules fired 800 times and none were
acknowledged". Same code path, no extra work.

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

### 8.2 Metric names

Names track the Prometheus ruler's own metrics wherever an equivalent exists,
so existing dashboards and existing operator knowledge carry over.

Go runtime and process collectors come from `client_golang` defaults.

Evaluation:

- `ruler_rule_evaluations_total` counter, by `rule_group`, `rule`
- `ruler_rule_evaluation_failures_total` counter, by `rule_group`, `rule`
- `ruler_rule_evaluation_duration_seconds` histogram, by `rule_group`
- `ruler_rule_group_iterations_total` counter
- `ruler_rule_group_iterations_missed_total` counter. Evaluation took longer
  than the group interval. This is the single most important operational
  signal, because a missed iteration means alerts are silently late.
- `ruler_rule_group_last_evaluation_timestamp_seconds` gauge
- `ruler_rule_group_last_duration_seconds` gauge

Alert state and delivery:

- `ruler_alerts_active` gauge, by `rule`, `state` (pending, firing)
- `ruler_alerts_sent_total` counter, by `alertmanager`
- `ruler_alerts_send_failures_total` counter, by `alertmanager`
- `ruler_notification_latency_seconds` histogram

ClickHouse query cost. Nothing else in this space exposes these, and they are
what make the guard rails in 6.7 observable rather than theoretical:

- `ruler_query_read_rows_total` counter, by `rule`, `team`
- `ruler_query_read_bytes_total` counter, by `rule`, `team`
- `ruler_query_memory_usage_bytes` histogram, by `rule`
- `ruler_query_duration_seconds` histogram, by `rule`

Source these from the ClickHouse Go driver's progress callbacks rather than
from `system.query_log`. The driver reports rows and bytes read during the
query itself, so there is no follow up query and no dependency on query log
retention.

These enable two things worth having: alerting on expensive alert rules, and
per team chargeback.

Validation and config, used by watch mode:

- `ruler_problem` gauge, by `rule`, `check`, `severity`. The `pint` analog.
- `ruler_config_last_reload_successful` gauge
- `ruler_config_last_reload_timestamp_seconds` gauge

### 8.3 Cardinality rule

Label metrics by rule and group only. **Never by alert instance.**

A rule returning 10,000 rows produces 10,000 alert instances and must still
produce exactly one metric series per rule. Getting this wrong turns the ruler
into the cardinality problem it exists to avoid. The `ruler_alerts_active`
gauge is a count, not a series per instance.

---

## 9. End to end testing

Everything under test is real. No mocked ClickHouse, no mocked Alertmanager, no
stubbed collector.

### 9.1 Compose stack

`compose.yaml`, all image versions pinned:

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
   payload it receives and exposes them for assertions.

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

---

## 10. Operational modes

Borrowed from `pint`:

- `ruler check ./rules/` runs validation in CI. Only checks rules changed in
  the pull request, and comments inline on the diff.
- `ruler` runs the eval loop and sends to Alertmanager.
- `ruler watch` re-validates live rules continuously and exports a
  `ruler_problem` gauge. Catches rules that *became* broken after a schema
  change, which CI cannot. Alert on your alerts.

Build order: eval loop and Alertmanager push, then tier 0 and 1 checks at load
time, then `ruler check` for CI, then tier 3 backfill, then watch mode.

Design the validation package to be re-runnable against loaded rules from day
one so that watch mode is a caller, not a rewrite.

---

## 11. Decisions made

- **Time window injection.** Full SQL, author must use `{{ .From }}` and
  `{{ .To }}`. See 6.4.
- **Ownership.** Open source project for now. Production ownership at scale is
  deferred, not answered. Revisit before anyone pages off it.
- **Test fixtures.** Own compose stack with real collector, real Alertmanager,
  and a custom emitter. See section 9.
- **Name.** Project and repository are `clickhouse-ruler`. Binary is `ruler`,
  following the Prometheus and Mimir convention. Module path
  `github.com/dennisme/clickhouse-ruler`.
- **Source definitions.** Sources live in their own file, separate from rule
  files, so CODEOWNERS can gate them. See 6.2.
- **Credentials.** No DSN. Address, database and username are written in the
  file; only the password comes from `password_file` or `password_env`, and
  setting both is an error. See 6.2.

## 12. Open questions

1. **Metrics tables.** The `pint` `promql/rate` and `promql/counter` checks have
   a loose analog for counter columns in OTel metrics tables. Worth it, or skip?
2. **Label precedence for `team` and `severity`.** See 6.3.1. Result columns
   currently override rule labels, so a query can redirect its own page.
   Leaning toward deriving `team` from the rule's directory path. Must be
   settled before the notifier is built.
3. **Restart loses pending state.** `ActiveAt` is held in memory only, so a
   ruler restart delays every pending alert by its full `for`. Prometheus
   solves this by restoring from an `ALERTS_FOR_STATE` series. Deferred.
4. **Ownership at scale.** Deferred, not solved. Operating a ruler that
   thousands of engineers page off means high availability, missed evaluation
   handling, clock skew, ClickHouse restarts mid window, and backfill after an
   outage. Revisit before anyone depends on it in production.
5. **Sharded clusters.** See 6.9 for the full list. Pinning
   `skip_unavailable_shards` to `0` is a correctness fix and should not wait
   for the rest; a dead shard currently risks resolving alerts instead of
   failing the evaluation. Proving it needs a second ClickHouse node in the
   compose stack, since a single node cannot reproduce the failure.

---

## 13. Sources

- [Alerts with ClickStack](https://clickhouse.com/docs/use-cases/observability/clickstack/alerts)
- [ClickStack API reference](https://clickhouse.com/docs/clickstack/api-reference)
- [Alerting arrives in ClickStack for ClickHouse Cloud](https://clickhouse.com/blog/alerting-arrives-in-clickstack-for-clickhouse-cloud)
- [ClickHouse Terraform provider](https://registry.terraform.io/providers/ClickHouse/clickhouse/latest/docs)
- [teamlapse/terraform-provider-clickstack](https://github.com/teamlapse/terraform-provider-clickstack)
- [justtrackio/provider-clickhouse](https://github.com/justtrackio/provider-clickhouse)
- [Grafana file provisioning for alerting](https://grafana.com/docs/grafana/latest/alerting/set-up/provision-alerting-resources/file-provisioning/)
- [Allow the editing of file provisioned alerts in the UI (grafana#92454)](https://github.com/grafana/grafana/issues/92454)
- [Grafana RBAC fixed and basic role definitions](https://grafana.com/docs/grafana/latest/administration/roles-and-permissions/access-control/rbac-fixed-basic-role-definitions/)
- [Manage alerting access using folders or data sources](https://grafana.com/docs/grafana/latest/alerting/set-up/configure-rbac/access-folders/)
- [grafana-operator alert rule group CRD](https://grafana.github.io/grafana-operator/docs/examples/alertrulegroup/full-notification-configuration/)
- [Cloudflare pint](https://github.com/cloudflare/pint)
- [burningalchemist/sql_exporter](https://github.com/burningalchemist/sql_exporter)
