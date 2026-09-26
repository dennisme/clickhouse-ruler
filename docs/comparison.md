# How it compares

Five ways to alert on ClickHouse, scored on the properties this project is
built around.

|                                     | Rules in git   | ClickHouse SQL | Your Alertmanager | File is the only write path | Validates the query |
| ----------------------------------- | -------------- | -------------- | ----------------- | --------------------------- | ------------------- |
| SigNoz + Operator                   | yes            | yes            | no, runs its own  | with external controls      | **no**              |
| ClickStack / HyperDX                | Terraform only | yes            | no                | no                          | **no**              |
| Grafana OSS + ClickHouse datasource | yes            | yes            | yes               | with external controls      | **no**              |
| sql_exporter + Prometheus           | yes            | metrics only   | yes               | yes                         | **no**              |
| clickhouse-ruler                    | yes            | yes            | yes               | yes                         | **yes**             |

"With external controls" is doing real work in that table. Both operators can
be pushed most of the way there: alert custom resources in a Helm repo laid
out per team, `CODEOWNERS` on those paths, and the UI's rule-creation routes
blocked at the Kubernetes ingress so only the operator's service account gets
through. Or, less strictly, diff what the platform holds against the manifests
and report anything with no file behind it. If you already run Kubernetes and
one of those platforms, that is probably cheaper than adopting this.

The catch is that none of it is a supported feature. Ingress rules match paths
an application may change on upgrade, and the failure mode is silent: alert
creation quietly starts working again. A diff reports sprawl rather than
preventing it.

The last column is the one nobody offers at any price, and it is the part that
does not have a workaround.

Be clear about what that column covers here. Offline, the checks read the
rule file: the time bound is enforced by requiring `{{ .From }}` and
`{{ .To }}` in the text, which catches the common mistake. `--online` asks
ClickHouse what the query actually is, reading no rows: `EXPLAIN AST` for the
tree, `DESCRIBE` for the columns the result will really have, and
`EXPLAIN ESTIMATE` for what one evaluation is predicted to read.
`--sample` goes one step further and reads rows, which is how a map key
nothing writes is caught.

What is not covered is replay: how often a rule would have fired over the
last week, which is
[spec 7.4](https://github.com/dennisme/clickhouse-ruler/blob/main/spec/validation.md)
and is not built.

## SigNoz and ClickStack

Both have alerting and both store alerts in their own application database.

SigNoz does use Alertmanager, and gives more of it as code than we first
credited. It maintains a fork, bundled into the SigNoz binary since v0.76.0,
and the operator exposes `RoutePolicy` and `PlannedMaintenance` custom
resources, so routing and maintenance windows are manifests rather than UI
state. That is the same idea as generating a route tree from the rules repo,
and they ship it today.

The gap is whose Alertmanager. It is theirs, embedded, with no documented way
to point it at a standalone instance. If you already run one, with your
routing, your silences and your on-call integrations wired into it, SigNoz
means a second one: two places to silence an incident and two route trees to
keep agreeing. That is what the table's Alertmanager column means, and it is
narrower than "does not have Alertmanager".

The SigNoz Operator is the closest thing to this project that exists. It
manages a `Rule` custom resource, so alert rules do live in git, and Argo CD
or Flux drive them like anything else. It answers drift too, by reconciling
rather than by preventing: "The operator re-checks each resource on an
interval and reverts changes made outside Kubernetes, such as edits in the
SigNoz UI." If you are already on Kubernetes and already running SigNoz, look
at it before you look at this.

What it does not obviously close is sprawl. Reverting changes to resources the
operator manages is not the same as stopping a rule existing that no file ever
created, and its documentation does not say it removes those. It is also
`v1alpha1`, Kubernetes only, and AGPL-3.0.

Both also have Terraform providers, which are weaker on ownership: the
provider calls the same API and writes the same mutable rows, so anyone with a
token can edit the rule out from under your file and no diff records it. The
ClickStack providers are ClickHouse Cloud only on top of that.

Locking the API down does not rescue it either. SigNoz's fine-grained access
control requires "an active SigNoz license" and is Cloud and Self-Hosted
Enterprise only, currently in beta. Same bind as Grafana OSS: the escape hatch
is real, and it is not in the free edition.

The larger cost is that adopting either one for alerting means adopting the
whole platform. Running SigNoz because it is the only thing that will evaluate
a scheduled SQL query and page someone is a lot of machinery for one job, when
your data is already in ClickHouse and your routing already goes through an
Alertmanager you run.

## Grafana OSS plus the ClickHouse datasource

The closest thing that already exists, and worth using if it fits you.

Alert rules can be provisioned from files, the query is real ClickHouse SQL,
and Grafana can forward to an external Alertmanager. File-provisioned rules
are marked read only, so rules you ship from git cannot be edited in the UI or
the API. Drift is solved.

Sprawl is not. Grafana OSS has no custom RBAC, and there is no setting for
"alert rules may only be created by provisioning". Anyone with Editor on a
folder can create a rule in it. In an install with thousands of users, one
Editor grant handed out for a dashboard also grants alert creation, and you
end up with alerts that page on-call which no pull request ever saw. Closing
the hole means default-Viewer plus per-team folder permissions, which breaks
ordinary dashboard work.

Git Sync, the newer Observability as Code work, does not close it either. It
is the closest thing Grafana has to the Prometheus model, and it is in OSS
rather than behind a licence, but it "only supports dashboards and folders".
Alerts are not supported yet, and the migration guide tells you to move alert
rules out of a folder before syncing it. There is an open request
(grafana/grafana#129913) for a mode where the repository is the only write
path, exactly the property this project is built on, but it is unanswered and
scoped to dashboards and folders.

Its sharding guidance points away from the ownership model too, which would
still matter if alert support shipped tomorrow. Git Sync recommends about
1,000 resources per repository connection and allows 10 connections per stack,
a hard limit on Cloud. That cap is a sync cost rather than a query cost: past
it, "the sync workflow puts noticeable load on Grafana itself". The guidance
is titled "Shard by capacity, not by team" and says to avoid one connection
per team because "it consumes connections quickly, doesn't scale as teams
grow". So repo layout follows capacity, not who owns what. That is the
opposite of the model here, where a directory is an ownership boundary for
review and a source is selected by label. Nothing is synced into a database,
so there is no connection to run out of and no cap on team directories.

If managing rules this way is the plan, the wider tooling is uneven: the
Terraform provider is the mature path, the Ansible collection is Cloud only,
the Operator ships `AlertRuleGroup`, `ContactPoint` and `NotificationPolicy`
CRDs, and the Crossplane
provider "is in an alpha stage, so it has not reached a stable state yet".

## sql_exporter plus Prometheus

Runs SQL on a schedule, turns the result into Prometheus metrics, then normal
Prometheus rules and Alertmanager apply. No new code, and a good answer for
simple cases.

The cost is that every alert needs a metric. High cardinality log and trace
queries blow up metric cardinality, and you lose alerting on the rows
themselves, so per-instance alerts get awkward.

## When you should not use this

If you already run OSS Grafana well, already keep its config in git, and your
team already thinks in Prometheus rules, the delta here is small. File
provisioning already makes those rules read only, the ClickHouse datasource
already runs real SQL, and Grafana already forwards to an external
Alertmanager.

A shop that disciplined has probably gone further and closed the creation path
too. Grafana OSS has no "alert rules may only be created by provisioning"
setting, so the only way to get there is default-Viewer plus per-team folder
permissions. If you have done that, you already have all four properties and
there is little here for you. Use what you have.

What this offers such a team is not the property, it is the price. That
lockdown is coarse: the role that stops someone creating an alert rule is the
same role that governs their dashboards, so you buy alert hygiene by making
ordinary dashboard work need a permission grant. Here, the file being the only
write path costs nothing, because there is no other write path to close and no
UI whose permissions you are borrowing.

The case gets stronger the further you are from that: when Grafana is not in
the path at all, when the install is large enough that per-team folder
permissions stop being maintainable, or when adding an observability platform
is a bigger change than adding one service.
