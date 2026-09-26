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
# rules/sources.yaml
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
becomes the value. Check the files, then run:

```bash
ruler check --sources rules/sources.yaml rules/
ruler check --online --sources rules/sources.yaml rules/
ruler run --rules ./rules --sources ./rules/sources.yaml \
  --alertmanager http://localhost:9093
```

`check` stays offline unless asked otherwise: `--online` asks ClickHouse what
the query is and reads no rows, `--sample` reads rows and implies `--online`.
An error-severity finding refuses to start the ruler.

## Documentation

The manual is at
[dennisme.github.io/clickhouse-ruler](https://dennisme.github.io/clickhouse-ruler/).

- [How it works](https://dennisme.github.io/clickhouse-ruler/how-it-works/):
  the two kinds of file, who owns which, and which half of the guarantee is
  the database's job.
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

Working:

- `ruler check ./rules/`, as text or as GitHub workflow commands annotating a
  pull request diff. Correctness checks always block; convention checks
  default to warnings an operator raises in `ruler.yaml` or per source, and
  `--explain` names the file that set each one.
- Offline checks: rule files parsed with a line number on every finding and
  strict unknown-field rejection, eleven checks on rules and twelve on the
  sources file, whose secrets come from a file or the environment.
- `--online` checks, which read no rows: the query as ClickHouse itself parsed
  it rather than as text, the result columns it will really produce, and what
  one evaluation is predicted to read against configurable ceilings.
- `--sample`, the one check that reads rows, confirming the map keys a rule
  reads exist in recent data. A renamed OTel attribute silences an alert
  forever and nothing else catches it.
- The ClickHouse user contract: `source/privileges` probes each source's user
  for the table functions it must not reach, `readonly = 2`, a constraint
  behind every limit, and the grant on its own table.
- Per-source exemptions, with a stated reason and an expiry date that fails
  the build once it passes. Rule files cannot carry one.
- `ruler run`: groups ticked on their own intervals and staggered, rules and
  sources evaluated concurrently under a shared query limit, the alert state
  machine, annotation templating, Alertmanager delivery with a resend cadence
  and its own expiry, resolved alerts retried, and a shutdown that does not
  cut an evaluation off.
- Metrics on `/metrics`, `/-/healthy` for the process and a `/-/ready` that
  can fail, structured logs naming the rule and source behind every failure,
  a `log_comment` on every query for reading cost back out of
  `system.query_log`, and two Grafana dashboards in `deploy/grafana`.
- What each rule costs the cluster, per rule and per team: rows and bytes
  read, peak memory and query duration, taken from the driver as the query
  runs rather than from a follow-up query.
- A ClickHouse and Alertmanager compose stack, with an end to end test taking
  a rule from a file all the way to a delivered notification.

Not built yet:

- No backfill. `--sample` reads rows to confirm a rule's attribute keys exist,
  but how often a rule would have fired over the last week needs the query run
  across historical windows. Spec 7.4.
- No `ruler watch`, so rules are not reloaded without a restart.
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

Requires Go, Docker, and [just](https://github.com/casey/just). Run `just` for
every recipe; [AGENTS.md](AGENTS.md) has the layout and the conventions.

```bash
just init               # mise tool versions and pre-commit hooks
just check              # everything CI runs
just test               # unit tests with -race, no container needed
just integration-clean  # start ClickHouse, run integration tests, tear it down
```

No mocked databases anywhere: integration tests run against real ClickHouse,
with real SQL and real rows.

## Design

The full design, the research behind it, and the open questions are in
[spec.md](spec.md), which indexes the rest of the spec under `spec/`.

## License

Apache-2.0, see [LICENSE](LICENSE).
