# Design

How the ruler works: rule files, sources, evaluation, alerting, tenancy,
guard rails and the user contract, scheduling.

Part of the [clickhouse-ruler spec](../spec.md). Section numbers are stable
and are what the code comments cite.

---

## 6. Design

### 6.1 Rule file format

Keep the Prometheus rule file schema as close to verbatim as possible so that
migrating an existing Prometheus rule is mechanical. Swap `expr` from PromQL to
ClickHouse SQL.

```yaml
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

`sources` is a selector over the labels a source carries, and it is the only
thing deciding which clusters the query runs against. It may match several, in
which case the rule evaluates once per source (6.10). An absent or empty
selector matches nothing.

`labels` are for routing and nothing else. They reach Alertmanager and play no
part in choosing a cluster, which is the separation that keeps a key like
`team` from meaning three things at once.

`window` is how much time the query examines: the distance between
`{{ .From }}` and `{{ .To }}`. It is the one field with no Prometheus
equivalent, because PromQL carries its own lookback in the expression and SQL
does not.

It defaults to the group `interval`, which reads exactly the data produced
since the previous evaluation. A rule wanting a wider view, a p99 that needs
more than a minute of traces to mean anything, sets it explicitly. Above, each
evaluation runs every minute over the last five minutes, so windows overlap.

A window shorter than the interval is a warning rather than an error. It is
valid SQL and it runs, but the gap between the end of one window and the start
of the next is never examined by any evaluation, so an incident living only in
that gap is invisible. Nothing in the query itself reveals this, which is why
it is linted.

### 6.2 Sources

A `source` names a table, its time column, and how to reach ClickHouse. Rules
do not reference one by name: a source declares which rules it accepts, and
they find each other by label (6.10). Borrowed from the ClickStack sources
concept.

Sources live in their own file, never inline in a rule file. Credentials, the
evaluation delay and the cost caps are operator concerns, and a separate file
is what lets CODEOWNERS stop rule authors editing them. See 6.6.

```yaml
sources:
  - name: otel_traces_dc1
    labels:
      team: payments
      cluster: dc1
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

| Field | Type | Default | Purpose |
|---|---|---|---|
| `name` | string | required | Identifies the source. Unique within the file, and it becomes the alert's `source` label (6.10.1). |
| `labels` | map | `{}` | What this source is. Rules select on them (6.10), and they are added to every alert it produces (6.10.1). |
| `address` | string | required | `host:port` of the ClickHouse to query. |
| `database` | string | required | Database to connect to. |
| `username` | string | required | The ClickHouse user, and therefore the tenancy boundary (6.6). |
| `password_file` | path | none | Reads the password from a file. Mutually exclusive with `password_env`. |
| `password_env` | string | none | Reads the password from an environment variable. |
| `table` | string | required | The table rules against this source read. |
| `timestamp_column` | string | required | The column the evaluation window is applied to. |
| `evaluation_delay` | duration | `1m` | How far behind live to evaluate (6.8). |
| `max_rows` | int | `1000` | Alert instances one evaluation may produce. |
| `max_execution_time` | duration | `30s` | ClickHouse query setting (6.7). |
| `max_memory_usage` | bytes | `1GiB` | ClickHouse query setting (6.7). |
| `max_concurrent_queries` | int | none | Queries the ruler may have in flight against this source at once, inside the ruler-wide cap (6.11). |
| `checks` | map | none | Tightens check severity for rules using this source (7.7). |

So the shortest usable source is `name`, `address`, `database`, `username`,
`table` and `timestamp_column`. Everything else has a default or is optional.

**`labels` describe what the source is**, and they do two jobs that do not
conflict: a rule's `sources` selector matches against them (6.10), and they
are written onto every alert the source produces (6.10.1). One map is enough
because both jobs read the same fact. They are protected on the alert: a query
may not contradict where it ran (6.3.1 level 4).

```yaml
labels: {team: payments, cluster: dc1, env: prod}
```

Nothing here is interpreted. `team`, `cluster` and `env` are conventions, not
keywords; the tool compares strings.

**The cost caps are per source.** `max_rows` is enforced by the ruler and caps how many alert instances one
evaluation may produce. `max_execution_time` and `max_memory_usage` are sent
to ClickHouse as query settings so the cluster does the enforcing, which is
the client side half of 6.7. They are per source rather than global because a
trace source and a log source do not cost the same, and they sit in the
operator's file rather than the author's for the reason the rest of this
section exists.

`max_concurrent_queries` is the one cap the ruler enforces on itself rather
than on a query: how many of its queries may be in flight against this
cluster at once, inside the ruler-wide limit (6.11). It has no default,
because a number picked here would be either at or above the ruler-wide cap,
where it does nothing, or below it, where it silently lowers throughput for a
single-source deployment that has nothing to protect itself from.

**Only the password comes from outside the file.** Everything else, the
username included, is written down and reviewable.

That split matters because of 6.6: the ClickHouse user is the tenancy
boundary, and row policies on that user do the isolation. A username hidden in
an environment variable cannot be reviewed, so nothing would stop a change
pointing one team's rules at a user with wider grants. The diff would show
nothing. `username` is therefore required rather than defaulted.

The password comes from `password_file` or `password_env`:

- Setting both is an error, not a precedence rule. If the two differ one is
  stale, and silently choosing either can authenticate with a credential that
  was supposed to have been rotated away.
- A file that cannot be read is an error naming the path, never the contents.
- An empty file is an error. A projected secret volume that has not populated
  yet reads as empty, and sending that as a real password produces a confusing
  auth failure rather than a clear config error.
- A trailing newline is stripped, because `echo secret > file` adds one and it
  is not part of the password.
- Neither set is legal: local development, and mTLS where ClickHouse
  authenticates the client certificate instead.

There is deliberately no DSN field. A DSN carries the password through string
handling, where any error that echoes its input puts the credential in a log.
Connection options are built field by field instead, and `Source.String`
redacts the password so printing one with `%v` cannot leak it.

### 6.3 Evaluation model

**One returned row is one alert instance.** Columns become labels. A column
named `value` becomes the alert value used in templating.

This is the thing that makes multi-instance alerts work, and it is what
sql_exporter handles poorly. A query returning one row per service produces one
alert per service, with no cardinality cost anywhere.

State machine is copied from Prometheus exactly. Inactive, pending, firing.
`for` duration, `keep_firing_for`. Do not invent new semantics here.

Each instance carries its own `for` timer and resolves on its own. Two rows
from the same rule can be in different phases on the same evaluation, and one
resolving does not disturb the other.

Instance identity is the final label set. It is *indexed* by a hash of that
label set: keys sorted, each key and value length-prefixed rather than joined
with a separator, because any separator byte can appear inside a ClickHouse
column value and a collision there would be one an author could construct.

**The hash is a bucket, not the identity.** Length prefixing closes the
construction of a collision but not its arithmetic: the result is 64 bits, and
two genuinely different label sets can land on one. So a hash hit is where the
comparison starts rather than ends, and the labels themselves decide. Two
instances sharing a hash stay two alerts, with their own values and their own
`for` timers. Prometheus keys on the hash alone and takes the odds; the
comparison costs one label-set equality on a bucket that holds one instance in
every real case, which is cheap enough not to take a bet at all.

**Two rows reaching the same final label set is an evaluation error, not a
merge.** This is a different failure from a hash collision: the rows are
genuinely distinct in the result and become identical only after the label
rules in 6.3.1 are applied. The precedence order makes it reachable rather
than exotic, because source labels overwrite result columns: a query grouping
by a `cluster` column, evaluated against a source whose labels already set
`cluster`, collapses every row onto one identity. Silently keeping the last
row would report one arbitrary value and discard the rest, during an
incident, with nothing in the output to say it happened. Prometheus treats
the same condition as `ErrDuplicateAlertLabelSet` and fails the evaluation,
which is the right call: the rule is asking for something it cannot express,
and the author needs to know.

### 6.3.1 Label precedence

Four sources contribute labels to an alert, weakest first:

1. **Group labels.** Set once for a whole group.
2. **Rule labels.** The rule's own `labels` block. Overrides group labels.
3. **Result columns.** Every returned column except `value`. Overrides both.
4. **Source labels.** The matched source's `labels` (6.10.1). These win, and
   they are protected.

Level 4 is above the query on purpose. A source's labels state a fact about
where the evaluation happened, and the query is in no position to know it
better: a result column setting `cluster` would be reporting something that is
simply not true. The same reasoning as `alertname`, applied to provenance
rather than identity.

`alertname` and `source` are written last and always come from the rule name
and the source name. A query returning either as a column cannot rename its
own alert or merge two clusters' alerts, because an identity that query data
can set is a routing hazard.

**Nothing is inferred from the directory.** An alert's labels are what the
rule file writes down, and a path never becomes one. A rules tree can be laid
out per team, per service, or flat, and the layout changes only who
`CODEOWNERS` sends the review to.

Deriving `team` from a directory was tried and removed. It reads well in the
common case and then behaves surprisingly everywhere else: a rule at the root
of the tree has no directory to derive from, a rule moved between directories
silently changes which team gets paged, and a label that appears in no file is
one nobody can grep for. Routing is too important to depend on where a file
happens to sit, so a rule that wants `team: payments` writes it.

A rule *may* override `team` with an explicit label. One team running
operations for another team's service is a real arrangement, not an abuse to
design out, and a tool that forbids it just gets worked around. The override
sits in the file, so it appears in a diff and `CODEOWNERS` gates who can write
it.

**Result columns may never set `team`, `alertname` or `source`.** This is not
about the
override above, which is deliberately allowed. It is about where the value
comes from. The Alertmanager route tree is generated from this repository
(6.5), so every `team` value that can ever be produced has to be readable from
the files. A `team` that arrives from a result column is runtime data: the
generator cannot enumerate it, no route matches it, and the alert lands in the
receiver on the root route, which is whatever the operator left there. That
failure shows up during an incident, which is the
worst time to discover a routing gap. `alertname` is protected for the same
reason it always was: an identity that query data can set is a routing hazard.
`source` is protected because it is what keeps two clusters' alerts apart
(6.10.1), and a query that could set it could merge them.

**`severity` stays overridable by a result column.** Lumping it in with `team`
would prevent no abuse, because the rule author already sets `severity`
freely through the labels block, so a query doing it grants no new power.
Against that, `if(value > 1000, 'critical', 'warning') AS severity` is a
genuinely useful pattern and banning it costs something real. If the route
tree ever keys on `severity` the way it keys on `team`, revisit this: the
enumerability argument above would then apply to it too.

### 6.4 Time window injection

Decided: the author writes complete SQL including `FROM`, and must use the
`{{ .From }}` and `{{ .To }}` template variables, which bind to the source's
time column. Validation rejects any query that omits them.

The SQL is real SQL and can be pasted into `clickhouse-client` with
substitution. Cost of this choice: the time bound is enforced by lint, not by
structure.

Considered and rejected: the ruler builds `FROM` and the time `WHERE` itself
and the author supplies only `SELECT`, `GROUP BY`, `HAVING`. Structurally
unbounded scans become impossible, but the resulting fragment is not valid SQL
and cannot be tested by hand.

The server side settings profile cap is the real guarantee either way. See 6.7.

### 6.5 Alertmanager integration

`POST /api/v2/alerts`. Firing alerts are re-sent on an interval so
Alertmanager does not expire them, `--resend-interval`, default 100s.

**Every firing alert carries its own `endsAt`**, set to `--resend-tolerance`
times the period it is actually re-sent on: whichever is longer of the resend
interval and the group's own interval, because the ruler cannot re-send
between two evaluations it is never called on.

The alternative is to send no `endsAt` and let Alertmanager apply its own
`resolve_timeout`, and that couples the ruler to a value in a config file it
cannot read. An operator who lowers `resolve_timeout` below the resend
interval gets a firing alert that expires between resends: Alertmanager
delivers a resolved notification for something still broken, then the next
evaluation fires it again. Nothing in the ruler can detect it. Sending the
expiry explicitly removes the coupling, so the resend interval sizes
notification traffic and nothing else. Prometheus does the same thing, for
the same reason.

**The tolerance is the budget for everything that can stop an alert being
re-asserted.** It defaults to 4, which is Prometheus' number: its `sendAlerts`
stamps `ValidUntil = ts.Add(4 * delta)` with `delta` of `max(interval,
resendDelay)`, under the comment "Allow for two Eval or Alertmanager send
failures". Worth reading twice, because it is the answer to a question this
spec used to leave open. Prometheus spends one budget on both kinds of
failure, a send it could not deliver and an evaluation it could not run,
because an evaluation that failed sends nothing either.

That is also what this ruler does, and the reason it is a flag rather than a
constant. A source whose query fails is skipped for that tick (6.11), so its
firing alerts are not re-posted and they are living on this budget. Prometheus
sized the budget for a local PromQL evaluation over data it already has. Ours
is a query to a separate database across a network, which fails more often and
for longer, and only an operator who knows their cluster can say how much
longer. At the default of 4 and a 100s resend interval, a ClickHouse outage
has about six and a half minutes before Alertmanager expires alerts that are
still true, delivers a resolved notification for each, and pages again when
the source comes back.

Raising the tolerance costs nothing but a longer wait for a genuine resolve
that the ruler failed to deliver, and the `endsAt` on each alert is what buys
the time. A tolerance below 2 is refused: one period expires an alert at the
exact moment it is next due, leaving no room for the send that would have
renewed it to fail.

The alternative was to re-assert on failure, sending a failed source's
currently firing instances from a read-only snapshot of `alert.State`. That is
strictly more correct and survives an outage of any length, and it is not
here, because it means claiming an alert is still true at a moment when the
ruler cannot check. Reach for it if a real outage beats a tuned tolerance.

**Annotations are rendered when the alert is evaluated, not when it is sent.**
They are part of what the alert is, which settles three things at once. A
resolved alert says what it said when it fired, rather than re-rendering from
whatever value it last held for a condition that has already gone away. A
template that will not render is an evaluation problem, so an operator is not
sent to Alertmanager to explain a broken `summary`. And one bad annotation
cannot fail the batch it travels in, which it used to do permanently, because
the failure repeated on every retry.

**A broken annotation never stops the page.** Each annotation renders
independently and the ones that worked are delivered untouched. An operator's
Alertmanager templates read them by name, and they are not equally likely to
fail: a `runbook_url` is often a fixed link while a `summary` interpolates
result columns, so discarding the set on the first failure tends to lose the
annotation a responder needed in order to keep the one that broke. Neither is
guaranteed, since a templated runbook is legal (7.3). The annotation that failed
carries its own error
as its value, where a human reading the page will see it, and the failure is
logged once per rule and source (8.4). Prometheus does the same, substituting
`<error expanding template: ...>`, on the reasoning that a ruler which drops a
page over a bad summary is worse than one that pages with a bad summary.

Whether a broken template should have reached production at all is a check-time
question, not a runtime one. `annotations/template` reports it at authoring
time, and an operator who wants it to block sets that check to `error` (7.6).

Templates are compiled once per rule rather than per evaluation, because they
are fixed for the rule's lifetime and a rule returning a thousand rows would
otherwise re-parse each annotation a thousand times every tick.

**A resolve is retried, because it is the one notification with nothing behind
it.** A firing alert that fails to send is re-sent on the next evaluation, and
on the one after, for as long as the condition holds. A resolve had exactly one
attempt: the state machine reported it and forgot it in the same step, so a
single failed notification lost it, leaving Alertmanager holding a firing alert
for something that had recovered until its `endsAt` ran out. The
notification-failure guarantee above covered firing alerts and quietly not
resolves.

So a resolved instance is kept and re-asserted for a retention window. Its
resolve time does not move while it is retained: repeating it says the same
thing again rather than resolving twice, and Alertmanager deduplicates identical
alerts. A condition that comes back inside the window is a new instance with its
own `for` timer, not the old one continuing, because the recovery already
happened and was already reported.

**The window is derived, not chosen.** It is the resend tolerance times
whichever of the resend interval and the group interval is longer, which is the
same span a firing alert's validity is stamped with, because both answer how
long delivery can still be in progress. Prometheus hard codes 15 minutes for
this; deriving it means an operator who widens either resend setting cannot
silently re-open the bug. Six and a half minutes at the defaults.

This is memory only, as it is in Prometheus. Retention buys surviving a failed
send, not surviving a restart: a ruler that stops mid-window forgets the resolve
either way, and that is 12.2's problem rather than this one's.

Alertmanager owns grouping, silences, inhibition, and routing. The ruler does
not.

Generate the Alertmanager route tree from the same repository, keyed on the
`team` label, so eval and routing share one source of truth.

Keep `source` and `cluster` out of `group_by` unless per-cluster paging is
wanted. A rule spanning an estate produces one alert per cluster (6.10.1), and
grouping is where that becomes one notification instead of N.

**The route tree needs a deliberate catch-all.** Teams are free to invent
labels on their rules and operators are free to invent them on sources
(6.10.1), so a label combination nobody wrote a route for is a matter of time
rather than a mistake. Alertmanager will still deliver it: the root route has
a receiver and everything unmatched falls through to it. The question is
whether that receiver is one somebody reads. A low-priority channel that
collects unrouted alerts turns a silent misroute into a visible backlog;
leaving the root pointing at a real on-call rotation turns it into pages for
the wrong people, and leaving it pointing at a receiver nobody watches turns
it into nothing at all.

Skip high availability and deduplication in v1. Alertmanager already dedupes
identical alerts, so running two ruler replicas is mostly safe already.

### 6.6 Tenancy and isolation

A rule runs as the ClickHouse user on the source it matched. Nothing is
derived from the directory path: nothing is (6.3.1), and 6.10 already decides
which rules reach which source.

Per-team isolation is therefore a source per team. Two sources can name the
same cluster and the same table with different users, and their labels decide
who may use which:

```yaml
sources:
  - name: traces_payments
    username: ruler_payments
    labels: {team: payments}
  - name: traces_search
    username: ruler_search
    labels: {team: search}
```

Row policies and grants then do the isolation inside ClickHouse. The tool does
not implement authorization, the database does.

That holds only while a query reads the tables it names. `remote()`, `url()`,
`s3()` and friends read data the row policies never see, so the guarantee
depends on those being refused, and on being refused by the database: the
SOURCES privileges are revoked on the user, per the contract in 6.7.2. The
tier 1 check in 7.3 reports the same mistake earlier and is not what makes the
guarantee hold (6.7.1). A tenancy claim that a table function can walk around
is not a tenancy claim, and neither is one that depends on a linter having
run.

`CODEOWNERS` gates the two halves separately, which is the split in 6.10. The
sources file is an admin path: adding a cluster, a user or a source is
reviewed by whoever operates ClickHouse, because it grants database access.
Rule directories are team paths, and a team writing an alert only has to carry
the labels its source requires. The database permission and the git permission
are not the same fact, and tying them to a directory convention would make
both worse.

### 6.7 Guard rails

Two layers. Lint predicts, the database enforces. Neither is trusted alone.

Server side, a settings profile on the ClickHouse user the rules run as.
Read limits and result limits are different things and both are needed: a
query can read a terabyte and return one row, so capping the result alone
caps nothing.

| Setting | Why |
|---|---|
| `readonly = 2` | a rule physically cannot mutate anything |
| settings constraints | the limits below cannot be raised, see next |
| `max_execution_time` | wall clock ceiling |
| `max_memory_usage` | per node, so per shard on a cluster (6.9) |
| `max_rows_to_read` | caps what is scanned, not what is returned |
| `max_bytes_to_read` | the same cap in the unit that actually bills |
| `max_result_rows`, `max_result_bytes` | caps what comes back to the ruler |
| `max_concurrent_queries_for_user` | one team cannot starve the others |
| `max_threads` | limits the share of the cluster one rule can take |
| `max_bytes_before_external_group_by` | spill a large aggregation rather than OOM the node |
| `result_overflow_mode = throw` | see below |
| `timeout_overflow_mode = throw` | see below |

**Both overflow modes must throw.** Their other setting truncates, which hands
the ruler a partial result that looks like a complete one. Fewer rows means
instances disappear, and disappearing instances resolve alerts. A rule that
exceeds its limits has to fail loudly, for the same reason
`skip_unavailable_shards` is pinned in 6.9.

**Every limit above needs a settings constraint, or it is advisory.** The
ruler sends settings with each query, which requires `readonly = 2`, because
`readonly = 1` forbids `SET` outright. But `readonly = 2` explicitly "allows
everything in readonly=1, plus SET", so the level that lets the ruler set a
limit is the same level that lets a rule raise it. And a rule does not even
need a second statement to try: ClickHouse accepts a `SETTINGS` clause on the
`SELECT` itself, so `SELECT ... SETTINGS max_execution_time = 9999` is one
statement that overrides what the ruler sent.

Constraints in the profile close it. `min`, `max`, `disallowed` and
`readonly`/`const` bound what a setting may become, and violating one throws
rather than clamping silently:

```xml
<profiles>
  <ruler>
    <readonly>2</readonly>
    <max_execution_time>10</max_execution_time>
    <constraints>
      <max_execution_time><max>10</max></max_execution_time>
      <max_memory_usage><max>1073741824</max></max_memory_usage>
      <max_result_rows><max>1000</max></max_result_rows>
      <result_overflow_mode><readonly/></result_overflow_mode>
      <timeout_overflow_mode><readonly/></timeout_overflow_mode>
    </constraints>
  </ruler>
</profiles>
```

Throwing is the behaviour we want, for the same reason both overflow modes
throw: a rule that tries to exceed its budget fails visibly instead of quietly
getting what it asked for.

The ruler sends the subset of these it knows per source; the rest belong in
the profile, where a rule cannot raise them. Tier 1 should also reject a
`SETTINGS` clause in rule SQL outright, so the failure arrives in CI rather
than at evaluation time.

Client side, at rule load and in CI: see section 7.

A runaway rule hurts one team's quota, not the cluster.

### 6.7.1 Where the line is

"Lint predicts, the database enforces" is the right instinct and too vague to
build on, because it leaves every individual check able to claim it is the
guarantee. The line is drawn once, here, and section 7 inherits it:

**A check we cannot make sound belongs to the database. A check the database
cannot make at all belongs to us. Nothing is a guarantee in both places.**

Measured against ClickHouse 25.8.2 with a user holding `readonly = 2`, a
profile with constraints, and `GRANT SELECT` on one table.

**Ours, and only ours.** No server-side equivalent exists, so the check is the
whole control and there is no backstop behind it:

| Check | Why the database cannot do it |
|---|---|
| `{{ .From }}` and `{{ .To }}` present | an unbounded query is valid SQL |
| `window` and `for` against the group interval | not a database concept |
| no `SELECT *` | result columns are alert identity (6.3), which the cluster knows nothing about |
| no `now()`, `today()`, `rand()` | see below: there is no server list of these |
| no `numbers()`, `generateRandom()`, `zeros()` | see below: no privilege gates them |
| annotation variables resolve to real output columns | the alert is ours, not the query's |
| attribute key presence (7.3 tier 2) | a missing map key is not an error to ClickHouse |
| predicted cost against the eval interval | the cluster sees one query, not its schedule |

Two of those rows were assumptions until they were tested, and both came back
worse than expected.

**There is no server-side source of truth for determinism.**
`system.functions` carries no `is_deterministic` column, so the banned function
list is ours to curate and therefore ours to keep current. It is configurable
for that reason: a list we maintain will be incomplete, and an operator who
finds the gap should not have to wait for a release.

**Generator table functions are gated by nothing.** `remote()`, `url()`,
`file()` and `s3()` are refused by privilege, but `numbers()` and
`generateRandom()` need no grant at all: as the locked-down user,
`SELECT count() FROM generateRandom(...)` ran until `max_execution_time` cut it
off. So the allowlist in 7.3 is the only preventive control for this class, and
the database's only answer is to time the query out after it has already cost
something. That makes the allowlist genuinely load-bearing, unlike the rest of
it.

**The database's, and only the database's.** We check these for fast feedback,
and the check is never what makes them true:

| Property | What actually enforces it | Observed |
|---|---|---|
| which tables and rows are readable | `GRANT SELECT` scoped to the source's table, plus row policies | reading an ungranted table in the same database is `ACCESS_DENIED`, `merge('otel','.*')` cannot reach it either, and `system` tables come back filtered to granted objects or refused outright |
| no external data | revoke every SOURCES privilege | `remote()`, `url()` and `file()` each fail naming the missing grant |
| no mutation | no `INSERT` or DDL grant, plus `readonly = 2` | `INSERT` and `CREATE TABLE` are refused on the grant, before `readonly` is consulted |
| cost ceilings cannot be raised | profile constraints | `SELECT ... SETTINGS max_execution_time = 9999` throws `SETTING_CONSTRAINT_VIOLATION`, and `readonly` reports itself as const |
| one team cannot starve the others | `max_concurrent_queries_for_user` | not measured |

So the tier 1 checks that refuse `remote()`, or a table outside the source's
own, are demoted: they are CI failing fast on a mistake, not the thing standing
between one team and another team's data. 6.6 should be read with that
correction. Its claim holds because of the grants, and it held in the probe
with no checker running at all.

**This is what unblocks tier 1.** 6.9 asks what `table:` means on a sharded
cluster before tier 1 can use it, on the assumption that getting it wrong is a
tenancy hole. Under the line above it is not: `table:` carries no security
meaning, so a wrong answer produces a wrong lint finding and nothing more.
Tier 1 proceeds on the single-node reading and 6.9 stays open.

### 6.7.2 The ClickHouse user contract

Everything in the right-hand column above is true only if the operator created
the user that way, and nothing checks that they did. A contract that exists
only as prose in this file is one that fails silently, on someone else's
cluster, in the direction of too much access.

So the contract ships as a reference `CREATE USER` and profile, and is
verified rather than assumed:

- **Grants.** `SELECT` on exactly the source's table and nothing else. No
  SOURCES privileges. No `INSERT`, no DDL, no `CREATE TEMPORARY TABLE`, which
  `remote()` needs in addition to `REMOTE`.
- **Profile.** `readonly = 2`, the settings of 6.7, and a constraint on each of
  them so a query cannot raise what the ruler sends.
- **Row policies**, where a source is narrower than its table.

**A user can read enough about itself to check this, with one wrinkle.**
`system.settings` exposes `value`, `min`, `max` and `readonly` per setting, so
the effective constraints are readable from the session with no extra
privilege. `SHOW GRANTS`, however, reports role membership rather than what the
roles contain: a user granted `REMOTE` through a role shows only
`GRANT the_role TO the_user`. Expanding it means walking `enabledRoles()` and
issuing `SHOW GRANTS FOR` each one, recursively, and roles are how an operator
of any size grants in the first place.

**So privileges are verified by probe, not by reading grant text.** The check
issues the queries that are supposed to be refused, against endpoints that
cannot do anything if they are not: `url('http://127.0.0.1:1/', ...)` is
denied on the missing privilege before any connection is attempted, and a
denial is the passing result. A probe cannot be fooled by a role, by an
implicit grant, or by a future release changing which privilege covers what.
Reading grants infers; running the query observes.

Settings constraints are read from `system.settings` rather than probed,
because a probe there would mean sending a query designed to exceed a limit.

**Probe both directions.** A refused probe proves a privilege is absent; it
says nothing about whether the source can read its own table. `SELECT 1 FROM
<table> LIMIT 0` on the same round trip proves the grant that has to be there,
so an under-granted user is a finding in CI rather than an `ACCESS_DENIED` on
the first evaluation at three in the morning.

**Assert on the error code, not the message.** `497 ACCESS_DENIED` is a pass.
Any other error is inconclusive and reports as inconclusive, because a probe
that treats "it failed somehow" as proof will pass a cluster that was merely
unreachable. No error at all is the finding.

### 6.7.3 When the contract is checked, and what that leaves open

Once per source, in three places, and never in the evaluation path:

- `ruler check`, per source. This is the one that matters, because the finding
  lands in the pull request changing the sources file, in front of the people
  who own it.
- `ruler run` at startup, per source, while connections are being opened
  anyway.
- On a `SIGHUP` reload, since a reload already reconciles sources. Checked
  before any connection is opened, the same as at startup, so a source refused
  at error severity is never connected to.

Not per evaluation. Grants do not change between two ticks in any way worth
paying for, and four refused queries on every tick would be load on the cluster
and latency in front of a page. The cost as written is a handful of statements
per source at load, each refused before it does any work: the `url()` probe
never opens a socket.

Behaviour on failure follows the check's severity, which 7.6 already makes
runtime behaviour rather than CI output. At `error` the source is refused and
its rules do not evaluate. At `warn` it loads and logs which assertions failed.
At `off` the probes are not sent at all.

**What this leaves open, stated rather than hidden.** An operator who grants
`REMOTE` an hour after startup is not noticed until the next check or reload,
which is why the reload checks it: without that, the window would be the
process lifetime rather than the gap between two deliberate acts.
That is acceptable because of 6.7.1 and would not be acceptable without it: the
probe is a report, and the grants are the control. The window where the report
is stale is a window where the database is still refusing the query.

### 6.7.4 What the operator owns

The line in 6.7.1 is only honest if the half on the other side of it is
written down, so this is the part to read before pointing a ruler at a cluster.
The tool cannot do any of it, and no check failing quietly means it was done.

**Decide what the source's user may read, and grant exactly that.** Not the
database, the table. Every check in section 7 is scoped to the source's own
table, so a wider grant is invisible to the tool and equally invisible in a
diff of the rules repository.

**Decide whether a row policy is needed.** A source narrower than its table,
one team's rows out of a shared table, is a row policy and nothing else. No
rule file can express it, and no check can detect that it is missing, because
a query returning another team's rows looks exactly like a query returning its
own.

**Put a constraint on every limit, not just a value.** A limit the ruler sends
without a constraint behind it is a default a rule can raise in one clause.
This is the one the probe reads rather than tests, so it is also the one most
likely to be half done: a profile with the settings and no `<constraints>`
block reports the right values and enforces nothing.

**Decide what happens when the contract cannot be met.** Managed ClickHouse
that will not expose settings profiles is a real constraint, not a reason to
turn the check off wholesale. Drop the assertion that cannot hold, keep the
rest, and write down what is now unenforced. 7.6's assertion list exists for
this.

**Expect the loud failures and the silent one to look nothing alike.** Too few
grants is an `ACCESS_DENIED` on every evaluation, counted and logged. Too many
grants is silence: every rule evaluates correctly and the boundary is simply
not there. The check exists for the second case, which is why its severity
governs the report and never the guarantee (7.6).

### 6.8 Evaluation delay

OTel data is not in ClickHouse the instant it happens. The collector batches,
and inserts land late. A rule evaluating the window ending at `now()` reads a
partially filled window and produces false resolves and flapping.

Every source carries an `evaluation_delay`. `{{ .To }}` binds to
`now() - evaluation_delay`, not to `now()`. Prometheus calls the same idea
query offset, Mimir calls it evaluation delay.

Default is 1m. Sources with slow batching need more. This is a production
requirement, not a test convenience, and it also makes the end to end tests
deterministic.

### 6.9 Sharded clusters

Not addressed yet. Everything below is a known gap, written down now because
one of the items is a silent correctness bug rather than a missing feature.

The query path itself needs no change. The author writes their own `FROM`, so
on a sharded cluster they name the Distributed table and the ruler never has
to know the difference. What needs work is the connection and the settings.

**`skip_unavailable_shards` must be pinned to `0`.** This is the one that
matters. The ClickHouse default is already `0`, meaning an unreachable shard
fails the query, but it is settable on a user or a profile. If it is ever `1`,
a distributed query with a dead shard *succeeds* and returns only the rows the
surviving shards held. Missing rows are indistinguishable from a recovered
condition: instances disappear from the state machine, their alerts resolve,
and the page that should have fired never does. It goes wrong silently, and
only during an outage, which is exactly when the alerts matter. The ruler
should send it explicitly with the other settings in 6.7 rather than inherit
whatever the profile says. Failing an evaluation loudly is always better than
evaluating a partial result.

**`address` must become a list.** The source schema takes one address and
`query.Open` passes `[]string{src.Address}` to a driver that already accepts
many. Against a sharded cluster that single node is both a point of failure
and the coordinator for every distributed query. Wants an `addresses:` list
and a connection open strategy.

**The cost caps are per node.** `max_execution_time` and `max_memory_usage`
are enforced by each node independently, so on a sharded cluster the real
ceiling is per shard, not per query. 6.2 and 6.7 describe them as a cluster
cap, which is loose. Fanout also means the coordinator merges results, so
`max_result_rows` is the only cap applying to the query as a whole.

**`evaluation_delay` must cover the slowest shard.** Insert lag is per shard,
and the delay has to clear the worst one, not the average. This is a larger
number rather than new configuration.

**What `table:` refers to becomes ambiguous.** It is parsed and validated but
never read by the querier today; it exists for the tier 1 checks in 7.3. On a
sharded cluster it could mean the local table or the Distributed one, and
those have different rows in `system.tables`. Decide this before tier 1 uses
it, not after.

Testing this needs a second ClickHouse node in the compose stack, a
`Distributed` table over both, and a test that stops one node and asserts the
evaluation fails rather than silently returning half the rows. Single-node
testing cannot catch the `skip_unavailable_shards` bug at all, which is the
argument for adding the node rather than reasoning about it on paper.

---

### 6.10 Source selection

A source is a cluster, a user and a table. Deciding which alerts run against
which source is therefore a cluster-to-alerts mapping, and it is done with
labels and a selector rather than names, so that neither side has to know
about the other.

**A rule does not name a source. It selects them.** A source carries `labels`
describing what it is; a rule carries a `sources` selector describing what it
wants. A source matches when every term in the selector is present and equal
in its labels.

```yaml
# sources.yaml, operator owned
sources:
  - name: payments_main
    username: ruler_payments
    labels: {team: payments, cluster: main}

  - name: payments_prod
    username: ruler_prod
    labels: {team: payments, cluster: prod, env: prod}
```

```yaml
# rules/payments/latency.yaml, author owned
- alert: HighP99Latency
  sources: {team: payments}           # both clusters
  labels:  {severity: warning}        # routing only
```

`sources: {team: payments, env: prod}` narrows to the prod cluster. **Adding
terms narrows**, which is the ordinary selector intuition and the reason this
direction was chosen over its inverse.

Naming a source would have been the obvious schema, and it is the wrong one
here. A name picks exactly one, so an estate of five clusters means five
copies of a rule differing only in that line, and adding a sixth means editing
every rule rather than adding one source. A selector lets the rule say what it
wants and the source say what it is, and neither has to be edited when the
other changes.

**`sources` decides which clusters run the query. `labels` decide nothing but
routing.** Keeping those separate is what stops a single key doing three jobs
at once: before this split, `team` was simultaneously a routing label, a
tenancy gate and a directory-derived default, which is why the question of
whether one team could reach another's cluster had no clean answer.

Nothing in the tool interprets any of these words. `team`, `cluster` and `env`
are conventions; the tool compares strings.

**An absent or empty selector matches nothing.** The usual selector convention
is that empty matches everything, and it is wrong for this field, because this
field grants access to a database user. Failing closed means a rule can never
reach a cluster by omission, and the cost is one line per rule. When a rule
genuinely should run everywhere, the operator puts a common label on every
source and the rule selects it, which makes "everywhere" an explicit statement
rather than the consequence of leaving a field out.

**A rule can match more than one source, and runs against all of them.** That
is what makes one rule definition work across an estate: the same latency rule
evaluates on every cluster whose source it matches.

**Matched sources have to be schema-compatible, and `rule/source-schema`
checks it.** The author writes `FROM otel.otel_traces` in the SQL, so every
source a rule matches must expose that table with those columns. Labelling
sources such that a rule only matches compatible ones is an operator's job,
and the failure it produces is a rule that resolves against both clusters and
means something different on each: a column one returns and the other does not
is a label on half the estate's alerts, and a column both return with
different types is a threshold compared against another type. Tier 1, because
the columns are the cluster's answer rather than the file's, so each source is
asked and the answers are compared (7.3). Reported once for the rule, naming
both sources and the column, because a finding per source says the rule is
broken against one cluster and correct against the other, which is the
confusing way to find out. A source nobody could reach, or whose user cannot
read the table, returned no columns and so has not disagreed with anything;
`rule/inspect` and `rule/table-access` already say what happened there. A
warning, because which sources a selector reaches and whether two clusters
carry the same table are both the operator's (7.6).

### 6.10.1 Identity when a rule matches several sources

A rule matching N sources evaluates N times, and each evaluation has to be a
separate alert. Without that, two clusters returning the same `ServiceName`
produce the same fingerprint (6.3), the second evaluation overwrites the
first, and one cluster recovering resolves the other's alert while it is still
broken.

**The source name is added to every alert as a `source` label**, and it is
protected the way `alertname` is (6.3.1): a result column may not set it. The
name is unique within the sources file by definition, which is exactly the
uniqueness the fingerprint needs.

**One map does both jobs.** A source's `labels` are what a rule's selector
matches against and what lands on the alert, because both read the same fact
about the source. That only works because the rule holds the selector: if the
source held requirements instead, its keys would already be on every rule that
matched and putting them on the alert would add nothing, so identity would
need a second field.

```yaml
sources:
  - name: traces_payments_dc1
    labels: {team: payments, cluster: dc1}
```

A rule selecting `{team: payments}` reaches it and its alerts carry both
`team: payments` and `cluster: dc1`, without the rule having named a cluster.

**Labels beyond `source` are free-form.** A source may carry whatever
an operator finds useful, `cluster`, `region`, `env`, `dc`, and they land on
every alert from that source. The tool assigns no meaning to any of them.
`cluster` is the common case and it is worth saying why it is not sufficient
on its own: 6.6 puts two sources with different users on the same cluster, so
both report the same `cluster` value, and a rule matching both would be back
to one fingerprint for two evaluations. `source` guarantees the split;
everything else makes the result legible.

Free-form labels move work to the route tree, and that is the right place for
it but it is not free. Every label a team invents on a rule and every label an
operator invents on a source is a dimension the routing has to account for.
6.5 covers what happens when it does not.

Source labels cannot come from group labels (6.3.1) instead, because one rule
spans several sources and a group label is one value for the whole group.

Notification volume is Alertmanager's problem, not ours. Two clusters firing
produce two alerts with different `source` labels; a route whose `group_by`
omits them collapses that into one notification, and a route that includes
them pages per cluster. Both are legitimate, and the choice belongs in the
route tree rather than in the fingerprint. Note that the compose stack for
tests uses `group_by: ['...']`, which groups by every label and therefore
notifies per source; that is a test convenience, not a recommendation.

**Setup is an admin action, authoring is not.** Adding a cluster, a user or a
source is a change to the operator-owned file and needs its `CODEOWNERS`
review. After that, a team writing an alert only has to carry the right
labels, and no further admin involvement is needed. That split is the reason
this is worth doing with labels rather than an allowlist of rule paths.

**Matching nothing is a warning, not an error.** A rule that matches no source
cannot run here, and the instinct is to fail. That instinct is wrong for two
reasons. Deployments are distributed (10.2), so a ruler in one datacenter
legitimately holds sources for its own clusters and nothing else; most rules
in a shared repository will match nothing on most rulers, and that is normal
rather than broken. And rollout has an order: when a cluster is added, rules
referencing it may land before the source does, and hard failure would block
every unrelated change in the repository until the ordering was fixed. The
check reports it, 8.2 exports a count of unmatched rules so the condition is
visible and alertable, and an operator who wants it blocking raises the
severity through 7.6.

This moves source matching from correctness to convention in 7.6. It is the
one place where "the rule cannot run" is not automatically an error, because
whether it can run depends on which ruler is asking.

---

### 6.11 Evaluation concurrency

One goroutine per rule group, each ticking on the group's own interval. Group
starts are staggered across that interval, so twenty groups on `1m` do not all
fire on the same second and stampede ClickHouse.

**An overrunning evaluation must not queue.** When an evaluation takes longer
than the interval, the boundaries that passed while it ran are skipped and
counted in `clickhouse_ruler_rule_group_iterations_missed_total` (8.2), not run late. A
backlog is how a ruler silently falls behind, and the counter is what makes
falling behind visible instead.

**Within a group, rules and their sources are evaluated concurrently.**
Sequential evaluation made a group's tick cost the sum of every query inside
it, so a group grew slower purely by having rules added to it, until it began
missing iterations. Each source has its own `alert.State` and they share
nothing (6.10.1), so parallel evaluation needs no lock.

Concurrency is bounded by a ruler-wide limit on how many queries may be in
flight at once, rather than by the shape of the configuration. The goroutines
are not what bounds load; the limit sits around the query itself, so a large
group queues against it instead of opening a connection per rule.

**The limit has two levels: ruler-wide, and per source inside it.** The thing
that actually needs protecting is each ClickHouse cluster, and one global
number is a loose proxy for that: a slow cluster holds slots that rules
against every other cluster then queue behind, so an outage on one source
would delay evaluation of sources that are perfectly healthy. A source may
therefore set `max_concurrent_queries`, sized from what that cluster can take.
A query takes the ruler-wide slot first and then its source's, and gives both
back when it returns. Taking them in that order keeps the ruler-wide number
the ceiling: were the source slot taken first, the sources' limits could add
up past it.

The per-source limit has no default. Any number picked would be either at or
above the ruler-wide cap, where it does nothing, or below it, where it
silently lowers throughput for the single-source deployment that is the common
case and has nothing to protect itself from. A source that sets nothing is
bounded only by the ruler-wide cap.

Both gates abandon a query that is still queued when shutdown cancels the
context, rather than running it after the ruler has stopped. Time spent
waiting for a source's slot is recorded in
`clickhouse_ruler_query_queue_wait_seconds` (8.2), which is what says a limit
is set too low: without it the knob cannot be sized and an operator is
guessing.

Notification state is shared across every group, so it is guarded. An
unsynchronised map there is not a subtle race but a fatal "concurrent map
writes" abort of the whole process, which is the worst available failure for a
daemon whose job is paging people. The lock is deliberately not held across
the POST to Alertmanager: holding it there would serialise every group's
notifications behind one slow Alertmanager, which is the opposite of what
running groups concurrently is for.
