# Validation

What is checked before a rule runs, where the line between our checks and
the database's enforcement falls, and how an operator configures severity.

Part of the [clickhouse-ruler spec](../spec.md). Section numbers are stable
and are what the code comments cite.

---

## 7. Validation

Modelled on Cloudflare `pint`, adapted for SQL.

This section is the reason the project exists. Section 2 found no tool that
inspects the ClickHouse query behind an alert: the Grafana Terraform provider
carries it as opaque `model` JSON, the SigNoz operator validates the shape of
its `Rule` resource rather than the SQL inside it, and ClickHouse settings
profiles cap what a query may consume without ever asking whether it has a
time bound. Storage was never the hard part.

### 7.1 One package, two entry points

`pint` is a separate binary because Cloudflare does not own Prometheus. We do
not have that constraint.

Validation is a single package called from both:

- `ruler check ./rules/` for CI
- the ruler itself at rule load time

A rule that fails validation fails to load, which fails the deploy. A rule that
dodges CI still cannot run. `pint` can only advise; we can enforce.

**A consumable GitHub Action is the third entry point.** Teams keep rules in
their own repositories, and telling each of them to write the download-and-run
YAML themselves guarantees a dozen slightly different versions, some pinned to
a stale release. Ship one:

```yaml
- uses: dennisme/clickhouse-ruler/action@v1
  with:
    path: rules/
```

It lives in this repository under `action/`, not in a repository of its own.
For a linter the action version *should* equal the tool version, and splitting
them makes users pin two things and gives us version skew to debug.
`golangci-lint-action` is separate only because it carries heavy caching and
version-resolution logic; a composite action that fetches a release binary and
runs `ruler check` does not.

The part that belongs in the binary rather than the action is the output
format. `ruler check --format=github` should emit workflow commands:

```text
::error file=rules/payments.yaml,line=12,title=rule/expr::expr must contain {{ .To }}
```

GitHub then renders each finding inline on the diff, which is the whole point
of carrying a file and line on every `Problem` (7.3). With that flag the action
is a few lines of YAML; without it the action has to parse our human-readable
output, which breaks every time the wording changes.

### 7.2 Do not write a SQL parser

Push parsing to ClickHouse: `EXPLAIN SYNTAX`, `EXPLAIN AST`,
`EXPLAIN QUERY TREE`, `EXPLAIN PLAN indexes=1`, `EXPLAIN ESTIMATE`,
`DESCRIBE (SELECT ...)`.

ClickHouse dialect parsers in Go exist but are incomplete, and the SQL surface
is too large to chase. Cost of this decision: there is no meaningful fully
offline mode. That is acceptable, because anyone running this tool already has
a ClickHouse connection by definition.

**There is no upstream package for this, and that is deliberate on their
side.** Neither official Go driver carries a parser: `clickhouse-go` and
`ch-go` speak the protocol and nothing more. Asked directly to extract the
parser as a standalone library with Go bindings, the way `pg_query_go` wraps
Postgres, ClickHouse declined: "ClickHouse uses an in-memory AST
representation, but it's just a C++ data structure and it's hard to use in
foreign code", and the recommendation was `EXPLAIN AST`, `EXPLAIN SYNTAX`,
`EXPLAIN QUERY TREE` and `clickhouse format` instead. That is this section,
arrived at from the other direction.

The same answer names the limit we live with: `EXPLAIN AST` "prints the tree
in some format. However, it's impossible to convert it back to the query."
Fine here, because the checks ask questions of the tree and never rebuild SQL
from it. It is also why 7.4's replay builds its own statement rather than
editing the author's.

**The alternative is a second implementation of the dialect, and it is a
replacement rather than a complement.** `AfterShip/clickhouse-sql-parser` is
MIT, actively maintained, and parses ClickHouse SQL to a typed Go AST with
round-trip formatting and source spans. Adopting it would buy a genuinely
offline mode and findings that point at a column inside the rule rather than
at the rule's first line.

What it costs is the thing this section is about. Our parse answers what *this
cluster* thinks the query is; a library answers what the library thinks. When
the two disagree only the first one matters, because the first one is what
runs the query, and the disagreements arrive silently as the dialect grows.
Users in that discussion report its syntax compatibility as limited, which is
the same finding from the other end.

**The risk we take instead is version coupling.** `EXPLAIN AST` output has no
stability guarantee, and the reader in `internal/query/ast.go` depends on its
indentation and node names.

What holds that down is the integration tests, which run every check through a
live `EXPLAIN AST` at the version pinned in `compose.yaml` and assert on the
answer, so a format change fails the build rather than quietly reporting
nothing. The captured fixtures do not protect against it and are not there for
that: a stale capture keeps parsing perfectly long after the server has moved
on. They exist so the reader can be tested without a container, and they are
captured rather than hand-written so that what they describe was true of a
real server once.

If the coupling ever becomes a real cost, the parser above is the fallback,
and it is also what an offline mode would be built on.

Sources: [ClickHouse discussion 60267, on a standalone SQL parser
library](https://github.com/ClickHouse/ClickHouse/discussions/60267),
[AfterShip/clickhouse-sql-parser](https://github.com/AfterShip/clickhouse-sql-parser).

This also rules out the obvious shortcut of matching keywords against the
query text. Searching for `DROP` or `INSERT` in a string flags a column named
`dropped_spans` and a literal `'INSERT failed'`, and it misses the same words
reached through a comment, a quoted identifier or different casing inside a
subquery. A check that both false-positives on ordinary rules and fails to
stop a determined author is worse than no check, because people route around
it and stop believing the rest. Ask ClickHouse what the query is, through
`EXPLAIN`, and decide from the answer.

### 7.3 Check tiers

The split is not offline versus online. It is how much each check reads.

| Tier | Reads | Cost | Default |
|---|---|---|---|
| 0 | nothing | free | on |
| 1 | metadata only | milliseconds | on |
| 2 | bounded data | seconds | on |
| 3 | backfill over history | expensive | opt in |

Tier 0, file only. Checks are namespaced like `pint`, and the name travels
with the finding so it can be silenced or grepped:

| Check | What it rejects |
|---|---|
| `rule/name` | empty alert name, or a duplicate within its group |
| `rule/source-match` | a rule whose labels match no source (warns, see 6.10) |
| `rule/expr` | empty `expr`, or one missing `{{ .From }}` or `{{ .To }}` |
| `rule/for` | negative `for`; warns when `for` is under the group interval |
| `rule/window` | negative `window`; warns when it is under the group interval |
| `labels/required` | missing or empty `team` or `severity` |
| `annotations/required` | missing `summary` or `runbook_url` |
| `annotations/runbook` | a `runbook_url` that is not an absolute http or https URL |
| `annotations/template` | an annotation that is not a parseable Go template |
| `rule/protected-label` | a query aliasing `team`, `alertname`, `source` or a source identity label, or a `labels` block setting `alertname` or `source` |

`rule/source-match` and `rule/protected-label` need the sources file as well
as the rule, so they run in the loader rather than the rule parser. They still
read no data and are tier 0 all the same.

Severities are not fixed here. 7.6 splits these into correctness, which always
error, and convention, which take a configurable severity defaulting to
`warn`.

YAML parsing is strict underneath all of them: an unknown field is an error,
not a warning, because the file is the only way to create a rule and a typo
must never produce one that silently does not alert.

`rule/expr` is the lint half of the time bound decision in 6.4. It needs no
connection, so it belongs here rather than in tier 1.

Tier 1, metadata only, reads no table data:

- `rule/syntax`, through `EXPLAIN AST`, which parses the statement and hands
  back the tree without reading a row. It also refuses a second statement
  itself, with `Syntax error (Multi-statements are not allowed)`, so nothing
  here hunts for semicolons in a string. A correctness check: a rule that
  will not parse cannot run.
- `rule/inspect` is not a finding about a rule at all. It is how the ruler
  reports that it could not ask, because silence would read as a rule that
  passed.
- table and columns exist, via `system.columns`
- annotation template variables resolve against the real output column names
  from `DESCRIBE (SELECT ...)`. Better than the `pint` equivalent, which has to
  infer labels through aggregations and sometimes gets it wrong.
- `EXPLAIN ESTIMATE` for predicted rows, parts, and marks
- `EXPLAIN PLAN indexes=1`: if granules selected is close to granules total,
  the primary key is not pruning anything
- eval interval against predicted cost. A rule on a 30s interval reading 400GB
  is arithmetic, and it is rejected
- `rule/nondeterministic`. `now()`, `today()` and `rand()` inside rule SQL
  break window alignment and make replays lie. The list is ours to curate and
  configurable to extend: `system.functions` has no `is_deterministic` column
  to read it from (6.7.1). `FINAL` and `clusterAllReplicas` are cost bombs and
  belong with the cost checks rather than here.
- `rule/table-function`. Two kinds, and only one of them has a backstop.
  `remote()`, `cluster()`, `url()`, `file()` and `s3()` read data the row
  policies never see, and the database refuses them on the SOURCES privileges
  (6.7.2), so the check here is early feedback on a mistake rather than the
  control. `numbers()`, `generateRandom()` and `zeros()` are the other kind:
  no privilege gates them at all, so this check is the only thing that
  prevents one and the database can only time it out afterwards. Allowlist
  rather than blocklist either way: a name nobody thought of should fail
  closed, and the allowlist ships empty, so an operator permits the one
  function they actually need rather than the tool guessing which are safe.

  **Found by position, never by name.** ClickHouse writes `Function numbers`
  for a table function and `Function now` for a scalar one; what separates
  them is that the first sits under a `TableExpression`. A name list would
  have to know every table function that exists, which is the blocklist this
  bullet refuses, and it would be wrong the first time one is added.
- databases and tables outside the source's own are refused. Also early
  feedback: the grants are what stop it (6.7.1), which is why this check may
  read `table:` naively without that being a tenancy question (6.9).
- `source/privileges`, the source's own user against the contract in 6.7.2.
  Not a check on the rule at all: it asserts that the guarantees the other
  checks are allowed to stop making are actually in place. Probes for the
  privileges, reads `system.settings` for the constraints, and reports each
  assertion by name so a finding says which half of the contract is missing.
- `rule/select-star`, an `Asterisk` node anywhere in the tree. The result
  columns become labels, so a schema change silently changes an alert's
  identity and every instance refingerprints.
- join count and subquery depth against a ceiling, as a proxy for cost that
  needs no data read.
- a `SETTINGS` clause on the query. The ruler's limits are sent per query and
  a query can override them in one statement, so this is rejected outright
  rather than reasoned about. The profile constraints in 6.7 are the backstop
  for anything that gets past here.

Tier 2, bounded data reads:

- attribute key presence. For OTel map columns such as
  `LogAttributes['payment_id']` the column exists even when the key does not.
  Sample a short recent window and confirm the key is actually present.

  **This is the highest value check in the tool.** Someone renames an OTel
  attribute, the alert silently stops firing forever, and nobody notices until
  the outage. That class of bug is most of the reason `pint` exists.
- single evaluation of the rule as of now, to confirm it runs

Tier 3, backfill. See 7.4.

Configuration:

```yaml
check:
  clickhouse: "clickhouse://ruler_ci@host:9000"   # presence enables tiers 1 and 2
  backfill:
    enabled: true
    window: 24h
```

Per team severity overrides by path or rule name matcher, so the central team
can hard fail while product teams get warnings during rollout. Without this,
nobody adopts.

### 7.4 The "would have fired N times" check

The `pint` `alerts/count` equivalent, and the reason the online checks are
worth the trouble.

`pint` has to issue range queries against Prometheus and rebuild eval windows
on the client. In ClickHouse the eval window is just a `GROUP BY`, so the whole
replay is one query:

```sql
SELECT
  toStartOfInterval(ts, INTERVAL {{ .EvalInterval }}) AS eval_window,
  {{ .LabelCols }},
  {{ .ValueExpr }} AS value
FROM {{ .Table }}
WHERE ts >= now() - INTERVAL {{ .BackfillWindow }}
GROUP BY eval_window, {{ .LabelCols }}
HAVING {{ .Predicate }}
```

Then collapse consecutive windows through `for` and the resolve logic.

**Report two numbers.** Eval hits and real alert instances differ wildly. 4,100
hits with `for: 5m` on one bad host is about 3 pages. Reporting only the large
number trains people to ignore the check.

Target output on a pull request:

```text
would fire: 3 alert instances (4,127 eval hits) over 24h
longest firing streak: 6h12m (never resolved)
top label sets:
  ServiceName=checkout     3,901 hits
  ServiceName=cart           198 hits
```

Top offenders matter as much as the count. "Fires 4,100 times" does not tell
anyone what to fix. "3,900 of them are one service" does.

Known limits, to be surfaced in the output rather than hidden:

- Replay in one query only works for bucketable rules. Window functions, self
  joins, and `argMax` over the eval window do not rewrite generically. Fall
  back to sequential evaluation, sampled and capped, for example 24 windows out
  of 24h rather than 2,880. Mark the result as sampled.
- TTL. If the table TTLs at 3 days, a 7 day backfill quietly under reports.
  Read TTL from `system.tables` and warn when the window exceeds it.
- Schema drift inside the window. A column added 6 hours ago makes a 24 hour
  backfill return nulls or error. Detect and say so rather than reporting zero.

### 7.5 Reuse in watch mode

Running the same backfill weekly against already deployed rules produces an
alert hygiene report for free: "these 5 rules fired 800 times and none were
acknowledged". Same code path, no extra work.

### 7.6 Check configuration

Which checks are mandatory is the operator's policy, not ours.

Hardcoding it made a rule that would run perfectly fail to load: a valid query
with a resolved source and correct time bounds, in a flat directory, produced
four errors and never evaluated. None of them was about whether the rule
works.

Rigid defaults narrow who can use the tool. "Point it at a directory and go"
has to work on the simplest possible layout, or the only consumers left are
organisations that already agree with every convention we happened to pick.

**The line is correctness versus convention.**

Correctness checks are not configurable, because a rule failing one cannot do
its job:

- `yaml/syntax`, `yaml/unknown-field`. A typo silently drops configuration, so
  the rule does not do what it says.
- `rule/name`, empty or duplicate within its group. No identity. Uniqueness
  is scoped to the group and deliberately no wider. An alert's identity is
  its full label set, not its name, so the same name in another group or
  another file is a different alert: it carries different group labels, and
  it reaches different sources, so `team` and `source` already separate the
  two in the fingerprint. Requiring globally unique names would push authors
  into `PaymentsHighErrorRate` prefixes, re-encoding in the name exactly what
  6.3.1 says belongs in labels, and two teams in a shared repository both
  wanting `HighErrorRate` is normal rather than a mistake. Prometheus makes
  the same call.
- `rule/group-name`, empty or repeated within one file. A group's identity is
  (file, name): that is what the scheduler keys a group by and what the
  `rule_group` metric label carries, so two groups sharing a name in one file
  become a single series with two goroutines reporting into it. The same name
  in a different file is fine, which is that identity working rather than a
  gap.
- `rule/expr`, empty or missing `{{ .From }}` or `{{ .To }}`. Cannot run, or
  scans unbounded on every evaluation.
- `rule/for`, `rule/window`, negative values. Nonsense.
- `rule/protected-label`. Breaks routing (6.3.1).

Convention checks carry a configurable severity of `error`, `warn` or `off`,
and where they take a list of keys that list is configurable too:

- `labels/required`, and which labels
- `annotations/required`, and which annotations
- `annotations/runbook`
- `annotations/template`, an annotation that will not parse. Default `warn`,
  because the alert still pages: the failure lands in the annotation text
  rather than stopping the notification (6.5). An operator who would rather a
  broken template never reach a pager raises it to `error`, which refuses the
  file. Parsing is as far as tier 0 reaches; whether a variable names a column
  the query returns needs the result, which is tier 1 (7.3).
- `rule/for` and `rule/window` shorter than the group interval
- `rule/source-match`, a rule whose labels match no source. Unlike everything
  else in this list it is not a matter of taste: it is here because whether a
  rule can run depends on which ruler is asking, so the same repository is
  legitimately unmatched on one ruler and fine on another (6.10, 10.2).
  Default `warn`.
- `rule/select-star` and `rule/nondeterministic`. Both produce rules that
  evaluate; they just evaluate badly, so both default to `warn`.
- `rule/table-function`, and which functions are permitted. The only check
  here defaulting to `error`, because it is the only preventive control
  rather than early feedback: `remote()` and `url()` are refused by the
  grants behind them, but `numbers()` and `generateRandom()` are gated by no
  privilege at all (6.7.1). Its list is an allowlist, which merges in the
  other direction, see 7.7.
- `source/privileges`, a source whose ClickHouse user does not meet the
  contract in 6.7.2, and which assertions are required. Default `warn`, with
  the full list required. The assertions are the check's `keys` list, the same
  field every other check with a list uses, so the merge in 7.7 needs no new
  rule for them.

**`source/privileges` is configurable for the same reason the others are, not
because the contract is optional.** An operator can drop the assertion their
cluster cannot satisfy and keep the rest, rather than turning the whole check
off to get past it. That is the shape that survives a real estate: a managed ClickHouse that
will not expose settings profiles is a reason to stop asserting constraints,
and no reason at all to stop asserting that the SOURCES privileges are
revoked. Union across scopes (7.7) still applies, so the platform baseline
cannot be dropped by a team.

It defaults to `warn` rather than `error` because a ruler pointed at an
existing cluster will fail it on day one, and a check that blocks the first
run gets turned off rather than fixed. What makes `warn` defensible here and
not merely lenient is that the finding is about the operator's own file: the
person who sees it is the person who can fix it, and nobody is waiting behind
them. An operator who wants the contract enforced raises it to `error`, and
one who is satisfied it holds by other means turns it `off`.

The severity governs the report, never the guarantee. A cluster where this
check is `off` is exactly as safe as one where it is `error` and passing;
what changes is whether anyone is told. That is the difference between this
and every other check in the list, where severity decides whether a rule runs.

**Severity is an escalation path, not a noise level.** This is the part that
decides everything else:

- `off`: nobody is asked.
- `warn`: the contributor decides. They can fix it or ship anyway, and no one
  else has to be involved.
- `error`: blocked. Either the rule changes, or a repo owner changes policy,
  which means a pull request against a file only the platform team can merge.

So the severity of a check is really a statement about who is on the critical
path when it fires. That is the argument for keeping errors rare. A check set
to `error` spends platform team attention every time it trips, and a config
where everything is an error makes the platform team a bottleneck on routine
contributions. Reserve it for the cases that genuinely warrant stopping
someone.

**Defaults are the current lists at `warn`.** Not `off`: an author should see
what good practice looks like on the first run, and an operator who wants it
mandatory changes one line. Not `error`: nothing about a missing runbook stops
a rule from evaluating correctly, and it does not deserve to pull a repo owner
into the loop. Someone who reads the warning and ignores it has made a choice,
and that is theirs to make.

This is also the escape hatch, and it is deliberately a social one rather than
a mechanism. There is no way to locally suppress a check that policy has set
to `error`. Needing one means asking a repo owner to change the policy file,
which is the CODEOWNERS workflow doing its job rather than being worked
around.

**Configuration lives in the operator's file**, `ruler.yaml`, alongside
`sources.yaml` in the CODEOWNERS lane from 6.6. This is what preserves the
argument in 7.1. Enforcement is not weakened by making policy configurable,
because rule authors still cannot reach the policy; the platform team sets it
and authors are still bound by it. What changes is that we stop guessing what
that policy should be.

```yaml
# ruler.yaml
checks:
  labels/required:
    severity: error
    keys: [team, severity, tier]
  annotations/required:
    severity: warn
  annotations/runbook:
    severity: off
  annotations/template:
    severity: error
```

Two consequences worth stating before this is built.

**Severity is runtime behaviour, not only CI output.** With hot reload, a check
at `error` means the ruler refuses the file and keeps the previous version of
it; `warn` means it loads and logs. Turning a check down does not just quiet
CI, it changes what the running ruler will accept.

**CI and the ruler must read the same `ruler.yaml`**, or a rule passes CI and
then fails to load, which is the drift 7.1 exists to prevent. That is the
reason the file belongs in the rules repository rather than in deployment
configuration.

Per-table policy is per-source policy today, because a `Source` names exactly
one table. If a source ever covers more than one, this needs revisiting rather
than being discovered by whoever tries it first.

### 7.7 Policy scopes and merging

Policy is set in more than one place, because a single
instance serves teams and datasources with genuinely different needs.

| Scope | Where | Owned by |
|---|---|---|
| instance | `ruler.yaml` at the rules root | platform |
| datasource | a `checks:` block in `sources.yaml` | platform |
| team | `ruler.yaml` in a team directory | that team |

A rule's effective policy is the **strictest** setting across every scope that
applies to it. Severity takes the maximum on `off < warn < error`. A required
key list takes the union. Nothing can lower a severity or remove a key.

**The merge is a maximum, so order does not matter.** There is no precedence
question to answer, no "which file wins", and no rule anyone has to memorise.
That is the whole reason for choosing this merge over last-one-loaded.

It also means a team-owned file is safe. A team directory is author-owned
through CODEOWNERS, so a team can write policy for its own rules, and the
worst it can do is make its own life stricter. Loosening the platform baseline
is impossible by construction rather than by convention, which is what keeps
7.1 true.

**Source and directory are independent, not nested.** A rule has both, and
neither contains the other: payments uses several sources, and `otel_traces`
is used by several teams. That is a lattice rather than a tree, and it is why
a maximum is the right merge. A tree would force one axis inside the other and
then force a winner between them.

**The shipped defaults are not a scope.** They are what a check falls back to
when no file configures it, and they take no part in the maximum. Treating
them as a scope makes `severity: off` unreachable, because the default warning
wins every merge: the one setting that means "nobody is asked" can then be
written and silently not take effect. Their key lists do still apply, so a
scope can add a required key and cannot drop one.

**A list's stricter direction depends on what the list is for.** A required
list gets stricter as it grows, so scopes union it. An allowlist gets stricter
as it shrinks, so scopes intersect it: unioning one would let a source permit
a table function the instance policy refused, and that is exactly the
loosening this section exists to prevent. Each check declares which kind its
list is.

Order independence survives, because both directions are commutative and
associative: intersection is the meet where union is the join, and the merge
is still a single pass with no precedence rule to remember.

An allowlist starts from the first scope that sets one rather than from the
shipped default, which is empty. Intersecting with an empty list would refuse
everything however the scopes were written, which is a different check from
the one anybody configured.

**New check settings must be monotonic or they do not belong here.** Severity
and both kinds of list have an unambiguous stricter direction. A setting without
one, a numeric threshold for example, breaks the order independence above and
needs a different home.

Instance and datasource scope exist. Team-level files do not: they add file
count and a trust question nobody has asked for yet, and because the merge is
variadic over scopes, adding them later changes call sites and nothing else.

### 7.8 Explaining a finding

Once policy comes from several files, "why is this an error?" has to have an
answer, or people stop trusting the tool and start ignoring it.

Every finding names three things:

- the rule file and line that triggered it
- the policy file and line that set the severity, carried on `Problem` as
  `PolicyFile` and `PolicyLine`
- the check's documentation, by stable anchor. Not built: each check needs a
  page to point at, written when the check is.

`ruler check --explain` prints the resolved policy for each rule with the
origin of every setting, so an author can see that `labels/required` is
`error` because `sources.yaml:12` raised it, not because of anything in their
own directory.

It also prints the sources a rule matched, because with 6.10 that is no
longer obvious from reading the rule: labels decide it, the match can be more
than one, and "which clusters will this actually run against" is the first
question an author asks. A rule matching nothing prints so explicitly rather
than printing an empty list.

The documentation link means each check needs a stable page or anchor to point
at, written when the check is. That is a real deliverable rather than a free
one, and it is the difference between a finding a contributor can act on alone
and one that turns into a question for the platform team, which by 7.6 is the
thing severity is supposed to be rationing.
