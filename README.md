# clickhouse-ruler

Alert rules for ClickHouse, defined as files in git, checked before they run,
evaluated against your cluster, and sent to the Alertmanager you already
operate.

Prometheus rule semantics. ClickHouse SQL instead of PromQL. No web UI, by
design. One service, not a platform.

> **Status: early.** It takes a rule from a file to a delivered notification
> today, but the query checks that need a live database are missing and nothing
> has operated it for real. See [Status](#status) before you depend on it.

## Why this exists

Prometheus alerts are files. They live in git, they get reviewed in pull
requests, and there is no other way to create one. That last part is what
makes alert standards hold up at scale.

Keeping ClickHouse alerts in git is a solved problem too, and it is worth
saying so plainly. SigNoz and Grafana both have operators with alert rule
CRDs, both have Terraform providers, and the SigNoz operator will even revert
an edit someone makes in the UI. If you are already running one of those
platforms on Kubernetes, you may not need this at all.

Two gaps are left.

**You have to run a platform to get it.** Every one of those paths comes
attached to SigNoz or Grafana. If your data is already in ClickHouse and your
routing already goes through an Alertmanager you operate, adopting an
observability platform so that one scheduled SQL query can page someone is a
lot of machinery for the job. The missing piece is the thing in between, and
it should not cost a platform migration.

**Nothing looks inside the query.** This is the one with no workaround. Every
tool named above stores the ClickHouse SQL as an opaque string and hands it to
the
database. A rule can lose its time bound and scan without limit on every
evaluation. It can read around the row policies meant to keep a team in its
own lane. It can go silent forever because someone renamed an OTel attribute,
and nobody finds out until the outage it was supposed to catch. All three
review perfectly as a diff. A rule is SQL, and an alerting tool that never
reads the SQL is checking the envelope rather than the letter.

So: the Prometheus model on top of ClickHouse, as one service rather than a
stack, with the query itself checked before it ever runs.

## How it works

Two kinds of file, owned by different people.

**Sources** are operator owned. What a cluster is, where to connect, which
ClickHouse user to connect as, how far behind live data to evaluate, and the
cost caps.

```yaml
# rules/sources.yaml
sources:
  - name: payments_prod
    labels: {team: payments, cluster: prod, env: prod}
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

Everything from `evaluation_delay` down has a default and can be left out.
`max_rows` caps how many alert instances one evaluation may produce; the other
two are sent to ClickHouse as query settings, so the cluster enforces the cost
cap rather than the ruler.

`labels` describe what this source is. Rules select on them, and they are
added to every alert the source produces, so an alert always says which
cluster it came from without the rule having named one.

Only the password lives outside the file. The username is not a secret and is
deliberately in plain sight: it is the tenancy boundary, so a reviewer has to
be able to see that `payments` connects as `ruler_payments` and not as
something with wider grants.

Use `password_env: SOME_VAR` instead if a file does not suit. Setting both is
an error rather than a precedence rule, because when the two disagree one of
them is stale and quietly picking either can authenticate with a credential
that was supposed to have been rotated away. No password at all is fine for
local development and for mTLS.

**Rules** are author owned. A rule names no source; it carries a `sources`
selector over source labels, and runs against every source that matches. One
rule definition covers an estate instead of being copied per cluster.

```yaml
# rules/payments/latency.yaml
groups:
  - name: api-latency
    interval: 1m
    rules:
      - alert: HighP99Latency
        sources:
          team: payments
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

`sources` is the only thing deciding which clusters the query runs against.
`labels` are for Alertmanager routing and nothing else. Adding a term to the
selector narrows it, so `{team: payments, env: prod}` would run on the prod
cluster alone.

An empty or missing selector matches nothing, deliberately: choosing a source
chooses the ClickHouse user the query runs as, so a rule should never reach a
cluster by leaving a field out. A rule that really should run everywhere
selects a label the operator put on every source.

Nothing is inferred from the directory. A path decides who reviews the file,
not what the alert is labelled, so an alert that wants `team: payments` says
so in its own labels.

`CODEOWNERS` then does the rest:

```text
/rules/sources.yaml    @platform-team
/rules/payments/       @payments
```

Four things to notice.

**One rule, many clusters.** The `sources` selector matches every source
carrying its labels, and the rule evaluates against each one. The alerts stay
separate: every alert carries a protected `source` label naming where it ran,
so one cluster recovering never resolves another's alert. Collapsing them into
a single notification is Alertmanager's `group_by`, not something baked into
the alert's identity.

**One returned row is one alert instance.** Columns become labels, the `value`
column becomes the value. A query returning one row per service produces one
alert per service, each with its own `for` timer, each resolving on its own.

**The ruler owns the time window.** `{{ .From }}` and `{{ .To }}` are bound as
ClickHouse query parameters, never pasted into the SQL text. A rule that does
not use them is rejected before it can run, because a query without a time
bound scans without limit on every evaluation.

`window` sets how far apart those two are, and defaults to the group
`interval`, which reads exactly the data produced since the last evaluation.
Above, the rule runs every minute over the last five minutes, so windows
overlap. Setting it shorter than the interval is a warning: the query still
runs, but the gap between one window and the next is never examined by any
evaluation.

**The query is checked, not just the file.** A rule is SQL, and SQL is where
the interesting failures live: a missing time bound that scans without limit,
a renamed OTel attribute that makes an alert silently stop firing, a table
function that reads around the row policies meant to contain a team. Putting
alerts in git does not address any of that, which is why every alternative in
the comparison below scores no on it regardless of how its rules are stored.

**Validation runs at load, not just in CI.** The same package backs
`ruler check` and the loader, so a rule that gets past CI still cannot run.
This is modelled on Cloudflare's `pint`, with one difference: `pint` can only
advise, because Cloudflare does not own Prometheus. We do, so we can enforce.

**Strictness is the operator's call.** A missing runbook does not stop a rule
from evaluating correctly, so by default it is a warning, and a warning is the
contributor's to act on. Raising it to an error means a repo owner has to be
involved to unblock someone, which is worth reserving for cases that deserve
it.

**Half of this is the database's job, and the split is deliberate.** Which
tables and rows a rule can read, whether it can reach data through `remote()`
or `url()`, whether it can write, and whether it can raise the limits the
ruler sends are all enforced by the ClickHouse user the source connects as.
No check makes them true, and none is trusted to: the checks that refuse a
table function or a foreign table are CI failing fast on a mistake, not the
thing standing between one team and another team's data.

That leaves a gap worth closing, because the contract is invisible when it is
not met. A user with too few grants fails loudly on every evaluation. A user
with too many fails silently: every rule evaluates perfectly, and nobody finds
out until someone writes the query that reads around the row policies. So
`source/privileges` checks the user itself, once per source, at check time and
at startup. It probes the queries that are supposed to be refused, reads the
settings constraints out of `system.settings`, and confirms the one grant that
has to be there. `deploy/clickhouse/init/02-ruler-user.sql` is the reference
user, and the integration tests connect as it.

**This one check is not an instance of the paragraph above it.** It takes a
severity like any other, and that severity decides whether anybody is told,
never whether the guarantee holds: the grants are what stop a query, so a
cluster where this check is off is exactly as safe as one where it passes. It
is a warning by default for a different reason than a missing runbook is. A
ruler pointed at an existing cluster fails it on the first run, and a check
that blocks the first run gets switched off rather than fixed.

## Running it

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
that user is what is being checked.

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

### What it exposes

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

## How it compares

|                                     | Rules in git   | ClickHouse SQL | Your Alertmanager | File is the only write path | Validates the query |
| ----------------------------------- | -------------- | -------------- | ----------------- | --------------------------- | ------------------- |
| SigNoz + Operator                   | yes            | yes            | no, runs its own  | with external controls      | **no**              |
| ClickStack / HyperDX                | Terraform only | yes            | no                | no                          | **no**              |
| Grafana OSS + ClickHouse datasource | yes            | yes            | yes               | with external controls      | **no**              |
| sql_exporter + Prometheus           | yes            | metrics only   | yes               | yes                         | **no**              |
| clickhouse-ruler                    | yes            | yes            | yes               | yes                         | file checks today   |

"With external controls" is doing real work in that table. Both operators can
be pushed most of the way there: alert custom resources in a Helm repo laid
out per team, `CODEOWNERS` on those paths, and the UI's rule-creation routes
blocked at the Kubernetes ingress so only the operator's service account gets
through. Or, less strictly, diff what the platform holds against the manifests
and report anything with no file behind it. If you already run Kubernetes and
one of those platforms, that is probably cheaper than adopting this.

The catch is that none of it is a supported feature. Ingress rules match paths
an application may change on upgrade, and the failure mode is silent: alert
creation quietly starts working again. A diff reports sprawl rather than
preventing it.

The last column is the one nobody offers at any price, and it is the part that
does not have a workaround.

Be clear about where that column stands here: today the checks read the rule
file, not the database. The time bound is enforced by requiring
`{{ .From }}` and `{{ .To }}` in the text, which catches the common mistake
but is not the same as asking ClickHouse what the query does. The checks that
need a connection, `EXPLAIN` for cost and plan, `system.columns` for whether
the table still has the column, `DESCRIBE` for whether an annotation
references a label the query actually returns, are specified in
[spec 7.3](spec/validation.md) and are not built. That is the next piece of work, and
until it lands this column reads "file checks today" rather than yes.

### SigNoz and ClickStack

Both have alerting and both store alerts in their own application database.

SigNoz does use Alertmanager, and gives more of it as code than we first
credited. It maintains a fork, bundled into the SigNoz binary since v0.76.0,
and the operator exposes `RoutePolicy` and `PlannedMaintenance` custom
resources, so routing and maintenance windows are manifests rather than UI
state. That is the same idea as generating a route tree from the rules repo,
and they ship it today.

The gap is whose Alertmanager. It is theirs, embedded, with no documented way
to point it at a standalone instance. If you already run one, with your
routing, your silences and your on-call integrations wired into it, SigNoz
means a second one: two places to silence an incident and two route trees to
keep agreeing. That is what the table's Alertmanager column means, and it is
narrower than "does not have Alertmanager".

The SigNoz Operator is the closest thing to this project that exists. It
manages a `Rule` custom resource, so alert rules do live in git, and Argo CD
or Flux drive them like anything else. It answers drift too, by reconciling
rather than by preventing: "The operator re-checks each resource on an
interval and reverts changes made outside Kubernetes, such as edits in the
SigNoz UI." If you are already on Kubernetes and already running SigNoz, look
at it before you look at this.

What it does not obviously close is sprawl. Reverting changes to resources the
operator manages is not the same as stopping a rule existing that no file ever
created, and its documentation does not say it removes those. It is also
`v1alpha1`, Kubernetes only, and AGPL-3.0.

Both also have Terraform providers, which are weaker on ownership: the
provider calls the same API and writes the same mutable rows, so anyone with a
token can edit the rule out from under your file and no diff records it. The
ClickStack providers are ClickHouse Cloud only on top of that.

Locking the API down does not rescue it either. SigNoz's fine-grained access
control requires "an active SigNoz license" and is Cloud and Self-Hosted
Enterprise only, currently in beta. Same bind as Grafana OSS: the escape hatch
is real, and it is not in the free edition.

The larger cost is that adopting either one for alerting means adopting the
whole platform. Running SigNoz because it is the only thing that will evaluate
a scheduled SQL query and page someone is a lot of machinery for one job, when
your data is already in ClickHouse and your routing already goes through an
Alertmanager you run.

### Grafana OSS plus the ClickHouse datasource

The closest thing that already exists, and worth using if it fits you.

Alert rules can be provisioned from files, the query is real ClickHouse SQL,
and Grafana can forward to an external Alertmanager. File-provisioned rules
are marked read only, so rules you ship from git cannot be edited in the UI or
the API. Drift is solved.

Sprawl is not. Grafana OSS has no custom RBAC, and there is no setting for
"alert rules may only be created by provisioning". Anyone with Editor on a
folder can create a rule in it. In an install with thousands of users, one
Editor grant handed out for a dashboard also grants alert creation, and you
end up with alerts that page on-call which no pull request ever saw. Closing
the hole means default-Viewer plus per-team folder permissions, which breaks
ordinary dashboard work.

Git Sync, the newer Observability as Code work, does not close it either. It
is the closest thing Grafana has to the Prometheus model, and it is in OSS
rather than behind a licence, but it "only supports dashboards and folders".
Alerts are not supported yet, and the migration guide tells you to move alert
rules out of a folder before syncing it. There is an open request
(grafana/grafana#129913) for a mode where the repository is the only write
path, exactly the property this project is built on, but it is unanswered and
scoped to dashboards and folders.

Its sharding guidance points away from the ownership model too, which would
still matter if alert support shipped tomorrow. Git Sync recommends about
1,000 resources per repository connection and allows 10 connections per stack,
a hard limit on Cloud. That cap is a sync cost rather than a query cost: past
it, "the sync workflow puts noticeable load on Grafana itself". The guidance
is titled "Shard by capacity, not by team" and says to avoid one connection
per team because "it consumes connections quickly, doesn't scale as teams
grow". So repo layout follows capacity, not who owns what. That is the
opposite of the model here, where a directory is an ownership boundary for
review and a source is selected by label. Nothing is synced into a database,
so there is no connection to run out of and no cap on team directories.

If managing rules this way is the plan, the wider tooling is uneven: the
Terraform provider is the mature path, the Ansible collection is Cloud only,
the Operator ships `AlertRuleGroup`, `ContactPoint` and `NotificationPolicy`
CRDs, and the Crossplane
provider "is in an alpha stage, so it has not reached a stable state yet".

### sql_exporter plus Prometheus

Runs SQL on a schedule, turns the result into Prometheus metrics, then normal
Prometheus rules and Alertmanager apply. No new code, and a good answer for
simple cases.

The cost is that every alert needs a metric. High cardinality log and trace
queries blow up metric cardinality, and you lose alerting on the rows
themselves, so per-instance alerts get awkward.

## When you should not use this

If you already run OSS Grafana well, already keep its config in git, and your
team already thinks in Prometheus rules, the delta here is small. File
provisioning already makes those rules read only, the ClickHouse datasource
already runs real SQL, and Grafana already forwards to an external
Alertmanager.

A shop that disciplined has probably gone further and closed the creation path
too. Grafana OSS has no "alert rules may only be created by provisioning"
setting, so the only way to get there is default-Viewer plus per-team folder
permissions. If you have done that, you already have all four properties and
there is little here for you. Use what you have.

What this offers such a team is not the property, it is the price. That
lockdown is coarse: the role that stops someone creating an alert rule is the
same role that governs their dashboards, so you buy alert hygiene by making
ordinary dashboard work need a permission grant. Here, the file being the only
write path costs nothing, because there is no other write path to close and no
UI whose permissions you are borrowing.

The case gets stronger the further you are from that: when Grafana is not in
the path at all, when the install is large enough that per-team folder
permissions stop being maintainable, or when adding an observability platform
is a bigger change than adding one service.

## Status

Honest picture of what exists today.

Working:

- `ruler check ./rules/`, with text output or GitHub workflow commands that
  annotate a pull request diff
- Configurable check severity. Correctness checks always block; convention
  checks default to warnings and an operator raises them in `ruler.yaml` or
  per source. `--explain` names the file that set each one
- A page per check family, published at
  [dennisme.github.io/clickhouse-ruler](https://dennisme.github.io/clickhouse-ruler/checks/)
  and linked from every finding. What a check ships as is generated from the
  table the resolver reads, so a page cannot state a default the tool does not
  have
- Rule file parsing, with line numbers on every finding and strict unknown
  field rejection
- Query checks that read the SQL rather than the file: a rule that will not
  parse, carries a second statement, selects `*`, reads through a table
  function, calls `now()`, sets its own `SETTINGS`, reads a table outside its
  source's database, or joins more than a configured ceiling is reported
  before it ever runs. Found in the tree ClickHouse itself parsed, never by
  matching words in the query text
- Checks that read the rule's real result columns, through
  `DESCRIBE (SELECT ...)`, which reads no rows: a rule naming a column a
  migration dropped, one whose result has no `value` column and so can never
  fire, one producing a label the ruler owns through a subquery or `SELECT *`,
  and an annotation reading something no column and no label will carry
- The ClickHouse user contract: `source/privileges` checks each source's user
  for revoked table-function privileges, `readonly = 2`, a constraint behind
  every limit the ruler sends, and the grant on its own table. Probed rather
  than read out of `SHOW GRANTS`, which reports role membership instead of
  what the roles contain
- Nine offline checks: seven on a rule file, plus the two needing the sources
  file (which sources the labels match, and whether the query sets a protected
  label)
- Sources file parsing, with secrets read from a file or the environment, and
  eleven checks
- Per-source exemptions: a source owner drops one check for their own cluster
  with a stated reason and an expiry date, and an exemption that has run out
  fails the build rather than lingering. `until` is the first day no longer
  covered, read as midnight UTC unless written as an RFC3339 timestamp with
  an offset. Rule files cannot carry one
- The alert state machine: pending, firing, resolved, `for`, `keep_firing_for`,
  per-instance identity
- Running a rule against real ClickHouse and getting alert samples back
- Loading a rules directory and selecting each rule's sources by label, so one
  rule evaluates against every cluster it matches
- Annotation templating, and sending to Alertmanager
- `ruler run`: a scheduler that ticks each rule group on its own interval,
  staggers groups so they do not stampede ClickHouse, evaluates rules and their
  sources concurrently under a shared query limit, and shuts down without
  cutting an evaluation off
- A resend cadence, so a still-firing alert is re-posted and carries its own
  expiry rather than inheriting Alertmanager's `resolve_timeout`. How much
  failure that expiry tolerates is `--resend-tolerance`, defaulting to what
  Prometheus gives itself
- Resolved alerts are retried rather than sent once, so a notification that
  fails does not leave Alertmanager showing an alert that has recovered
- Metrics on `/metrics`, with `/-/healthy` and `/-/ready`, and structured logs
  naming the rule and source behind every failure
- A ClickHouse and Alertmanager compose stack, with an end to end test that
  takes a rule from a file all the way to a delivered notification

Not built yet:

- The query checks stop short of reading data. A rule scanning a terabyte, or
  returning nothing at all, still passes. Spec 7.3, the rest of tier 1 and all
  of tier 2.
- No `ruler watch`, so rules are not reloaded without a restart.
- No ClickHouse query cost metrics. Rows and bytes read per rule need a driver
  progress callback. Spec 8.2.
- No Alertmanager route tree generation.
- No pull request summary comment. Findings annotate the diff inline today;
  a table of what each rule will cost per evaluation is spec 7.10, and the
  measured version of it needs the cost metrics above.

Known gaps that will change:

- Sharded clusters are only partly handled. `skip_unavailable_shards` is
  pinned to `0`, so a dead shard fails the evaluation rather than silently
  resolving alerts, but `address` still takes a single node and the cost caps
  are per shard. See spec 6.9.
- Team-scoped policy files are not read. Policy comes from `ruler.yaml` and
  from each source. See spec 7.7.

## Development

Requires Go, Docker, and [just](https://github.com/casey/just). Run `just` on
its own to list every recipe.

```bash
go run ./cmd/ruler check --sources rules/sources.yaml rules/
go run ./cmd/ruler check --online --sources rules/sources.yaml rules/
just init               # mise tool versions and pre-commit hooks
just check              # lint, unit tests with -race, markdownlint
just test               # unit tests with -race, no container needed
just lint               # golangci-lint, formatting included
just fix                # apply every fix golangci-lint can make
just integration-clean  # start ClickHouse, run integration tests, tear it down
```

If you want the database to stay up between runs:

```bash
just compose-up
just integration
just compose-down
```

`compose-down` deletes the named volume as well as the containers. That is on
purpose: ClickHouse only applies `deploy/clickhouse/init` to an empty data
directory, so a schema change silently does nothing if the old volume is still
there. Everything is scoped to this project, so no global `docker prune` is
ever involved.

There are no mocked databases anywhere. Integration tests run against real
ClickHouse, with real SQL and real rows.

## Design

The full design, the research behind it, and the decisions that are still open
are in [spec.md](spec.md), which indexes the rest of the spec under `spec/`.

## License

Apache-2.0, see [LICENSE](LICENSE).
