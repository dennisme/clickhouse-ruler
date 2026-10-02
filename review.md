# Operator and consumer review

A read of the running code against `spec/` and the git history, from three
angles: what an operator cannot see, what a rule author has to put up with, and
what is outright broken. All tests pass as of this review.

Items that have since been fixed are at the bottom under Closed, with what
closed them. Numbering never changes, so a reference to one of these survives
the item being fixed.

## Bugs

None open. The ones that were are under Closed, with their original numbers.

## Operator unclear

### Single Alertmanager

`--alertmanager` takes one URL (`internal/notify/client.go:34`). Prometheus fans
out to a set. An HA pair needs a load balancer the operator supplies, and
`docs/running.md` does not say so. The `alertmanager` metric label implies a
plurality that does not exist.

Spec 6.5 now carries the argument and the ordering: Alertmanager's own docs say
not to load balance in front of it, so a list is the right shape here even
though a source keeps one address. It waited on the send leaving the evaluation
goroutine, because fan-out multiplies that worst case by the number of
endpoints, and that item is now closed, so nothing is in front of this one.

### Flags need a restart and the docs do not say it

`SIGHUP` re-reads three files. `--alertmanager`, `--query-concurrency`,
`--recheck-interval`, `--resend-interval`, `--resend-tolerance` and `--log-level`
all need a process restart. The flag table in `docs/running.md` never mentions
it.

## Metrics and logs

### No `--log-format=json`

Spec 8.4 declines it until somebody asks. Asking: structured `slog` output in
text form means every Kubernetes log pipeline re-parses it. One handler swap.

### No reload attempt counter

`config_last_reload_successful` is a gauge, so a reload that failed and then
succeeded between two scrapes is invisible. Low priority; Prometheus has the same
hole.

## Consumer toil

### A rule names its cluster after all

The README says "A rule names no cluster. It selects sources by label, and runs
against every." But `expr` hardcodes `FROM otel.otel_traces`, and only
`{{ .From }}` and `{{ .To }}` are templated (`internal/query/query.go:28`). The
source file already declares `database`, `table` and `timestamp_column`, and
`table:` is "never read by the querier" (`spec/design.md:1284`).

So a rule selecting four sources only works if all four spell the database and
table identically. Otherwise it is one rule per cluster, which is what the label
selector exists to avoid. This is the gap between the pitch and the thing, and it
is not stated anywhere an author would read.

Either expose `{{ .Database }}`, `{{ .Table }}` and `{{ .TimestampColumn }}`, or
say plainly in the quick start that a selector spanning clusters requires
identical table naming.

### `ruler check` takes a directory only

An author fixing one rule in an 800-rule tree checks the tree. `-changed-since`
helps in CI, not at a desk. Accepting a file path would be a few lines.

### `--summary` and `--explain` need stdout gymnastics

Three flags each carry their own rule about which stream they land on depending
on `--format`. Workable and documented, but a lot of rules for one command.

## Closed

Original numbering and original text kept, so a reference written before the
fix still points at the right item.

### An Alertmanager outage shows up as missed iterations

**Closed by taking the send off the evaluation goroutine.** Delivery is one
worker behind a bounded queue, `--notification-queue-capacity`, ten thousand
alerts, Prometheus' number and unit. The queue sits in front of `notify.Cadence`
rather than between it and the client, because the Cadence deliberately does not
record a failed send and a queue behind that record would mark an alert
delivered before anything was: each item carries the alerts, the `now` of the
evaluation that produced them and the group's interval, and the worker calls
`Cadence.Send`. A full queue drops the oldest rather than blocking or dropping
the newest, and a drop is not a lost page, because nothing is recorded as sent
and the next evaluation hands the same instances over, spending the same
`--resend-tolerance` a failed send spends. `Result.SendError` is gone, since a
field on an evaluation's result would be nil on every evaluation of a ruler that
cannot deliver anything; the log line moved to the worker, which carries the
group and the rule for it. Four series say what the queue made invisible: depth,
capacity, queue wait and alerts dropped, with the tick delay buckets on the wait
and a new term in the lag budget rather than a redefinition of notification
latency. `Shutdown` now drains the queue inside the same timeout it gives
evaluations, and the worker's sends no longer run on an evaluation's context, so
the wait means what it always claimed. The argument is in spec 6.5.

`Cadence.Send` runs inside the rule's evaluation goroutine
(`internal/scheduler/ruleeval.go`, end of `Evaluate`). Four attempts at a 10s
timeout plus backoff is roughly 42s worst case. A group on a 30s interval starts
missing iterations, which spec 8.2 calls the single most important operational
signal, for a cause that is not evaluation.

Either bound the send by the group interval, or say this next to the missed
iterations expression in `docs/operations.md`. Spec 6.5 makes this a blocker for
a list of Alertmanagers rather than a standalone annoyance.

### Two spec'd metrics are still absent

**Closed.** The gauge is set from the configuration on every load beside
`clickhouse_ruler_rules_unmatched` (`internal/scheduler/scheduler.go`, `build`),
so a group whose interval was edited reports the one it is now running. The
histogram is observed in the one wrapper every group's `Eval` goes through,
beside the duration already recorded there. Both go with their group in
`deleteGroup`. The buckets are exponential from 50ms by a factor of three
rather than `prometheus.DefBuckets`, which stops at ten seconds and would read a
group five seconds late the same as one three minutes late; the argument is in
spec 8.8. `docs/operations.md` gained *How late a group is running*, and the
duration panel now draws the interval as a line, which closes the omission spec
8.6 named.

`clickhouse_ruler_rule_group_interval_seconds` and
`clickhouse_ruler_rule_group_tick_delay_seconds` (spec 8.8). The scheduler holds
both inputs already: `GroupSpec.Interval`, and `tickAt` against `s.clock.Now()`
in the wrapper at `internal/scheduler/scheduler.go`, `startLocked`.

Without the interval gauge every cadence expression hardcodes a number the rule
file is free to change. Without tick delay, a ruler running consistently late
without ever overrunning an interval reads healthy.

### 5. mTLS to ClickHouse is documented and does not exist

**Closed by making TLS expressible.** `secure: true` connects over TLS and
verifies the server against the host's trust store, which is the whole of the
managed service case, and `tls_config` carries `ca_file`, `cert_file`,
`key_file`, `server_name` and `insecure_skip_verify` for a cluster whose trust
is not the default one. Key material is paths, never inline, read when the
sources file is parsed, so a wrong path is a `source/tls` error with the line of
the field. `insecure_skip_verify` is a `source/tls-insecure` error that an
`exempt` entry clears, with a reason and a date. The resolved `tls.Config` is
carried on the source and handed to the driver by `query.Open`, so the online
check, the privileges check and the re-check pass all reach the cluster the same
way. The three sentences are now true: a client certificate with no password is
a legal source.

Three places say a source with no credentials covers mTLS:

- `internal/source/source.go:336`: "No password at all is legal: local
  development against the compose stack, and mTLS where ClickHouse
  authenticates the client certificate."
- `spec/design.md:168`: "Neither set is legal: local development, and mTLS where
  ClickHouse authenticates the client certificate instead."
- `docs/how-it-works.md:55`: "No password at all is fine for local development
  and for mTLS."

`query.Open` builds `clickhouse.Options` field by field and never sets `TLS`
(`internal/query/querier.go:46`), so every connection is plaintext native
protocol. There is no field in the sources file for a CA, a client certificate
or a key, and nothing anywhere resolves one. An operator who read any of those
three lines and dropped the password is not authenticating by certificate, they
are connecting as a ClickHouse user with no password over an unencrypted socket,
which is the opposite of what they were told they were doing.

So the sources file cannot express TLS at all, not only mTLS: a cluster that
requires encryption in transit is unreachable by this ruler. That is a larger
gap than the stale comment, and the comment is what hides it.

Two fixes and they are different sizes. The comment correction is three lines
and makes the gap visible. Real support is a sources file schema change,
`tls_config` with the shape the ecosystem already uses, plus the same
`password_file` posture for the key: an operator concern, in the operator's
file, under their CODEOWNERS (spec 6.6). Picking the second does not remove the
need for the first in the meantime.

### The re-check pass reads production rows by default

**Closed by deciding it stays on.** The reads it was measured against are reads
this ruler already makes on every tick, and 7.3's consent posture is about
`ruler check` on somebody's laptop rather than a running daemon. What was
actually wrong was the documentation: the comment in `internal/scheduler` claimed
nothing runs unless configured, the constant was documented as the value used
"when an operator asks", and spec 10.4 said an hour by default and in the same
sentence that it must not start unless asked. All three now say the same thing,
`docs/running.md` carries the cost arithmetic, and a test pins the wired
default.

`--recheck-interval` defaults to `1h` (`cmd/ruler/run.go:54`,
`spec/operations.md:1992`), but `internal/scheduler/scheduler.go:35` says "zero
when an operator did not ask for it. The pass reads real data, so nothing runs on
a ruler that never configured it."

One of those is wrong. The consent posture everywhere else, where `-sample` and
`-backfill` are opt-in per spec 7.3, argues the default should be off. At
minimum, fix the comment.

### 2. `rate(failures)/rate(evaluations)` can exceed 1

**Closed.** Both counters now count one evaluation of one rule against one
cluster, so the ratio is a share: a rule matching four clusters with one down
reads 0.25. Recorded in spec 8.2 as a deliberate divergence from the Prometheus
metric of the same name, which has no cluster to count per. A single-source
ruler reads the same number either way.

`internal/scheduler/scheduler.go:489` increments `EvaluationsTotal` once per rule
per tick, then `:491` adds `len(res.SourceErrors)` to `EvaluationFailuresTotal`. A rule
matching four sources that all fail records 4 failures against 1 evaluation.

`docs/operations.md:51` ships exactly that ratio with `> 0.1`, read as "10% of
evaluations failing". For a four-source rule, one bad cluster reads 1.0 rather
than 0.25. Either count the denominator per source-evaluation, or drop the ratio
for a plain failure rate.

### The notification latency histogram includes failed sends

**Closed.** The histogram observes only sends Alertmanager accepted. Labelling
by outcome was the alternative and buys little: the failing population's shape
is fixed by the retry policy, so it is a near-constant plus an extra matcher on
every expression. Batches attempted is the histogram's count plus
`clickhouse_ruler_alerts_send_failures_total`, which spec 8.2 and the docs now
say.

`internal/scheduler/sender.go:24` observes before the error check, so a 42s retry
storm lands in the p99 that the `> 5s` alert at `docs/operations.md:191` reads.
A reachability problem then fires the latency alert as well. Either skip the
observation on error, or label it by outcome.

### 3. The readiness probe blocks for the whole reload

**Closed.** `ready` reads an `atomic.Pointer` the reload publishes and takes no
lock. The snapshot option in the fix note below was what the code already did,
so it was never the contention: the probe always read under the lock and pinged
outside it, and what blocked was acquiring a lock a reload holds end to end.

`connect` holds `r.mu` across every `query.Open` and across `sched.Reload`, which
waits for every in-flight evaluation to return (`cmd/ruler/reload.go:161`).
`ready` takes the same mutex (`cmd/ruler/reload.go:292`).

The chart probe is `timeoutSeconds: 3, failureThreshold: 3, periodSeconds: 10`
(`deploy/chart/clickhouse-ruler/values.yaml:201`). A reload behind one slow
ClickHouse query makes the pod NotReady on every `SIGHUP`.

Fix: snapshot the queriers under the lock and ping outside it, or hold what
readiness reads in an `atomic.Pointer` that a reload swaps.

### 4. `readyTimeout` is 5s, the chart probe timeout is 3s

**Closed.** `scheduler.ReadyTimeout` is 2s, exported so a test reads it against
the chart's own `readinessProbe.timeoutSeconds` and the two cannot drift apart
again. Readiness also pings every source at once rather than in sequence, which
the tighter budget needed: asked one at a time, a single cluster that hangs
spends the whole budget before the next is tried, so the shorter deadline would
have reported a healthy multi-source ruler as not ready.

`internal/scheduler/http.go:15` against
`deploy/chart/clickhouse-ruler/values.yaml:204`. kubelet gives up first, so the
reason-carrying body the handler exists to produce never reaches anybody.

### 1. `clickhouse_ruler_problem` leaks a stuck series when a rule file is renamed

**Closed by `4da0286`.** `deleteGoneSeries` clears by rule and file together
(`internal/scheduler/scheduler.go:424`), so a rule that moved leaves no series
behind.

The evaluation and re-check feeds clear by `{rule, file, check, source}`
(`internal/scheduler/scheduler.go:476`, `internal/scheduler/recheck.go:75`).
Nothing in the reload path clears by `file`:

- `deleteGroup` does not touch `Problem` (`internal/scheduler/metrics.go:385`)
- `deleteRule` does not touch `Problem` (`internal/scheduler/metrics.go:401`)
- `deleteRuleName` does, but only when no group holds that alert name any more
  (`internal/scheduler/scheduler.go`, `deleteGoneSeries`). A rename keeps the
  name, so it never fires.
- `ReportLoadFindings` clears by `check`, and only for load-owned checks
  (`internal/scheduler/load.go:31`)

Move a rule from `payments/checkout.yaml` to `payments/checkout_latency.yaml`
and the old series sits at 1 until the process restarts. `clickhouse_ruler_problem > 0`
is the shipped alert (`docs/operations.md:327`), so it pages forever with no fix
available to the person it is addressed to. Spec 8.2 says a finding that went
away has to stop being a series; this breaks that contract.

The same leak happens when a rule is deleted from one group while another group
still holds a rule by that alert name.

Confirmed against `deleteGoneSeries` with a throwaway test: one series survives
a move from `rules/old.yaml:payments` to `rules/new.yaml:payments`.

### The Alertmanager URL is never verified

**Closed by `3c6dfec`, merged in #64.** A malformed URL refuses to start, and
`clickhouse_ruler_alertmanager_last_probe_successful` reports whether the
configured Alertmanager answered its last probe. Deliberately not a readiness
term: one Alertmanager serves every replica.

No startup check, not a readiness term, and `alerts_send_failures_total` only
moves once something fires. A typo'd `--alertmanager` is a ready, green, silent
ruler until the first incident. This is the worst failure mode in the thing and
it has no signal at all.

### The re-check pass has no observability

**Closed by `5f3718d` and `7070009`.** The pass reports its last completion,
its last duration, and the clusters it could not sample.

`internal/scheduler/scheduler.go:544` calls `runGroup(ctx, s.clock, spec, nil, nil)`
and does not wrap `spec.Eval` the way the real group loop does. So the pass has no
last-run timestamp, no iteration count, no missed count and no failure count.

If it stalls or overruns its hour, `rule/attribute-key` simply never raises,
which is the exact failure spec 8.6 names: an empty panel looks identical to a
healthy system. Cheapest fix on this list and the biggest hole.

### One log line is in neither table

**Closed by `7070009`.** The line is in spec 8.4's table and in the log table
in `docs/operations.md`.

`rule loaded with a finding that should have blocked the merge`
(`internal/scheduler/load.go:63`) is missing from spec 8.4 and from the log table
in `docs/operations.md`. Spec 8.7 promises "every log line, with what it means",
and this is the line for the newest and least obvious behaviour: a rule that got
past CI is running anyway.

## Order to fix

1. The list of Alertmanagers, which the send queue unblocked.
2. The consumer toil, starting with what a rule can template: the gap between
   "a rule names no cluster" and `FROM otel.otel_traces` is the one an author
   meets first.
