# clickhouse-ruler

Status: research / draft spec
Date: 2026-09-19
Binary: `ruler`

A Go service that evaluates alert rules defined as flat YAML files against a
ClickHouse cluster and sends the resulting alerts to Alertmanager. Prometheus
rule file semantics, ClickHouse SQL instead of PromQL.

This file is the index. The long sections live beside it in `spec/`, so
reading about one part of the ruler does not mean loading all of it.

## Where each section lives

Section numbers never change. They are what 146 comments across the code cite,
in the form `spec 6.7.1`, and what a goal prompt names. A section that moves to
another file keeps its number and this table is the only thing that changes.

| Section | Lives in | About |
|---|---|---|
| 1. Problem | this file | What is missing and for whom |
| 2. Research: what exists today | [spec/research.md](spec/research.md) | SigNoz, ClickStack, Grafana, sql_exporter, operators |
| 3. The gap | this file | The five properties, and which have workarounds |
| 4. Why a new tool | this file | The three arguments, weakest last |
| 5. Non-goals | this file | What this will never do |
| 6. Design | [spec/design.md](spec/design.md) | Rule files, sources, evaluation, alerting, tenancy, guard rails, the user contract, scheduling |
| 7. Validation | [spec/validation.md](spec/validation.md) | Check tiers, the line between our checks and the database's, check policy |
| 8. Observability of the ruler itself | [spec/operations.md](spec/operations.md) | Metrics, the HTTP surface, logging, finding a rule's queries in ClickHouse, dashboards |
| 9. End to end testing | [spec/operations.md](spec/operations.md) | The compose stack and what it proves |
| 10. Operational modes | [spec/operations.md](spec/operations.md) | Validation for others, deployment topologies |
| 11. Decisions made | [spec/decisions.md](spec/decisions.md) | Settled questions, with the reasoning |
| 12. Open questions | this file | What is not settled |
| 14. Documentation | this file | What the README says, what the site says, what this says |
| 13. Sources | this file | Everything cited |

Where to start, by what you are changing:

- a rule file, how an alert is evaluated, or how one reaches Alertmanager: 6
- what a rule is checked for, and what blocks: 7
- whether a question is already settled: 11, before reopening it

---

## 1. Problem

Prometheus alerts are files. They live in git, get reviewed in pull requests,
and cannot be created any other way. That property is what makes alert
standards enforceable at scale.

ClickHouse-backed stacks have most of this now. SigNoz and ClickStack store
alerts in an application database and expect the UI, but Terraform providers
put definitions in git, and the SigNoz and Grafana operators go further: a
`Rule` custom resource is a rule file, and the SigNoz operator even reverts
edits made in the UI. Section 2 describes all of it. Anyone claiming there is
no way to keep ClickHouse alerts in git is out of date.

Two things are still missing.

The first is scope. Every one of those paths arrives attached to a platform,
so keeping alerts in git means running SigNoz or Grafana. For a team whose
data is already in ClickHouse and whose routing already goes through an
Alertmanager they operate, that is a large amount of machinery for one job.

The second is that nothing looks inside the query. Every tool above stores the
ClickHouse SQL as an opaque string and hands it to the database. A rule can
lose its time bound and scan without limit on every evaluation, read around
the row policies meant to contain a team, or go silent for good because
someone renamed an OTel attribute. The file reviews perfectly in all three
cases. A rule is SQL, and an alerting tool that never reads the SQL is
checking the envelope rather than the letter.

We want the Prometheus model on top of ClickHouse, as one service rather than
a stack, and we want the query itself to be checked.

---

## 3. The gap

Alerts as code already exists. SigNoz, ClickStack and Grafana all ship a
Terraform provider, and 2.1 through 2.4 show they work. The gap is not a
missing file format.

The gap is what you have to run to get one. Every provider above is attached
to a platform, and taking the provider means taking the platform: Grafana's
means running Grafana Alerting, SigNoz's means running SigNoz. Standing up an
entire observability stack so that one scheduled SQL query can page someone is
a large amount of machinery for a small job, and it is the main reason this
exists. The data is already in ClickHouse. The routing already goes through an
Alertmanager. The only missing piece is the thing in between, and it should
not cost a platform migration.

Underneath that, no tool gives all four of these at once:

1. Rules defined as flat files in git.
2. Raw ClickHouse SQL as the query language.
3. Alertmanager as the notification path.
4. No second write path, so the file is the only way a rule can exist.

Item 4 has a workaround, and 2.6 describes it: an operator plus CODEOWNERS
plus ingress rules gets most of the way. It is unsupported and fails silently
on upgrade, but it is real, and claiming otherwise would be dishonest.

There is a fifth item with no workaround at all, and it is the one section 7
is about: **the query behind the alert is checked.** Bounded in time,
affordable, reading only what the team owns, and still returning the columns
it did last week.

Every tool in section 2 stores a ClickHouse query as an opaque string. None
inspects it. A rule can lose its time bound, scan the cluster on every
evaluation, or go silent because someone renamed an OTel attribute, and the
file it lives in will look perfect in review.

### 3.1 When this is not worth it

Worth being honest about, because it is a large part of the audience.

If you already run OSS Grafana well, already keep its configuration in git,
and your team already thinks in Prometheus rules, then the delta is small.
File provisioning marks those rules read only, the ClickHouse datasource runs
real SQL, and Grafana forwards to an external Alertmanager.

Assume such a team has also closed the creation path, because a disciplined
one will have. There is no setting for it, so what they will have done is set
the default org role to Viewer and granted folder permissions per team, which
is the only control OSS offers (2.4). That gets them all four properties, and
for them this tool adds nothing.

The difference is what the property costs. That lockdown is coarse, because
the role that stops someone creating an alert rule is the same role that
governs their dashboards: alert hygiene is bought by making ordinary dashboard
work require a permission grant. Section 4 gets the same property for free, by
having no write path to close rather than by closing one.

The case for a separate tool gets stronger the further you are from that:
when Grafana is not already in the path, when the installation is large enough
that per-team folder permissions stop being maintainable (2.4), or when adding
an observability platform is a bigger change than adding one service.

---

## 4. Why a new tool

Three arguments, ordered by how well each survives contact with 2.6.

**The query is checked.** Section 7. This is the one with no workaround
anywhere: no tool in section 2 inspects the SQL it schedules, and no amount of
GitOps around them changes that. If only one reason survives, it is this one.

**One service instead of a platform.** Section 3. The scope of what has to be
operated is the difference that holds regardless of how good the operators
get.

**Removing the UI makes enforcement free.** The weakest of the three now, and
worth stating honestly: 2.6 shows the property is approximable with an
operator, `CODEOWNERS` and ingress rules. What follows is why it is still
better to have it by construction than to assemble it.

The argument was never "no YAML format exists", because one does.

Prometheus ruler needs no RBAC because it has no write API. The rule store is a
directory. Git is the access control list. `CODEOWNERS` is the role model. Pull
request review is the approval workflow. Authorization comes from deleting the
write path, not from building a permission system.

That is the property we are copying. Everything else follows from it.

---

## 5. Non-goals

- No UI for creating or editing rules. Ever. This is the whole point.
- No notification routing, grouping, silencing, or inhibition. Alertmanager
  already does all of it, and it is your Alertmanager rather than one this
  project ships. That includes its configuration: we do not generate a route
  tree from the rules repository (6.5). The file is yours, it is already under
  whatever review and change policy you run it with, and writing into it
  collides with that policy for a tree a person can write by hand.
- No recording rules in v1. Add later only if materialized views are not
  enough.
- No replacement for SigNoz or ClickStack dashboards. This tool alerts, it does
  not visualize.
- No backwards compatibility with SigNoz or HyperDX alert JSON.

---

## 12. Open questions

1. **Metrics tables.** Answered on both halves, and still open on the one that
   decides it.

   `pint`'s `promql/rate` and `promql/counter` need to know whether a metric is a
   counter, and nothing in a PromQL expression says so, so `pint` asks a
   Prometheus server through its metadata API and the answer is as good as what
   that server currently holds: a metric nothing is scraping right now has no
   type, and two exporters disagreeing about one leave the check guessing. Here
   the same fact is a column. `AggregationTemporality` says whether `Value` is a
   total or an increment and `IsMonotonic` says whether the series only climbs,
   both on every row of `otel_metrics_sum`, so a check could read the type out of
   the table a rule already names, at tier 1, with no second service to ask. That
   makes the analog cheaper and more exact than the thing it is an analog of,
   which is the opposite of what this entry assumed when it called it loose.

   What was missing was never the check. It was a table to prove one against: no
   rule in the tree, no example in the docs and no assertion in the tests read a
   metrics table, so the mistake the check would catch had never been made
   anywhere it could be observed. That is fixed. Two integration tests read what
   the collector wrote into `otel_metrics_sum` and show a threshold on a
   cumulative counter firing long after the condition passed and going quiet at a
   restart, with the per-series delta beside it firing and then stopping, and the
   site has the idioms as SQL somebody can paste (14).

   **It stays open, pending evidence that the mistake is common.** Being able to
   reproduce a bug is not the same as knowing anybody writes it, and this project
   has one estate of rules to look at: its own. A check costs a name in the table,
   a page, a severity and a policy key forever (7.8), so the question now is a
   count rather than a design.

   When it is built it is a warning rather than an error, because the heuristic
   has legitimate exceptions. A rule asking whether a counter was ever nonzero,
   or whether a process has restarted by reading the total dropping, reads
   `Value` raw and means to. `error` on a check with real exceptions is how a
   contributor learns to reach for an exemption, and 7.6 rations that.
2. **Ownership at scale.** Deferred, not solved. Operating a ruler that
   thousands of engineers page off means high availability, missed evaluation
   handling, clock skew, ClickHouse restarts mid window, and backfill after an
   outage. Revisit before anyone depends on it in production. One part of it has
   moved: 8.8 says what an operator can see of cadence and of the delay between a
   condition and a notification, and why the delay carries no target from us.
   Seeing it is not handling it, so this item stands.
3. **Sharded clusters.** Settled, and kept here because this is where it was
   asked. Every part of it is now decided and proven on the two node stack: the
   `skip_unavailable_shards` pin, `table:` naming the Distributed table, one
   `address` rather than a list, and a predicted cost scaled to the cluster by the
   shard count out of `system.clusters` or reported unestimated where that count
   cannot be read. 6.9 has the whole of it and `decisions.md` has the arguments.
   What 6.9 still says about shards is two facts rather than two gaps:
   `max_execution_time` and `max_memory_usage` are enforced per node, so the real
   ceiling on a cluster is per shard, and `evaluation_delay` has to clear the
   slowest shard, which is a larger number rather than new configuration.

4. **Who hears that an annotation template failed.** Settled, and kept here
   because this is where it was asked. Where the error goes was decided first:
   the page goes out, the failed annotation carries a short marker, and
   `ruler_error` carries the error, which is a name the ruler owns and
   `annotations/protected` reserves (6.5). The owner is decided now too. A broken
   template is the author's defect, so it is raised on
   `clickhouse_ruler_problem`, the one surface that names the team and the file
   and clears when somebody fixes it, under `annotations/template` rather than a
   name of its own: the same check answers what a file can see, what the query's
   columns can see, and what only a running ruler can, the way `rule/cost` is one
   name for a prediction and a measurement. The finding names the annotation and
   the keys the alert did not carry, and the raw Go error rides beside it in the
   log line's `error` field, which is the only line this event writes. What this
   entry said the finding needed is half there already: the scheduler carries a
   rule's `team` and `file` into evaluation and rebuilds the gauge per check, so
   the only new decision was when it clears. That is twice decided now: a pass
   which rendered no annotations at all does not clear it, and rendering is judged
   per source, so the finding clears on the source that raised it rather than on
   any source that rendered (6.5).

5. **Whether one bad file may refuse a whole reading.** Decided, and it turned
   out to be two questions wearing one name. A check's severity says who has to
   be involved to unblock a contributor (7.6). Whether a finding stops the ruler
   reading the files at all is a separate property, and fusing the two is what
   made this entry hard: `annotations/protected` is fixed at `error` because an
   annotation a responder reads is worth a repo owner's attention, and that same
   `error` was taking a restarted ruler down over a name collision in a field
   that does not route.

   Severity stays where it is. What is new is that a finding refuses a start only
   when there is nothing to read, which is `yaml/syntax` and `ruleset/directory`
   and nothing else. Every other finding blocks a merge, loads, and raises
   `clickhouse_ruler_problem` so the ruler says out loud what got past CI. 11 has
   the decision and the reasoning; 7.6 has the check table's half of it and 8.2
   the gauge's.

   **What the upstream rulers settle, and what they do not.** Prometheus and
   vmalert both refuse a start and tolerate a reload, and both refuse the whole
   reading rather than the file at fault, so the asymmetry and the granularity in
   11 are precedent rather than invention. Neither has grown a per-file
   tolerance, in rulers old enough that the demand would have reached them, which
   is why dropping a file was never the answer here either: a rules tree is
   loaded as a tree. That also disposes of "the offending file" having no good
   meaning for `rule/duplicate-alert`, `rule/source-match` or
   `ruleset/directory`, since nothing needs the term once dropping a file is off
   the table.

   Where they stop helping is the check this entry was really about. Neither
   reserves an annotation namespace, so nothing upstream corresponds to
   `annotations/protected` and no precedent was available. The nearest thing is
   `-rule.validateTemplates`, which is template parsing rather than a reserved
   name, so it corresponds to `annotations/template`: on by default there and
   `warn` here, which makes this ruler the more permissive of the two on that
   check rather than the stricter.

   They differ on reporting rather than on refusing, and there the findings model
   here was already ahead: vmalert accumulates a failure per file and returns all
   of them, Prometheus stops at the first bad file.

6. **How a merged rule reaches a running ruler.** Decided and built. A rule's
   life is pull request, approval, deployment, running, alert sent, and the
   deployment step was the one nothing specified: how a merged rule file gets
   onto the disk the ruler reads, what sends the `SIGHUP`, and how long a fix
   takes to reach a page. 10.2 answers all three. A sidecar delivers the files
   and triggers the reload, the ruler watches nothing, and the lag is the
   sidecar's sync period plus one reload.

   The artifacts are there: a Helm chart, so the answer to "how do I run this" is
   a command rather than a paragraph, and a values file per delivery beside it,
   the git-sync sidecar 10.2 chose as the default and a ConfigMap mount for a
   small estate. The chart also proves the decision, because a symlinked rules
   root and a ConfigMap mount are both layouts the loader has to read exactly
   once.

   What the chart does not carry is a `PrometheusRule` or the dashboards, and
   10.2 says why each is left out rather than pending. The chain itself is run
   rather than rendered. CI templates the chart and validates the manifests,
   which cannot show that a commit arrives, so a `kind` cluster carries one: a
   read only `git daemon` serves the rules repository, the test commits inside
   that pod, and the sha it produces is waited on through the symlink, the
   merged rule's group and the reload gauge. The refusal path is asserted
   beside it, because a reload the ruler refused has to be a failed hook with
   the previous configuration still running. 10.2 has the fixture and each
   link.

   One smaller thing on the same chain belonged to 7 below rather than here, and
   is done. There was no CI example that gated a merge: `docs/running.md` showed
   `ruler check --online --summary` and the cost table as a pull request comment,
   and nothing showed `ruler check` as a required status or what `--format=github`
   renders as in the diff. The site has a page for it now, and 10.3 says why that
   page rather than `action/README.md` is where the checker is documented.

7. **The pull request checker.** Built. 7.1 chose a composite action in
   `action/` consumed as `dennisme/clickhouse-ruler/action@v1`, and 10.3 settled
   its wiring down to checksum verification, tag discipline, permissions and the
   JSON feed behind the summary comment. All of it ships.

   The binary half is four flags: `--format=github` emits the workflow commands
   GitHub renders on a diff, `--markdown` writes the comment's table beside
   them in the same run, `--format=json` is the feed for anyone integrating the
   checks elsewhere, and `--changed-since` is the changed-file expansion 10.3
   puts in the binary rather than in the action. Each is reachable by hand,
   which is the property 10.1 asks for: the action can only wire together flags
   a laptop already has.

   `action/action.yml` is the wiring. It verifies the release checksums file
   before executing anything, runs the checks, and updates one summary comment
   in place rather than appending one per push. It owns five inputs the binary
   does not have, and each is wiring rather than behaviour: which release to
   run, a prebuilt binary to run instead, the rules path, whether to comment,
   and the token. A test reads the inputs back and fails on any other one that
   is not a `ruler check` flag, so the property survives the next edit.

   What exercises it here is `action/testdata`, a rules directory that fails on
   purpose, and the `action` workflow. Three jobs, because there are three ways
   this breaks: the checker in the checkout, so a change to `internal/lint` is
   what the action runs; the summary comment, which needs a token and an API;
   and a checksum-verified download of a published release, which is the only
   thing the archive names exist for. Each asserts the run failed on rules that
   fail, because an action that silently passes is the failure worth catching.

   Tag discipline is code rather than intention now: the release workflow moves
   the floating major tag once the binaries and the chart are published, never
   for a prerelease, and the action resolves such a tag to the newest release
   under that major. A branch is refused, because a checker whose version
   nobody can state is not one to gate a merge on.

   Documented now as well, which it was not when this entry first said it all
   ships. `dennisme/clickhouse-ruler/action@v1` appeared in `action/README.md`
   and in no other file in the tree: not the README, not the site, not the nav.
   The one argument in 4 with no workaround anywhere was the one thing a reader
   could not find, and a checker nobody can find gates nothing. 10.3 has the
   decision about which file is its home and what that leaves in `action/`.

---

## 14. Documentation

Three places describe this project and they keep describing the same things,
which is how a README reaches six hundred lines and how a default ends up
stated in two places with one of them wrong.

What each is for:

- **The README is a front door.** What this is, why it exists, a quickstart
  somebody can paste, an honest status section, and links. Somebody deciding
  whether to keep reading is the only reader it has. Target is under two
  hundred lines, and every section that grows past a screen is a section that
  belongs on the site.
- **The site is the manual.** Reference material, in pages that are navigated
  rather than scrolled: how evaluation works, what is exposed, the check
  pages (7.8), the operations page (8.7), the deployment page (10.2), and how
  this compares to the alternatives. It is built `--strict`, so a link to a
  page that does not exist fails the build.
- **This spec is the reasoning.** Why each of those is the way it is. It is
  written for whoever changes the code, and it is the only one of the three
  where an argument belongs.

**The status section stays in the README**, alone of the reference material.
It is the honesty section, it is what somebody checks before depending on
this, and a status published one navigation click away from the front door is
a status people find after they have already started. One home, and the
convention in `AGENTS.md` that it is kept current when something lands.

**A fact lives in one of the three, and the other two link to it.** The metric
table is currently in 8.2, in the README, and partly on the operations page,
which is two copies waiting to disagree. The rule is the site for what the
tool does, the spec for why, and the README for neither. Where the site needs
a fact the code decides, it is generated (7.8), not transcribed.

The cost of this split is links. Moving a section off the README breaks every
deep link into it, including the ones on the site, and `--strict` does not
check links to `github.com` because it cannot. Whoever does the move walks
both directions.

Done. The README is a front door again, and `how-it-works`, `running` and
`comparison` are pages on the site.

There is a fourth place, and it is not an exception to the rule above. `action/`
carries a README of its own because GitHub renders it for whoever follows a
`uses:` line into the directory, and a reader who arrives there is owed
orientation rather than a redirect to a search box. It is a front door for one
directory, the same job the README does for the repository, and 10.3 says what
that allows it to say and what belongs on the site instead.

**A diagram is a mermaid fence, and it has to earn its place.** Mermaid
because GitHub renders the fence natively and the theme renders the same one,
so a diagram is text in the file it explains rather than an image somebody has
to rebuild and commit, and it keeps working where the check pages are read
(7.8). Three earn it: which clusters a rule's selector matches and what each
alert is then labelled, where one evaluation's window and `evaluation_delay`
sit against the clock, and the chain from a merge to a reload including the
branch where the ruler refuses. Each is a fan-out, a timeline or a sequence,
which is a shape prose has to walk the reader through one edge at a time. The
alert state machine is the one deliberately left undrawn: it is Inactive,
Pending, Firing copied from Prometheus exactly (6.3), so a diagram of it would
restate what its reader already has.

**The flag reference is generated from the flag set, for the reason the check
pages are generated from the check table.** `running.md` opened by promising
every flag the binary takes and then explained each one wherever its behaviour
came up, across two pages, so the one question an operator asks first, what can
I set, had no answer anywhere. A hand-written table would answer it and go
stale the first time a default moved, which is the failure 7.8 already names:
a page can be perfectly consistent with itself and describe a tool that behaves
differently, and the gate that regenerates and diffs would certify it.

So the flag set is the table of record. `flag.FlagSet` already holds every
flag's name, default and usage string, and the usage strings are what `--help`
prints, so generating from them means the page and the terminal cannot disagree
either. What this costs is where the flags are registered: a generator cannot
import `package main`, so registration moves to `internal/cli`, which the
commands and the generator both read. The command bodies stay where they are.

The split is 7.8's split. The generated region carries the facts, which are the
name, the default and the usage line. Everything about why a flag exists and
what happens at the edges of it stays hand written outside the markers, and the
table links each flag to the section that explains it where there is one.

## 13. Sources

- [SigNoz: managing alerts via the API](https://signoz.io/docs/userguide/alerts-management/#managing-alerts-via-the-api)
- [SigNoz Terraform provider](https://signoz.io/docs/alerts-management/terraform-provider-signoz/)
- [SigNoz roles and IAM, prerequisites](https://signoz.io/docs/manage/administrator-guide/iam/roles/#prerequisites)
- [SigNoz Alertmanager configuration](https://signoz.io/docs/manage/administrator-guide/configuration/alertmanager/)
- [SigNoz/alertmanager fork](https://github.com/SigNoz/alertmanager)
- [Grafana infrastructure as code](https://grafana.com/docs/grafana/latest/as-code/infrastructure-as-code/)
- [Grafana Git Sync](https://grafana.com/docs/grafana/latest/as-code/observability-as-code/git-sync/)
- [Git Sync usage and performance limitations](https://grafana.com/docs/grafana/latest/as-code/observability-as-code/git-sync/usage-limits/)
- [Git Sync: shard by capacity, not by team](https://grafana.com/docs/grafana/latest/as-code/observability-as-code/git-sync/usage-limits/#shard-by-capacity-not-by-team)
- [Git Sync known limitations](https://grafana.com/docs/learning-paths/git-sync-use/known-limitations/)
- [Git Sync: optional enforcement mode (grafana#129913)](https://github.com/grafana/grafana/issues/129913)
- [SigNoz Operator](https://github.com/SigNoz/signoz-operator)
- [grafana-operator alerting support proposal](https://grafana.github.io/grafana-operator/docs/planning/proposals/002-alerting-support/)
- [ClickHouse: permissions for queries](https://clickhouse.com/docs/en/operations/settings/permissions-for-queries)
- [ClickHouse: constraints on settings](https://clickhouse.com/docs/en/operations/settings/constraints-on-settings)
- [Grafana: provision alerting resources](https://grafana.com/docs/grafana/latest/alerting/set-up/provision-alerting-resources/)
- [Add a setting to allow UI changes to provisioned alerts (grafana#57315)](https://github.com/grafana/grafana/issues/57315)
- [Alerts with ClickStack](https://clickhouse.com/docs/use-cases/observability/clickstack/alerts)
- [ClickStack API reference](https://clickhouse.com/docs/clickstack/api-reference)
- [Alerting arrives in ClickStack for ClickHouse Cloud](https://clickhouse.com/blog/alerting-arrives-in-clickstack-for-clickhouse-cloud)
- [ClickHouse Terraform provider](https://registry.terraform.io/providers/ClickHouse/clickhouse/latest/docs)
- [teamlapse/terraform-provider-clickstack](https://github.com/teamlapse/terraform-provider-clickstack)
- [justtrackio/provider-clickhouse](https://github.com/justtrackio/provider-clickhouse)
- [Grafana file provisioning for alerting](https://grafana.com/docs/grafana/latest/alerting/set-up/provision-alerting-resources/file-provisioning/)
- [Allow the editing of file provisioned alerts in the UI (grafana#92454)](https://github.com/grafana/grafana/issues/92454)
- [Grafana RBAC fixed and basic role definitions](https://grafana.com/docs/grafana/latest/administration/roles-and-permissions/access-control/rbac-fixed-basic-role-definitions/)
- [Manage alerting access using folders or data sources](https://grafana.com/docs/grafana/latest/alerting/set-up/configure-rbac/access-folders/)
- [grafana-operator alert rule group CRD](https://grafana.github.io/grafana-operator/docs/examples/alertrulegroup/full-notification-configuration/)
- [Cloudflare pint](https://github.com/cloudflare/pint)
- [burningalchemist/sql_exporter](https://github.com/burningalchemist/sql_exporter)
- [ClickHouse discussion 60267, on exposing the SQL parser as a library](https://github.com/ClickHouse/ClickHouse/discussions/60267)
- [AfterShip/clickhouse-sql-parser](https://github.com/AfterShip/clickhouse-sql-parser)
- [ClickHouse EXPLAIN reference](https://clickhouse.com/docs/sql-reference/statements/explain)
- [Prometheus recording rules, on validating rule files and applying changes only if all rule files are well-formatted](https://prometheus.io/docs/prometheus/latest/configuration/recording_rules/)
- [Prometheus rule manager, restoring the previous rule set when a load fails](https://github.com/prometheus/prometheus/blob/main/rules/manager.go)
- [Prometheus startup and reload path, including `--config.auto-reload` and the config checksum](https://github.com/prometheus/prometheus/blob/main/cmd/prometheus/main.go)
- [vmalert](https://docs.victoriametrics.com/vmalert/)
- [vmalert startup and reload path, including `-rule.validateTemplates` and `-configCheckInterval`](https://github.com/VictoriaMetrics/VictoriaMetrics/blob/master/app/vmalert/main.go)
- [vmalert rule parsing, accumulating a failure per file](https://github.com/VictoriaMetrics/VictoriaMetrics/blob/master/app/vmalert/config/config.go)
