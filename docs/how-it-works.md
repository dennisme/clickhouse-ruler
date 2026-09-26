# How it works

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
[the comparison](comparison.md) scores no on it regardless of how its rules
are stored.

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
