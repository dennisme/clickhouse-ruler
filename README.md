# clickhouse-ruler

Alert rules for ClickHouse, defined as files in git, evaluated against your
cluster, and sent to Alertmanager.

Prometheus rule semantics. ClickHouse SQL instead of PromQL. No web UI, by
design.

> **Status: early. Not usable end to end yet.** See [Status](#status) before
> you try to run it.

## Why this exists

Prometheus alerts are files. They live in git, they get reviewed in pull
requests, and there is no other way to create one. That last part is what
makes alert standards hold up at scale.

Prometheus needs no permission system for rules because it has no write API.
The rule store is a directory. Git is the access control list. `CODEOWNERS` is
the role model. Pull request review is the approval workflow. You get
authorization by deleting the write path, not by building a permission system.

ClickHouse-backed observability stacks do not work this way. Alerts live in an
application database and are created by clicking around a web interface. There
is a REST API and a Terraform provider, so you can keep definitions in git,
but they write to that same database: the file is a client of the API rather
than the source of truth. Anyone with a token can still add an alert that
pages your on-call, and no diff ever shows it.

The bigger cost is what you have to run to get even that far. Every one of
those providers comes attached to a platform, so alerts as code means adopting
Grafana Alerting, or running SigNoz. That is a lot of machinery for one job
when your data is already in ClickHouse and your routing already goes through
an Alertmanager you operate. The missing piece is the thing in between, and it
should not cost a platform migration.

This project is the Prometheus model on top of ClickHouse, as one service
rather than a stack.

## What the alternatives are missing

|                                     | Rules in git   | ClickHouse SQL | Alertmanager | File is the only way to create a rule |
| ----------------------------------- | -------------- | -------------- | ------------ | ------------------------------------- |
| SigNoz                              | Terraform only | yes            | no           | **no**                                |
| ClickStack / HyperDX                | Terraform only | yes            | no           | **no**                                |
| Grafana OSS + ClickHouse datasource | yes            | yes            | yes          | **no**                                |
| sql_exporter + Prometheus           | yes            | metrics only   | yes          | yes                                   |
| clickhouse-ruler                    | yes            | yes            | yes          | yes                                   |

The last column is the one nobody else offers, and it is the reason to build
rather than adopt.

### SigNoz and ClickStack

Both have alerting. Both store alerts in their own application database and
expect you to create them in the UI. Neither has a rule file format, and
neither sends to an external Alertmanager. SigNoz vendors its own Alertmanager
fork internally.

Both have a Terraform provider, and SigNoz's alert docs point at theirs as the
infrastructure as code answer, so HCL in git is genuinely possible. What it
does not give you is ownership. The provider calls the same API and writes the
same mutable rows, so anyone with a token can still edit the rule out from
under your file, and no diff records it. Your file describes a rule; it does
not own it. The ClickStack providers are ClickHouse Cloud only on top of that.

Locking the API down does not rescue it either. SigNoz's fine-grained access
control requires "an active SigNoz license" and is Cloud and Self-Hosted
Enterprise only, currently in beta. Same bind as Grafana OSS below: the escape
hatch is real, and it is not in the free edition.

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
opposite of the model here, where the directory is the ownership boundary and
one path decides both the `CODEOWNERS` reviewer and the `team` label. Nothing is synced into a database, so there is no connection to
run out of and no cap on team directories.

If managing rules this way is the plan, the wider tooling is uneven: the
Terraform provider is the mature path, the Ansible collection is Cloud only,
the Operator does not list alerting among its resources, and the Crossplane
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
provisioning already makes rules read only, the ClickHouse datasource already
runs real SQL, and Grafana already forwards to an external Alertmanager. Three
of the four properties are yours, and the fourth is a policy problem inside an
install you have already tuned. Use what you have.

The case for a separate service gets stronger the further you are from that:
when Grafana is not in the path at all, when the install is large enough that
folder permissions stop being a workable control, or when adding a whole
observability platform is a bigger change than adding one service.

## How it works

Two kinds of file, owned by different people.

**Sources** are operator owned. Where to connect, which ClickHouse user to
connect as, how far behind live data to evaluate, and the cost caps.

```yaml
# rules/sources.yaml
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

Everything from `evaluation_delay` down has a default and can be left out.
`max_rows` caps how many alert instances one evaluation may produce; the other
two are sent to ClickHouse as query settings, so the cluster enforces the cost
cap rather than the ruler.

Only the password lives outside the file. The username is not a secret and is
deliberately in plain sight: it is the tenancy boundary, so a reviewer has to
be able to see that `payments` connects as `ruler_payments` and not as
something with wider grants.

Use `password_env: SOME_VAR` instead if a file does not suit. Setting both is
an error rather than a precedence rule, because when the two disagree one of
them is stale and quietly picking either can authenticate with a credential
that was supposed to have been rotated away. No password at all is fine for
local development and for mTLS.

**Rules** are author owned, and reference a source by name.

```yaml
# rules/payments/latency.yaml
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

`CODEOWNERS` then does the rest:

```text
/rules/sources.yaml    @platform-team
/rules/payments/       @payments
```

Four things to notice.

**The directory owns the page.** `team` comes from the rule file's path, so
`rules/payments/` pages payments without anyone writing it down. A rule
may override it with
an explicit `team:` label, because one team running operations for another
team's service is a real arrangement. What a rule may not do is produce `team`
from a result column: the Alertmanager route tree is generated from the files,
and a value that only exists at query time has no route.

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

**Validation runs at load, not just in CI.** The same checks are meant to run
in `ruler check` and inside the ruler itself, so a rule that gets past CI
still cannot run. The package is shared already; the binary is not built yet. This is modelled on Cloudflare's `pint`, with one difference:
`pint` can only advise, because Cloudflare does not own Prometheus. We do, so
we can enforce.

## Status

Honest picture of what exists today.

Working:

- Rule file parsing, with line numbers on every finding and strict unknown
  field rejection
- Ten offline checks: eight on a rule file, plus the two that need the whole
  directory (the source exists, and the query does not set a protected label)
- Sources file parsing, with secrets read from a file or the environment, and
  eleven checks
- The alert state machine: pending, firing, resolved, `for`, `keep_firing_for`,
  per-instance identity
- Running a rule against real ClickHouse and getting alert samples back
- Loading a rules directory, deriving each rule's owning team from its path,
  and resolving its source by name
- Annotation templating, and sending to Alertmanager
- A ClickHouse and Alertmanager compose stack, with an end to end test that
  takes a rule from a file all the way to a delivered notification

Not built yet:

- No command line tool. There is no `ruler` binary to run.
- No scheduler. Nothing calls the querier on an interval, so evaluation has to
  be driven by hand.
- No resend cadence, so a firing alert is not refreshed and will expire.
- No `/metrics` endpoint.
- No Alertmanager route tree generation.

Known gaps that will change:

- Validation is stricter than it should be, and not configurable. `team`,
  `severity`, `summary` and `runbook_url` are all required at error severity,
  so a rule that would run perfectly fails to load if it omits one. Pointing
  the loader at a flat directory of rule files fails for the same reason,
  because `team` is derived from a subdirectory. Spec 7.6 and 7.7 decide how
  this becomes operator-configurable, defaulting to warnings.
- Sharded clusters are not handled. `skip_unavailable_shards` is not pinned,
  so a dead shard can silently resolve alerts instead of failing the
  evaluation. See spec 6.9.

## Development

Requires Go, Docker, and [just](https://github.com/casey/just). Run `just` on
its own to list every recipe.

```bash
just init               # mise tool versions and pre-commit hooks
just test               # unit tests with -race, no container needed
just lint               # golangci-lint
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
are in [spec.md](spec.md).

## License

Apache-2.0, see [LICENSE](LICENSE).
