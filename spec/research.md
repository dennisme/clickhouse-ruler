# What exists today

Every other way to keep ClickHouse alerts in git, and what each one costs.
Background for sections 3 and 4 rather than anything to implement.

Part of the [clickhouse-ruler spec](../spec.md). Section numbers are stable
and are what the code comments cite.

---

## 2. Research: what exists today

### 2.1 SigNoz

Alerts are created in the UI or through the REST API, `GET /api/v1/rules` and
`POST /api/v1/rules`, and stored in the SigNoz application database.

There is an official Terraform provider, and the alerts documentation points
at it as the infrastructure as code answer. It is a real one, so HCL in git is
genuinely possible. What it does not change is ownership: the provider calls
the same API and writes the same mutable rows, so the file describes a rule
without owning it, and anything with a token can still edit the rule out from
under the file. That is the same gap as 2.3.

**The SigNoz Operator is the closest thing anyone has built to this project**,
and it deserves a straight description rather than a dismissal. It manages a
`Rule` custom resource, described as "An alert rule", alongside Dashboard,
SavedView, RoutePolicy and others. Because those are ordinary custom
resources, "tools such as Argo CD and Flux handle them without plugins or
custom sync logic". So SigNoz does have a rule file format, and rules can live
in git.

It also answers drift, by a different route than section 4 takes: "The
operator re-checks each resource on an interval and reverts changes made
outside Kubernetes, such as edits in the SigNoz UI." Reconciliation undoes a
UI edit rather than preventing it. That is a real answer, and it is more than
the Terraform provider offers.

What it does not obviously close is sprawl. Drift correction reverts changes
to resources the operator manages; an alert someone creates in the UI that has
no custom resource is not a managed resource, and whether the operator removes
it is not stated in its documentation. That is the distinction in section 3,
item 4: not "can a file own a rule", which the operator answers, but "can a
rule exist that no file created".

Three further caveats, none of them disqualifying:

- The API group is `resources.signoz.io/v1alpha1`. Alpha.
- It is Kubernetes only, and it still requires running SigNoz. The adoption
  cost in section 3 is unchanged.
- It is AGPL-3.0, which some organisations weigh differently from Apache-2.0.

SigNoz does run Alertmanager, and gives more of it as code than a quick look
suggests. It maintains a fork, bundled into the SigNoz binary since v0.76.0,
and the operator exposes `RoutePolicy` ("A notification route policy") and
`PlannedMaintenance` ("A downtime schedule") custom resources. Routing and
maintenance windows are therefore manifests rather than UI state, which is the
same idea as 6.5 generating a route tree from the rules repository, shipped
already.

The gap is ownership, not absence. It is their Alertmanager, embedded, and
the documented configuration covers its own external URL and SMTP rather than
pointing it at a standalone instance. For a team that already runs one, that
means a second: two places to silence an incident, two route trees to keep in
agreement, and on-call integrations wired into whichever one the alert
happened to come from.

It also does not accept Prometheus rule YAML for ClickHouse-backed queries.

Fine-grained access control does not close the gap either, because it is not
in the open source edition. The roles documentation lists its prerequisite as
"an active SigNoz license" and marks the feature as SigNoz Cloud and
Self-Hosted Enterprise only, currently in beta. So the same bind as Grafana
OSS in 2.4: the escape hatch exists, behind a paywall.

### 2.2 ClickStack / HyperDX

ClickStack does have alerting. Same shape as SigNoz.

Two alert types:

- Search alerts. A saved search plus a threshold.
- Chart alerts. A dashboard tile's SQL aggregation plus a threshold.

Thresholds support `>=`, `>`, `<=`, `<`, `=`, `!=`, between, and outside.

Notification targets are Slack webhook, generic webhook, Slack bot token, and
PagerDuty. Slack bot token and PagerDuty are ClickHouse Cloud only. There is no
Alertmanager support.

Alerts are stored in the HyperDX application database. Created in the UI.

### 2.3 Closest thing to alerts as code today

REST APIs exist for both editions:

- Self-hosted OSS: `POST http://<hyperdx>:8000/api/v2/alerts`, Bearer token from
  a personal API access key. Full CRUD.
- ClickHouse Cloud: `https://api.clickhouse.cloud/v1/organizations/<ORG>/services/<SVC>/clickstack/alerts`,
  HTTP basic auth with Cloud API key credentials.

Infrastructure as code wrappers, all Cloud only:

- Official `ClickHouse/clickhouse` Terraform provider, `clickhouse_clickstack_*`
  resources covering alerts, dashboards, saved searches, sources, webhooks.
- `teamlapse/terraform-provider-clickstack`, community, v0.1, roughly 13
  commits, not production ready.
- `justtrackio/provider-clickhouse`, a Crossplane provider generated with Upjet
  from the official Terraform provider. Gives Kubernetes YAML and continuous
  reconcile.

All of these write to the same mutable application database. The file is a
client of the API, not the source of truth.

The other cost is adoption. Terraform providers exist for SigNoz, ClickStack
and Grafana, so "alerts as code" is available in all three, but only by taking
their whole stack with it. Using Grafana's provider means using Grafana
Alerting; using SigNoz's means running SigNoz. Running an entire observability
platform because it is the only thing that will evaluate a scheduled SQL query
and page someone is a large amount of machinery for one job, especially for a
team whose data already lives in ClickHouse and whose routing already goes
through an Alertmanager they run.

### 2.4 Grafana plus ClickHouse datasource

The closest existing thing to what we want.

Grafana unified alerting supports file-provisioned alert rules
(`apiVersion: 1`, `groups:`, `rules:`). The official
`grafana-clickhouse-datasource` plugin lets a rule run raw ClickHouse SQL.
Grafana can forward firing alerts to an external Alertmanager.
`grafana-operator` covers alerting through `AlertRuleGroup`, `ContactPoint`
and `NotificationPolicy` CRDs; its proposal records them as "status:
Implemented". Unlike the SigNoz operator in 2.1, that proposal says nothing
about drift detection or reverting UI edits, so it provisions rather than
reconciles.

The wider infrastructure as code story is uneven, which matters if the plan is
to manage rules this way. The Terraform provider covers all major resources
and is the mature path. The Ansible collection is Grafana Cloud only. The
Crossplane provider covers all major resources but the
documentation states it "is in an alpha stage, so it has not reached a stable
state yet".

What it gets right: file-provisioned rules are stamped `provenance: file` and
become read only in both the UI and the API. Rules shipped from git cannot
drift.

Why it does not solve the problem at scale:

- Grafana OSS has no custom RBAC. Scoped roles and fine grained alerting
  permissions are Enterprise and Cloud only.
- Access to alert rules in OSS is controlled by folder permissions plus
  datasource query permissions.
- There is no setting for "alert rules may only be created by provisioning".
  Any user with Editor on a folder can create a rule there.

So Grafana OSS prevents drift but not sprawl. In an installation with thousands
of users, one Editor grant handed out for a dashboard also grants alert
creation in that folder. The result is shadow alerts that page on call and were
never reviewed. Closing that hole means setting the default org role to Viewer
and managing folder permissions for every team, which breaks normal dashboard
work.

### 2.4.1 Git Sync

Grafana's newer Observability as Code work adds Git Sync, which syncs a
Grafana instance against a Git repository rather than pushing through an API.
That is much closer to the Prometheus model than Terraform is, so it is worth
being precise about why it does not close this gap.

It does not reach alerts. The usage limits page states "Git Sync only supports
dashboards and folders", and that alerts, data sources, panels and other
resources are not supported yet. The migration guidance is blunter still: move
alert rules and other unsupported resources out of a folder before syncing it,
because Git Sync recreates dashboards and folders but does not recreate
alerts.

This is a scope limit, not a licensing one, which makes it different from the
RBAC story in 2.4 and 2.1. Git Sync is available in OSS, Cloud and Enterprise.
Full-instance sync is marked experimental. So there is no paywall to complain
about here; the feature simply does not cover the resource we care about.

A second limit is structural rather than a missing feature, and it is the one
that would still matter if alert support shipped tomorrow.

Git Sync's usage limits recommend roughly 1,000 resources per repository
connection, and allow 10 connections per stack: the default on-prem, and a
hard limit on Grafana Cloud. The 1,000 is a sync cost, not a query cost.
Beyond it "the sync workflow puts noticeable load on Grafana itself, which may
result in slower syncs and increased database load". Nothing here is about the
datasource.

The guidance that follows is titled "Shard by capacity, not by team", and it
says plainly: "When you have many teams or tenants, it's tempting to create
one connection per team so each team maps to its own connection. Avoid this:
it consumes connections quickly, doesn't scale as teams grow, and on Grafana
Cloud a single stack can't be granted the hundreds of connections this would
require."

That is the opposite of the model here. The directory *is* the ownership
boundary: `rules/payments/` maps to `@payments` in `CODEOWNERS` and derives
the `team` label in 6.3.1 from the same path. Git Sync asks for repo
layout to follow capacity shards instead, so the structure stops encoding who
owns what, and per-team review gates get harder rather than easier. With ten
connections, one per team runs out at ten teams and the documentation tells
you not to try.

There is no equivalent limit here, because there is nothing to sync. The ruler
reads files from disk; it does not import them into a database that then has
to be kept consistent. There is no connection object to run out of, and the
number of team directories is bounded by nothing.

There is an open request, grafana/grafana#129913, for an enforcement mode that
would make the repository the only write path and reject writes that do not
come through provisioning. That is exactly the property section 4 is built on.
It is unanswered, and it is scoped to dashboards and folders, so even if it
ships it would not apply to alert rules.

The conclusion is unchanged: alert sprawl outside of code is still ungoverned
in Grafana OSS. Git Sync moves dashboards to the model we want and leaves
alerts where they were, and its sharding guidance points away from the
per-team ownership boundary that makes the model work in the first place.

### 2.5 sql_exporter plus Prometheus

`burningalchemist/sql_exporter` runs SQL against ClickHouse on a schedule and
exposes the results as Prometheus metrics. Normal Prometheus rules and normal
Alertmanager then apply. No new code.

Limits: every alert needs a scrape-interval metric. High cardinality log
queries blow up the metric cardinality. You lose "alert on the raw rows"
semantics, so multi-instance alerts are awkward.

### 2.6 Operators plus external controls

Both operators can be pushed most of the way to section 4's property without
this project, and the pattern deserves writing down rather than ignoring.

A team could run the SigNoz or Grafana operator, keep alert custom resources
in a Helm repository laid out per team or per service, gate those paths with
`CODEOWNERS`, and then close the UI write path from outside the application:
block the API routes the UI uses to create alert rules at the Kubernetes
ingress, allowing only the operator's own service account through. Failing
that, run a detective control instead: list what the platform actually holds,
diff it against the custom resources, and report anything with no manifest
behind it. The SigNoz operator's reconcile loop already reverts edits to
resources it manages, so that diff is the remaining gap.

This works. It is not a straw man, and for a team already invested in
Kubernetes and one of those platforms it is very likely the cheaper answer.

What it costs is that none of it is a supported feature:

- Ingress rules match on paths an application is free to change between
  releases. The control breaks on upgrade, silently, and the failure mode is
  that alert creation quietly works again.
- Blocking at the ingress blocks everyone, so the operator has to be excepted,
  and the exception is then the thing to get wrong.
- Anyone with direct access to the service, inside the cluster or through a
  port-forward, is past it.
- A detective control reports sprawl rather than preventing it. That is worth
  a great deal more than nothing, and it is not the same property.

Section 4's claim should be read accordingly. The property is not unobtainable
elsewhere; it is unobtainable elsewhere *from the tool itself*, and everything
above is assembled around a tool that would rather you did not need it. Here
it is the default, because there is no second write path to close.

That distinction matters less than it used to, which is why 3 leads with
adoption cost. What none of these approaches address at all is section 7:
whether the ClickHouse query behind the alert is bounded, affordable, and
still returns what it did last week.
