# clickhouse-ruler

Alert rules for ClickHouse, defined as files in git, checked before they run,
evaluated against your cluster, and sent to the Alertmanager you already
operate.

Prometheus rule semantics. ClickHouse SQL instead of PromQL. No web UI, by
design. One service, not a platform.

!!! warning "Status: early"

    It takes a rule from a file to a delivered notification today, and the
    checks that read the query through a live database are in, but nothing
    has operated it for real. See the
    [status section](https://github.com/dennisme/clickhouse-ruler#status)
    before you depend on it.

## What is here

This site is the manual. The
[README](https://github.com/dennisme/clickhouse-ruler#readme) is the front
door and carries the status; everything longer than a screen lives here.

- **[How it works](how-it-works.md)** — the two kinds of file, who owns
  which, how a rule reaches a cluster, and which half of the guarantee is the
  database's job rather than a check's.
- **[Running it](running.md)** — every flag, and the metrics and logs it
  exposes.
- **[Operations](operations.md)** — what to watch with the number that means
  trouble, what each log line means, what refuses to start, and the
  `system.query_log` queries that say what a rule cost.
- **[Deployment topologies](deployment.md)** — five ways to run it, including
  what running more than one ruler actually costs.
- **[How it compares](comparison.md)** — SigNoz, ClickStack, Grafana,
  `sql_exporter`, and when to use one of them instead.

## The checks

Every finding the ruler reports names a check, and every check has a section
explaining what it rejects, what breaks when it fires, and how to fix a rule
that trips it.

- **[Every check](checks/index.md)** — the whole catalogue, what each one
  ships as, and whether an operator can change it.
- **[Rule checks](checks/rule.md)** — a rule file's identity, labels,
  annotations, timing, and the SQL it carries.
- **[Source checks](checks/source.md)** — reaching a cluster, the caps sent
  with every query, and the ClickHouse user behind it.
- **[File and policy checks](checks/policy.md)** — YAML that will not parse,
  and a policy file configuring something it cannot configure.

What a check ships as is generated from the same table the resolver reads, so
a page here cannot state a default the tool does not have.

## The reasoning

The [spec](https://github.com/dennisme/clickhouse-ruler/blob/main/spec.md) is
why each of these is the way it is: what is checked and why, where the line
between these checks and the database's own enforcement falls, and how
severity is configured.
