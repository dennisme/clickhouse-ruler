# How it works

Two kinds of file, owned by different people.

**Sources** are operator owned. What a cluster is, where to connect, which
ClickHouse user to connect as, how far behind live data to evaluate, and the
cost caps. `address` is one endpoint, so put whatever already makes your
cluster reachable there: a managed service's hostname, a load balancer, or a
DNS name covering several nodes. A node's own address works and ties the rules
repository to your cluster's topology, which is a change you then make in two
places.

```yaml
# rules/ruler.yaml
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
    max_concurrent_queries: 4
```

Everything from `evaluation_delay` down can be left out. `max_rows` caps how
many alert instances one evaluation may produce; `max_execution_time` and
`max_memory_usage` are sent to ClickHouse as query settings, so the cluster
enforces the cost cap rather than the ruler. Those three have defaults.

`max_concurrent_queries` has none: it bounds how many of the ruler's queries
may be in flight against this cluster at once, inside the ruler-wide
`--query-concurrency` limit, so a cluster that has gone slow cannot hold every
slot while rules against healthy clusters queue behind it. Left out, the
source is bounded by the ruler-wide limit alone.

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
local development, and for mTLS, where the client certificate is what
ClickHouse authenticates.

**Connections are plaintext unless the source says otherwise.** `secure: true`
connects over TLS and verifies the server against the host's trust store, which
is all a managed service needs, ClickHouse Cloud included.

```yaml
  - name: payments_cloud
    address: abc123.eu-west-1.aws.clickhouse.cloud:9440
    database: otel
    username: ruler_payments
    password_file: /run/secrets/ruler/payments
    secure: true
    table: otel_traces
    timestamp_column: Timestamp
```

A cluster whose trust is not the default one uses `tls_config` instead, which
turns TLS on by itself:

```yaml
    tls_config:
      ca_file: /run/secrets/ruler/internal-ca.pem
      cert_file: /run/secrets/ruler/client.pem
      key_file: /run/secrets/ruler/client-key.pem
      server_name: ch-prod.internal
```

`ca_file` is the CA the server is verified against, and it replaces the host's
trust store rather than adding to it: a self-signed cluster is reached by
supplying its own CA and nothing else, and no public CA can then vouch for that
name. `cert_file` and `key_file` are the pair ClickHouse authenticates for
mTLS, and a source with them and no password is legal. `server_name` defaults to the host in `address`, so it is
only written when that host is not the name on the certificate. All four are
paths, never inline material, for the reason `password_file` is: a key pasted
into the operator's file is a key in a git history. A path that is wrong fails
`ruler check` rather than the first evaluation.

`insecure_skip_verify: true` turns verification off, and costs an `exempt`
entry for `source/tls-insecure` with a reason and a date. Giving the ruler the
self-signed certificate as `ca_file` is the fix that needs no exemption.

**A rotated client certificate needs nothing.** `cert_file` and `key_file` are
read at each TLS handshake rather than held, so a certificate manager that
writes a new pair over the same paths is picked up on the driver's next
connection. A replaced `ca_file` does need a reload, because the roots cannot be
re-read in place, and so does a replaced `password_file`. See
[rotating a credential or a certificate](operations.md#rotating-a-credential-or-a-certificate).

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

The query above names its table and its timestamp column literally, which is
the shorter thing to read and works while every matched source spells them the
same way. A selector spanning clusters that do not can write `{{ .Table }}` and
`{{ .TimestampColumn }}` instead, and each source supplies its own. The
database is never templated: it is set on the connection, so `FROM otel_traces`
already resolves per source. Those four are the only variables an `expr` may
read, and any other is a `rule/expr` finding.

An empty or missing selector matches nothing, deliberately: choosing a source
chooses the ClickHouse user the query runs as, so a rule should never reach a
cluster by leaving a field out. A rule that really should run everywhere
selects a label the operator put on every source.

Nothing is inferred from the directory. A path decides who reviews the file,
not what the alert is labelled, so an alert that wants `team: payments` says
so in its own labels.

`CODEOWNERS` then does the rest:

```text
/rules/ruler.yaml    @platform-team
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

**Metrics tables are in scope, and they read differently.** Every example on
this page queries a traces table, where a row is an event. The collector's
ClickHouse exporter writes metrics into tables of their own, where a row is a
reading on a series and a counter's `Value` is usually a running total rather
than something to compare against a threshold. A rule that reads one the way it
would read a traces table fires forever and goes quiet at a restart, so the
idioms have a page of their own: [rules over metrics tables](metrics.md).

**The query is checked, not just the file.** A rule is SQL, and SQL is where
the interesting failures live: a missing time bound that scans without limit,
a renamed OTel attribute that makes an alert silently stop firing, a table
function that reads around the row policies meant to contain a team. Putting
alerts in git does not address any of that, which is why every alternative in
[the comparison](comparison.md) scores no on it regardless of how its rules
are stored.

**Validation runs at load, not just in CI.** The same package backs
`ruler check` and the loader, so a rule that gets past CI still cannot run.
This is modelled on Cloudflare's `pint`, with one difference: `pint` can only
advise, because Cloudflare does not own Prometheus. We do, so we can enforce.

**Evaluation notices a rule going wrong on its own.** A rule can be correct
when it merges and wrong months later, because the schema moved and nobody
edited the file. Every evaluation already knows what its result looks like, so
each one is compared against the previous one: a column dropped, renamed or
retyped, two clusters that stopped agreeing, or a query that stopped running at
all, raises `clickhouse_ruler_problem` with the team and the file to fix. It is a
report and never a refusal, so the rule keeps evaluating and keeps paging while
somebody fixes it. What the comparison cannot see is a renamed OTel map key,
because the query still parses, still returns the same columns and simply matches
nothing, so that one question is re-asked against recent data on a slow timer of
its own, `--recheck-interval`.

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

## What an alert is labelled

An alert's labels are what Alertmanager routes on and what an annotation
template can read, so it is worth knowing exactly where each one comes from.
Four sources, applied in this order, each overwriting a name the one before it
set:

1. **The group's `labels`**, which every rule in that group starts with.
2. **The rule's own `labels`**, overlaid on the group's.
3. **The query's result columns**, every column except `value`. A query
   selecting `service_name` produces a `service_name` label, spelled and cased
   exactly as the column is.
4. **The source's `labels`**, which say where the evaluation actually happened.

Then `alertname` and `source` are written last, and nothing can overwrite
them.

The source wins over the query deliberately. Its labels state which cluster
answered, and a result column claiming otherwise is reporting something
untrue. `alertname` and `source` are last for a stronger reason: they are the
alert's identity, and an identity that query data could set is a routing
hazard.

The precedence is what happens at evaluation time, and it is mostly not what you
will meet, because a query that reaches for one of these names does not get as
far as being overwritten:
[`rule/protected-label`](checks/rule.md#rule-protected-label) refuses the file.
A column aliased `AS team` or `AS alertname` is an `error` that cannot be turned
down, so the collision is caught in the pull request rather than resolved
silently at 3am. What the order above really decides is the ordinary case: a
group label a rule overrides, and a source label that names the cluster.

Annotations are rendered over exactly this label set, plus `.value` for the
number the alert fired on. Nothing else is in scope, which is what
[`annotations/template`](checks/rule.md#annotations-template) is about.

## Sharded clusters and partial data

A rule writes its own `FROM`, so on a sharded cluster you name the Distributed
table and nothing else changes. The ruler does not rewrite your SQL, does not
expand a cluster name, and does not know how many shards answered. What it does
insist on is a complete answer.

ClickHouse can answer a distributed query whose shard is unreachable two ways.
By default it fails the query and names the node it could not reach. With
`skip_unavailable_shards = 1` it drops that shard, returns the rows the rest of
the cluster held, and reports success. The ruler sends the setting as `0` with
every query rather than inheriting whatever a profile says, and the reference
user in `deploy/clickhouse/init/02-ruler-user.sql` pins it `CONST` so a rule
cannot raise it back.

**A partial result is a wrong answer, not a smaller one.** Alert expressions are
aggregates: `count()` over three of four shards is a different number, and a p99
over a subset of the data is a different number, and a threshold comparison
cannot tell either of those from a healthy result. The rows that go missing are
alert instances, so they leave the state machine and their alerts resolve. A
shard outage would resolve exactly the alerts most likely to matter, during the
outage, with nothing logged.

So a missing shard fails the evaluation instead:

| | failing the evaluation | a partial result |
| --- | --- | --- |
| pending `for` timers | kept running | kept running |
| firing alerts | stay firing | **resolve falsely** |
| conditions on the missing shard | invisible, and reported | **invisible, silently** |
| what the operator sees | log, counter, `rule/execution` | nothing |

The failure is counted in
`clickhouse_ruler_rule_evaluation_failures_total`, logged as `rule evaluation
failed against a source` with what the database replied, and raised on
`clickhouse_ruler_problem` as `rule/execution` against the team that owns the
file. ClickHouse names the unreachable node in that message. Alert state is
untouched, so the next successful evaluation sees the `for` timer it would have
seen had the query never failed, and firing alerts keep being sent for as long
as the outage lasts. [Evaluation
failures](operations.md#evaluation-failures) is the operator side of this.

**Draining a node is not a missing shard.** A shard is a set of replicas, and
this only happens when every replica of one shard is unreachable. Restarting or
draining one replica fails over to another, and adding or removing a shard is a
change to the cluster definition rather than a shard that went missing. On a
cluster with one replica per shard, any node restart does take a shard away, and
the ruler treats that as the outage it is.

**There is no per-rule setting for tolerating it.** A rule selects sources by
label and runs against every cluster that matches, so how much of a cluster may
be missing is a property of that cluster rather than of an alert, and a rule
carrying the setting would apply it to clusters where it makes no sense.

Two rough edges remain on a sharded cluster, both in
[spec 6.9](https://github.com/dennisme/clickhouse-ruler/blob/main/spec/design.md):
`max_execution_time` and `max_memory_usage` are enforced by each node
independently, so the real ceiling is per shard rather than per query, and
`evaluation_delay` has to clear the insert lag of the slowest shard rather than
the average one. A shard lagging further behind than the delay allows returns no
error at all, because nothing is unavailable; its newest rows are simply not
there yet.
