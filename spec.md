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
  project ships. Generating a route tree from the rules repository (6.5) is
  writing Alertmanager's configuration, not doing its job.
- No recording rules in v1. Add later only if materialized views are not
  enough.
- No replacement for SigNoz or ClickStack dashboards. This tool alerts, it does
  not visualize.
- No backwards compatibility with SigNoz or HyperDX alert JSON.

---

## 12. Open questions

1. **Metrics tables.** The `pint` `promql/rate` and `promql/counter` checks have
   a loose analog for counter columns in OTel metrics tables. Worth it, or skip?
2. **Restart loses pending state.** `ActiveAt` is held in memory only, so a
   ruler restart delays every pending alert by its full `for`. Prometheus
   solves this by restoring from an `ALERTS_FOR_STATE` series. Deferred.
3. **Ownership at scale.** Deferred, not solved. Operating a ruler that
   thousands of engineers page off means high availability, missed evaluation
   handling, clock skew, ClickHouse restarts mid window, and backfill after an
   outage. Revisit before anyone depends on it in production.
4. **Sharded clusters.** `skip_unavailable_shards` is now pinned to `0`, so a
   dead shard fails the evaluation rather than silently resolving alerts. The
   rest of 6.9 is outstanding: `address` takes a single node, the cost caps
   are per shard rather than per query, and `evaluation_delay` has to cover
   the slowest shard. Proving any of it needs a second ClickHouse node in the
   compose stack, since one node cannot reproduce the failure. What `table:`
   means on a sharded cluster is no longer a blocker for tier 1: under 6.7.1
   it carries no security meaning, so a wrong answer is a wrong finding rather
   than a tenancy hole. It is still a correctness question for the checks that
   read it.
5. **Generating the route tree with free-form labels.** 6.5 says the tree is
   generated from the repository, keyed on `team`. With teams inventing rule
   labels and operators inventing source `labels` (6.10.1), what the generator
   should do with a combination no route covers is unsettled: emit a catch-all
   branch, refuse to generate, or report it and continue. Not urgent, because
   nothing generates a route tree yet.

6. **Matched sources have to be schema-compatible.** A rule writes
   `FROM otel.otel_traces` in its SQL, so every source its selector matches
   must expose that table with those columns. Nothing checks it, and getting
   it wrong surfaces as a tier 1 failure against one source and not another,
   which is a confusing way to find out. A check comparing matched sources'
   tables and column types is the obvious fix and needs tier 1 first. See
   6.10.
7. **Query concurrency is bounded globally, not per source.** A slow cluster
   holds slots that rules against every other cluster then queue behind, so an
   outage on one source delays evaluation of sources that are perfectly
   healthy. A per-source limit is the shape this probably wants, and sizing it
   needs per-source capacity that nothing collects yet. See 6.11 for the
   current behaviour and why it was left here.

8. **Nothing checks that two rules cannot produce the same alert.** 7.6
    scopes `rule/name` uniqueness to the group, on the reasoning that group
    labels and `source` already separate two same-named rules in the
    fingerprint. That reasoning is an assumption about how the files happen
    to be written, not something enforced. Two rules with the same `alert`
    name, the same `sources` selector and no distinguishing group or rule
    labels produce the same final label set, and are therefore the same
    alert to Alertmanager and to `notify.Cadence`, which keys `lastSent` on
    the fingerprint alone. Each rule then overwrites the other's cadence and
    whichever evaluated last decides what Alertmanager holds.

    This is a tier 0 check: alert name, static labels and selector are all
    in the files. It needs the sources file as well as the rule, so it
    belongs in the loader next to `rule/source-match` rather than in the
    rule parser. It cannot be exact, because result columns contribute
    labels that are only known at evaluation time, so it can only flag rules
    whose *static* identity already collides. That is the reachable case and
    it is worth flagging.

---

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
