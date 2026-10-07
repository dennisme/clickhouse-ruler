# clickhouse-ruler

Alert rules for ClickHouse, defined as files in git, checked before they run,
evaluated against your cluster, and sent to the Alertmanager you already
operate.

Prometheus rule semantics. ClickHouse SQL instead of PromQL. No web UI, by
design. One service, not a platform.

> **Status: early.** It takes a rule from a file to a delivered notification
> today, and the checks that read the query through a live database are in,
> but nothing has operated it for real. See [Status](#status) before you
> depend on it.

## Why this exists

Prometheus alerts are files. They live in git, they get reviewed in pull
requests, and there is no other way to create one. That last part is what
makes alert standards hold up at scale.

Keeping ClickHouse alerts in git is a solved problem too, and it is worth
saying so plainly. SigNoz and Grafana both have operators with alert rule
CRDs and Terraform providers. If you already run one on Kubernetes, you may
not need this. Two gaps are left.

**You have to run a platform to get it.** If your data is already in
ClickHouse and your routing already goes through an Alertmanager you operate,
adopting an observability platform so one scheduled SQL query can page
someone is a lot of machinery for the job.

**Nothing looks inside the query.** This is the one with no workaround. Every
tool named above stores the SQL as an opaque string and hands it to the
database. A rule can lose its time bound and scan without limit on every
evaluation, read around the row policies meant to keep a team in its own
lane, or go silent forever because someone renamed an OTel attribute. All
three review perfectly as a diff. A rule is SQL, and an alerting tool that
never reads the SQL is checking the envelope rather than the letter.

So: the Prometheus model on top of ClickHouse, as one service rather than a
stack, with the query itself checked before it ever runs.

## Quick start

Two files, owned by different people. A source says what a cluster is, and
only the password lives outside the file:

```yaml
# rules/ruler.yaml
sources:
  - name: payments_prod
    labels: {team: payments, env: prod}
    address: clickhouse:9000
    database: otel
    username: ruler_payments
    password_file: /run/secrets/ruler/payments
    table: otel_traces
    timestamp_column: Timestamp
```

A rule names no cluster. It selects sources by label, and runs against every
one that matches:

```yaml
# rules/payments/latency.yaml
groups:
  - name: api-latency
    interval: 1m
    rules:
      - alert: HighP99Latency
        sources: {team: payments}
        expr: |
          SELECT ServiceName, quantile(0.99)(Duration) / 1e6 AS value
          FROM otel_traces
          WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
          GROUP BY ServiceName
          HAVING value > 1000
        window: 5m
        for: 5m
        labels: {severity: warning}
        annotations:
          summary: "{{ .ServiceName }} p99 is {{ .value }}ms"
```

One returned row is one alert instance: columns become labels, `value`
becomes the value.

That claim about naming no cluster holds literally while the matched sources
agree on their table and timestamp column names, since the rest of the query is
text you typed. Where they disagree, write `{{ .Table }}` and
`{{ .TimestampColumn }}` and each source supplies its own.

Check the files, then run:

```bash
ruler check --config rules/ruler.yaml rules/
ruler check --online --config rules/ruler.yaml rules/
ruler run --rules ./rules --config ./rules/ruler.yaml
```

`check` stays offline unless asked otherwise: `--online` asks ClickHouse what
the query is and reads no rows, `--sample` reads rows and implies `--online`,
and `--backfill` replays each rule over a past range and implies `--online`
too. Neither of the two that read rows implies the other.
A file the ruler cannot read refuses to start it. Every other finding loads and
is raised on `clickhouse_ruler_problem`, because a ruler that will not start
pages nobody.

## Install

A release archive, the container image, or `go install`. Every release carries
a `tar.gz` per platform and a `checksums.txt` covering all of them:

```bash
curl -sSLO https://github.com/dennisme/clickhouse-ruler/releases/download/v0.1.0/clickhouse-ruler_0.1.0_Linux_x86_64.tar.gz
tar -xzf clickhouse-ruler_0.1.0_Linux_x86_64.tar.gz
./ruler version
```

```bash
docker pull ghcr.io/dennisme/clickhouse-ruler:v0.1.0
go install github.com/dennisme/clickhouse-ruler/cmd/ruler@v0.1.0
```

Verifying the checksum, the bind mount the container needs, the unprivileged
user it runs as, and why a `go install` build reports its version as `dev` are
all on
[Install](https://dennisme.github.io/clickhouse-ruler/install/).

## Documentation

The manual is at
[dennisme.github.io/clickhouse-ruler](https://dennisme.github.io/clickhouse-ruler/).

- [How it works](https://dennisme.github.io/clickhouse-ruler/how-it-works/):
  the two kinds of file, who owns which, and which half of the guarantee is
  the database's job.
- [Rules over metrics
  tables](https://dennisme.github.io/clickhouse-ruler/metrics/): why a counter
  read the way a traces table is read fires forever, and the shapes that work.
- [Install](https://dennisme.github.io/clickhouse-ruler/install/): the three
  ways in, and how to tell which build you are running.
- [Checking a pull
  request](https://dennisme.github.io/clickhouse-ruler/pull-requests/): the
  action that gates a merge, and what a required status does not catch.
- [Running it](https://dennisme.github.io/clickhouse-ruler/running/): every
  flag, and the metrics and logs it exposes.
- [Operations](https://dennisme.github.io/clickhouse-ruler/operations/): what
  to watch with the number that means trouble, what each log line means, what
  refuses to start, and what a rule cost in `system.query_log`.
- [Deployment](https://dennisme.github.io/clickhouse-ruler/deployment/): the
  five topologies, including what running more than one ruler costs.
- [Checks](https://dennisme.github.io/clickhouse-ruler/checks/): a page per
  check family, linked from every finding and generated from the table the
  resolver reads, so a page cannot state a default the tool does not have.
- [How it compares](https://dennisme.github.io/clickhouse-ruler/comparison/):
  SigNoz, ClickStack, Grafana, `sql_exporter`, and when to use one instead.

## Status

Honest picture of what exists today.

Working, with the manual linked for each:

- **Checks in a pull request.** `ruler check ./rules/`, as text or as GitHub
  workflow commands annotating the diff. Rule and source files are parsed with a
  line number on every finding and unknown fields rejected outright. Correctness
  checks always block; convention checks are warnings an operator raises in
  `policy.yaml`, per source or per team directory, and `--explain` names the file
  that set each one. Exemptions carry a reason and an expiry that fails the build
  once it passes. `--summary` writes the cost table for a pull request comment.
  [Checks](https://dennisme.github.io/clickhouse-ruler/checks/).
- **A merge gate you can require.** A composite action,
  `dennisme/clickhouse-ruler/action@v1`, annotating the diff on the line at
  fault and keeping one summary comment up to date, checking only the rules a
  pull request affects. Everything it does is a flag the binary already has.
  [Checking a pull
  request](https://dennisme.github.io/clickhouse-ruler/pull-requests/).
- **Three tiers that read the cluster, each consented to on its own.**
  `--online` reads no rows: the query as ClickHouse itself parsed it, the columns
  it will really produce, whether every cluster a rule matched agrees on them,
  and what one evaluation is predicted to read against configurable ceilings,
  scaled to the cluster on a sharded one. `--sample` reads rows once, confirming
  the map keys a rule reads exist in recent data, which is the rename that
  silences an alert forever. `--backfill` replays a rule over a past range and
  reports how many alerts it would have produced against how many evaluations
  merely matched.
- **The ClickHouse user contract, probed rather than assumed.**
  `source/privileges` asks each source's user for the table functions it must not
  reach, `readonly = 2`, a constraint behind every limit, the grant on its own
  table, and the one system table a sharded cost needs.
  [How it works](https://dennisme.github.io/clickhouse-ruler/how-it-works/).
- **`ruler run`.** Groups ticked on their own intervals and staggered, rules and
  sources evaluated concurrently under a ruler-wide query limit with an optional
  per-source limit inside it, the alert state machine, annotation templating,
  Alertmanager delivery with a resend cadence and its own expiry, resolved alerts
  retried, and a shutdown that does not cut an evaluation off. `SIGHUP` replaces
  what is running, keeping the `for` timer of every pending alert and keeping the
  running version when a file cannot be read.
  [Running it](https://dennisme.github.io/clickhouse-ruler/running/).
- **Credentials and TLS on both ends, rotated without a restart.** A source and
  an Alertmanager cluster each take a password from a file or the environment
  rather than from the file in git, and a `tls_config` with Prometheus' own five
  field names. A client certificate and key are read at each handshake, so a
  certificate manager rotating the pair needs nothing signalled; a replaced CA
  needs a `SIGHUP`, and what a reload cannot read leaves the material already
  running in place rather than falling back to the host's trust store.
  [Running it](https://dennisme.github.io/clickhouse-ruler/running/).
- **An install, and the chain from a merge to a reload proven on a cluster.** A
  Helm chart with a values file per delivery, the git-sync sidecar that reloads
  the ruler and a ConfigMap mount for a small estate. A `kind` cluster carries a
  commit from a read only git repository through the sidecar and waits for the
  merged rule's group to evaluate and the reload gauge to read 1, with a refused
  reload asserted to leave the previous configuration running.
  [Deployment](https://dennisme.github.io/clickhouse-ruler/deployment/).
- **An operator surface.** Metrics on `/metrics`, `/-/healthy` for the process
  and a `/-/ready` that can fail, structured logs naming the rule and source
  behind every failure, a `log_comment` on every query for reading cost back out
  of `system.query_log`, what each rule cost the cluster per rule and per team
  taken from the driver as the query runs, and two Grafana dashboards in
  `deploy/grafana`.
  [Operations](https://dennisme.github.io/clickhouse-ruler/operations/).
- **What broke after it merged, reported to whoever owns it.** Every evaluation
  is compared against the one before it, and a slow timer beside it re-asks the
  one question no evaluation can: a renamed OTel map key leaves the query
  parsing, returning the same columns and matching nothing forever. A column
  dropped or retyped, two clusters that stopped agreeing, a query over its
  ceiling, a query that stopped running, an annotation template that will not
  render against a real alert, or a source that no longer meets the contract
  raises `clickhouse_ruler_problem` with the file to fix. It reports and
  never refuses, so the rule keeps evaluating and keeps paging.

Not planned: generating your Alertmanager route tree. That file is yours and
already under your own review policy, so writing into it is not this tool's
job. The rest of the non-goals are in [spec 5](spec.md#5-non-goals).

Limits to know about, accepted rather than waiting on work:

- **Rules over metrics tables are documented and proven, and nothing checks
  them.** The dev stack carries the OTel exporter's metrics `sum` and `gauge`
  tables, and the tests read what the collector wrote into them: a cumulative
  counter compared against a threshold keeps firing long after the condition
  passed and goes quiet at a restart, and the per-series delta beside it fires
  and then stops. [Rules over metrics
  tables](https://dennisme.github.io/clickhouse-ruler/metrics/) has both correct
  shapes, the wrong one, the two columns that decide which is which, and the
  window a delta needs. What no check does is notice the wrong one in a pull
  request, which is spec 12.1.

- A restart loses pending alert state, and that is the answer rather than a
  gap. `ActiveAt` is held in memory, so every alert part way through its `for`
  starts again, and one whose condition clears inside that second `for` never
  pages. Firing alerts survive, because Alertmanager holds them until the expiry
  on the last send. Keeping the state means granting the ruler somewhere to write
  and owning a schema and a retention policy for it, to buy back one `for` of
  latency on a restart, and Prometheus behaves the same way when its own state
  series is unavailable. So reload rather than restart: `SIGHUP` keeps pending
  alerts. See
  [operations](https://dennisme.github.io/clickhouse-ruler/operations/#what-a-restart-loses)
  and spec 11.

- Sharded clusters are handled, with two limits of their own.
  `skip_unavailable_shards` is pinned to `0`, and a two node stack with an
  unreachable shard proves what that buys: the evaluation fails rather than
  returning half the cluster's rows and resolving the alerts the missing shard
  held. `table:` is the table a rule reads, so it is the `Distributed` one, and
  the checks that read it act on that. A predicted cost is scaled to the cluster
  by the shard count from `system.clusters`, which the user contract grants, and
  is reported as not estimated where that count cannot be read rather than
  comparing one shard's number against a cluster's ceiling. The two limits are
  that `max_execution_time` and `max_memory_usage` are enforced per node, so the
  real ceiling on a cluster is per shard, and that `evaluation_delay` has to
  cover the slowest shard. A source names one endpoint, which is decided rather
  than missing: making it highly available is the operator's job, the same as the
  Alertmanager URL. See spec 6.9.

## Development

Requires Go, Docker, and [just](https://github.com/casey/just). Run `just` for
every recipe; [AGENTS.md](AGENTS.md) has the layout and the conventions.

```bash
just init               # mise tool versions and pre-commit hooks
just check              # everything CI runs
just test               # unit tests with -race, no container needed
just integration-clean  # start the stack, run integration tests, tear it down
just compose-up         # start the stack and leave it running
just compose-volume     # add background telemetry to a running stack
just compose-down       # tear it down, volumes and built images included
```

No mocked databases anywhere: integration tests run against real ClickHouse,
with real SQL and real rows.

The stack is two ClickHouse nodes, an OpenTelemetry collector, Alertmanager, the
ruler, Prometheus and Grafana. Rows reach the table the way they do in
production: a test posts spans as OTLP and the collector writes them, against
the trace schema copied verbatim from the collector's own ClickHouse exporter.
Grafana is on <http://127.0.0.1:3000> with the shipped dashboards provisioned, so
a panel can be looked at rather than reasoned about.

`just compose-volume` adds `telemetrygen`, which sends a constant stream of
spans so a check is read against a busy table rather than one holding only what
a test put there. It is opt-in because every assertion in the tree was written
against a table only tests write to.

[RELEASE.md](RELEASE.md) covers cutting a release: one `v*` tag publishes the
archives, the image, the chart and the floating major tag.

## Design

The full design, the research behind it, and the open questions are in
[spec.md](spec.md), which indexes the rest of the spec under `spec/`.

## License

Apache-2.0, see [LICENSE](LICENSE).
