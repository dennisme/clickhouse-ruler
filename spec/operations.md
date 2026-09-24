# Operating the ruler

Metrics, logging, the end to end test stack, and how this gets deployed.

Part of the [clickhouse-ruler spec](../spec.md). Section numbers are stable
and are what the code comments cite.

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
so existing dashboards and existing operator knowledge carry over. That is
about the suffix: `_rule_group_iterations_missed_total` means here what it
means there.

The prefix is deliberately ours. Every name is namespaced
`clickhouse_ruler_`, not `ruler_`, because `ruler` is a component name rather
than a product name and the ecosystem already uses it: Mimir and Cortex expose
`cortex_ruler_*`, Loki exposes `loki_ruler_*`. A series called
`ruler_alerts_sent_total` in a Prometheus scraping more than one thing does not
say whose it is. The `job` label answers that while you are looking at the
series, and stops answering it the moment a name is pasted into an alert
expression, a recording rule, or a screenshot in an incident channel, which is
where a metric name has to speak for itself. Carrying a Prometheus dashboard
over already means rewriting the prefix, so this costs nothing that was free
before.

Go runtime and process collectors come from `client_golang` defaults.

Evaluation:

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

A missed iteration means the evaluation took longer than the group interval.
It is the single most important operational signal here, because alerts are
then silently late.

`clickhouse_ruler_rule_evaluation_failures_total` counts evaluations that did not happen,
which is what the Prometheus metric it is named after counts. An annotation
that would not render is not one of those: the evaluation produced alerts and
they were delivered, carrying the template error where the annotation should be
(6.5). It gets its own counter rather than a label on this one, because a label
would make every carried-over dashboard query read high, and because the two
have different audiences: a failed evaluation is an operator's problem and a
broken template is the rule author's. `annotation` is a label worth having,
since it names what to fix and an annotation is static configuration rather
than anything data can multiply (8.3).

Alert state and delivery:

| Metric | Type | Labels |
| --- | --- | --- |
| `clickhouse_ruler_alerts_active` | gauge | `rule_group`, `rule`, `state` (pending, firing) |
| `clickhouse_ruler_alerts_sent_total` | counter | `alertmanager` |
| `clickhouse_ruler_alerts_send_failures_total` | counter | `alertmanager` |
| `clickhouse_ruler_notification_latency_seconds` | histogram | none |

`clickhouse_ruler_alerts_active` carries the group because an alert name may repeat
across groups (7.6), and without it two same-named rules would report into one
series. It remains a count per rule, never a series per instance (8.3).

`clickhouse_ruler_alerts_sent_total` counts alerts, not batches, so it reads the same way
as the Prometheus metric it is named after. The latency histogram already
carries a count per send, so there is no separate batch counter.
`clickhouse_ruler_alerts_send_failures_total` counts a failed batch once however many
alerts it held, because it delivered none of them.

ClickHouse query cost. Nothing else in this space exposes these, and they are
what make the guard rails in 6.7 observable rather than theoretical:

| Metric | Type | Labels |
| --- | --- | --- |
| `clickhouse_ruler_query_read_rows_total` | counter | `rule`, `team` |
| `clickhouse_ruler_query_read_bytes_total` | counter | `rule`, `team` |
| `clickhouse_ruler_query_memory_usage_bytes` | histogram | `rule` |
| `clickhouse_ruler_query_duration_seconds` | histogram | `rule` |

Source these from the ClickHouse Go driver's progress callbacks rather than
from `system.query_log`. The driver reports rows and bytes read during the
query itself, so there is no follow up query and no dependency on query log
retention.

These enable two things worth having: alerting on expensive alert rules, and
per team chargeback.

Validation and config, used by watch mode:

| Metric | Type | Labels |
| --- | --- | --- |
| `clickhouse_ruler_problem` | gauge | `rule`, `check`, `severity` |
| `clickhouse_ruler_rules_unmatched` | gauge | `rule_group` |
| `clickhouse_ruler_config_last_reload_successful` | gauge | none |
| `clickhouse_ruler_config_last_reload_timestamp_seconds` | gauge | none |

`clickhouse_ruler_problem` is the `pint` analog. `clickhouse_ruler_rules_unmatched` counts rules this
ruler loaded that match no source it holds, so it will never evaluate them
(6.10). Expected to be non-zero on a per-data-centre ruler reading a shared
repository, and expected to return to zero after a cluster rollout finishes.
Alerting on it staying raised is how the soft failure in 6.10 stops being
ignored: the check warns at authoring time, this catches the case where nobody
read the warning.

Of these four, only `clickhouse_ruler_rules_unmatched` exists. The other three belong to
`ruler watch`, and so does the query cost table above, which needs a driver
progress callback inside `internal/query`.

### 8.3 Cardinality rule

Label metrics by rule and group only. **Never by alert instance.**

A rule returning 10,000 rows produces 10,000 alert instances and must still
produce exactly one metric series per rule. Getting this wrong turns the ruler
into the cardinality problem it exists to avoid. The `clickhouse_ruler_alerts_active`
gauge is a count, not a series per instance.

### 8.4 Logging

A counter that moved says something failed. It does not say which rule, which
source, or what the database replied, and those are the three things an
operator needs before they can act. Logs are the other half of this section,
not a duplicate of it.

`log/slog` from the standard library, text output, on stdout. `--log-level`
takes `debug`, `info`, `warn` or `error` and defaults to `info`. An
unparseable level is refused at startup rather than defaulted. There is no
`--log-format` until somebody asks for one.

Two streams, two audiences. Usage errors, lint findings and the refusal to
start are CLI output, unstructured, on stderr: a person ran a command and the
command has something to say about what they typed. Everything the daemon says
once it is running is a log line, structured, on stdout.

What is logged:

| Level | Event | Fields |
| --- | --- | --- |
| info | ruler running | `rules`, `listen` |
| info | shutting down | `timeout` |
| error | rule evaluation failed against a source | `rule_group`, `rule`, `source`, `error` |
| error | sending alerts to alertmanager failed | `rule_group`, `rule`, `error` |
| warn | annotation template failed, the alert carries the error instead | `rule_group`, `rule`, `source`, `annotation`, `error` |
| error | metrics listener stopped | `listen`, `error` |
| warn | shutdown timeout expired with evaluations still running | `timeout` |

A shutdown that gives up is the only signal an operator gets that a query or a
send was cut off part way through, which is why it is logged rather than
returned silently. A shutdown also cancels evaluations already running, and
each cancelled source logs an evaluation failure like any other, because that
is what it is: the counter has always recorded it and the log now says so.

The cardinality rule in 8.3 is written about metrics and the same reasoning
holds for logs: one line per failed source and one per failed send, never one
per alert instance. A rule returning 10,000 rows that cannot be delivered
writes one line, not 10,000.

Credentials never reach a log. A ClickHouse driver error may echo connection
detail, so every error `internal/query` returns goes through `redact` first.
The address and the database survive, because an operator reading the line
needs them; the password does not.

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
  `clickhouse_ruler_problem` gauge. Catches rules that *became* broken after a schema
  change, which CI cannot. Alert on your alerts.

Built so far: rule and source parsing with tier 0 checks, the alert state
machine, the querier, annotation templating and the Alertmanager client, and
`ruler check` with configurable policy. `ruler` and `ruler watch` do not
exist: nothing calls the querier on an interval yet.

Next: the eval loop and hot reload, then tier 1 checks at load time, then tier
2, then tier 3 backfill, then watch mode.

The validation package is already re-runnable against loaded rules, so watch
mode is a caller rather than a rewrite.

### 10.1 Validation as something other people can use

Worth recording, because section 7 turns out to be the part nobody else has.

The checks are useful to anyone scheduling ClickHouse SQL, not only to this
ruler. Three shapes this could take without becoming a different project:

- A validating admission webhook for the SigNoz operator's `Rule` resources,
  which today validates the shape of the custom resource and not the SQL it
  carries.
- A CI validator over `terraform show -json`, for the Grafana provider, which
  keeps the alert query as opaque `model` JSON that the provider does not
  inspect.
- The `github` output format already emitted by `ruler check`, which needs no
  integration beyond running the binary.

This argues for keeping the check package free of assumptions about where a
rule came from. It already takes parsed rules and a policy rather than a
directory, so the cost of preserving that is low, and it is worth paying
before a fourth caller makes it expensive.

None of this is scheduled. It is written down so the interface does not drift
somewhere that makes it impossible.

### 10.2 Deployment topologies

The label mapping in 6.10 exists so that these are all the same binary with a
different sources file, rather than four products.

- **One ruler, one cluster.** Sources need no labels at all.
- **Ruler per data centre.** Each holds sources for its own clusters, so data
  is queried locally rather than across a link. A shared rules repository is
  read by all of them, and each evaluates the subset matching its sources.
  This is the topology that makes an unmatched rule normal rather than broken.
- **Central rulers, highly available.** Several rulers with the same sources
  file. Alertmanager already deduplicates identical alerts (6.5), so running
  more than one is mostly safe, and 12.3 is where the remaining sharp edges
  live.
- **Ruler per team.** A team runs its own, points it at its own sources, and
  consumes the central rules repository for the safety checks in section 7.
- **Ruler as a service.** The team operating ClickHouse owns the sources file
  and the clusters, and other teams contribute only rules. This is the split
  in 6.10: adding a cluster or a user is an admin change, writing an alert
  against one is not.

None of these need code that does not already exist, with one exception: a
rule matching several sources has to evaluate once per source, and its alert
instances must stay distinct per source. That is a fingerprint question
(6.3), and it is open, see 12.6.

### 10.3 Checking a pull request

Two decisions, both of which look like implementation detail and are not.

#### Changed-file filtering belongs in the binary

"Only checks rules changed in the pull request" is what this section has
always said, and it is easy to read as "the action passes the changed paths
to the checker". That is wrong, because the changed set of *files* is not the
affected set of *rules*:

- A change to `sources.yaml` can move a source's labels, so a rule in an
  untouched file stops matching, starts matching, or matches a different
  cluster (6.10).
- A change to `ruler.yaml`, or to a `checks:` block on a source, can raise a
  check, so a rule that warned yesterday blocks today (7.7).
- A source's `database`, `table` or caps changing alters what the tier 1
  checks conclude about rules nobody edited (7.3).

Computing which rules a change affects needs the ruleset binding and the
policy merge. Both live in the validation package, and reimplementing either
inside an action is the second validation path 7.1 exists to prevent. So the
expansion is the binary's job, and the action supplies only the base
reference and a checkout deep enough to reach the merge base.

It is also the reason this is not a CI-only feature. `ruler check
--changed-since origin/main ./rules/` answers the same question on a laptop,
before anything is pushed.

Three rules it has to follow:

- **The base is the merge base**, not the previous commit. A branch with
  several commits, or one that has been rebased, gets the wrong answer from
  `HEAD~1`.
- **Some paths force a full run.** A change to the sources file or to any
  policy file affects rules the diff does not name, so the filter widens to
  everything rather than trying to be clever about which rules a label change
  reached.
- **An unresolvable base checks everything and says so.** A shallow checkout
  with no merge base is a reason to do more work, never less. Filtering that
  silently checks nothing is the failure mode that makes a green build
  meaningless.

Filtering is off unless asked for. `ruler check` with no flag checks the whole
directory, because that is what the loader does at startup, and a CI run whose
scope quietly differs from the loader's is a rule that passes review and fails
to load.

#### The action is composite, and owns nothing but the wiring

7.1 chose a composite action in `action/`, consumed as
`dennisme/clickhouse-ruler/action@v1`. Holding to that, with two additions
that belong in the spec rather than in whoever writes it:

- **The download is verified.** A composite action that fetches a release
  binary and executes it is a supply chain step. Releases publish a checksums
  file; the action checks it before running anything.
- **Tags are disciplined.** The action version should equal the tool version,
  which means a release moves the floating major tag. Without that, everyone
  pinned to `@v1` runs whatever the tag pointed at the day they wrote it.

Not a Node action: it would add npm, a committed bundle, and a second
dependency tree to a repository whose entire dependency list is three Go
modules, and it would buy nothing that shell cannot do. Not a Docker action
either: an image pull per job for no isolation benefit, since what runs is a
static binary.

**What the binary does not do is talk to GitHub.** Inline annotations need no
API at all: `--format=github` writes workflow commands to stdout and GitHub
renders them on the diff. The summary comment in 7.10 does need the API, a
`pull-requests: write` token, and logic to update one comment in place rather
than appending one per push. That belongs in the action, which already runs
inside GitHub's own environment, and it keeps a GitHub client out of a service
whose dependencies are otherwise ClickHouse and Alertmanager.

The split that makes it possible: the binary can emit its findings as JSON,
and the action reads that to build the comment. The same output serves anyone
integrating the checks elsewhere (10.1), which is an argument for it existing
independent of the comment.

#### What the action exposes

Inputs are the flags `ruler check` already has — the rules path, the sources
file, the policy file, the output format, whether to run the online checks —
plus the base reference for filtering and whether to post a summary. Nothing
is invented for the action that the binary does not already support, so
anything achievable in CI is achievable by hand.

Permissions are worth documenting on the action rather than left to be
discovered: `contents: read` is enough for the offline checks and inline
annotations, and the summary comment additionally needs
`pull-requests: write`. A pull request from a fork gets neither a writable
token nor secrets, so it cannot run the online checks or post a comment. That
is correct behaviour, and 7.10 explains why `pull_request_target` is not the
way around it.
