# clickhouse-ruler

Alert rules for ClickHouse, defined as files in git, checked before they run,
evaluated against your cluster, and sent to the Alertmanager you already
operate.

Prometheus rule semantics. ClickHouse SQL instead of PromQL. No web UI, by
design. One service, not a platform.

!!! warning "Status: early"

    It takes a rule from a file to a delivered notification today, and nothing
    has operated it for real. See the
    [status section](https://github.com/dennisme/clickhouse-ruler#status)
    before you depend on it.

## What is here

This site documents the checks. Every finding the ruler reports names a check,
and every check has a section explaining what it rejects, what breaks when it
fires, and how to fix a rule that trips it.

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

## Everything else

The [README](https://github.com/dennisme/clickhouse-ruler#readme) covers
installing and running it. The
[spec](https://github.com/dennisme/clickhouse-ruler/blob/main/spec.md) is the
design: what is checked and why, where the line between these checks and the
database's own enforcement falls, and how severity is configured.
