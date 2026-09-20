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
a web UI. Terraform providers exist and do put definitions in git, but they
write to that same database through the same API, so the file never becomes
the only source of truth. Getting even that much means running their platform.

We want the Prometheus model on top of ClickHouse, as one service rather than
a stack.

---

## 2. Research: what exists today

### 2.1 SigNoz

Alerts are created in the UI or through the REST API, `GET /api/v1/rules` and
`POST /api/v1/rules`. They are stored in the SigNoz application database.
There is no rule file format.

There is an official Terraform provider, and the alerts documentation points
at it as the infrastructure as code answer. It is a real one, so HCL in git is
genuinely possible. What it does not change is ownership: the provider calls
the same API and writes the same mutable rows, so the file describes a rule
without owning it, and anything with a token can still edit the rule out from
under the file. That is the same gap as 2.3.

SigNoz vendors its own Alertmanager fork internally, alongside its own ruler
package. It does not accept Prometheus rule YAML for ClickHouse-backed queries
and does not route to an external Alertmanager.

Fine-grained access control does not close the gap either, because it is not
in the open source edition. The roles documentation lists its prerequisite as
"an active SigNoz license" and marks the feature as SigNoz Cloud and
Self-Hosted Enterprise only, currently in beta. So the same bind as Grafana
OSS in 2.4: the escape hatch exists, behind a paywall.

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

The other cost is adoption. Terraform providers exist for SigNoz, ClickStack
and Grafana, so "alerts as code" is available in all three, but only by taking
their whole stack with it. Using Grafana's provider means using Grafana
Alerting; using SigNoz's means running SigNoz. Running an entire observability
platform because it is the only thing that will evaluate a scheduled SQL query
and page someone is a large amount of machinery for one job, especially for a
team whose data already lives in ClickHouse and whose routing already goes
through an Alertmanager they run.

### 2.4 Grafana plus ClickHouse datasource

The closest existing thing to what we want.

Grafana unified alerting supports file-provisioned alert rules
(`apiVersion: 1`, `groups:`, `rules:`). The official
`grafana-clickhouse-datasource` plugin lets a rule run raw ClickHouse SQL.
Grafana can forward firing alerts to an external Alertmanager.
`grafana-operator` exposes a `GrafanaAlertRuleGroup` CRD for the Kubernetes
flavour of the same thing.

The wider infrastructure as code story is uneven, which matters if the plan is
to manage rules this way. The Terraform provider covers all major resources
and is the mature path. The Ansible collection is Grafana Cloud only. The
Operator's own comparison lists dashboards, data sources and folders, not
alerting. The Crossplane provider covers all major resources but the
documentation states it "is in an alpha stage, so it has not reached a stable
state yet".

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

### 2.4.1 Git Sync

Grafana's newer Observability as Code work adds Git Sync, which syncs a
Grafana instance against a Git repository rather than pushing through an API.
That is much closer to the Prometheus model than Terraform is, so it is worth
being precise about why it does not close this gap.

It does not reach alerts. The usage limits page states "Git Sync only supports
dashboards and folders", and that alerts, data sources, panels and other
resources are not supported yet. The migration guidance is blunter still: move
alert rules and other unsupported resources out of a folder before syncing it,
because Git Sync recreates dashboards and folders but does not recreate
alerts.

This is a scope limit, not a licensing one, which makes it different from the
RBAC story in 2.4 and 2.1. Git Sync is available in OSS, Cloud and Enterprise.
Full-instance sync is marked experimental. So there is no paywall to complain
about here; the feature simply does not cover the resource we care about.

A second limit is structural rather than a missing feature, and it is the one
that would still matter if alert support shipped tomorrow.

Git Sync's usage limits recommend roughly 1,000 resources per repository
connection, and allow 10 connections per stack: the default on-prem, and a
hard limit on Grafana Cloud. The 1,000 is a sync cost, not a query cost.
Beyond it "the sync workflow puts noticeable load on Grafana itself, which may
result in slower syncs and increased database load". Nothing here is about the
datasource.

The guidance that follows is titled "Shard by capacity, not by team", and it
says plainly: "When you have many teams or tenants, it's tempting to create
one connection per team so each team maps to its own connection. Avoid this:
it consumes connections quickly, doesn't scale as teams grow, and on Grafana
Cloud a single stack can't be granted the hundreds of connections this would
require."

That is the opposite of the model here. The directory *is* the ownership
boundary: `rules/payments/` maps to `@payments` in `CODEOWNERS` and derives
the `team` label in 6.3.1 from the same path. Git Sync asks for repo
layout to follow capacity shards instead, so the structure stops encoding who
owns what, and per-team review gates get harder rather than easier. With ten
connections, one per team runs out at ten teams and the documentation tells
you not to try.

There is no equivalent limit here, because there is nothing to sync. The ruler
reads files from disk; it does not import them into a database that then has
to be kept consistent. There is no connection object to run out of, and the
number of team directories is bounded by nothing.

There is an open request, grafana/grafana#129913, for an enforcement mode that
would make the repository the only write path and reject writes that do not
come through provisioning. That is exactly the property section 4 is built on.
It is unanswered, and it is scoped to dashboards and folders, so even if it
ships it would not apply to alert rules.

The conclusion is unchanged: alert sprawl outside of code is still ungoverned
in Grafana OSS. Git Sync moves dashboards to the model we want and leaves
alerts where they were, and its sharding guidance points away from the
per-team ownership boundary that makes the model work in the first place.

### 2.5 sql_exporter plus Prometheus

`burningalchemist/sql_exporter` runs SQL against ClickHouse on a schedule and
exposes the results as Prometheus metrics. Normal Prometheus rules and normal
Alertmanager then apply. No new code.

Limits: every alert needs a scrape-interval metric. High cardinality log
queries blow up the metric cardinality. You lose "alert on the raw rows"
semantics, so multi-instance alerts are awkward.

---

## 3. The gap

Alerts as code already exists. SigNoz, ClickStack and Grafana all ship a
Terraform provider, and 2.1 through 2.4 show they work. The gap is not a
missing file format.

The gap is what you have to run to get one. Every provider above is attached
to a platform, and taking the provider means taking the platform: Grafana's
means running Grafana Alerting, SigNoz's means running SigNoz. Standing up an
entire observability stack so that one scheduled SQL query can page someone is
a large amount of machinery for a small job, and it is the main reason this
exists. The data is already in ClickHouse. The routing already goes through an
Alertmanager. The only missing piece is the thing in between, and it should
not cost a platform migration.

Underneath that, no tool gives all four of these at once:

1. Rules defined as flat files in git.
2. Raw ClickHouse SQL as the query language.
3. Alertmanager as the notification path.
4. No second write path, so the file is the only way a rule can exist.

Item 4 is the one nobody offers even after adopting their stack, because the
provider writes to the same mutable database the UI does.

### 3.1 When this is not worth it

Worth being honest about, because it is a large part of the audience.

If you already run OSS Grafana well, already keep its configuration in git,
and your team already thinks in Prometheus rules, then the delta is small.
Grafana file provisioning marks rules read only, the ClickHouse datasource
runs real SQL, and Grafana forwards to an external Alertmanager. Three of the
four properties are already yours, and the fourth is a policy problem inside
an installation you have already tuned.

The case for a separate tool gets stronger the further you are from that:
when Grafana is not already in the path, when the installation is large enough
that folder permissions are not a workable control (2.4), or when adding an
observability platform is a bigger change than adding one service.

---

## 4. Why a new tool

Two arguments, in order of weight.

**One service instead of a platform.** See section 3. The scope of what has to
be operated is the difference that survives every other comparison.

**Removing the UI makes enforcement free.** The argument was never "no YAML
format exists", because one does.

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

**`team` is special.** It defaults to the rule file's directory name, the same
derivation 6.6 intends for the ClickHouse user, so `rules/payments/`
gives `team: payments` without anyone writing it down. The default is applied
as a group label, which means the ordering above still holds and a rule can
override it.

The directory name is used exactly as written, with nothing stripped from it.
`rules/payments/` gives `payments` and `rules/team-payments/` gives
`team-payments`. Rewriting part of a path would make the mapping from
directory to team something a reader has to know rather than something they
can read, and it would mean two differently named directories producing the
same label.

A rule *may* override `team` with an explicit label. One team running
operations for another team's service is a real arrangement, not an abuse to
design out, and a tool that forbids it just gets worked around. The override
sits in the file, so it appears in a diff and `CODEOWNERS` gates who can write
it.

**Result columns may never set `team` or `alertname`.** This is not about the
override above, which is deliberately allowed. It is about where the value
comes from. The Alertmanager route tree is generated from this repository
(6.5), so every `team` value that can ever be produced has to be readable from
the files. A `team` that arrives from a result column is runtime data: the
generator cannot enumerate it, no route matches it, and the alert lands in the
catch-all or nowhere. That failure shows up during an incident, which is the
worst time to discover a routing gap. `alertname` is protected for the same
reason it always was: an identity that query data can set is a routing hazard.

**`severity` stays overridable by a result column.** Lumping it in with `team`
would prevent no abuse, because the rule author already sets `severity`
freely through the labels block, so a query doing it grants no new power.
Against that, `if(value > 1000, 'critical', 'warning') AS severity` is a
genuinely useful pattern and banning it costs something real. If the route
tree ever keys on `severity` the way it keys on `team`, revisit this: the
enumerability argument above would then apply to it too.

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

Not built, and it contradicts 6.2 as written. See open question 5.

The intent: derive a ClickHouse user from the rule file's directory path, so
`rules/payments/*.yaml` runs as the `ruler_payments` user.

What actually happens today is different. A rule names a source, the source
carries a `username`, and that is the user it connects as. The directory
decides the `team` label and nothing else. Two consequences follow, and both
need settling before any of the isolation below can be relied on:

- 6.2 requires `username` in the sources file precisely so a reviewer can see
  it. That is a different mechanism from deriving it from a path, and only one
  of them can be the answer.
- A source names one user, so two teams referencing the same source connect as
  the same user. Row policies cannot separate them, which is exactly what the
  paragraph below claims they do. Per-team isolation would need either a
  source per team or a source-plus-team to user mapping.

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
| `rule/source-exists` | a `source` that no sources file defines |
| `rule/protected-label` | a query aliasing `team` or `alertname`, or a `labels` block setting `alertname` |

The last two need the whole directory rather than one file, so they run in the
loader rather than the rule parser, but they read nothing and are tier 0 all
the same.

Every check here is currently an error except the two interval comparisons,
which warn. That is not where it should end up: 7.6 splits these into
correctness and convention, and moves the convention ones to a configurable
severity defaulting to `warn`. Not built yet.

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

### 7.6 Check configuration

Decided. Which checks are mandatory is the operator's policy, not ours.

The tool currently hardcodes it: `team` and `severity` are required labels,
`summary` and `runbook_url` are required annotations, all at error severity.
That makes a rule which would run perfectly fail to load. A valid query with a
resolved source and correct time bounds, in a flat directory, produces four
errors and never evaluates. None of them is about whether the rule works.

Rigid defaults narrow who can use the tool. "Point it at a directory and go"
has to work on the simplest possible layout, or the only consumers left are
organisations that already agree with every convention we happened to pick.

**The line is correctness versus convention.**

Correctness checks are not configurable, because a rule failing one cannot do
its job:

- `yaml/syntax`, `yaml/unknown-field`. A typo silently drops configuration, so
  the rule does not do what it says.
- `rule/name`, empty or duplicate. No identity.
- `rule/source`, `rule/source-exists`. Nothing to query.
- `rule/expr`, empty or missing `{{ .From }}` or `{{ .To }}`. Cannot run, or
  scans unbounded on every evaluation.
- `rule/for`, `rule/window`, negative values. Nonsense.
- `rule/protected-label`. Breaks routing (6.3.1).

Convention checks carry a configurable severity of `error`, `warn` or `off`,
and where they take a list of keys that list is configurable too:

- `labels/required`, and which labels
- `annotations/required`, and which annotations
- `annotations/runbook`
- `rule/for` and `rule/window` shorter than the group interval

**Severity is an escalation path, not a noise level.** This is the part that
decides everything else:

- `off`: nobody is asked.
- `warn`: the contributor decides. They can fix it or ship anyway, and no one
  else has to be involved.
- `error`: blocked. Either the rule changes, or a repo owner changes policy,
  which means a pull request against a file only the platform team can merge.

So the severity of a check is really a statement about who is on the critical
path when it fires. That is the argument for keeping errors rare. A check set
to `error` spends platform team attention every time it trips, and a config
where everything is an error makes the platform team a bottleneck on routine
contributions. Reserve it for the cases that genuinely warrant stopping
someone.

**Defaults are the current lists at `warn`.** Not `off`: an author should see
what good practice looks like on the first run, and an operator who wants it
mandatory changes one line. Not `error`: nothing about a missing runbook stops
a rule from evaluating correctly, and it does not deserve to pull a repo owner
into the loop. Someone who reads the warning and ignores it has made a choice,
and that is theirs to make.

This is also the escape hatch, and it is deliberately a social one rather than
a mechanism. There is no way to locally suppress a check that policy has set
to `error`. Needing one means asking a repo owner to change the policy file,
which is the CODEOWNERS workflow doing its job rather than being worked
around.

**Configuration lives in the operator's file**, `ruler.yaml`, alongside
`sources.yaml` in the CODEOWNERS lane from 6.6. This is what preserves the
argument in 7.1. Enforcement is not weakened by making policy configurable,
because rule authors still cannot reach the policy; the platform team sets it
and authors are still bound by it. What changes is that we stop guessing what
that policy should be.

```yaml
# ruler.yaml
checks:
  labels/required:
    severity: error
    keys: [team, severity, tier]
  annotations/required:
    severity: warn
  annotations/runbook:
    severity: off
```

Two consequences worth stating before this is built.

**Severity is runtime behaviour, not only CI output.** With hot reload, a check
at `error` means the ruler refuses the file and keeps the previous version of
it; `warn` means it loads and logs. Turning a check down does not just quiet
CI, it changes what the running ruler will accept.

**CI and the ruler must read the same `ruler.yaml`**, or a rule passes CI and
then fails to load, which is the drift 7.1 exists to prevent. That is the
reason the file belongs in the rules repository rather than in deployment
configuration.

Per-table policy is per-source policy today, because a `Source` names exactly
one table. If a source ever covers more than one, this needs revisiting rather
than being discovered by whoever tries it first.

### 7.7 Policy scopes and merging

Design, not yet built. Policy is set in more than one place, because a single
instance serves teams and datasources with genuinely different needs.

| Scope | Where | Owned by |
|---|---|---|
| instance | `ruler.yaml` at the rules root | platform |
| datasource | a `checks:` block in `sources.yaml` | platform |
| team | `ruler.yaml` in a team directory | that team |

A rule's effective policy is the **strictest** setting across every scope that
applies to it. Severity takes the maximum on `off < warn < error`. A required
key list takes the union. Nothing can lower a severity or remove a key.

**The merge is a maximum, so order does not matter.** There is no precedence
question to answer, no "which file wins", and no rule anyone has to memorise.
That is the whole reason for choosing this merge over last-one-loaded.

It also means a team-owned file is safe. A team directory is author-owned
through CODEOWNERS, so a team can write policy for its own rules, and the
worst it can do is make its own life stricter. Loosening the platform baseline
is impossible by construction rather than by convention, which is what keeps
7.1 true.

**Source and directory are independent, not nested.** A rule has both, and
neither contains the other: payments uses several sources, and `otel_traces`
is used by several teams. That is a lattice rather than a tree, and it is why
a maximum is the right merge. A tree would force one axis inside the other and
then force a winner between them.

**New check settings must be monotonic or they do not belong here.** Severity
and key lists both have an unambiguous stricter direction. A setting without
one, a numeric threshold for example, breaks the order independence above and
needs a different home.

Scoping ships in two steps. Instance and datasource first, since both are
platform owned and cover the need. Team-level files add file count and a trust
question that nobody has asked for yet, and the merge semantics are identical,
so adding them later is additive and costs no rework.

### 7.8 Explaining a finding

Once policy comes from several files, "why is this an error?" has to have an
answer, or people stop trusting the tool and start ignoring it. This is a
requirement of the scoping in 7.7, built alongside it rather than afterwards.

Every finding names three things:

- the rule file and line that triggered it, which `Problem` already carries
- the policy file and line that set the severity, which it does not
- the check's documentation, by stable anchor

`ruler check --explain` prints the resolved policy for each rule with the
origin of every setting, so an author can see that `labels/required` is
`error` because `sources.yaml:12` raised it, not because of anything in their
own directory.

The documentation link means each check needs a stable page or anchor to point
at, written when the check is. That is a real deliverable rather than a free
one, and it is the difference between a finding a contributor can act on alone
and one that turns into a question for the platform team, which by 7.6 is the
thing severity is supposed to be rationing.

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
- **Label precedence.** `team` defaults to the rule's directory, an explicit
  `team:` label may override it, and a result column may never set `team` or
  `alertname`. See 6.3.1.
- **Check configuration.** Correctness checks are fixed; convention checks
  take a configurable severity and key list, defaulting to the current lists
  at `warn`. Rigid defaults narrow who can use the tool. Severity decides who
  is on the critical path to unblock a contributor, so errors stay rare.
  See 7.6.
- **Team derivation.** The directory name is the team, used verbatim. No
  prefix is stripped, so the label always matches what the path says.
  See 6.3.1.
- **Policy scoping.** Policy is set at instance, datasource and team scope,
  and a rule gets the strictest setting that applies to it. The merge is a
  maximum, so no precedence rule exists and no scope can loosen another.
  See 7.7.

## 12. Open questions

1. **Metrics tables.** The `pint` `promql/rate` and `promql/counter` checks have
   a loose analog for counter columns in OTel metrics tables. Worth it, or skip?
2. **Restart loses pending state.** `ActiveAt` is held in memory only, so a
   ruler restart delays every pending alert by its full `for`. Prometheus
   solves this by restoring from an `ALERTS_FOR_STATE` series. Deferred.
3. **Ownership at scale.** Deferred, not solved. Operating a ruler that
   thousands of engineers page off means high availability, missed evaluation
   handling, clock skew, ClickHouse restarts mid window, and backfill after an
   outage. Revisit before anyone depends on it in production.
4. **Sharded clusters.** See 6.9 for the full list. Pinning
   `skip_unavailable_shards` to `0` is a correctness fix and should not wait
   for the rest; a dead shard currently risks resolving alerts instead of
   failing the evaluation. Proving it needs a second ClickHouse node in the
   compose stack, since a single node cannot reproduce the failure.
5. **How a rule gets its ClickHouse user.** 6.6 derives it from the directory
   path; 6.2 requires it in the sources file so a reviewer can see it. Those
   are different mechanisms and the code implements the second, so the
   isolation story in 6.6 does not hold today: two teams referencing the same
   source connect as the same user, and row policies cannot tell them apart.
   Either sources become per-team, or a source-plus-team to user mapping is
   added. Settle before anything depends on per-team isolation.

---

## 13. Sources

- [SigNoz: managing alerts via the API](https://signoz.io/docs/userguide/alerts-management/#managing-alerts-via-the-api)
- [SigNoz Terraform provider](https://signoz.io/docs/alerts-management/terraform-provider-signoz/)
- [SigNoz roles and IAM, prerequisites](https://signoz.io/docs/manage/administrator-guide/iam/roles/#prerequisites)
- [Grafana infrastructure as code](https://grafana.com/docs/grafana/latest/as-code/infrastructure-as-code/)
- [Grafana Git Sync](https://grafana.com/docs/grafana/latest/as-code/observability-as-code/git-sync/)
- [Git Sync usage and performance limitations](https://grafana.com/docs/grafana/latest/as-code/observability-as-code/git-sync/usage-limits/)
- [Git Sync: shard by capacity, not by team](https://grafana.com/docs/grafana/latest/as-code/observability-as-code/git-sync/usage-limits/#shard-by-capacity-not-by-team)
- [Git Sync known limitations](https://grafana.com/docs/learning-paths/git-sync-use/known-limitations/)
- [Git Sync: optional enforcement mode (grafana#129913)](https://github.com/grafana/grafana/issues/129913)
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
