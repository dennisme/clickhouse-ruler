# Running it

Every flag the binary takes, and everything it exposes once it is up.

```bash
ruler run --rules ./rules --config ./rules/ruler.yaml
```

Where alerts go is a section of the operator's file rather than a flag,
because the credential that reaches it has to come from a file and never from
argv:

```yaml
# ruler.yaml
alertmanagers:
  - urls:
      - http://alertmanager-0:9093
      - http://alertmanager-1:9093
      - http://alertmanager-2:9093
    basic_auth:
      username: ruler
      password_file: /run/secrets/ruler/alertmanager
```

A file that is not valid YAML refuses to start, and so does a rules directory
that cannot be read. Every other finding is printed and the ruler runs anyway,
raising `clickhouse_ruler_problem` for anything that should have blocked the
merge, because a ruler that will not start pages nobody. A source failing the
user contract at error severity is refused on its own: its rules stop
evaluating and every other source carries on.

**`urls` names every member of one Alertmanager cluster, and there is no
balancer in front of it.** Members gossip and deduplicate identical
alerts, so every alert is posted to every member and the cluster's own
deduplication is what makes that safe. A balancer collapses it: it picks one
member, and a member partitioned from its peers accepts a page no other member
ever learns about, with nothing on either side reporting it because the POST
succeeded. Alertmanager's own documentation asks for the list rather than the
balancer, and this is the list.

**The list is one cluster, not two destinations.** Two unrelated Alertmanagers
given here are not two routes for the same alert: a send is delivered as soon as
one of them accepts it, so while the other is down its pages are silently not
re-tried. Two clusters that must both receive everything are two rulers.

**The credential is the cluster's, not an address's.** One `basic_auth` or one
`authorization` on the set is used for every url in it, because the set is the
cluster. The names are Prometheus' own, so an operator who has configured an
Alertmanager before reads familiar keys, and the secret itself is never in the
file: it comes from `password_file` or `password_env`, as a source's does.
Setting both of those is refused, as is setting `basic_auth` beside
`authorization`, since they write the same header.

A password in the URL is refused rather than ignored. Go's HTTP client would
turn it into an `Authorization` header and it would work, which is exactly the
problem: it would also be in the pod spec, the rendered chart manifest and any
dump that echoes argv.

**An Alertmanager that requires TLS is reached with a `tls_config`.** The
scheme is what turns it on, so every url in the set is `https://` once the
block is written, and material that no url can reach is refused rather than
ignored. A cluster behind a public CA needs the `https` url and nothing else:

```yaml
alertmanagers:
  - urls: [https://alertmanager-0:9093]
    tls_config:
      ca_file: /run/secrets/ruler/alertmanager-ca.pem
      cert_file: /run/secrets/ruler/alertmanager.pem
      key_file: /run/secrets/ruler/alertmanager-key.pem
```

The five fields are the five a source takes, with Prometheus' own names, and
they fail the same ways: an unreadable or empty file, a bundle with no
certificate in it, and a `cert_file` without its `key_file` are each an error
from `alertmanager/tls` naming the line. `insecure_skip_verify` keeps the
connection encrypted and stops anything identifying the server at the other
end of it, so it is an error that only a dated exemption clears, and it is the
one `alertmanager` check that does not refuse the start. See
[the alertmanager checks](checks/alertmanager.md).

**A rotated secret or CA costs a `SIGHUP`. A changed url costs a restart.** The
credential is re-read on every reload and swapped in place, so rotating it
disturbs nothing: no firing alert is re-posted and no resend timer is reset.
The url list is read once at startup, because each member owns a probe
goroutine and a metric series of its own. A reload that sees a different list
logs that a restart is needed and keeps delivering to the endpoints it already
has, so an operator who edited the list is told the running process still has
the old one.

If the block does not read cleanly on a reload, the material already running is
kept. A secret file that is briefly unreadable is what a rotation looks like
half way through, and applying what that resolves to, which is nothing, would
turn a working delivery path into a 401 on every send. An unreadable `ca_file`
is the same shape: what it resolves to is the host's trust store in place of
your private CA.

TLS material sits on the reload side, in two different ways. A replaced
`cert_file` and `key_file` need nothing at all, because the pair is read at each
handshake. A replaced `ca_file` needs the signal: the roots live inside a built
HTTP transport, so a reload rebuilds each client's transport, finishes the sends
and probes already in flight on the one they started with, and closes the
connections the old transport had pooled so none of them keeps serving on roots
your file no longer names. The line `applied the changed alertmanager
tls_config` is printed only when the material moved.

**Nowhere to send refuses the start.** No `alertmanagers` block, no urls in it,
or any `alertmanager/*` finding at error severity, and `ruler run` exits 2
naming the file. This is not one of the findings that loads and raises
`clickhouse_ruler_problem`: those cost one rule behaving as written, where this
costs every alert in the checkout.

`ruler check` stays offline unless it is asked not to. `--online` runs the
checks that need a connection, connecting as each source's own user, because
that user is what is being checked. Those read metadata and no rows.

`--sample` additionally runs the checks that read rows, and implies `--online`.
It is separate because a connection is not consent: asking ClickHouse what a
query is costs a parse, while sampling runs statements against the source's
data. The sample is bounded by `max-sample-rows` and reads as the source's own
user, so row policies apply to it.

## Checking one file

Paths after the rules directory narrow what is reported:

```bash
ruler check --config rules/ruler.yaml rules/ rules/payments/latency.yaml
ruler check --config rules/ruler.yaml rules/ rules/payments/
```

The rules directory is still required, and the whole tree is still read. That
is the point: the severity a finding carries comes from the `policy.yaml` beside
the root and the team files above the rule, and a duplicate alert name is a
fact about the tree. Naming a subdirectory as the root instead reads a
different policy and misses those collisions, so a check can pass at your desk
and block in CI.

A path naming no rule file the loader read is an error, including the sources
file and a policy file, whose findings belong to the tree rather than to a
path. A run that quietly reported nothing would look exactly like a clean one.

Paths combine with `--changed-since`: asking for both reports the changed files
among the paths named.

## What a check exits with

| Code | What it means |
| --- | --- |
| `0` | nothing found, or nothing found at `error` severity |
| `1` | at least one `error`-severity finding |
| `2` | the command could not run: a bad flag, an unreadable rules directory, an unparseable operator's file |

Findings and failures are separated so a CI job can tell "your rules are wrong"
from "the tool could not run".

**A warning exits 0.** Only an `error`-severity finding fails the command, so
`ruler check` as a required status gates exactly the checks your policy sets to
`error`, and nothing else. Every check that warns by default, including
[`annotations/template`](checks/rule.md#annotations-template) and
[`rule/cost`](checks/rule.md#rule-cost), passes. Raising one to `error` in
`policy.yaml` is how it starts blocking, and doing that is the same decision as
refusing a ruler that reads the file: the loader and CI run the same checks at
the same severities.

**An unreachable cluster also exits 0.** A source `--online` could not connect
to, or a query it could not inspect, is a warning:
[`rule/inspect`](checks/rule.md#rule-inspect) per rule, and an inconclusive
[`source/privileges`](checks/source.md#source-privileges) per source. Nothing was
learned, which is not the same as nothing being wrong, and blocking on it would
let a network blip fail a deploy. A fork's pull request cannot reach a cluster at
all, by design. The consequence for a job reading only the exit code is that a
green check can mean the online checks did not run, so read the output, or fail
the job on `rule/inspect` where the cluster is meant to be reachable from CI.

## Replaying a rule over the past

`--backfill` replays every rule over a past range and reports how many alerts
it would have produced, which is
[`rule/alert-count`](checks/rule.md#rule-alert-count). It implies `--online`
and is its own flag rather than part of `--sample`: a sample is one read per
rule and a replay is one per window, so it is consented to separately.

```bash
ruler check --backfill --config rules/ruler.yaml rules/
ruler check --backfill --backfill-range 168h --backfill-step 5m \
  --config rules/ruler.yaml rules/
```

`--backfill-range` is how far back the replay reaches, 24h by default.
`--backfill-step` is the gap between the evaluations it replays, defaulting to
each rule's own group interval, which is the cadence its `for` timer is
measured in.

Both are durations on the command line rather than keys in `policy.yaml`,
because a policy ceiling is a whole number and these are spans. What does
belong in the policy file is the two ceilings: `max-alerts`, which the count is
measured against, and `max-rows-read`, which the whole replay's predicted total
is measured against before any of it runs.

## The cost table

`--summary` writes a markdown table of what every rule is predicted to read,
one row per rule and source. `-` writes it to stdout.

```bash
ruler check --online --config rules/ruler.yaml --summary cost.md rules/
```

```markdown
| File | Alert | Source | Rows | Interval |
| --- | --- | --- | --- | --- |
| rules/payments/latency.yaml | HighP99Latency | payments_prod | 4127000 | 30s |
| rules/payments/latency.yaml | HighP99Latency | payments_staging | 90000 | 30s |
```

It needs `--online`, because the numbers come from `EXPLAIN ESTIMATE` and only
the cluster can answer. Nothing is executed and no rows are read. `-` needs
`--format=text` and is refused otherwise, which is the same answer `--markdown
-` gets: in every other format stdout belongs to a machine, so a table in the
middle of it is read as annotations or breaks the JSON document. Name a path
instead and the format does not matter.

A rule that matches two sources is two rows, never one averaged: the same SQL
is cheap on staging and ruinous on production, and that gap is the row worth
looking at. The interval sits beside the count because the pair is the
sentence that matters, "4.1 million rows every 30 seconds". The numbers are
predictions, with the accuracy [`rule/cost`](checks/rule.md#rule-cost)
describes, and a cell that is not a count says why rather than saying zero.

Posting it is the workflow's job. The ruler writes a file, and a job that can
comment on a pull request already holds the token for it:

```bash
ruler check --online --config rules/ruler.yaml --summary cost.md rules/
gh pr comment "$PR" --body-file cost.md
```

[Checking a pull request](pull-requests.md) is the whole of that path: the
action that runs it, and what a fork can and cannot do, which is reach no
cluster and post nothing.

Every flag `ruler run` takes is read once at startup, so nothing in this table
is picked up by a reload: changing one needs the process restarted, which
[what a reload refuses](operations.md#what-a-reload-refuses) has the whole of.

| Flag | Default | What it does |
| --- | --- | --- |
| `--rules` | required | rules directory |
| `--config` | `ruler.yaml` | operator's file |
| `--policy` | `policy.yaml` beside `--rules` | policy file |
| `--listen` | `:9090` | address for `/metrics`, `/-/healthy`, `/-/ready`, and `/-/reload` when it is enabled |
| `--query-concurrency` | `8` | rule queries allowed against ClickHouse at once, across every group; `0` is unbounded. A source can set `max_concurrent_queries` to bound itself further inside this |
| `--recheck-interval` | `1h` | how often loaded rules are re-checked against recent data for the map keys they read, which no evaluation can see; `0` turns the pass off. One bounded query per rule per source, sharing `--query-concurrency` with evaluation |
| `--resend-interval` | `100s` | how often a still-firing alert is re-posted |
| `--resend-tolerance` | `4` | how many resend periods a firing alert stays valid for, so how many consecutive failed evaluations or sends it survives, and how long a resolved alert is retried for. `4` is Prometheus' own number. Minimum `2` |
| `--notification-queue-capacity` | `10000` | how many alerts may wait to be sent to Alertmanager before the oldest are dropped. The send runs off the evaluation goroutine, so this is what an Alertmanager outage fills instead of a group's interval. Prometheus' notifier bounds itself at the same number in the same unit |
| `--shutdown-timeout` | `30s` | how long shutdown spends finishing what is in progress: first the evaluations in flight, then whatever they left in the send queue |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error` |
| `--log-format` | `text` | `text` or `json`. The same lines with the same field names either way, so JSON is for a log pipeline that would otherwise parse structured output back out of text |
| `--enable-reload-endpoint` | off | serve `POST /-/reload`, which re-reads the same files `SIGHUP` does. For deployments where a signal cannot reach the process, such as a sidecar syncing rules into a shared volume |

## What it exposes

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
| `clickhouse_ruler_rule_group_interval_seconds` | gauge | `rule_group` |
| `clickhouse_ruler_rule_group_tick_delay_seconds` | histogram | `rule_group` |
| `clickhouse_ruler_alerts_active` | gauge | `rule_group`, `rule`, `state` |
| `clickhouse_ruler_alerts_sent_total` | counter | `alertmanager` |
| `clickhouse_ruler_alerts_send_failures_total` | counter | `alertmanager` |
| `clickhouse_ruler_notification_latency_seconds` | histogram | none |
| `clickhouse_ruler_notification_queue_length` | gauge | none |
| `clickhouse_ruler_notification_queue_capacity` | gauge | none |
| `clickhouse_ruler_notification_queue_wait_seconds` | histogram | none |
| `clickhouse_ruler_notifications_dropped_total` | counter | none |
| `clickhouse_ruler_alertmanager_last_probe_successful` | gauge | `alertmanager` |
| `clickhouse_ruler_rules_unmatched` | gauge | `rule_group` |
| `clickhouse_ruler_problem` | gauge | `rule`, `check`, `severity`, `team`, `file`, `source` |
| `clickhouse_ruler_source_problem` | gauge | `source`, `check`, `severity`, `file` |
| `clickhouse_ruler_config_reloads_total` | counter | `outcome` |
| `clickhouse_ruler_config_last_reload_successful` | gauge | none |
| `clickhouse_ruler_config_last_reload_timestamp_seconds` | gauge | none |
| `clickhouse_ruler_config_info` | gauge | `revision`, `rules_root` |
| `clickhouse_ruler_recheck_last_completion_timestamp_seconds` | gauge | none |
| `clickhouse_ruler_recheck_last_duration_seconds` | gauge | none |
| `clickhouse_ruler_recheck_sample_failures_total` | counter | `source` |
| `clickhouse_ruler_query_read_rows_total` | counter | `rule`, `team`, `source` |
| `clickhouse_ruler_query_read_bytes_total` | counter | `rule`, `team`, `source` |
| `clickhouse_ruler_query_memory_usage_bytes` | histogram | `rule` |
| `clickhouse_ruler_query_duration_seconds` | histogram | `rule`, `rule_group`, `team`, `source` |
| `clickhouse_ruler_query_queue_wait_seconds` | histogram | `source` |
| `clickhouse_ruler_query_concurrency_wait_seconds` | histogram | `rule_group` |
| `clickhouse_ruler_query_concurrency` | gauge | none |
| `clickhouse_ruler_queries_in_flight` | gauge | none |
| `clickhouse_ruler_build_info` | gauge | `version`, `revision`, `goversion` |

`clickhouse_ruler_rule_evaluations_total` and
`clickhouse_ruler_rule_evaluation_failures_total` count one evaluation of one
rule against one cluster. A rule selects sources by label, so a rule matching
four clusters makes four evaluations per tick, each with its own alert state and
its own way to fail. This is the one metric here that means something different
from the Prometheus ruler's metric of the same name, which has no cluster to
count per, and it is what makes the failure ratio in
[operations](operations.md#evaluation-failures) a share: counted once per rule, a
rule whose four clusters all failed read 4.0. A single-source ruler reads the
same number either way.

The two send counters are per endpoint, which is what `alertmanager` labels:
`clickhouse_ruler_alerts_sent_total` is what each member took, and
`clickhouse_ruler_alerts_send_failures_total` is the batches it refused. A
cluster with one member down reads as failures against that one label while the
others keep counting deliveries, and delivery as a whole is still working,
because one member accepting is enough and gossip carries the alert to the rest.

`clickhouse_ruler_notification_latency_seconds` holds deliveries Alertmanager
accepted, and it carries no `alertmanager` label because it times the whole
fan-out: a batch is posted to every endpoint at once, and a page is out once the
slowest of them has it. A delivery that failed took as long as
`--resend-tolerance` and the retry backoff say, which is the ruler's own
configuration rather than anything Alertmanager did, so it is counted by
`clickhouse_ruler_alerts_send_failures_total` and left out of here. Batches
attempted is this histogram's count plus that counter.

The four queue metrics are the delivery path in front of that. A send is handed
to one worker and posted from there, so the retry ladder no longer runs on a
group's goroutine: an Alertmanager outage fills
`clickhouse_ruler_notification_queue_length` instead of making a group miss
iterations. Read it against
`clickhouse_ruler_notification_queue_capacity`, which is
`--notification-queue-capacity` as a series so no expression has to hardcode it.
`clickhouse_ruler_notification_queue_wait_seconds` is time spent waiting and is
deliberately not part of the latency histogram above, which holds only the send
itself. `clickhouse_ruler_notifications_dropped_total` counts alerts dropped
because the queue was full, oldest first, or because a shutdown could not
deliver them in time; nothing was recorded as sent in either case, so the next
evaluation of that rule enqueues the same instances and the drop spends the same
`--resend-tolerance` a failed send spends.

The two problem gauges are the only metrics here not addressed to whoever
operates the ruler, and the only ones worth reading by their labels rather than
their value. `clickhouse_ruler_problem` is a rule that broke after it merged, so
it carries the `team` that owns it and the `file` to edit, the `check` names the
page explaining the finding, and `source` is the cluster it was found against, so
a rule broken on one of four clusters does not read like a rule broken on all
four. `clickhouse_ruler_source_problem` is the
operator's half: a source whose ClickHouse user no longer meets the contract the
checks rely on. Both are rebuilt from each pass rather than incremented, so a
series that disappears is somebody fixing something.
[Operations](operations.md#a-rule-that-broke-while-running) has the whole of how
to read them, including what clears them and what does not.

`clickhouse_ruler_build_info` is always 1 and exists for its labels: it says
which build each replica is running, which matters during a rollout that only
half landed. `version` is the release tag and reads `dev` for a binary built
outside a release. `ruler version` prints the same facts, plus whether the
tree was dirty, for anyone who can reach the binary.

`clickhouse_ruler_alertmanager_last_probe_successful` is the one delivery
series that exists before anything fires. The two send counters are labelled
`alertmanager`, so neither has a series until a send has been attempted, which
means a ruler pointed at a host that does not resolve has nothing to alert on
until the first page it fails to deliver. This gauge is filled by a probe of
Alertmanager's own `/-/ready` every 30 seconds, 1 when it answered and 0 when it
did not, so a wrong address is visible from startup. Each endpoint is probed on
its own timer and reports its own series, so a cluster with one member down says
which member, and one member that hangs delays nobody else's reading. The probe sends no alert
and touches no alert state, and it is deliberately not part of `/-/ready` here:
one Alertmanager serves every replica, so a readiness term would take a whole
deployment unready during a rolling restart of it.
[Operations](operations.md#the-alertmanager-not-answering) has what to watch.

The three reload metrics are about the files rather than the rules.
`clickhouse_ruler_config_reloads_total` counts attempts, `succeeded` or
`refused`, and is the only one that still shows a refusal somebody retried
before the next scrape; a startup load is not counted, so a restart does not
read as a reload. `clickhouse_ruler_config_last_reload_successful` is the last
attempt, so a reload the ruler refused holds it at 0 until one succeeds;
`clickhouse_ruler_config_last_reload_timestamp_seconds` is only stamped by a
load that succeeded, so it dates the configuration actually being evaluated.
[Operating the ruler](operations.md) has what a reload refuses and what survives
one.

**The re-check pass is on by default, and what it costs is arithmetic you can
do.** One bounded query per rule per matched source per interval: 800 rules each
matching four clusters is 3,200 queries an hour, roughly one a second, sharing
`--query-concurrency` with evaluation so it cannot outrun the cap your rules
already run under. Each one reads recent data for the map keys a rule uses and
nothing else.

It ships on because it is the only thing that sees a renamed OTel map key: a
query that still parses, still returns its columns, still succeeds on every tick
and matches nothing for ever. Off, it would be a check that does not exist on
every ruler whose operator never read this page. `--recheck-interval=0` turns it
off, and a longer interval is the answer if the reads are too many rather than
turning it off entirely.

The three re-check metrics are about the pass `--recheck-interval` runs, and they
exist because it is the one feed whose silence reads as good news: a renamed map
key is only ever found by sampling recent data, so a pass that stopped running
reports no findings and looks exactly like an estate where nothing was renamed.
`clickhouse_ruler_recheck_last_completion_timestamp_seconds` is read as
`time() - it` against the interval, and
`clickhouse_ruler_recheck_last_duration_seconds` is what the last pass cost,
which grows with the rule file while the interval does not.
`clickhouse_ruler_recheck_sample_failures_total` counts clusters that would not
answer, which is the operator's to fix and deliberately not a finding against a
rule author: the ruler could not ask, so it learned nothing about the rules
reading that cluster. All three are absent on a ruler started with
`--recheck-interval=0`.

`clickhouse_ruler_config_info` names the configuration itself, and is always 1
for its labels the way `clickhouse_ruler_build_info` is: that one says which
binary a replica runs, this one says which rules it loaded. `revision` is a hash
over the rule files and team policy files read from the rules root, each with its
path in the tree and its contents, truncated to twelve hex characters. It is a
hash and not a commit because the ruler fetches nothing, so nothing hands it a
sha, and because the hash is the better answer to whether a fleet agrees:
`count by (revision) (clickhouse_ruler_config_info)` reads 1 when every replica
is evaluating the same files, whatever each of them was told the commit was.
`rules_root` is the resolved root they came from, which is where a commit rides
along when a deployment named one.

The `rule_group` label, and the `file` label on `clickhouse_ruler_problem`, name
a rule by its path inside the rules tree: `payments/checkout.yaml:checkout`. Not
by where the tree is mounted, because a deployment publishes a revision by
pointing a symlink at a directory named after the commit, and an identity built
from that path would rename every series on every merge.

The four query cost metrics come from the ClickHouse driver's own callbacks
as the query runs, so they cost no extra query and do not depend on how long
`system.query_log` is kept. They are recorded whether or not the evaluation
succeeded, because a rule that trips a cap is the one worth finding. `team`
is read from the rule's labels and is empty when the author set none, which
is a rule nobody has claimed rather than one owned by nobody in particular.

`source` says which cluster a rule read from, because a rule evaluates against
every source its selector matches: without it one histogram folds every cluster
together and a team's bill cannot name the cluster it came from.

There are two concurrency limits and a metric for each.
`clickhouse_ruler_query_queue_wait_seconds` is the wait for a slot against a
source's own `max_concurrent_queries`. Only sources that set that limit appear,
so a series here means the limit exists and is being hit; a p99 climbing toward
the group interval means it is set too low.
`clickhouse_ruler_query_concurrency_wait_seconds` is the wait against
`--query-concurrency`, which every query passes through, labelled by group
because a group's rules all fire on one tick and a group with more rules than
there are slots queues against itself. Read it against
`clickhouse_ruler_query_concurrency`, the number of slots, and
`clickhouse_ruler_queries_in_flight`, how many queries are running or waiting
right now. Those two are `prometheus_engine_queries_concurrent_max` and
`prometheus_engine_queries` under another prefix.

`clickhouse_ruler_annotation_failures_total` is separate from the evaluation failures on
purpose: an annotation that will not render still pages, carrying a marker in
place of the annotation and the template error in `ruler_error` beside it, so it
is the rule author's bug rather than a failed evaluation. It counts how often
that happened; whether the rule is still broken is
`clickhouse_ruler_problem{check="annotations/template"}`.

Two are worth alerting on. `clickhouse_ruler_rule_group_iterations_missed_total` rising
means an evaluation took longer than its group interval, so alerts are silently
late. `clickhouse_ruler_rules_unmatched` staying above zero means this ruler loaded rules
that match none of its sources and will never evaluate them, which is expected
during a rollout and a problem if it persists.

Logs are `log/slog` on stdout, `--log-level` deep, and `--log-format` picks
whether they are text for a terminal or JSON for a pipeline. A failed query, a
failed send and a shutdown that gave up each write one line naming the rule
group, the rule and the source involved. One line per rule and per source,
never per alert instance. Passwords never reach a log; a ClickHouse driver
error is scrubbed before it is returned, keeping the address and database an
operator needs.

Usage errors and check findings are separate: unstructured, on stderr, because
those are for the person who typed the command.

What to watch, with the number that means trouble, what each log line means,
and the `system.query_log` queries that say what a rule cost are on
[the operations page](operations.md).
