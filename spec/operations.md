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
- `POST /-/reload`, only when `--enable-reload-endpoint` is set

There are no rule create, update, or delete endpoints, and there never will be.
That is section 4 restated as an API decision. The absence of a write path is
the security model.

**`POST /-/reload` is not a write path**, which is why it can exist beside that
sentence. It re-reads the same files from the same paths that `SIGHUP` re-reads,
through the same loader and the same checks (7.1), and a caller supplies nothing:
no body is read, and a request cannot name a rule, a file or a source. What it
can do is make the ruler read the disk at a moment the caller chose, which is
what a signal already does.

It exists because a signal is not deliverable everywhere the ruler runs. A
sidecar that syncs a rules repository into a volume has the files and no way to
signal the process beside it without a shared process namespace or an exec into
the container, and both of those are larger holes than one endpoint that reloads.
This is the same reason Prometheus has it, and it is off by default for the same
reason theirs is: an endpoint that makes a process re-read its disk is a lever
worth opting into rather than one every deployment carries.

The response says what happened, because a caller that has to read the ruler's
logs to find out whether its own request worked is no better than the signal it
replaced. 200 with `reloaded` when the new files are running, 500 with the
refusal's reason when they are not, which is the same reason the log line
carries. A refused reload leaves the running configuration alone (7.6), so the
failure is a report rather than an outage.

One reload at a time. The endpoint's request is handed to the same loop that
serves `SIGHUP` rather than reloading on the HTTP handler's goroutine, so a
signal and a request arriving together are two reloads in sequence rather than
two loaders racing over one set of queriers.

**The two health endpoints answer different questions, and today they do not.**
Both return 200 unconditionally, which makes them the same endpoint written
twice. What each has to mean:

- `/-/healthy` is about the process. It is alive and its listener is serving,
  which is what an unconditional 200 already says. Correct as it stands.
- `/-/ready` is about whether sending traffic here is useful, and that is a
  different question: rules loaded, and at least one source answering. A ruler
  that cannot reach any ClickHouse is running perfectly and evaluating nothing.

The distinction is not decoration, because the two are wired to different
things. A failed `/-/healthy` gets a process restarted; a failed `/-/ready`
takes it out of a load balancer or stops a rollout. A readiness probe that
cannot fail turns a deployment of a ruler that cannot reach its cluster into a
successful one, and 10.2's highly available topology depends on that
distinction: with several rulers behind one deployment, readiness is what stops
a rollout replacing working replicas with broken ones.

Readiness is deliberately not a source-by-source answer. One unreachable
cluster out of twelve is a finding for `source/privileges` and the evaluation
failure counters, not a reason to declare the whole ruler unfit, and a probe
that flaps with any cluster's availability gets disabled by whoever is on call.

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

Nothing here carries the group's configured interval, and nothing measures how
late a tick started when it did not overrun one. Both are cadence questions an
operator asks in the terms of this table and cannot get answers to from it, and
8.8 says what they need.

A missed iteration means the evaluation took longer than the group interval.
It is the single most important operational signal here, because alerts are
then silently late.

`clickhouse_ruler_rule_evaluation_failures_total` counts evaluations that did not happen,
which is what the Prometheus metric it is named after counts. An annotation
that would not render is not one of those: the evaluation produced alerts and
they were delivered, carrying a marker where the annotation should be and the
template error in `ruler_error` beside it (6.5). It gets its own counter rather than a label on this one, because a label
would make every carried-over dashboard query read high, and because the two
have different audiences: a failed evaluation is an operator's problem and a
broken template is the rule author's. `annotation` is a label worth having,
since it names what to fix and an annotation is static configuration rather
than anything data can multiply (8.3).

`clickhouse_ruler_problem` carries the cluster the finding was found against, so
a rule broken on one of four clusters does not read like a rule broken on all
four, and so a pass that reached one cluster can rebuild that cluster's series
without touching another's. Cardinality is rules times checks times the sources
each rule matched: bounded by the files, and nothing data can multiply (8.3).
`rule/source-schema` carries an empty `source`, because a comparison between
clusters belongs to none of them.

The counter stays now that the same failure also raises `annotations/template` on
`clickhouse_ruler_problem`, because the two answer different questions. The
counter says how often it happened and which annotation, and it never goes down.
The gauge says whether it is still happening and whose rule it is, and it clears
on the pass where the source that raised it rendered clean (6.5). An alert belongs on the gauge; the counter is what
a dashboard plots beside the evaluations that produced it.

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
| `clickhouse_ruler_query_read_rows_total` | counter | `rule`, `team`, `source` |
| `clickhouse_ruler_query_read_bytes_total` | counter | `rule`, `team`, `source` |
| `clickhouse_ruler_query_memory_usage_bytes` | histogram | `rule` |
| `clickhouse_ruler_query_duration_seconds` | histogram | `rule`, `rule_group`, `team`, `source` |
| `clickhouse_ruler_query_queue_wait_seconds` | histogram | `source` |
| `clickhouse_ruler_query_concurrency_wait_seconds` | histogram | `rule_group` |
| `clickhouse_ruler_query_concurrency` | gauge | none |
| `clickhouse_ruler_queries_in_flight` | gauge | none |

Source these from the ClickHouse Go driver's progress callbacks rather than
from `system.query_log`. The driver reports rows and bytes read during the
query itself, so there is no follow up query and no dependency on query log
retention.

These enable two things worth having: alerting on expensive alert rules, and
per team chargeback, both per cluster. 8.8 is why `source` is on them and what
each label costs.

The last three are the exception to the `rule` and `team` labelling above,
because they measure the concurrency limits in 6.11 rather than what a rule
cost. There are two limits and they are read differently.

`clickhouse_ruler_query_queue_wait_seconds` is the per-source one. Queueing
there is a property of the cluster the limit protects: every rule against a
saturated source waits, and which rule happened to wait says nothing about what
to change. Only sources that set `max_concurrent_queries` reach it, so a series
here means a limit exists and is being hit.

`clickhouse_ruler_query_concurrency_wait_seconds` is the ruler-wide one, and it
is labelled by group because that is who pays for it: a group's rules all fire
on one tick, so a group holding more rules than the cap has slots queues against
itself. `clickhouse_ruler_query_concurrency` is the cap that wait is read
against, because the wait alone does not say how many slots it was queueing for,
and `clickhouse_ruler_queries_in_flight` is what it is being read against right
now, counting queries running or waiting. The last two are
`prometheus_engine_queries_concurrent_max` and `prometheus_engine_queries` by
another prefix, so both the names and the reading carry over. 8.8 is why the
three exist.

Validation and config:

| Metric | Type | Labels |
| --- | --- | --- |
| `clickhouse_ruler_problem` | gauge | `rule`, `check`, `severity`, `team`, `file`, `source` |
| `clickhouse_ruler_source_problem` | gauge | `source`, `check`, `severity`, `file` |
| `clickhouse_ruler_rules_unmatched` | gauge | `rule_group` |
| `clickhouse_ruler_config_last_reload_successful` | gauge | none |
| `clickhouse_ruler_config_last_reload_timestamp_seconds` | gauge | none |

`clickhouse_ruler_problem` is the `pint` analog, and it is aimed at somebody
other than the operator. A rule that broke under a schema change is fixed by
whoever owns the query, so the labels have to say whose it is and where to edit:
`team` from the rule's effective labels, empty when nobody claimed it for the same
reason the cost metrics leave it empty (8.2), and `file` so the finding names a
path rather than a rule somebody then has to grep for. `check` is what makes it
actionable at all, because every check has a page and every finding links to it
(7.8), so the annotation on an alert built from this gauge lands the owner on an
explanation instead of on our dashboard.

`clickhouse_ruler_source_problem` is the same instrument for the other audience,
and it is a second gauge rather than a label on the first. `source/privileges`
reports a user that does not meet the contract in 6.7.2, which is nothing about a
rule: no rule is broken by a missing grant, the fix is a grant, and the person
holding the sources file is the person who can make it. Three things follow from
that and each of them says split:

- **Different receiver.** A rule finding is routed on `team` and a contract
  finding is routed to whoever operates the ruler, so there are two alerts with
  two receivers whichever way this is modelled. The saving of one expression was
  the only argument for one gauge, and it was never real.
- **Different labels, both fully populated.** A contract finding has no rule to
  name and a rule finding names its source inside its own text, because making the
  source a dimension there would multiply the cardinality below by the sources a
  rule matched. One gauge means every series of both carries a permanently empty
  dimension and a paragraph here explaining which.
- **Different lifecycle.** The rule feeds rebuild per pass, scoped per check, on
  the evaluation's clock and the re-check timer's. The contract rebuilds per load,
  which is the cadence 6.7.3 already gives it. Two clearing cadences on one vector
  is a trap for whoever adds the next feed.

What they keep in common is `check`, so a finding on either still links to the
page that explains it (7.8), and `severity`, so either still says what the same
finding would do in CI.

Cardinality is rules times checks for the first and sources times contract
assertions for the second, bounded by the rules loaded and by the sources file,
and both are rebuilt rather than incremented: findings that went away have to stop
being series or the alert never clears. That makes a failed pass dangerous in
the other direction, because blanking the gauge because the ruler could not ask
would resolve every finding at once and read as a fix. A pass that fails leaves
the previous answer standing and says so through the failure metrics instead.

**We do not route it.** The tree is the operator's (6.5), so what ships is the
expression, an explanation of what raising it means and who fixes it, and
nothing that writes into their Alertmanager. Leading a horse to water is the
whole of the offer.

`clickhouse_ruler_rules_unmatched` counts rules this
ruler loaded that match no source it holds, so it will never evaluate them
(6.10). Expected to be non-zero on a per-datacenter ruler reading a shared
repository, and expected to return to zero after a cluster rollout finishes.
Alerting on it staying raised is how the soft failure in 6.10 stops being
ignored: the check warns at authoring time, this catches the case where nobody
read the warning.

Of these five, the two problem gauges are the ones fed from somewhere other than
the loader. `clickhouse_ruler_source_problem` is fed by the contract check, per
source at startup and on a reload, which is the cadence 6.7.3 already gives it, so
publishing it costs no extra query. `clickhouse_ruler_problem` is fed from three
places, and 10.4 is why: most of what
it reports is drift the evaluation can see for free by comparing itself against
the last one (6.3.2), and the rest is `rule/attribute-key`, which needs its own
query on its own timer. One finding on the evaluation feed is not drift at all:
`annotations/template` is raised again at runtime for a template that would not
render against a real alert, which the evaluation is the first thing able to know
(6.5). It belongs on this gauge for the reason the drift findings do, that the
author owns the fix and the team and the file are the only way to reach them, and
it keeps the name the pull request used so one setting covers both. The third is the load feed, which carries the fixed
checks that no longer refuse a reading (7.6), so a file that merged past the
checker says so instead of running silently. Each feed rebuilds only the checks it
owns, so a finding answered on one clock is not blanked by a pass on another. The reload pair exists
because `SIGHUP` reloads the files, and the two deliberately do not say the same
thing. `clickhouse_ruler_config_last_reload_successful` is about the last
attempt, so a refused reload leaves it at 0 until one succeeds, which is the
alert: the rules that are running are valid and nothing about them looks wrong,
so a ruler running last week's rules is invisible otherwise.
`clickhouse_ruler_config_last_reload_timestamp_seconds` is about the
configuration being evaluated, so a refused reload leaves it alone. The query it
exists for is `time() - clickhouse_ruler_config_last_reload_timestamp_seconds`,
read as how old the running rules are, and stamping it on a refusal would answer
that with the moment the ruler declined to change anything.

The query cost table above is read from the driver's callbacks as the query
runs. Progress packets carry what each block read rather than a running
total, so they are added; memory arrives as a per-thread gauge, so the
query's cost is the highest any thread reached. `team` comes from the rule's
effective labels and is empty when the author set none, which is left
visible: chargeback that hides what is unattributed is chargeback nobody can
reconcile.

Cost is recorded whether or not the evaluation succeeded, because a rule that
trips a cap is the one an operator is looking for. What it records is
whatever the server reported before the failure, which is nothing when the
query was refused outright: a result overflow throws before the first
progress packet, so that evaluation reports a duration and no rows. Timeouts
and memory caps, which are the expensive rules, report what they read.

Build:

| Metric | Type | Labels |
| --- | --- | --- |
| `clickhouse_ruler_build_info` | gauge | `version`, `revision`, `goversion` |

Always 1, one series per process, where the labels are the whole payload.
`prometheus_build_info` and `cortex_build_info` are the same shape, so a
version panel carried over works by changing the metric name. It answers
which build each replica is running, which `ruler version` answers only for
somebody who can reach the binary, and that is the question during a rollout
that half landed.

`version` is the release tag, stamped at build time, and reads `dev` for
anything built outside the release pipeline. `revision` and `goversion` come
from the toolchain's own build record rather than from a second stamp, so
there is one symbol a release can get wrong instead of three. Whether the
tree was dirty is not a label: a released binary never is, so it would only
distinguish one developer's laptop build from another's.

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
| info | rule matched no source | `rule_group`, `rule`, `file`, `team` |
| info | shutting down | `timeout` |
| error | rule evaluation failed against a source | `rule_group`, `rule`, `source`, `error` |
| error | sending alerts to alertmanager failed | `rule_group`, `rule`, `error` |
| warn | a rule broke while running | `rule_group`, `rule`, `check`, `severity`, `team`, `file`, `feed`, `problem`, and `error` on an `annotations/template` finding |
| error | metrics listener stopped | `listen`, `error` |
| warn | shutdown timeout expired with evaluations still running | `timeout` |

One line per unmatched rule, at startup and on every reload, because
`clickhouse_ruler_rules_unmatched` is a count per group and a count cannot be
read back into names. The names are in the loader's findings too, under
`rule/source-match`, but those are CLI output printed once (7.6) and an operator
reading the gauge weeks later has neither the terminal nor the checkout. `info`
rather than `warn`: on a ruler per datacenter reading a shared repository this is
the normal state, and the check at authoring time already decided how loud it is.

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

### 8.5 Finding the ruler's queries in ClickHouse

Half of operating this thing happens on the other side of the connection. A
rule that times out, reads more than the estimate predicted, or trips a memory
cap leaves its evidence in `system.query_log` on the cluster, not in anything
the ruler exposes, and 8.2 cannot help: a counter says an evaluation failed,
and the query text, the rows read and the peak memory are all over there.

Today the only way to pick those queries out of `query_log` is the source's
username, which fails exactly when it matters. A username identifies a source,
so it cannot separate one rule from another, and 10.2's ruler-per-team and
ruler-as-a-service topologies deliberately share a user across many rules.

**So every query the ruler sends carries a `log_comment`.** ClickHouse stores
that setting per query in `system.query_log`, which turns "what did this rule
cost last night" into a `WHERE` clause. It carries the rule and its group, not
the SQL and not the alert's labels: the query text is already in the log, and
label values are data, which would put unbounded cardinality into a column
operators group by (8.3 applies here too).

It is a query setting rather than a comment in the SQL, so it cannot change
what runs, and it sits beside the caps in 6.7 that every evaluation already
sends. The sampling and inspection queries carry it as well, since a check that
read more than expected is the same question asked at check time.

### 8.6 Dashboards

Metric names that carry over (8.2) are not the same as somebody having
something to look at. Two dashboards ship in `deploy/grafana/dashboards`,
because there are two audiences and one dashboard for both serves neither.

- **Operations.** Is the ruler doing its job: missed iterations first, because
  that is 8.2's most important signal, then evaluation failures, evaluation
  duration against the group interval, staleness of the last evaluation, send
  failures and notification latency. Then the three things about the process
  rather than about an evaluation: whether the last reload was accepted and when
  the running configuration was read, the sources whose user no longer meets the
  contract, and which build each replica is running.
- **Alert rules.** Whether a team's own rules work: which of their rules are
  failing to evaluate, which matched no source and will therefore never run,
  what is firing and pending now, which annotations will not render, and
  `clickhouse_ruler_problem` broken out by check and team, which is every way a
  rule broke after it merged. The distinction from the first one is ownership. A
  rule author cannot act on notification latency and should not be shown it, and
  by the same rule the problem gauge belongs here rather than on operations: it is
  the one signal in 8.2 addressed to whoever owns the query. It is also the one
  panel here with no rule group to filter on, since the gauge carries team and
  file instead, so the dashboard carries a `team` variable for narrowing it and
  the operations page has the author's path through the two signals that are
  theirs rather than the operator's.

They are files in the repository rather than screenshots in a wiki, for the
reason rules are: a dashboard that is provisioned from git is one that can be
reviewed, and one that can be fixed when a metric is renamed.

**Every metric in 8.2 is on one of them.** A signal an operator has to already
know about to go looking for is a signal that reaches nobody at 3am, and the
reload pair is the case that proves it: a refused reload is silent by design,
because the rules that are running are valid and nothing about them looks wrong.
So a second test asserts the reverse of the one below, that every metric the
registry carries is drawn by some panel or fills some variable. Adding a metric
means adding it to a dashboard in the same change, or deciding out loud that
nobody needs to see it.

**A panel querying a metric nobody exposes renders an empty graph, which looks
exactly like a healthy system.** That is the failure this area produces, and it
is the same shape as a check page documenting a default the tool does not have
(7.8), so it gets the same answer: a test reads every expression in both
dashboards and asserts each metric it names is registered. Renaming a metric
then fails the build rather than quietly emptying a panel somebody is on call
with.

Two consequences of shipping them. The dashboards may only use the metrics in
8.2, because anything else is a panel that cannot work; where one would need a
signal that does not exist yet, the gap is named in this spec rather than
papered over with an expression that returns nothing. And they are portable: a
datasource variable rather than a hardcoded UID, since the UID is local to
whoever imports it.

**What that test does not catch is the label.** It asserts that every metric a
panel names is registered, and a metric name is only half of an expression. A
panel grouping by `rule` on a metric labelled `rule_group` alone passes it and
renders exactly the empty graph the test was written to prevent, as does a
matcher on a label whose values are not what the author assumed. Names are
checked without a running system; labels need series. The stack in 9.8 is
where that half is answered, and until it exists this gate is the weaker of
the two claims this section makes.

One further omission worth stating rather than discovering. Duration is shown
against nothing, because the group's interval is configuration and 8.2 exposes
no metric carrying it. A panel cannot draw the line an operator is meant to
read the duration against, so it says so in its description instead. Exposing
the interval as a gauge is the obvious fix, and declining it here rested on its
only consumer being a dashboard. 8.8 is where that stops being true: the interval
is what every cadence expression is read against, so the gauge is carried there
with the rest of the cadence work rather than as a panel's convenience.

### 8.7 The operations page

Everything in 8.1 through 8.6 is a mechanism. What an operator needs at three in
the morning is the reading of it, and that is a page rather than a spec section:
`docs/operations.md`, published with the check pages.

Five things belong on it, and nothing else:

- **What to watch, as expressions they can paste.** A missed iteration, an
  evaluation failure rate, a last evaluation going stale, send failures,
  notification latency. Each with the number that means trouble and what to do
  about it, because a threshold nobody can justify is a threshold somebody
  silences.
- **Every log line, with what it means.** The table in 8.4 says what is logged.
  The page says what to do when a line appears, which is a different table and
  the one people actually need.
- **What refuses to start, what loads anyway, and why each is deliberate.** A
  file nobody can read stops the ruler, and that is the whole list (7.6); a source
  failing the user contract is refused on its own while every other source carries
  on (6.7.3). Both look like an outage to somebody who has not read 7.1, and both
  are the design working. The other half is newer and is the page's debt: a
  finding that blocks a merge loads and raises `clickhouse_ruler_problem` under its
  own check name, so the page has to say what the two expensive ones cost, a rule
  missing a time bound and a rule setting its own `SETTINGS`, and how to see them
  in `system.query_log`. A rule loaded against the author's intent is cheaper than
  a ruler that refused to start, and only if somebody is watching.
- **The ClickHouse side.** `system.query_log` queries keyed on the
  `log_comment` from 8.5: what a rule cost, what it read, what timed out.
- **A rule that broke while running**, which is the one item here not addressed
  to the operator. `clickhouse_ruler_problem` (8.2, 10.4) is fixed by whoever
  owns the query, so this section has a second job the other four do not: it has
  to be readable by somebody who has never operated this ruler and does not want
  to. It says what a raised gauge means, that the rule is still evaluating and
  still paging, that `team` and `file` name who and where, and that `check` is
  the link to the explanation. It also says what clearing means, because a gauge
  that clears when the schema is fixed and a gauge that clears because a pass
  could not run are different events and only the first is good news.

What does not belong on it is a second description of the checks. Those have
their own pages and their own generated facts (7.8), and an operations page
restating a severity default is one more thing to go stale. That applies to the
item above as much as the rest: it explains the signal and hands the reader to
the check page, it does not re-explain `rule/attribute-key`.

### 8.8 Query latency, cadence and the lag budget

Four questions an operator asks during an incident that 8.2 does not answer.
All four are metric shape rather than mechanism, so they are named here rather
than discovered against a dashboard that renders an empty graph (8.6).

**How long a given cluster's queries take.**
`clickhouse_ruler_query_duration_seconds` carries `rule` alone, and a rule
evaluates against every source its selector matches (6.10), so a rule spanning
four clusters folds four clusters into one histogram. "Is this cluster slow" is
the first question asked when alerts arrive late and it is the one this metric
cannot be asked. It needs `source`, which is the label
`clickhouse_ruler_query_queue_wait_seconds` already carries for the same reason:
how long a query takes is a property of the cluster as much as of the rule
pointed at it.

`rule_group` and `team` belong on it as well, for separate reasons. `rule_group`
because the group is the scheduling unit (6.11), so a group's query latency
against its interval is the arithmetic behind a missed iteration, and the
existing `clickhouse_ruler_rule_evaluation_duration_seconds` is whole-tick wall
time across concurrently evaluated rules rather than what any query took. `team`
because rows and bytes carry it and duration does not, so chargeback can say what
a team read and not what they made the cluster spend time on.

None of the three multiplies cardinality the way 8.3 warns about. A rule belongs
to one group and carries one `team`, so both are determined by the `rule` label
and add no series. `source` multiplies by the fanout of a rule's selector, which
is the number of clusters an operator deliberately pointed it at, and it is the
same multiplier the cost counters take below.

Any of them can be dropped again. A histogram's series count is its label set
times its buckets, so these are the most expensive metrics here to widen, and a
deployment that finds the fanout too dear should lose a dimension rather than
lose the metric. `team` goes first, because the cost counters already answer
ownership and latency by team is a convenience on top of them. `rule_group` goes
next, because a group's cadence is answered by the group metrics and this label
only saves a join. `source` is the one that cannot go: without it the question
that started this section has no answer at all. Dropping a label is a one line
change and 8.3 is the rule it is measured against, not this table.

**Rows and bytes take `source` in the same slice, and that is chargeback per
cluster.** The cost counters carry `rule` and `team`, so they say what a team read
and not where they read it. A team whose rules span a shared cluster and a cluster
of their own gets one number covering both, which is the number nobody can act on:
the shared cluster is where their reading costs somebody else something, and it is
the only part of their bill an operator can negotiate about. The same label
answers the smaller question that comes first, which is which cluster a bill came
from when the total moved and no rule changed.

Counters are the cheap half of this. One series each, no buckets, so `source` on
rows and bytes costs the selector fanout and nothing more, against a histogram
where the same label multiplies every bucket.

`clickhouse_ruler_query_memory_usage_bytes` stays on `rule` alone until somebody
asks. Peak memory is a property of the query rather than of the cluster it ran on,
the cap it is read against in 6.7 is the same wherever the rule evaluates, and it
is a six bucket histogram that would pay the fanout for a reading nobody has
needed yet.

Once the cost counters carry `source`, a source leaving the configuration has to
take their series with it, the way queue wait's already does. Left behind, they
read as a cluster this ruler still bills for.

**The wiring already knows all three, which is why this is one slice and not
three.** `internal/query` receives a `query.Attribution` carrying the group and
the team, because `log_comment` needs the group already (8.5), and the querier
holds its own source. So nothing has to be threaded from the scheduler: what
changes is the `Recorder` signature and the adapter in `cmd/ruler` that satisfies
it, and the adapter exists precisely so neither side learns the other's
vocabulary. One signature carries the whole cost family, which is the argument for
doing duration and the counters together rather than adding `source` to one now
and to the other when the first chargeback question arrives.

**Why a group is late when every cluster it reads is fast.** The answer is the
ruler's own cap, `--query-concurrency`, and until it is measured it is the one
term of the budget below that is ours and invisible. A group's rules are
evaluated concurrently on one tick (6.11), so a group holding more rules than
the cap has slots queues against itself: fifty rules against a cap of eight and
a two second query make a fourteen second tick out of nothing but the cap, while
every per-query metric reads healthy. Query duration is timed around the query
alone, and `clickhouse_ruler_query_queue_wait_seconds` times the per-source gate
only, so today this shows up as evaluation duration and then as a missed
iteration with no term to blame.

It is a histogram labelled `rule_group`, because the question arrives as "why
was my group late" and the group is who paid. Not labelled by source as well:
that is the reading queue wait already gives, and a histogram pays the label on
every bucket. Zeros are recorded, which is the opposite of the rule for queue
wait, and for a reason: a source cap is absent unless configured, so a zero
there would read as a limit doing something, while the ruler cap is on unless
turned off and a query that found a slot waiting is exactly the reading that says
the ruler runs below it. A ruler started with the cap off records nothing.

`clickhouse_ruler_query_concurrency` ships with it, for the reason the interval
gauge below ships with the cadence work: nine seconds of queueing says nothing
without the number of slots it queued for, and an operator who hardcodes the cap
reads every expression against a flag somebody else can change. One series per
process, zero meaning unbounded, as the flag does.
`clickhouse_ruler_queries_in_flight` is the other half of that reading, counting
queries running or waiting so saturation is visible now rather than after a
histogram fills. Waiting queries are counted, because a query this ruler is
trying to send is load whether or not it got through a gate.

**Neither ruler upstream reports this, and the check is worth recording.** The
Prometheus ruler's own rule concurrency gate does not queue at all: it is
`sema.TryAcquire` in `rules/manager.go`, and a rule that cannot get a slot is
evaluated inline in `rules/group.go` instead of waiting. There is no wait there
to measure, which is why there is no metric. What it offers for the same question
is `prometheus_rule_group_last_rule_duration_sum_seconds`, the sum of each rule's
duration regardless of concurrency, read against the group's duration to see how
much concurrency the group actually got: two series and a division, answering
"was I parallel" rather than "what did I wait". vmalert blocks the way we do, on
a buffered channel per group, and exposes nothing about the wait either.

One layer down, Prometheus does measure exactly this. The PromQL engine gates on
`--query.max-concurrency` and times the gate, reported as
`prometheus_engine_query_duration_seconds{slice="queue_time"}` with
`prometheus_engine_queries` and `prometheus_engine_queries_concurrent_max`
beside it. That is the shape we are copying, including both gauges. What it
cannot do is attribute: one gate serves API queries and rule evaluations, the
histogram is labelled by `slice` and nothing else, so an operator cannot ask
which group paid. Carrying `rule_group` is the one thing here that is ours.

What an operator does about it is three things in order, and the order matters
because the obvious one is wrong here. Lengthen the group's interval. Raise the
cap, if the cluster has headroom. Split the group, last, and knowing that
splitting divides the burst rather than the tick: a group's tick costs its
slowest rule, not the sum of its rules, so a split helps a group of many cheap
rules and does nothing for a group held up by one expensive one. What it buys is
that each half gets its own stagger offset, so the queue is shorter and arrives
at a different point in the interval. `docs/operations.md` says this next to the
missed iterations expression, because that is where the question starts.

**How far apart one alert's runs are.** Per alert this cannot be answered and
will not be. Scheduling is per group: one goroutine per group, and the rules
inside it evaluated concurrently on one tick (6.11). The spacing of an alert's
runs is the spacing of its group's ticks, and an operator asking about a single
alert is asking about its group whether they know it or not. Answering in the
group's terms is the accurate answer; anything per rule would be a number we
invented.

**This is not a divergence from Prometheus, and that is worth having checked.**
Read against `rules/group.go` and `rules/manager.go` upstream, the cadence model
is the same one in every part an operator can observe. There is no per-rule
interval there either: every rule in a group shares the group's single
`interval`. The stagger is the same construction, an offset of
`hash(name, file) mod interval`, which is what ours computes from the same two
fields. An overrun skips the boundaries that passed and counts them rather than
running them late, under a metric whose suffix we already carry. The interval
gauge proposed below is `prometheus_rule_group_interval_seconds` by another
prefix, labelled by the same group key, so it is a name that carries over rather
than one we coined (8.2).

One thing does differ, and it runs the other way from a divergence an operator
would have to learn. Upstream evaluates a group's rules sequentially unless the
`concurrent-rule-eval` feature flag is set, because a recording rule can feed the
next rule in the file and the order is part of the contract. We evaluate them
concurrently always (6.11), and we can because there are no recording rules here:
an alerting rule has no dependents, so there is no order to preserve. The
observable effect is that a group's tick costs its slowest rule rather than the
sum of all of them, which makes our missed iterations rarer than the same
configuration would produce upstream. Cadence expressions carried over from a
Prometheus ruler therefore read correctly here, and read better.

`docs/operations.md` says the per-group part in a sentence next to the cadence
expressions, because the question arrives as "why was my alert late" rather than
as a question about groups, and somebody who has to infer the scheduling unit
from a label name will infer it wrong.

The group's spacing is nearly reported already. A group advances an absolute
schedule rather than sleeping for its interval, so an observed gap is the
interval or an exact multiple of it, and the multiples are counted by
`clickhouse_ruler_rule_group_iterations_missed_total`. Two things sit between
that counter and the question as asked.

The configured interval is not a series, so nothing can express how far from it a
run landed. 8.6 records this as a dashboard omission and declines it there as a
metric whose only consumer is a panel. That reasoning does not survive this
section. The interval is what every cadence expression here is read against, and
without it an operator hardcodes a number that the rule file is free to change
under them.

Delay inside an interval is not measured at all. A tick can fire exactly on
schedule and start late: the goroutine is woken by the clock, then the evaluation
waits on the ruler-wide concurrency cap or on the source's own (6.11). The
scheduler's wrapper holds both the scheduled tick time and the actual start, and
observes the difference nowhere. This is the signal that catches a ruler running
late without ever overrunning an interval, which is the case
`clickhouse_ruler_rule_group_iterations_missed_total` reads as healthy.

So, two more series per group:

| Metric | Type | Labels |
| --- | --- | --- |
| `clickhouse_ruler_rule_group_interval_seconds` | gauge | `rule_group` |
| `clickhouse_ruler_rule_group_tick_delay_seconds` | histogram | `rule_group` |

Both are removed with their group like everything else labelled `rule_group`
(8.2), and the gauge is set from the configuration on every load, so a group
whose interval changed reports the interval it is now running rather than the one
it started on.

**Whether any of this can carry a promise.** Not end to end, and the reason is
worth stating exactly, because "no latency SLO" and "no promise about anything"
are different answers.

The delay from a condition becoming true in ClickHouse to a notification leaving
the ruler is a sum, and its terms do not share an owner:

```text
delivery lag  =  evaluation_delay        the operator's configuration (6.8)
              +  0..interval             scheduling granularity
              +  tick delay              ours
              +  concurrency wait        ours, the ruler-wide cap (6.11)
              +  queue wait              ours, the source's cap (6.11)
              +  query duration          the rule's SQL, and the cluster
              +  for                     the rule author's configuration
              +  notification latency    ours, and the operator's Alertmanager
```

Four terms are ours. The rest are the operator's cluster, the operator's
configuration and the author's SQL, and a single figure covering those would be a
promise about somebody else's hardware. An end to end latency target from us
would be that figure, which is why there is not one.

What ships instead is the formula and a metric for every term, so the operator
sets the number for their own deployment and can see which term spent the budget.
That is the same posture as 8.2 on routing `clickhouse_ruler_problem`: the
mechanism and the reading are ours, the threshold is theirs, and leading a horse
to water is the whole of the offer.

The terms that are ours are the ones a claim could later be made about.
Tick delay stays near zero while a group's evaluation fits inside its interval.
Queue wait is zero unless the source sets `max_concurrent_queries`, so a
non-empty histogram means a limit exists and is being reached. Concurrency wait
is zero while no group asks for more slots at once than the cap holds, so it is
the term a growing rule file moves first. Notification latency is one send to an
Alertmanager the operator runs. Naming them is not
targeting them: a target needs a deployment somebody has operated, and nobody has
operated this one, so the numbers wait for evidence rather than being chosen here.

**The expressions.** These are what the section is for, because a formula an
operator has to translate into PromQL is a formula they will not use. The three
that need only the cost labels and the concurrency wait are on the operations
page and on the dashboard, which is 8.7's first item. The cadence ones wait on the two group metrics above:
a panel querying a metric nobody exposes renders an empty graph, which looks
exactly like a healthy system (8.6), so they live here until that lands.

How long a cluster's queries take, which is the question that started this
section:

```promql
histogram_quantile(0.99, sum by (source, le) (
  rate(clickhouse_ruler_query_duration_seconds_bucket[5m])
))
```

Swap `source` for `rule`, `rule_group` or `team` for the same reading by alert,
by group or by owner. Add the wait for a slot to get what the rule actually
waited, which is the number the operator feels rather than the one the database
reports:

```promql
histogram_quantile(0.99, sum by (source, le) (
  rate(clickhouse_ruler_query_duration_seconds_bucket[5m])
))
+ histogram_quantile(0.99, sum by (source, le) (
  rate(clickhouse_ruler_query_queue_wait_seconds_bucket[5m])
))
```

That addition is why `source` is the label queue wait already carries and the
one duration cannot do without: the two join on it and on nothing else.

What a group spent waiting for a slot at the ruler's own cap, which is the term
that reads as a slow group and a fast cluster:

```promql
histogram_quantile(0.99, sum by (rule_group, le) (
  rate(clickhouse_ruler_query_concurrency_wait_seconds_bucket[5m])
))
```

Read against `clickhouse_ruler_query_concurrency`, which says how many slots
that was queueing for, and against the group's interval, which says whether it
matters.

What a team read, and from which cluster, which is the chargeback the counters
exist for:

```promql
sum by (team, source) (rate(clickhouse_ruler_query_read_bytes_total[1h]))
```

An empty `team` is a rule nobody has claimed rather than a rule owned by the empty
string, and it is left visible for the reason 8.2 gives: chargeback that hides
what is unattributed is chargeback nobody can reconcile.

A group's query latency against its own interval, as a fraction, so one
expression covers a fleet whose groups run on different intervals:

```promql
histogram_quantile(0.99, sum by (rule_group, le) (
  rate(clickhouse_ruler_query_duration_seconds_bucket[5m])
))
/ max by (rule_group) (clickhouse_ruler_rule_group_interval_seconds)
```

Approaching 1 is a group about to miss iterations. This is the panel 8.6 says
draws its duration against nothing today.

How far apart a group's runs actually were, against how far apart they were
configured to be, which is the ask in 2 stated as it was asked:

```promql
3600 / increase(clickhouse_ruler_rule_group_iterations_total[1h])
- max by (rule_group) (clickhouse_ruler_rule_group_interval_seconds)
```

Zero means the group ran on its interval for the hour. A positive number is the
mean seconds of spacing above the configured interval, and it can only be
positive: an absolute schedule cannot run early. Where the time went is the next
two expressions, and they are different faults. Whole intervals lost:

```promql
increase(clickhouse_ruler_rule_group_iterations_missed_total[1h]) > 0
```

Lateness inside an interval, which the counter above reads as healthy:

```promql
histogram_quantile(0.99, sum by (rule_group, le) (
  rate(clickhouse_ruler_rule_group_tick_delay_seconds_bucket[1h])
))
```

And the budget itself, per group, as far as series can carry it:

```promql
  max by (rule_group) (clickhouse_ruler_rule_group_interval_seconds)
+ histogram_quantile(0.99, sum by (rule_group, le) (
    rate(clickhouse_ruler_rule_group_tick_delay_seconds_bucket[1h])
  ))
+ histogram_quantile(0.99, sum by (rule_group, le) (
    rate(clickhouse_ruler_query_concurrency_wait_seconds_bucket[1h])
  ))
+ histogram_quantile(0.99, sum by (rule_group, le) (
    rate(clickhouse_ruler_query_duration_seconds_bucket[1h])
  ))
+ histogram_quantile(0.99, sum by (le) (
    rate(clickhouse_ruler_notification_latency_seconds_bucket[1h])
  ))
```

**It is a floor, and saying so is the point.** The source's own queue wait is
absent, because it is labelled by cluster and this expression is per group, so
the two join on nothing: a group reading a capped source adds that histogram
itself. Two more terms are configuration rather than measurement:
`evaluation_delay` on the source (6.8) and `for` on the rule. Neither is a series, and neither should become one to make this
expression tidier, because an operator reads both out of the files they wrote.
The expression answers what the ruler contributed, they add their own two numbers,
and the sum is their alerting delay. An expression that quietly omitted the two
largest terms in many deployments and called itself the lag would be worse than
no expression.

**What we do with it is nothing, deliberately.** The same stance as
`clickhouse_ruler_problem` in 8.2: what ships is the expression, what it means and
which term to go after, and nothing that writes into somebody's Alertmanager. An
operator who wants an alert when their budget is spent has the query, and an
operator who does not want one is not carrying a rule they never asked for. We
can recommend it and we cannot impose it, and a threshold we picked for a cluster
we have never seen is one they would silence anyway.

---

## 9. End to end testing

Everything under test is real. No mocked ClickHouse, no mocked Alertmanager, no
stubbed collector.

### 9.1 Compose stack

`compose.yaml`, all image versions pinned. Items 1, 4, 5, 7 and 8 exist today.
Items 2 and 3 arrive with the generators, and item 6 is not a container at all
for the reason it gives.

1. **ClickHouse**, two nodes. Schema in `deploy/clickhouse/init`, the
   OpenTelemetry Collector ClickHouse exporter trace table reproduced verbatim
   from `exporter/clickhouseexporter` in `opentelemetry-collector-contrib`:
   same columns, types, codecs, skip indexes, `PARTITION BY` and `ORDER BY`.
   Copying it rather than trimming it means a rule that works in the tests
   works against real collector output, and it keeps `ResourceAttributes`
   available, which is where `deployment.environment`, `service.namespace` and
   the `k8s.*` keys live. Only the engine and the TTL differ, and both are
   local-development concerns. Both nodes run the same image and the same init,
   so the ruler's user and its one table exist on each, and the clusters in
   `deploy/clickhouse/cluster.xml` put a `Distributed` table over them: one over
   both shards and one whose second shard never answers. That second cluster is
   the only way to find out what an evaluation does when part of a cluster is
   gone, which is what 6.9 needed the node for.
2. **OpenTelemetry collector**, ClickHouse exporter, batch timeout set low so
   data lands in seconds rather than tens of seconds.
3. **Telemetry generators.** Two of them, see 9.2.
4. **Alertmanager**, real, configured with a webhook receiver pointing at the
   sink.
5. **Ruler**, the code under test, built by compose from the `Dockerfile` in
   this checkout rather than pulled. A published image is the last release, and
   the stack exists to run what is in the tree, which is the same reason the
   action's first job runs the checker in the checkout (10.3). The price is a Go
   build the first time and after any change under `cmd` or `internal`; the
   layer cache keeps it off every other `compose-up`.

   Its rules are not the ones in `cmd/ruler/testdata`. That tree is a fixture
   whose `sources.yaml` a test rewrites in place, and the address it names is
   reachable from the host rather than from inside a container. The stack's own
   rules and sources are `deploy/stack`, mounted read only, addressing the
   ClickHouse nodes by service name.

   They evaluate and they do not fire. The alerts reaching Alertmanager are the
   integration tests' own, and a stack rule paging continuously into the webhook
   sink in item 6 would put deliveries nobody asked for in front of every
   assertion that reads it. Evaluating is all 9.8 needs: the iteration,
   evaluation, duration and query cost series come from a rule that found
   nothing, and a panel that is empty until something fires is one 9.8 does not
   demand an answer from.
6. **Webhook sink**, a small HTTP server that records every notification
   payload it receives and exposes them for assertions. Not a container today:
   it runs inside the integration test so assertions can read the payloads
   directly, which is why Alertmanager routes to `host.docker.internal` on a
   fixed port. Moving it into the stack only becomes worthwhile once the ruler
   itself is a container and no test process is left to host it.
7. **Prometheus**, scraping the ruler. Not for the ruler's benefit: it is
   what the dashboards query, and without it 8.6's panels have nothing behind
   them at all. See 9.8. Its config is `deploy/prometheus`, one target, and the
   scrape interval is short for the reason the test rules use a short one (9.4):
   what a test waits for is the first scrape, and the wait is that interval.
8. **Grafana**, provisioned from `deploy/grafana`: a datasource pointing at
   item 7, and a dashboard provider pointing at the files 8.6 ships. Nothing
   is configured through its UI, for the reason rules are files: a dashboard
   that exists only in somebody's browser cannot be reviewed. It is here so a
   person can open the dashboards and see them working; no test reads it, and
   9.8 says why.

Items 5, 7 and 8 arrive together, because each is useless without the one
before it. A Prometheus with nothing to scrape holds no series, and a Grafana
with no Prometheus renders the same empty panels the whole exercise is meant
to catch.

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

### 9.7 Which ClickHouse versions are tested

7.2 explains why the readers in `internal/query` are coupled to a server
version, and 9.1 pins one. This section is about the rest of them, and about
what may be said in public.

**The axis is long term support releases, not the last N releases.**
ClickHouse ships monthly, so a promise about "the current version and the
three before it" expires every month and describes a set nobody runs. The
releases operators actually stand up are the long term support ones, two a
year, supported for a year after that. Three legs:

| Leg | Server | Required |
| --- | --- | --- |
| pinned | the current long term support release, the version in `compose.yaml` | yes |
| previous | the long term support release before it | yes |
| newest | `latest-alpine`, whatever released most recently | no |

The newest leg stays advisory for the reason 7.2 already gives: a server
changing its output is worth knowing about and is not the problem of whoever
opened the next pull request. The previous long term support leg is required,
because a version somebody is running is not a weather report.

**Tested is reported, never promised.** The table this produces says which
servers the suite passed against and when, and that is the whole claim. It is
not a support matrix, there is no deprecation policy behind it, and a red leg
on an older server is a fact rather than a commitment to fix it. The project
has no external users (10.2 is topologies people could run, not topologies
anyone runs), and promising compatibility to nobody costs the one thing we
have, which is the freedom to change a reader when a server changes its
output.

**The table is generated from the matrix.** A hand written compatibility
table is a screenshot of a build that has already moved, which is the same
failure as a check page stating a default the tool does not have (7.8) and a
panel querying a metric nobody exposes (8.6). It gets the same answer: the
matrix is the source, the published table is output, and a leg that is not in
continuous integration is not in the table.

**The floor is unknown and the slice that builds this finds it.** Every check
that reads server output has some oldest version it still parses:
`EXPLAIN AST`, `EXPLAIN ESTIMATE`, `EXPLAIN PLAN indexes=1`,
`DESCRIBE (SELECT ...)`, the privileges probes in 6.7.2, and the
`system.query_log` columns 8.5 is read back through. Nothing has ever asked
where that floor is. Finding it is a one-off walk backwards through releases,
and the answer belongs in the table as the oldest version tested rather than
as the oldest version supported, which is a different sentence and one this
project is not in a position to write.

None of this exists yet. Today the matrix is the two legs 7.2 describes and
there is no published table.

### 9.8 Dashboards that render

The gate in 8.6 proves a panel names a metric something registers. It cannot
prove the panel draws a line, because a name with the wrong grouping label or
an impossible matcher is still a name. Answering the other half needs series,
which needs a ruler that is running, a Prometheus that scraped it, and rules
that produced something worth plotting.

The stack already has the hard parts: real ClickHouse, real rules, real
evaluations, real alerts. Items 7 and 8 in 9.1 add the two that are missing,
and the assertion is then cheap: run each dashboard's expressions against
Prometheus through its own query API and require a series back.

**Assert against Prometheus, not against Grafana.** Grafana renders; it does
not decide whether an expression matches anything. Driving its API, or worse
its browser, would be a large amount of machinery to learn something the
datasource already knows, and it would fail for reasons that have nothing to
do with the dashboards. Grafana is in the stack so a person can open the
dashboards and see them working, which is worth having on its own and is not
what the test reads.

Two things this cannot promise, and they should not be attempted. A panel
whose expression returns a series is not a panel whose axis, unit or legend is
right, and no test is going to tell us a graph is legible. And a test that
demands every panel be non-empty will fail on the panels that are empty when
the system is healthy, which is most of the alert rules dashboard: a rule that
is firing during the run is a rule the test has to make fire. The assertion is
that the query is answerable, not that the answer is interesting.

**The test reads the dashboards, not a list kept beside them.** It sits in
`internal/dashboards` next to the gate 8.6 already ships and goes through the
same `Expressions`, so a panel added to a file is a query this test sends
without anybody remembering to add it. A checked-in list of
expressions would be the hand written compatibility table 9.7 refuses for the
same reason: a copy of the thing is a copy of where the thing used to be.

**Variables are substituted the way Grafana substitutes them.** What is in a
panel is not valid PromQL. `rule_group=~"$rule_group"` and `[$__rate_interval]`
are Grafana's, and Prometheus rejects both, so something has to fill them in and
the choice of what decides how much the test is still asserting. Each dashboard
variable is replaced by its own `allValue`, which is what the datasource
receives when a viewer leaves the dropdown on All: the substitution is read out
of the file rather than invented, so a variable whose `allValue` stops matching
anything is a failure here rather than a difference the test papers over.
`$__rate_interval` has no definition to read, because Grafana computes it from
the panel's time range and the datasource's scrape interval, so the test names a
duration and that is the one value it decides on its own.

**One assertion does need a series, and it is not a panel's.** Answerable is a
low bar on its own: a well-formed query against a metric nobody ever exposed is
answered with an empty result just as happily, so a stack whose ruler never
started would pass every panel. What that cannot fake is the chain items 5 and 7
exist for, so a second assertion waits for one series the ruler only produces by
running, being scraped and evaluating something. It names a single metric rather
than walking the dashboards, because this is a question about the stack and not
about the files.

---

## 10. Operational modes

Borrowed from `pint`:

- `ruler check ./rules/` runs validation in CI. Only checks rules changed in
  the pull request, and comments inline on the diff.
- `ruler run` runs the eval loop, sends to Alertmanager, and reports rules that
  became broken while it was running (10.4). Alert on your alerts.

There is no third mode. `ruler watch` was going to be one, re-validating loaded
rules continuously, and 10.4 is why it collapsed into `ruler run` instead.

Built so far: `ruler check` with configurable policy, tiers 0 through 2 of
section 7 including the checks that read the query through the database and
the one that reads rows, and `ruler run`, which ticks groups on their
intervals, evaluates against every matched source, delivers to Alertmanager,
re-checks loaded rules against recent data on `--recheck-interval`, and reloads
all three files on `SIGHUP`. The observability in section 8 is complete.

Hot reload is `SIGHUP` and nothing else: nothing watches the filesystem,
because whoever rolled the files out is the only party that knows when they are
complete. A reload is all or nothing, keeps the pending state of every rule that
is still the same rule, and refuses a reading only where there is nothing to
read (7.6). What it does not do is notice a rule that became broken while nothing
changed on disk, which is 10.4's job rather than the signal's.

Tier 3 backfill is in, behind `ruler check --backfill` (7.4).

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
- **Ruler per datacenter.** Each holds sources for its own clusters, so data
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

None of these need code that does not already exist. The one exception used to
be a rule matching several sources, which has to evaluate once per source with
its alert instances staying distinct per source; that is settled, because
`source` is part of an alert's label set and therefore of its fingerprint
(6.3).

#### How a merged rule reaches the ruler

Every topology above assumes the rules are on a disk the ruler can read, and
that something asks it to re-read them. That is the deployment step, and a rule
author's first question after merging a fix is when it starts running.

**The watching belongs outside the ruler.** Section 10 says hot reload is
`SIGHUP` and nothing else, and the references agree. Prometheus reloads on a
signal, serves `/-/reload` only when started with `--web.enable-lifecycle`, and
offers polling as an interval it does not enable by default; vmagent and vmalert
are the same shape, down to a `configCheckInterval` that is off unless asked
for. Neither watches its files. What watches, in both ecosystems, is a sidecar:
`prometheus-config-reloader` and `configmap-reload` follow the mounted directory
and then call the reload endpoint. So the ruler's job is to expose a trigger,
and the platform's job is to pull the files and pull the trigger.

**The default is `git-sync` with its exec hook.** It clones the rules
repository into a worktree and flips a symlink at the rules path, so the swap is
atomic and the ruler cannot read a tree half written, which is the objection
that ruled out a watcher in the first place. It then runs an exec hook after
each successful sync, and that hook posts to `/-/reload`, served when the ruler
runs with `--enable-reload-endpoint` (8.1). Nothing new is needed on our side.

The chain is then pull request, merge, sync, symlink flip, hook, reload, and the
lag is the sync period plus one reload. Every link is observable from outside:
`clickhouse_ruler_config_last_reload_timestamp_seconds` dates the configuration
running, and `clickhouse_ruler_config_last_reload_successful` says whether the
last attempt was refused (8.2).

**A ConfigMap mount is the small-estate case**, for an estate whose rules fit in
one object and whose authors are the operators. There is no exec hook there, so
it needs a reloader sidecar watching the mount, and kubelet's own atomicity
comes from the same trick: the real files sit in a timestamped directory, `..data`
is a symlink to it, and each file at the root is a symlink through `..data`.

Which is the part that constrains the loader rather than the chart. Both
layouts hand the ruler a rules path built out of symlinks, and both have to load
their rules exactly once: a symlinked root that is walked without being resolved
finds no rule files at all, and a mount walked without skipping `..*` finds
every rule twice and reports each as a duplicate of itself. The compose stack
this repository already runs is the third case and the simplest, a real
directory and a signal.

#### Running more than one ruler

The highly available topology above is worth spelling out, because "mostly
safe" is doing a lot of work in one bullet and an operator deciding between one
ruler and three needs the actual trade.

**It works by duplication, not by coordination.** Two rulers with the same
sources file produce the same alerts: an alert's identity is its final label
set, and every part of that set comes from the files and the query result
rather than from the process, so both rulers arrive at the same fingerprint and
Alertmanager deduplicates them (6.5). That is how Prometheus HA pairs work and
it needs no leader election, no shared state and no new features here.

**Which makes the sources file the thing to get right.** Source `labels`
(6.10.1) land on alerts, so two rulers meant to be replicas of each other must
carry the same ones. A label naming the replica, the obvious thing to reach
for when two processes are emitting the same alert, is precisely what breaks
deduplication: it makes every alert two alerts, and the route tree then routes
both.

**The cost is query load, and it is linear.** Each replica evaluates every
matched rule against every matched source, so three rulers is three times the
queries and three times the cost caps in 6.7 consumed. That is the argument for
sharding by source rather than replicating for its own sake, and it is why the
per-source concurrency limit in 6.11 matters more here than anywhere else: a
cluster already carrying triple the rule traffic is the one that starts queueing.

**Two rough edges, both known.** A ruler holds `ActiveAt` in memory, so a
replica that restarts, or one added to an existing set, re-serves every pending
alert's full `for` before it will fire (12.2). And each ruler computes its own
window from its own clock, so skew between replicas shifts what each one reads;
`evaluation_delay` absorbs the ordinary case, and 12.3 is where the rest of
that sits, still deferred.

Neither is a reason to run one ruler instead of three. They are reasons the
deduplicated alerts can disagree about *when*, never about *what*, and an
operator should know that before they are paged about it.

#### What the chart ships beside the ruler

A chart that installs the process and nothing else leaves two things to
whoever installs it, and both of them are the difference between a ruler that
is running and a ruler somebody can operate: something has to scrape it, and
the pod has to be allowed to stop the way the process expects to.

**Scraping is opt-in because a CRD is not a dependency the chart can assume.**
Everything on the operations page (8.7) is an expression against `/metrics`,
and none of it works until a Prometheus is pointed at the port. The usual
answer is a `ServiceMonitor`, which is an object the Prometheus Operator
defines: render one on a cluster that does not have the operator installed and
the whole install fails, on a resource the ruler itself does not need. So the
chart templates it and leaves it off, which is the only default that renders
everywhere. It carries the interval, the scrape timeout and relabelings,
because a monitor nobody can tune is one somebody deletes and rewrites.

Scrape annotations were the other candidate and are declined. They are a
convention rather than an interface: nothing honours `prometheus.io/scrape`
unless a scrape config was written to read it, so a chart putting them on by
default writes configuration for a reader that may not exist, and reads as
though scraping is handled when it is not. A value people can set is the same
annotation without the claim.

**The grace period has to outlast the drain, and by default it does not.**
`--shutdown-timeout` gives an in-flight evaluation 30 seconds to finish, then
the HTTP surface gets five more. Kubernetes' default
`terminationGracePeriodSeconds` is also 30, so on the defaults kubelet's
`SIGKILL` lands exactly as the drain ends: the evaluation is cut off anyway,
and `shutdown timeout expired with evaluations still running` (8.4), the one
line that reports it, may never be written. The chart derives the grace period
from the timeout rather than letting the two defaults collide, and the timeout
is a value in seconds rather than a Go duration because Helm cannot parse one
and a second knob that can disagree with the first is the bug this is fixing.

**A disruption budget is the other half of running more than one.** The
highly available topology above is several rulers with no shared state, and
12.2 is what a restart costs: every pending alert serves its `for` again, and a
condition that clears inside that second `for` never pages. A drain that takes
every replica at once therefore loses exactly what the replicas were for. The
budget is opt-in because a `minAvailable` of one on a single-replica release
blocks the drain instead of shaping it, and a chart that made node maintenance
fail by default would be worse than the thing it prevents.

`priorityClassName` is the same argument at the node rather than at the drain:
the component that reports the outage should not be the first one evicted by
it. The chart carries the field and sets nothing, because the class names are
the cluster's.

**Two things are deliberately not packaged yet.** The dashboards in
`deploy/grafana/dashboards` stay files to import: Helm reads only what is inside
the chart directory, so packaging them means either a second copy to drift or a
chart that renders differently from a checkout than from the registry, which is
a poor trade for an object imported once. And no `PrometheusRule` ships. Every
threshold on the operations page is stated next to the reasoning for it, and an
alert file carrying the thresholds without the reasoning is what gets silenced
at 3am and never re-enabled. Shipping one is its own decision, with its own
argument about which of those signals belong to whoever operates the ruler.

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

How that lands as behaviour: `--changed-since <ref>` resolves the merge base
of `HEAD` and `ref`, takes every path that differs between the merge base and
the work tree, and adds the files git does not track yet, because a rule
written and not yet committed is exactly the rule an author wants checked. The
sources file and any `ruler.yaml` in that set widen the run to everything. A
base that will not resolve, a shallow checkout or a directory that is not a
repository all widen it too, and say on stderr which of those happened.

Filtering narrows the findings, not the reading. The loader still walks the
whole tree, because the binding and the duplicate checks are answers about the
tree rather than about a file, and a finding is then kept when it belongs to a
changed file. So the filter can only ever drop a finding a full run would also
have reported, never invent one, and the online checks a filtered run pays for
are the same ones an unfiltered run pays for.

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
- **Tags are disciplined.** The action version equals the tool version, so a
  release moves the floating major tag. Without that, everyone pinned to `@v1`
  runs whatever the tag pointed at the day they wrote it. The release workflow
  moves it, last, once the binaries and the chart are published, and never for
  a prerelease.

  Which leaves the action having to accept a tag that cannot name a release
  asset, because an asset carries the full version and `v1` is not one. It
  resolves the newest release under that major and says which one it picked.
  A branch is not a release and is refused: the alternative is a checker whose
  version nobody can state, gating a merge.

  Divergence stays possible on purpose. The version input names any release, so
  a consumer can hold the binary back while taking the action forward, which is
  the direction the floor below allows.

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

The split that makes it possible is two outputs rather than one. `--markdown`
writes the comment body, a table with a row per finding, beside the annotations
in the same run: asking for a comment never runs the checks twice, which with
`--online` would mean every query twice. `--format=json` emits the findings as
an array for anyone integrating the checks elsewhere (10.1), which is an
argument for it existing whether or not a comment is posted.

The table belongs to the binary and not to the action for the same reason the
cost table does (7.10). A markdown table and its cell escaping are the same
problem whoever reads it, a second one written in shell is a second one to keep
right, and shell in an action is code no test in this repository can reach. What
the action adds is the one thing the binary cannot know: the URL a path is
appended to, which is where the files are served from rather than anything about
the rules. A path the repository cannot spell is printed without a link rather
than with a broken one.

Two things about that array are contract rather than convenience. A severity
is its name, `warning` or `error`, never the number: the numeric order exists
so policy merging can take a maximum across scopes (7.7), and publishing it
would turn an internal ordering into something a consumer parses and we can no
longer reorder. And every finding carries its documentation link, the same one
the other two formats print, because a check name a reader cannot look up is
the author's problem turned into a question for whoever owns policy (7.8).

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

#### Where the checker is documented

A fourth place describing this project, where 14 allows three. `action/`
carries a README because GitHub renders it for whoever arrives from a `uses:`
line in somebody else's workflow, and that reader needs orienting. What it must
not carry is a second manual, which is what it had become: the inputs, the
permissions split and the tag rules were stated there and in no other file, so
the site's reader had to leave the site to configure the thing the site spends
a section arguing for.

The site is the manual, so the page is the home. `action/README.md` is
orientation and a link: what the action is, the shortest workflow that works,
and where the rest lives. The page owns the inputs, the permissions split, the
tag discipline, what an annotation looks like once GitHub has rendered it, and
what `ruler check` does and does not gate as a required status.

The page repeats one thing the code decides, which is the input list, so a test
reads `action.yml` and the page and fails when they disagree. Same gate 7.8
puts on the check pages, for the same reason: an input renamed in the wiring and
left alone on the page is a workflow somebody writes from the documentation and
cannot run.

#### The checker in CI and the rulers in the fleet

CI and the ruler run the same validation from the same package (7.1), which
holds only while they are the same version. Skew has a safe direction and an
unsafe one, and it is worth stating which is which.

A checker newer than the fleet blocks a rule the rulers would have accepted:
noisy, and nobody is paged for it. A checker older than the fleet passes a rule
a ruler then refuses, and because a refused reading refuses a start, that lands
as a replica that cannot come back. So the requirement is a floor rather than a
pin to latest: **the checker must be at least as new as the oldest ruler still
running.** During a rollout two versions are live and the floor is the older of
them.

The floor is a query rather than a piece of tribal knowledge.
`clickhouse_ruler_build_info` carries the version as a label, shaped like
`prometheus_build_info` for exactly this kind of use (8.2), so
`min by (version) (clickhouse_ruler_build_info)` over the fleet is the number
the pin has to meet.

Pinning to a floating `latest` satisfies the safe direction always, and the
reason not to is churn: a release that adds a check turns every open pull
request red without anyone touching a rule. So the version is pinned, and the
pin belongs to whoever upgrades the rulers. A reusable workflow wrapping the
composite action puts it there: consumers call the workflow, the platform owns
the file the version is written in, and upgrading the fleet is one edit in a
repository the platform already has. Rule authors never hold the number.

#### What to require on a rules repository

Guidance rather than code, and it belongs in the spec because two of the three
are the mitigation for failures named elsewhere in this document.

**Require branches to be up to date before merging**, or a merge queue once
that serialises too much. This is what catches two pull requests that are each
green against the base and not green together, which `rule/duplicate-alert`,
`rule/source-match` and `ruleset/directory` can all produce because they are
findings about a pair or a tree rather than a file (7.3).

**The required status must not be filtered by path.** A workflow skipped by a
`paths` filter reports no status at all, and a required status that never
reports blocks every pull request. In a rules repository there is nothing worth
filtering anyway.

**`CODEOWNERS` on the sources file**, which carries addresses, credentials and
caps and which no rule author needs to read (6.2). Not on `ruler.yaml`: policy
merges as a maximum, severities take the strictest and allowlists intersect
(7.7), so a team file cannot loosen what the instance set and a guard there
would protect nothing.

### 10.4 Reporting rules that broke while running

The failure this exists for: a rule merges, passes every check, runs correctly
for months, and then the schema moves under it. Nothing in the file changed, so
CI has nothing to run and `SIGHUP` has nothing to reload. The rule is now wrong
and the only two events that would surface it are the next rollout and the
outage.

`ruler watch` was the planned answer, as a third mode re-validating loaded rules
on a timer. It is not built and it is not going to be, because most of what it
would have re-asked is answerable from the evaluations already happening.

**Two feeds come from what is already running, and the split between them is
whether an extra query is needed.** A third, at load, is below.

The first is free and lives in the evaluator. Every evaluation already knows the
result's column names and types, its cost, and whether it errored, so comparing
each evaluation against the previous one detects a dropped or retyped column,
two sources that stopped agreeing, a cost that crossed a ceiling, and a query
that failed, which reports under `rule/execution`. No query, and the latency is
one group interval. 6.3.2 is the design, including why the comparison is on the result's shape per rule and
never on row counts per alert: zero rows is the healthy state of most alert
rules, so a row-count comparison fires on every resolve.

The second is a timer, and it exists for exactly one check.
`rule/attribute-key` (7.3) catches the OTel map key rename, which is the highest
value check in the tool and the one thing in this section that no amount of
watching the evaluation reveals: the query succeeds, the shape is unchanged, and
it matches nothing forever. Answering it means sampling recent data, which is a
query the evaluation does not make, so it gets its own interval.

**A third feed, at load, for what the checker should have stopped.** 7.6 splits a
check's severity from whether it refuses a reading, so a finding that blocks a
merge now loads instead of stopping the ruler. Something has to say so, or a rule
that merged past CI runs with nobody told: a rule whose query sets its own
`SETTINGS`, one missing a time bound, one producing a `team` column, a file with a
field nobody recognises. Each is raised on `clickhouse_ruler_problem` under its
own check name, and each is rebuilt per reading the way the other two rebuild per
pass, so fixing a file and reloading clears it. The gauge carries no `feed` label
and gains none for this: the check name already says which findings are the load
feed's, since the fixed checks are only ever raised by it, and the log line is
where `feed` is written.

This feed is the reason refusing a start could be narrowed at all. Without it the
choice was a refusal or silence, and the argument for refusing was that silence
is worse. It also differs from the other two in what it proves: the evaluation
and re-check feeds report a rule that broke after it was reviewed, while this one
reports a review that did not happen, so an operator seeing it should be asking
why the checker did not run rather than what changed in ClickHouse.

**What the timer costs, and therefore how it is sized.** One bounded query per
rule per pass, against real data. That is affordable hourly and absurd every
minute, and the interval is a setting rather than a derived value because how
often a schema moves is a property of the organisation. It shares the ruler-wide
and per-source query limits with evaluation (6.11) rather than getting its own
budget, because a re-check pass that starved alerting would be trading the
outage it exists to prevent for a worse one. Evaluation is the work that cannot
wait; re-checking is the work that can.

**It does not gate anything.** All three feeds report into
`clickhouse_ruler_problem` (8.2) and none unloads a rule, refuses an evaluation,
or resolves an alert. Refusing belongs to an unreadable file alone (7.6). See 6.3.2 for why: a ruler that dropped a rule on a finding would stop
paging for the condition on the strength of a schema change nobody reviewed.

**The validation package is already re-runnable against loaded rules** (7.1), so
no feed here is a rewrite. The evaluator comparison keeps the previous result's
shape per rule and source in `internal/scheduler`, and `query.Run` returns that
shape and what the query cost beside the samples rather than reading the column
types, spending them on scanning and dropping them. The timer calls the same
`query.Sample` the checks CI runs call, through the same policy resolution, and
sizing it is `--recheck-interval` with zero for not at all.

**All three feeds are built.** The evaluator feed reports every tick, the load
feed once per reading, and the timer
runs on `--recheck-interval`, an hour by default and zero for not at all, since a
pass that reads real data must not start on a ruler nobody asked. Each feed
rebuilds only the gauge series of the checks it owns: `rule/columns`,
`rule/source-schema`, `rule/cost`, `rule/execution` and `annotations/template`
are the evaluation's, `rule/attribute-key` is the timer's, and the load feed owns
the fixed checks that no longer refuse a reading. Scoping the rebuild by check is what keeps
one clock from resolving the other's findings, and it is also what lets a pass
where nothing answered leave the previous answer standing per check rather than
for the whole rule.

**Scoped by source as well as by check**, because that is the grain the evidence
arrives at. A cluster that replied says what its result's shape is and what its
query cost; a cluster the ruler reached at all says whether its query runs,
whether it answered or refused; a cluster with rows says whether the annotations
render against them. None of the three says anything about the cluster beside it,
so a pass that reached three of four clusters rebuilds three quarters of this
rule's series and leaves the fourth standing. Before the `source` label the gauge
could not express that, and the rebuild had to be rule-wide: whether the query
runs was then answered only by a pass that reached every source, because one
cluster's reply would otherwise have claimed the others were fine.

The exception is `rule/source-schema`, which is a comparison between clusters and
answered for the rule rather than for one of them, so it carries no source. It
takes two replies to answer: one reply compares with nothing, and a pass where the
second cluster refused the query cannot say the two still agree, so clearing on it
would read as somebody having reconciled them. A rule left matching a single source
is the other way round, since the comparison can never be raised again and a
finding from when it matched two would otherwise stand for ever.

Because the series is where a finding lives, and the series outlive a reload,
nothing has to be remembered in the evaluator to keep a finding standing while
its cluster is quiet. What does have to happen is that a source nothing reaches
any more loses its series, since clearing one takes a pass against that cluster:
the reconciliation that closes a connection deletes them (8.2).

**No feed ships without its page.** The evaluator feed's is the
"a rule that broke while running" section of the operations page, with the
paragraph on how evaluation notices drift at all on how-it-works. The signal is addressed to somebody who
owns a rule and may never have operated this ruler, so a gauge nobody explained
is a gauge whose finding lands on the operator anyway, which is the outcome this
whole section exists to avoid. 8.7 says what the operations page has to carry.
Two rules keep it from sprawling: it explains the signal and links to the check
page rather than re-explaining the check (7.8), and it states which feed found a
thing, because "your rule's result changed shape", "your rule's map key is gone
from recent data" and "this file should never have merged" are different problems
with different fixes and arrive on different clocks. The load feed's share of
that page is 7.6's debt: what a rule that loaded anyway costs, and how to see it. How evaluation notices drift at all belongs on the how-it-works
page, in a paragraph, not a section: it is one comparison on a result the
evaluation already had.
