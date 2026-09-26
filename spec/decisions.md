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
  position to know better. `alertname` and `source` are written last and are
  protected: an identity query data can set is a routing hazard. See 6.3.1.
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
  carries its error as its value. Hard or soft is a check-time choice through
  `annotations/template`; at runtime the page always goes out. Anything else
  means a mistake in one annotation costs a responder the others next to it.
  See 6.5 and 7.6.
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
