# Decisions made

Decisions already made, each with the reasoning that produced it. Read this
before reopening one.

Part of the [clickhouse-ruler spec](../spec.md). Section numbers are stable
and are what the code comments cite.

---

## 11. Decisions made

- **Time window injection.** Full SQL, author must use `{{ .From }}` and
  `{{ .To }}`. See 6.4.
- **Ownership.** Open source project for now. Production ownership at scale is
  deferred, not answered. Revisit before anyone pages off it.
- **Test fixtures.** Own compose stack with real collector, real Alertmanager,
  and a custom emitter. See section 9.
- **Name.** Project and repository are `clickhouse-ruler`. Binary is `ruler`,
  following the Prometheus and Mimir convention. Module path
  `github.com/dennisme/clickhouse-ruler`.
- **Source definitions.** Sources live in their own file, separate from rule
  files, so CODEOWNERS can gate them. See 6.2.
- **Credentials.** No DSN. Address, database and username are written in the
  file; only the password comes from `password_file` or `password_env`, and
  setting both is an error. See 6.2.
- **Label precedence.** Four levels, weakest first: group labels, rule labels,
  result columns, then the matched source's labels. The source wins over the
  query because it states where the evaluation happened and the query is in no
  position to know better. `alertname`, `source` and `team` are written last and
  are protected: an identity query data can set is a routing hazard, and `team` is
  what routes the page. `team` joined them when `rule/protected-label` stopped
  refusing a start, since until then the refusal was the only thing keeping a
  result column from overriding the rule's own team. See 6.3.1.
- **The ruler owns the `ruler_` annotation prefix.** A failed annotation
  template puts its error in an annotation the ruler writes, so the name is
  reserved and `annotations/protected` refuses a rule that sets one. A prefix
  rather than the single name `ruler_error`, so the next ruler-owned field needs
  no second reserved name. Fixed at `error` and not renameable: turning it off
  restores the silent overwrite, on the one evaluation nobody watches. See 6.5.
- **A failed annotation template is a finding on the rule's owner.** The page and
  the log already carry the error, and neither names a team, a file, or whether it
  is still broken, so the failure is also raised on `clickhouse_ruler_problem`.
  Under `annotations/template`, the name a pull request already uses for it, the
  way `rule/cost` is one name for a prediction and a measurement: one page, and one
  setting that covers the merge and the backstop. The runtime half exists because
  the check warns by default, because a repository with no address gets no tier 1
  finding, and because a schema can move after the merge. A pass that rendered no
  annotations does not clear it. See 6.5, 8.2.
- **Source selection is a selector on the rule.** A source carries `labels`
  saying what it is; a rule carries a `sources` selector saying what it wants.
  Adding terms narrows. The reverse, sources declaring requirements on rules,
  was tried and removed: it meant adding labels could only ever widen a rule's
  reach, so targeting one cluster out of several was inexpressible. See 6.10.
- **An empty selector matches nothing.** Selectors conventionally treat empty
  as "everything", and that is wrong for a field which picks the database user
  a query runs as. A rule must never reach a cluster by omission. Running
  everywhere is an operator putting a shared label on every source and a rule
  selecting it, which states the intent instead of inheriting it. See 6.10.
- **Nothing is inferred from the directory.** Labels come from the rule file,
  never from a path. Layout decides who reviews a change and nothing else.
  Deriving `team` from a directory was tried and removed: a rule at the tree
  root has no directory, moving a file silently repoints who gets paged, and a
  label in no file is one nobody can grep for. See 6.3.1.
- **The Alertmanager route tree is not generated.** Keying a generated tree on
  `team` so that eval and routing share one source of truth was the plan, and
  it is dropped. Alertmanager's configuration is owned by whoever runs that
  Alertmanager and already sits under their review and change policy, so a
  generator writes into a file whose production rules are theirs. Against that
  cost it buys a tree keyed on one label, which is hand writable and then
  static. A flag can add it later without changing anything else. What replaces
  it is documentation: an alert whose labels say whose rule it is and which file
  to edit is actionable without us touching config. See 5 and 6.5.
- **There is no `ruler watch`.** A third mode re-validating loaded rules on a
  timer was the plan. Most of what it would have re-asked is free from the
  evaluations already running: every evaluation knows its result's column names
  and types, its cost and whether it errored, so comparing one against the last
  catches a dropped or retyped column, two sources that stopped agreeing, a cost
  over a ceiling and a query that failed, at one group interval and no extra
  query. The last of those needed a name of its own, `rule/execution`, because
  "the query stopped running" is not `rule/inspect`. **The comparison is on the result's shape, per rule, never on row
  counts, per alert**: zero rows is the healthy state of most alert rules, so a
  row-count comparison fires on every resolve and no threshold fixes that. What
  is genuinely left is `rule/attribute-key`, the OTel map key rename, which
  needs its own sampling query because the shape and the row count both look
  healthy while the rule matches nothing forever. That one check gets a slow
  timer inside `ruler run`, on `--recheck-interval`. No feed on that gauge refuses
  or unloads anything; all of them report to `clickhouse_ruler_problem`, whose labels name
  the team and the file because the fix belongs to the rule's owner rather than
  the ruler's operator, and each rebuilds only the checks it owns so one clock
  cannot resolve the other's findings. See 6.3.2, 8.2 and 10.4.
- **Identity across sources.** A rule matching several sources produces one
  alert per source, kept apart by a protected `source` label. A source's other
  labels are free-form, travel onto its alerts, and are the operator's to
  route, which means a catch-all worth reading. Collapsing alerts into one
  notification is Alertmanager's `group_by`, not the fingerprint's job. See
  6.5 and 6.10.1.
- **How a rule gets its ClickHouse user.** From the source it matched. 6.6's
  directory derivation is dropped: per-team isolation is a source per team,
  which needs no directory convention.
- **Matching nothing is a warning.** Whether a rule can run depends on which
  ruler is asking, so a shared repository is legitimately unmatched on a ruler
  holding another datacenter's sources, and a rollout may land rules before
  the source for a new cluster. See 6.10 and 10.2.
- **Backfill replays the rule rather than rewriting it.** A replay runs the
  rule's own SQL once per evaluation the range holds, re-rendering
  `{{ .From }}` and `{{ .To }}` a step at a time, and collapses the results
  through the same alert state machine production uses. The rejected
  alternative is the bucketed rewrite 7.4 used to describe: one
  `GROUP BY toStartOfInterval(...)` over the whole range, which scans the data
  once instead of once per window. It cannot be built from what a rule is. The
  rewrite needs the predicate, the value expression and the label columns as
  separate fields, and `expr` is free-form SQL with nothing to extract them
  from, which is the same reason 7.2 refuses to parse SQL ourselves. It is also
  the only option that reads the data once, so it is the one a second strategy
  would be, and that is why the seam exists at all: a named type in Go with one
  implementation, and no `strategy:` key in any file until there is a second
  one to name. See 7.4.

- **What this project is for.** Keeping ClickHouse alerts in git is a solved
  problem, by operators and Terraform providers. The two gaps left are running
  one service instead of a platform, and checking the query itself. See 1, 3
  and 4.
- **Check configuration.** Correctness checks are fixed; convention checks
  take a configurable severity and key list, defaulting to the current lists
  at `warn`. Rigid defaults narrow who can use the tool. Severity decides who
  is on the critical path to unblock a contributor, so errors stay rare.
  See 7.6.
- **Nothing is inferred from the directory.** Labels come from the rule file,
  never from a path. Layout decides who reviews a change and nothing else.
  See 6.3.1.
- **Policy scoping.** Policy is set at instance, datasource and team scope,
  and a rule gets the strictest setting that applies to it. The merge is a
  maximum, so no precedence rule exists and no scope can loosen another.
  See 7.7.
- **A resolve is retried for a derived window, in memory.** A resolved instance
  is kept and re-asserted for the resend tolerance times the period it is
  re-sent on, so a failed resolve has further attempts instead of one. Derived
  rather than copied from Prometheus' 15 minutes, so widening a resend setting
  widens the window with it. No persistence: surviving a restart is a separate
  question. See 6.5.
- **Annotations are rendered at evaluation time and stored on the alert.** A
  resolved alert then says what it said when it fired, a broken template is an
  evaluation problem rather than a delivery one, and one unrenderable annotation
  cannot block a batch. See 6.5.
- **Metrics are namespaced `clickhouse_ruler_`, never bare `ruler_`.** `ruler`
  is a component name that Mimir, Cortex and Loki also expose, so the bare
  prefix does not say which ruler produced the series once it leaves the
  browser it was read in. See 8.2.
- **`clickhouse_ruler_rule_evaluation_failures_total` counts evaluations that did not
  happen, and nothing else.** A degraded evaluation that still delivered its
  alerts is `clickhouse_ruler_annotation_failures_total`, a separate counter, because this
  name tracks Prometheus' own and a dashboard carried over from a Prometheus
  ruler has to keep reading true. See 8.2.
- **A broken annotation template never stops a page.** Each annotation renders
  independently, the ones that worked are delivered, and the one that failed
  carries a short fixed marker while `ruler_error` carries the error itself. A
  consumer parsing `summary` keeps a known short string, a responder still sees
  that something is wrong, and a machine has one field to read. Hard or soft is a
  check-time choice through `annotations/template`; at runtime the page always
  goes out. Anything else means a mistake in one annotation costs a responder the
  others next to it. See 6.5 and 7.6.
- **Labels decide identity; the fingerprint is a bucket.** Two instances that
  hash alike stay two alerts, compared on their label sets. Prometheus keys on
  the hash alone, and the comparison is cheap enough that there is no reason to
  take the bet. See 6.3.
- **Two rows reaching one identity fail the evaluation.** Not a merge and not a
  warning: the rule is asking for something it cannot express, so it counts as
  an evaluation failure, leaves the state untouched like any other, and names
  the collapsed label set in the log. See 6.3.
- **A failed source lets its firing alerts expire, and the margin is tunable.**
  A source whose query fails is skipped for that tick, so its firing alerts are
  not re-asserted and they live on the `endsAt` of the last successful send.
  That is Prometheus' behaviour, and `--resend-tolerance` defaults to
  Prometheus' number so an operator who changes nothing gets what a Prometheus
  ruler would have given them. It is a flag because Prometheus sized that
  budget for a local evaluation and ours is a query to a separate database.
  Re-asserting from a read-only snapshot of `alert.State` was the alternative,
  and it is not here: it means claiming an alert is still true when the ruler
  cannot check. See 6.5.
- **A check we cannot make sound belongs to the database; a check the database
  cannot make at all belongs to us.** Drawn once in 6.7.1 so that no individual
  check can claim to be the guarantee. Tenancy, mutation, external data and the
  cost ceilings are the grants and the profile; time bounds, `SELECT *`,
  nondeterminism, generator table functions, annotation variables and predicted
  cost are the checker. The tier 1 checks that refuse `remote()` or a foreign
  table are demoted to fast feedback, which is also what lets `table:` be read
  naively and unblocks tier 1 ahead of 6.9.
- **The banned-function list is ours to curate.** `system.functions` has no
  `is_deterministic` column, measured on 25.8.2, so there is nothing to read it
  from and the list will be incomplete. Configurable for that reason. See
  6.7.1.
- **Generator table functions are the one place the allowlist is load
  bearing.** `numbers()` and `generateRandom()` need no privilege, so unlike
  `remote()` and `url()` there is no grant behind the check, and the database's
  only answer is a timeout after the cost is spent. See 6.7.1.
- **The user contract is verified by probe, not by reading grants.**
  `SHOW GRANTS` reports role membership rather than what the roles contain, so
  a user granted `REMOTE` through a role reads as unprivileged. The check
  issues the queries that are supposed to be refused, against endpoints that
  cannot act if they are not, and treats the denial as the pass. Constraints
  are still read from `system.settings`, which does expose the effective
  `min`, `max` and `readonly`. See 6.7.2.
- **No upstream package parses ClickHouse SQL in Go, and upstream declined to
  ship one.** The official drivers carry no parser, and the request to expose
  ClickHouse's own was answered with "use EXPLAIN". The alternative,
  `AfterShip/clickhouse-sql-parser`, is a second implementation of the dialect
  that answers what a library thinks the query is rather than what the cluster
  thinks, and only the second one runs it. The cost we take instead is
  depending on an output format with no stability guarantee, held down by
  fixtures captured from a real server. See 7.2.
- **Tier 1 reads the parsed query, not its text.** `EXPLAIN AST` returns the
  tree ClickHouse itself built, so a table function is found by sitting under
  a `TableExpression` rather than by matching a name, and a nested one inside
  a CTE is found the same as one at the top. It also refuses a second
  statement itself, which settles multi-statements without anyone hunting
  semicolons in a string. See 7.2 and 7.3.
- **The table-function allowlist ships empty, and defaults to `error`.** The
  operator knows which functions their rules need; the tool does not, and a
  name nobody thought of has to fail closed. Error rather than warn because
  this is the one check that is preventive rather than early feedback: the
  grants refuse `remote()` and `url()`, and nothing refuses `numbers()`. See
  6.7.1 and 7.6.
- **A list's stricter direction is per check.** Required lists union across
  scopes, allowlists intersect. Both are commutative, so 7.7's order
  independence is untouched, and a source still cannot loosen what the
  instance policy set. See 7.7.
- **The contract is checked once per source, never per evaluation.** In
  `ruler check`, at startup and on reload. Grants do not change between two
  ticks in any way worth four refused queries of latency in front of a page,
  and a stale report is safe because the grants are the control rather than
  the report (6.7.1). Both directions are probed: the privileges that must be
  absent, and the one grant that must be present, so an under-granted source
  fails in CI instead of on its first evaluation. See 6.7.3.
- **`source/privileges` is a configurable check with a required-assertion
  list, defaulting to `warn`.** Its severity governs the report and never the
  guarantee, which is what separates it from every other check: a cluster
  where it is `off` is as safe as one where it passes. `warn` because a ruler
  pointed at an existing cluster fails it on day one, and a check that blocks
  the first run gets switched off rather than fixed. See 7.6.
- **`clickhouse_ruler_alerts_sent_total` counts alerts, and there is no batch counter.**
  The name tracks a Prometheus metric that counts alerts, so counting batches
  read wrong on any dashboard carried over. "How much traffic is Alertmanager
  taking" is a real question, but `clickhouse_ruler_notification_latency_seconds` already
  has an observation count per send, so a second counter would be a third way
  to ask something nothing is asking yet. See 8.2.
- **Query concurrency is bounded per source as well as ruler-wide.** The
  ruler-wide cap stays as the ceiling; inside it a source may set
  `max_concurrent_queries`, so a cluster that has gone slow holds only its own
  slots and rules against healthy clusters keep running. This was deferred
  while nothing showed what a source cost, and the query cost metrics in 8.2
  answered that. No default: a number picked here would be either at or above
  the ruler-wide cap, where it does nothing, or below it, where it silently
  lowers throughput for a single-source deployment that has nothing to protect
  itself from. `clickhouse_ruler_query_queue_wait_seconds` is what an operator
  sizes the limit from, labelled by source because queueing is a property of
  the cluster rather than of whichever rule happened to wait. See 6.11.
- **Two rules that produce the same alert are a check of their own,
  `rule/duplicate-alert`, at `warn`.** `rule/name` scopes uniqueness to the
  group on the reasoning that group labels and `source` separate two
  same-named rules in the fingerprint, and that reasoning is a statement
  about how the files happen to be written. Rules agreeing on their alert
  name, their effective labels and the sources their selector reached share
  one fingerprint, so they share one entry in the resend cadence and one
  alert in Alertmanager: whichever evaluated last wins and a resolve from
  either can end the other's page. Tier 0, and in the loader rather than the
  rule parser, because deciding it needs the sources file as well as the
  rule. `warn` rather than `error` because the finding is about two rules at
  once, usually in two files with two owners, and the author who trips it
  often owns neither the other file nor the policy. It is not exact: result
  columns are level 3 of 6.3.1 and exist only at evaluation time, so a
  flagged pair may distinguish itself at runtime on a column one of them
  returns. Reporting it anyway is the safe direction,
  because such a pair is relying on that with nothing in either file saying
  so, while the reverse case needs the result and is not a tier 0 question.
  See 7.6.
- **Team-scoped policy files are read, and the rules tree reserves two
  names.** A `ruler.yaml` in a team directory applies to every rule at or
  below it, alongside the instance file and the matched sources. It needs no
  precedence rule, because the merge is a maximum and the worst a team can do
  with its own file is hold itself to more than the baseline. Which files are
  policy is decided by name, not by content: `ruler.yaml` is policy,
  `sources.yaml` is the sources file the quick start keeps beside the rules,
  and neither is a rule file. Sniffing for a `groups:` key instead would guess
  about a file whose name the author can already read, and would report
  nothing useful about a rule file that misspelled that one key. The
  `ruler.yaml` at the rules root is the instance scope and not also a team
  file: the severity would be the same either way under a maximum, but the
  origin a finding carries is what `--explain` prints, and it has to name the
  scope that actually set it. See 7.7.
- **Matched sources have to agree on a rule's result, and disagreement is
  `rule/source-schema` at `warn`.** A rule selects sources rather than naming
  one and writes its own table into the SQL, so every cluster a selector
  reaches has to carry that table with the columns the query reads. Tier 1,
  because the columns are the cluster's answer rather than the file's: each
  source is asked through the `DESCRIBE` the result checks already run, and
  the answers are compared afterwards. That is also the only place the
  comparison can happen, since one inspection sees one source, which is why
  the columns leave an inspection the way its cost estimate already does.
  Reported once for the rule, naming both sources and the column, because per
  source it would say the rule is broken against the cluster that has drifted
  and correct against the one that has not, which is the confusing answer the
  check exists to replace. A source nobody could reach, or whose user cannot
  read the table, has not disagreed with anything: it produced no columns, so
  it is not compared, and `rule/inspect` and `rule/table-access` have already
  said what happened. `warn` rather than `error` even though the rule
  genuinely cannot be correct everywhere it runs, and the reason is who is on
  the critical path: which sources a selector reaches is decided by the labels
  an operator put on them, and whether two clusters carry the same table is a
  migration the rule's author does not own, so an error would block a rule
  change behind another team's cluster. The severity of the finding is the
  operator's to raise, and for them it is about a file they can act on.
  See 6.10, 7.3, 7.6.
- **Hot reload is `SIGHUP`, and the same rule across a reload is the same
  label set.** Nothing watches the filesystem: whoever rolled the files out is
  the only party that knows when a rules tree is complete, and a watcher would
  read one half written. A reload goes through the loader startup uses, so
  validation still has one entry point for the ruler and one for CI (7.1), and
  an error-severity finding refuses the whole reading while a warning is
  reported and applied (7.6). What carries over is the alert state of every
  rule whose name, effective labels and source have not changed, because those
  three are what an alert's identity is assembled from (6.3): change one and
  every instance being tracked has a fingerprint no later evaluation will
  produce, so it would resolve on the next tick and be re-created with its
  `for` starting from zero. Refusing to carry it is that outcome in one step
  instead of two, and without a resolve notification for an alert that did not
  recover. Everything else about a rule is free to change and the instance
  survives: a new threshold, a new window, a shorter `for`, an edited
  annotation. State is looked up per group and alert name, so a rule moved to
  another file or group starts fresh anyway: matching on labels alone across the
  whole tree would hand a deleted rule's state to an unrelated rule that happens
  to agree with it, and a move already renames every series the rule has. The notify.Cadence outlives a reload too, so a firing alert keeps
  its place in the resend interval rather than being re-posted because somebody
  edited a file. See 6.3, 6.5, 6.10.1, 7.6.
- **A refused reload keeps the previous version and says so on one of the two
  reload gauges, not both.** `clickhouse_ruler_config_last_reload_successful`
  is about the last attempt, so a refusal sets it to 0 and it stays there until
  a load succeeds: that is the alert, because the rules still running are valid
  and nothing about them looks wrong from the outside.
  `clickhouse_ruler_config_last_reload_timestamp_seconds` is about the
  configuration being evaluated, so a refusal leaves it alone. It exists for
  `time() - ...`, read as how old the running rules are, and stamping it on a
  refusal would answer that with the moment the ruler declined to change
  anything. The refusal is all or nothing, including the files in the reading
  that are fine, because a rules tree is loaded as a tree and half of one is not
  a configuration anybody wrote down. Series for a group or rule the reload
  dropped are deleted, since a counter left at its last value is
  indistinguishable from a rule that is loaded and quiet. See 7.6, 8.2.
- **A sidecar delivers the rules and triggers the reload, and the default is
  `git-sync`.** The ruler exposes a trigger and owns no delivery, which is what
  Prometheus and vmalert both settled on and why the watching in that ecosystem
  lives in `prometheus-config-reloader` rather than in the server. `git-sync`
  earns the default by solving atomicity for free: it clones into a worktree and
  flips a symlink at the rules path, so the tree is never read half written, and
  its exec hook runs after each successful sync, which is the thing that posts to
  `/-/reload`. A ConfigMap mount is the small-estate case and has no hook, so it
  needs a reloader sidecar of its own. Both layouts put the loader in front of a
  symlink, which is why reading each of them exactly once is a property the
  loader owes rather than a detail of a chart. See 10.2.
- **In-process `inotify` is rejected, and interval polling is the option held in
  reserve.** A watcher inside the ruler would have to watch the directory rather
  than the file, because a ConfigMap mount updates by flipping a `..data`
  symlink and a watch on a path sees nothing; events are dropped on overlayfs and
  over NFS, so it would need a periodic resync anyway, which is why even the
  dedicated reloaders poll alongside their watch; a single logical change arrives
  as a burst needing debounce; and all of it buys back the one property the
  signal has, that whoever rolled the files out says when they are complete. If
  the ruler ever reloads itself, it polls on an interval and compares a hash
  first. The polling is what Prometheus and vmalert both offer and neither
  enables by default: `--config.auto-reload` is off with a 30s interval behind
  it, and `-configCheckInterval` is 0. The hash is Prometheus' half alone, which
  checksums the file each tick and skips the reload when it has not changed;
  vmalert parses the rules and compares the parsed groups instead. Ours goes
  before the read rather than after, and the hash is not an optimisation: a
  reload re-checks the user contract and issues statements per source, so
  comparing afterwards the way vmalert does would already have put rule traffic
  on every cluster for a file nobody edited. See 6.7.3, 10.2.
- **A reload tolerates what a start refuses, and a start stays strict.** Which
  findings refuse a reading at all is narrowed by the decision below, to
  `yaml/syntax` and `ruleset/directory`; this is about the two occasions, and
  holds for whatever the set contains. The same files that keep the previous
  version running on a reload refuse to start the
  process, which makes a refused reload a hazard ahead of the next restart rather
  than only a stale configuration: the ruler survives on the version it already
  had, `/-/ready` keeps passing because rules are loaded and a source answers, and
  the replica is lost the next time anything unrelated restarts it. The
  asymmetry is deliberate in both directions. Tolerating a bad reading at startup
  would mean a process that is up, failing readiness and paging nobody, and
  refusing to reload is the only way to keep a valid configuration running when
  the one on disk is not.

  **Both upstream rulers already do exactly this**, which is worth stating
  because an asymmetry argued only from first principles reads as an invention.
  Prometheus loads its rule files through one `reloadConfig` at startup and on
  `SIGHUP`, and its rule manager returns `error loading rules, previous rule set
  restored` when a file fails: on a signal that error is logged and the rules
  already running stay, and at startup the same error aborts the run group and
  the process exits non-zero. Its documentation states the granularity too, that
  the changes are only applied if all rule files are well-formatted, so a
  refusal covers every file the globs matched rather than the one at fault.
  vmalert is the same shape in different code: a parse failure at startup is a
  `Fatalf`, and the same failure on the reload path sets a config error, logs it
  and continues on the configuration it already had. Neither offers a way to
  load the good files and drop the bad ones. See 7.6, 8.1, 12.5.
- **A severity and a refusal to start are two axes, and only a file nobody can
  read refuses.** These were one property until 12.5 was worked through, and
  fusing them is what made that question hard to answer. A severity says who has
  to be involved to unblock a contributor: a warning is the author's to act on,
  an error needs a repo owner to change policy (7.6). Whether a finding stops the
  ruler reading the files at all is a different question with different stakes,
  and answering it with the severity meant `annotations/protected` took a
  restarted ruler down over a name collision in a field that does not route.

  A finding refuses a start when there is nothing to read, and that is
  `yaml/syntax` and `ruleset/directory`. Every other finding blocks a merge,
  loads, and raises `clickhouse_ruler_problem` under its own check name. The
  argument is the one this whole section keeps returning to: a ruler that will
  not start pages nobody, so the blast radius of refusing is every alert in the
  checkout and the blast radius of loading is one rule behaving as written rather
  than as intended. CI is where this is caught, the checker gates the merge
  (10.3), and a refusal only ever fires when CI did not run or somebody merged
  past it. Punishing every other team's alerts for that is a shared fate nobody
  chose.

  **The severities do not move, which is the point of splitting the axes.**
  `annotations/protected` stays fixed at `error` because an annotation a
  responder reads is worth a repo owner's attention, and it stops blocking a
  start because `ruler_error` is a convenience that saves an Alertmanager
  template a step rather than the operational signal, which is the metrics.
  `rule/protected-label` stays fixed at `error` because a label collision has no
  route in a generated tree, and it stops blocking a start for the same reason.

  **Unknown fields were the hardest of these and the schema stays strict.** The
  reader walks the YAML by hand and reports an unrecognised key as a finding with
  a line number, which is what lets the checker point at it in a diff, so
  strictness costs nothing and is kept. What moves is where it is enforced. A
  strict schema that refuses a start is a version skew hazard rather than a typo
  net: 10.2 has rule files and the binary deploying independently, so a ruler
  rolled back behind files that use a field the older binary does not know would
  refuse to come up, which is an outage caused by our own strictness. `yaml/type`
  travels with it, since a field of the wrong shape leaves the reader able to skip
  that node and load the rest.

  **Two of these hand the bill to somebody who is not the author, and they are
  documented rather than excepted.** A rule missing `{{ .From }}` or `{{ .To }}`
  scans unbounded on every evaluation, and a rule with its own `SETTINGS`
  replaces the limits the ruler sends (7.3). Both cost the cluster rather than
  the ruler, and the person paying is whoever owns ClickHouse rather than whoever
  wrote the rule. They still load, because the alternative is the whole checkout
  refusing, and what makes that safe rather than a foot gun is owed in two
  places: the gauge names the check, the team and the file, and the operations
  page says what each of the two costs and how to see it (8.7). A signal an operator
  was never told to watch is the same as no signal.

  **One runtime defect had been hiding behind the refusal.** `labelsFor` writes
  `alertname` and `source` last, so a rule that sets either is silently ignored
  and safe to run, and a matched source's `alert_labels` win over the query too.
  `team` was neither: a result column named `team` overrode the rule's own label,
  and `team` is what routes the page and labels the gauge. That never happened
  because `rule/protected-label` refused the start, so the refusal was load
  bearing without saying so. The ruler now wins `team` the way it already wins
  `alertname` and `source`, and raises the gauge when a query produces one, which
  is what makes the check merge-only rather than merge-only and a routing hazard.
  See 6.3.1, 7.6, 8.2, 12.5.
- **A reload re-checks the user contract.** 6.7.3 lists three places the
  contract is checked and a reload is one of them, so this is the decision to
  honour it rather than to make. The argument for doing it is the window 6.7.3
  names: a grant revoked while the ruler runs is not noticed until the next
  check or reload, and a ruler up for a month that never re-read the report
  would make that window the process lifetime. The argument against is cost, and
  it does not hold: a handful of statements per source, each refused before it
  does any work, at a moment an operator chose, and never once per evaluation.
  It runs before any connection is opened, so a source refused at error severity
  is never connected to, and it refuses that source alone while the rest of the
  reload proceeds. See 6.7.3, 7.6.
- **A restart loses pending state and that is accepted, not solved.**
  `ActiveAt` lives in memory, so a restart makes every pending alert serve its
  `for` again and a condition that clears inside that second `for` never pages.
  Prometheus restores it from an `ALERTS_FOR_STATE` series written back to its
  own storage. The ruler has no storage: it reads ClickHouse as a user pinned to
  `readonly = 2` (6.7.2), so keeping alert state means granting it somewhere to
  write and owning a schema, a retention policy and a migration path for it,
  which buys back one `for` of latency on a restart. Prometheus behaves the same
  way when that series is unavailable. What is done instead is a reload that
  keeps pending alerts (7.6), an alert expiry long enough that a firing alert
  survives an ordinary deploy (6.5), and documenting the loss where an operator
  reads about restarts rather than leaving it as an open question. This is the
  answer for a single ruler and for a replica joining a set; the high
  availability question in 12 is still open and is the one that would change it.
  See 6.5, 6.7.2, 7.6.
- **A source names one endpoint, not a list of nodes.** The ClickHouse driver
  accepts many addresses and 6.9 originally read that as something the source
  schema owed it. It does not. A list buys client side failover for a cluster
  with nothing in front of it, and every other way of reaching a cluster already
  solves that better: a managed service is one FQDN, a self hosted cluster is
  usually behind chproxy, HAProxy or a Kubernetes service, and a DNS name with
  several records is one name. What the list costs is worse than what it buys.
  It puts cluster topology in the rules repository, so replacing a node becomes
  a pull request in the alerting repo and a stale entry is silent because the
  driver simply uses another. It splits the `system.query_log` readback in 8.5,
  which is per node: once queries can land on more than one, the cost per rule
  and the operator queries on the operations page cover a fraction of the
  traffic and nothing says which. And it is a load balancer with no health
  checks, no weighting and no draining, competing with the one the operator
  runs. So the endpoint is the operator's to make available, and the ruler's
  behaviour when it disappears is already correct: the evaluation fails, alert state is untouched, `for` timers keep
  running and firing alerts keep being sent (6.3.2, 8.7). The case this leaves
  out is a bare cluster with no name and no balancer in front of it, where the
  answer is that a source is cheap: point a second one at another coordinator
  and let one selector match both, which is the sharding by source argument in
  10.2. What this costs is typing, and it is worth naming: a team with a
  production cluster and a disaster recovery one writes two source blocks that
  differ in a name, a label and an address, and the strict parser rejects the
  YAML merge key that would otherwise share the rest. A list would be the wrong
  way to buy that back. Source labels land on alerts (6.10.1), so one source
  holding both addresses produces an alert that cannot say which cluster
  answered, and a route tree that cannot either. A standby is usually behind, so
  failing over inside the driver evaluates rules against lagging data on any
  connection blip, quietly, and swallows the fact worth knowing, which is that
  the primary was unreachable. Two sources also carry a decision a list makes
  for you: whether the standby is alerted on at all, and under which labels. If
  the duplication becomes a real complaint the answer is a `defaults:` block in
  the sources file that each source inherits and overrides, chosen over YAML
  merge keys because every finding carries a line number and a merged field's
  line points at the anchor rather than at text the reader wrote; an inherited
  field's finding points at the `defaults:` line and names the source. Two
  boundaries on it, both load bearing. It carries the boilerplate fields and
  never `name`, `labels` or `address`, because those three are what one source
  is: a default label set would put the same labels on two clusters' alerts and
  bring back the attribution problem this decision exists to avoid. And it
  carries no `checks:` block, because policy merges as a strictest-wins maximum
  with deliberately no precedence (7.7), which is the opposite of the override a
  default needs, and one block holding both rules would be unreadable. The
  Alertmanager URL was originally cited here as the same case and is not one:
  Alertmanager's clustering expects a sender to post to every member rather than
  to a balancer, so a list is the right answer there for reasons none of the
  above touch. 6.5 has that argument. See 6.5, 6.6, 6.9, 6.10.1, 7.7.
- **`table:` is the table a rule reads, so on a sharded cluster it is the
  Distributed table.** 6.9 left this open on the assumption that getting it
  wrong was a tenancy hole; 6.7.1 settled that it is not, so what is at stake is
  whether a check reports on the table a rule queries or on a different table of
  the same name. The query path already answers it: an author writes their own
  `FROM`, and on a sharded cluster they name the Distributed table, because the
  local `MergeTree` holds one shard of the rows. A `table:` naming the local one
  would describe a table no rule reads. The local table's name is cluster
  topology, and a rules repository should no more carry it than it carries the
  node list, which is the same argument the one endpoint decision above makes.
  What that is worth is measurable on the two node stack. The sample behind
  `rule/attribute-key` is the one check that reads rows, and against the
  Distributed table it saw both shards' rows and found a key on each; against
  the coordinator's local table it saw one shard and reported the other shard's
  key as one nothing writes. That is a false finding on a rule that works, on
  exactly the rules hardest to be sure about, which is how a check gets switched
  off. `DESCRIBE TABLE` needs no help: a Distributed table carries the full
  structure, `Map` columns included, which is all the key check reads it for.
  The `table-readable` assertion in 6.7.2 gets stronger rather than weaker,
  which was not obvious: `SELECT 1 FROM <distributed> LIMIT 0` contacts every
  shard, so it proves the fanout and the grant on each node rather than one row
  in the coordinator's `system.tables`. Against the dead shard cluster the same
  statement fails with `279` naming the unreachable address, and that reports
  inconclusive, which is the honest answer to "can this user read the source's
  table" when part of the cluster is gone.
  **Two questions only a local `MergeTree` can answer, and they resolve
  through.** Both are backfill caveats (7.4): the retention, read from
  `engine_full` in `system.tables`, and the column history, read from
  `system.parts_columns`. A Distributed table's `engine_full` carries no `TTL`
  clause and a Distributed table has no parts, so asked naively both answer
  nothing, and nothing is indistinguishable from a replay with no caveat to
  make. So they read the engine first and, when it is `Distributed`, take the
  local database and table out of its own arguments and ask about those. That
  parses reliably because ClickHouse normalises what it stores:
  `Distributed(ruler_shards, currentDatabase(), otel_traces)` comes back as
  `Distributed('ruler_shards', 'otel', 'otel_traces')`, quoted literals with
  `currentDatabase()` already resolved. The residue is stated rather than
  hidden. The local table it resolves to is the coordinator's own copy, so both
  answers are one shard's answer to a cluster question and a shard with a
  different retention or a half applied `ALTER` is not covered; the caveat names
  the table it read so nobody reads it as a claim about the cluster. A
  coordinator carrying no local copy cannot answer at all, and that is reported
  as unanswerable rather than passed over, because a caveat that silently
  stopped appearing looks exactly like a replay that earned none. Reading every
  shard instead would need `clusterAllReplicas` or `remote()`, which the user
  contract revokes, and granting SOURCES back to qualify a count is a worse
  trade than the caveat.
  **What this does not decide.** `rule/cost` needed the shard count, which needed
  this decision first, and that is the entry below rather than a gap here. Nor
  does anything check that `table:` and a rule's own `FROM` name the same table,
  so a source naming the Distributed table while a rule reads the local one has
  its keys sampled against a table the rule does not read. That is the same class of mistake as a rule reading another
  database, which `rule/foreign-table` only warns about for the reason 6.7.1
  gives, and no privilege separates the two tables either. See 6.9, 6.7.2, 7.3,
  7.4.
- **A sharded rule's predicted cost is the coordinator's estimate times the
  shard count, and the count is read from `system.clusters`.** `EXPLAIN ESTIMATE`
  over a Distributed table answers for the coordinator's own parts alone,
  measured as one part under the local table's name, so an unscaled prediction on
  `N` shards is out by `N` and out in the direction that lets a rule through. The
  count has two possible sources and they are not equivalent. `uniq(_shard_num)`
  off the Distributed table needs no grant and reads rows, which moves this check
  out of tier 1 into the tier an operator consents to, for a number that is a
  property of topology rather than of data. `system.clusters` reads no row a rule
  could read, and it holds the number directly, at the price of one grant the
  contract in 6.7.2 did not have: measured as `497 ACCESS_DENIED` naming
  `SELECT(cluster, shard_num) ON system.clusters`. The grant is the cheaper price,
  so it is what the contract now asks for, and the cluster to count is the
  Distributed engine's own first argument rather than anything a source states,
  which keeps topology out of the sources file the same way `table:` does.
  **Scaling rather than measuring, and said out loud.** Multiplying assumes the
  shards hold roughly the same amount, and several ordinary things break that: a
  sharding key on a hot low cardinality column, a shard added later, since
  ClickHouse does not rebalance the rows already written, a different retention on
  one shard, and writes sent to a local table rather than through the Distributed
  one. The error goes both ways, so this is not a ceiling that can be called
  conservative: a coordinator holding more than its share overstates the cluster
  and one holding less understates it. It is inside what this check already
  claims: 7.3 warns rather than blocks because
  the optimiser's own estimate can be out by an order of magnitude on a skewed
  primary key, and a rule that is wrong by the shard skew was already wrong by
  more than that. What is not affordable is letting the number read as a
  measurement, so the finding says it is the coordinator's parts times the shard
  count and the summary table carries the same marker for every rule that raised
  no finding. What measures the real cluster read is an evaluation: the rows and
  bytes a query actually read arrive on the driver's progress callback and are
  already recorded per rule and per team (8.2). That is tier 2 and it is what an
  operator watching a cost should read; this check exists to catch the rule before
  it runs at all.
  **A count that cannot be read turns the ceiling off rather than guessing.**
  Comparing a shard's number against a cluster's ceiling is the failure this
  entry exists to remove, so a source whose user cannot read `system.clusters`
  has its cost reported unestimated, saying the estimate covers the coordinator's
  parts only. This is its own answer rather than the refusal a denied `EXPLAIN
  ESTIMATE` reports, because a user who cannot read the rule's table and a user
  who cannot count the cluster behind it are different facts and have different
  fixes. It turns the check off on exactly the clusters where cost matters most,
  which is why it is the fallback and the grant is the answer.
  **The missing grant is reported by the contract check, not by the cost check.**
  A fallback nobody is told about is the silence this whole entry is against: an
  unscaled cost breaches no ceiling, so the cost check has no finding to carry it
  and the summary table only exists when somebody asked for one. So it is the
  `clusters-readable` assertion in 6.7.2, which reports once per source in CI, at
  startup and on a reload, is droppable on its own by an operator who cannot grant
  it, and is asked only where the source's table is Distributed. Per source rather
  than per rule because one grant on one user is not a fact about a rule, and the
  alternative reports the same sentence once for every rule that matched the
  cluster. See 6.9, 6.7.2, 7.3, 7.10.
- **A dashboard that renders is proven by Prometheus, not by Grafana.** The gate
  in 8.6 proves a panel names a metric something registers, which a name with the
  wrong grouping label or an impossible matcher passes just as easily, so the
  panel still draws nothing and an operator reads the empty graph as a healthy
  system. Answering the other half needs series, which needs the ruler running,
  a Prometheus that scraped it and rules that evaluated, and those are items 5, 7
  and 8 of the stack in 9.1. They belong together because each is useless without
  the one before it: a Prometheus with nothing to scrape holds no series and a
  Grafana with no Prometheus renders the same empty panels the exercise is meant
  to catch.
  **The assertion is that the query is answerable.** Driving Grafana's API, or
  its browser, is a large amount of machinery to learn something the datasource
  already knows, and it fails for reasons that have nothing to do with the
  dashboards. Grafana renders; it does not decide whether an expression matches
  anything. So the test sends every expression to Prometheus and requires a
  reply. It does not judge an axis, a unit or a legend, and it does not demand a
  non-empty result, because most of the alert rules dashboard is empty while
  nothing is wrong and a test that demanded otherwise would have to make a rule
  fire to pass.
  **The ruler in the stack is built from the checkout and its rules do not
  fire.** A pulled image is the last release rather than the code under test,
  which is the distinction the action's own workflow already draws (10.3), and
  the layer cache keeps the build off every `compose-up` that changed no Go. The
  rules are `deploy/stack` rather than `cmd/ruler/testdata`, because that tree is
  a fixture a test rewrites in place and the address in its sources file is the
  host's rather than a container's. They evaluate and stay silent so the only
  alerts reaching Alertmanager are the ones a test sent: the sink in 9.1 is
  shared, and a rule paging continuously would put deliveries nobody asked for in
  front of every assertion that reads it. Evaluating is enough, since the series
  9.8 queries come from a rule that found nothing.
  **Grafana's variables are filled from the dashboard's own answer.** An
  expression in a panel is not valid PromQL, so the test substitutes, and what it
  substitutes decides how much is still being asserted. A dashboard variable is
  replaced by its `allValue`, which is what the datasource receives when a viewer
  leaves the dropdown on All, so the value is read out of the file rather than
  invented and a variable that stops matching anything fails here. Only
  `$__rate_interval` is the test's own, because Grafana computes it from the
  panel's time range and the scrape interval and there is nothing in the file to
  read. See 9.1, 9.8, 8.6.
- **A rule is proven against what a collector wrote, not against rows a test
  inserted.** The trace schema in `deploy/clickhouse/init` is copied verbatim
  from `exporter/clickhouseexporter` so that a rule which works in the tests
  works against real collector output, and until the collector was in the stack
  nothing had ever demonstrated that: every row came from a test's own `INSERT`.
  The collector writes as the admin user, which is where the stack's other
  writes come from, because the ruler's users hold `SELECT` on one table and
  nothing else and an exporter holding `INSERT` would contradict the contract
  6.7.2 exists to prove. Schema creation is off so the table it writes is the
  verbatim copy rather than one the exporter made.
  **Volume is opt-in.** `telemetrygen` sits behind a compose profile and off by
  default. It is in the stack because a check read against a table holding only
  what a test put there has never met a busy one, and it is off because every
  assertion in the tree was written against a table only tests write to: rows
  arriving on their own would be a second author of the data all of them read.
  **The deterministic emitter is a package, and it carries no dependency.**
  `internal/spans`, driven by the tests and called by nothing else, because a
  command would need a flag surface, a place in the release and a reason for an
  operator to run it while the thing it exists for is an assertion.
  `telemetrygen` gives volume and cannot express "exactly 40 spans over 1000ms
  on `ServiceName=checkout` starting at T+30s", and without that an alert count
  is not assertable. It posts OTLP over HTTP with a JSON body, the encoding the
  protocol defines for producing telemetry without an SDK, rather than taking on
  the OpenTelemetry Go SDK and its exporter: three modules and their
  dependencies to build a payload a struct literal covers. Nothing was added to
  `go.mod`.
  **A count is the assertion, and a second service is what makes it one.** The
  end to end test emits two services the same way and only one of them is slow,
  so a rule firing on arrival rather than on duration fails. Asserting that an
  alert arrived proves the path; asserting that exactly one did proves the
  query. See 9.1, 9.2, 9.4.
- **The deployment chain is proven on a cluster, not rendered.** `helm.yaml`
  templates the chart over every values file that matters and validates the
  output against the Kubernetes schemas, which is cheap and cannot show that a
  commit arrives. Every link after the merge was unproven, and so were the two
  symlink layouts 10.2 says constrain the loader rather than the chart: a
  symlinked root walked without being resolved finds no rules at all, a mount
  walked without skipping `..*` finds every rule twice, and both were unit tested
  against neither a real kubelet nor a real git-sync worktree.
  **`kind` rather than `k3d`**, because it is what the chart ecosystem tests on
  and because `kind load docker-image` puts the image built from this checkout
  into the cluster in one command. The stack's ruler is built rather than pulled
  for the reason a published image is the last release (9.1 item 5), and that
  reason does not change on a cluster.
  **The rules repository is `git daemon`, read only, and the test commits inside
  the pod.** `--export-all` over a `--base-path` with `receive-pack` off, so
  nothing can push over the wire, and the sha the in-pod commit produces is what
  every later assertion waits for. Two cheaper fixtures were tried first.
  `alpine/git` ships no `git-daemon` binary at all. Serving the bare repository
  as static files from a web server works only without the chart's default
  `depth: 1`, since the dumb HTTP transport does not support shallow
  capabilities, and a fixture that needs the default turned off is a test that no
  longer covers an install. `git://` is accepted by git-sync and yields the same
  worktree and symlink an SSH remote does, and `ci/git-sync-values.yaml` keeps
  its SSH remote because that file documents a real deployment.
  **Each link is waited on through a metric, and the refusal path is asserted
  too.** The sha, the symlink pointing at the worktree holding it, the merged
  rule's group appearing in `clickhouse_ruler_rule_group_iterations_total`, then
  `clickhouse_ruler_config_last_reload_successful` reading 1.
  `clickhouse_ruler_config_last_reload_timestamp_seconds` is the one link not
  waited on, because a unix timestamp exposed as a float is rounded to the
  nearest ten seconds and two reloads inside that window read as one number. 8.2 exists so the chain is observable from outside, and a
  test reading log lines would prove something nobody operates on. The exec hook
  posts with `--fail`, so a refused reload has to surface as a failed hook that
  git-sync retries with the previous configuration still running; a hook
  reporting success there would hide the case the severities in 7.9 are about.
  See 10.2, 9.1, 8.2.

- **A rule's identity is its path relative to the rules root.** `rule_group` is
  that path plus the group name, and the `file` label on
  `clickhouse_ruler_problem` is that path. The file has to be in the identity
  because a group name is unique only within its file (7.6), and the resolved
  absolute path was the wrong spelling of it: git-sync publishes a revision as a
  symlink at a worktree named after the commit, the loader resolves that symlink
  before walking, so every merge renamed every series the ruler exposes. Rates
  break across the rename, alerts on the old names resolve while the new ones
  start from zero, and the three layouts in 10.2 disagreed about what the same
  rule was called. Relative to the root they all agree, and the label is also the
  path its reader can act on: an absolute path inside a container is not one a
  rule author can open. The stagger offset is keyed on it too, so a sync no
  longer reshuffles when every group ticks. See 8.2, 10.2.
- **The revision the ruler states is a hash of the files it loaded, not a
  commit.** `clickhouse_ruler_config_info{revision, rules_root}`, always 1, one
  series per configuration version. The ruler fetches nothing, so nothing hands
  it a sha; the only sha within reach is the one git-sync puts in the name of the
  worktree it links to, and reading that would be inferring a commit from a
  sidecar's naming convention, which is wrong on a ConfigMap mount, wrong on a
  plain directory, and wrong the day git-sync changes its layout. The hash is
  also the stronger answer to the question the metric exists for: two replicas on
  one commit whose volumes disagree carry the same sha and different rules, where
  two that hash the same are running the same files. It covers every rule file
  and every team policy file under the root, each contributing its path relative
  to the root and its contents, truncated to twelve hex characters. `rules_root`
  is the resolved root, so a commit still rides along where a deployment named one,
  without the ruler claiming to know that it did. One label on an info metric
  rather than a dimension of every series, because it changes on every sync
  whether the rules did or not. See 8.2, 10.2.
