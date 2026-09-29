# Deployment topologies

Five ways to run this, all the same binary with a different sources file.
Which one you want depends on who owns the clusters and who writes the rules.

## How a rule reaches the ruler

The topologies below all assume the rules are on a disk the ruler can read and
that something asks it to re-read them. That is the Helm chart's job, and the
answer is a command:

```sh
kubectl create secret generic ruler-clickhouse \
  --namespace monitoring --from-literal=password='...'

helm install ruler oci://ghcr.io/dennisme/charts/clickhouse-ruler \
  --namespace monitoring --values deploy/examples/git-sync.yaml
```

The chart version is the tool version, so `--version 1.2.3` installs the chart
that runs the `1.2.3` image.

**The ruler watches nothing.** Hot reload is `SIGHUP`, and `POST /-/reload`
where a signal cannot be delivered. Whatever rolled the files out is the only
party that knows when they are complete, so the watching lives beside the ruler
rather than inside it. The chart runs the ruler with
`--enable-reload-endpoint`, which the binary does not do by default.

**git-sync is the default delivery.** A sidecar clones the rules repository
into a worktree and flips a symlink at the rules path, so the ruler cannot read
a tree half written, and its exec hook posts to `/-/reload` after each
successful sync. The chain is merge, sync, symlink flip, hook, reload, and the
lag is the sync period plus one reload.

The chart points the ruler at the symlink plus `rules.subdirectory`, which with
the defaults is `/rules/current/rules`. An init container syncs once before the
ruler's first load, because the ruler refuses to start on a rules path it cannot
read and an empty volume is one.

[`deploy/examples/git-sync.yaml`](https://github.com/dennisme/clickhouse-ruler/blob/main/deploy/examples/git-sync.yaml)
is that topology as a values file.

**A ConfigMap mount is the small-estate case**, for an estate whose rules fit in
one object and whose authors are its operators. There is no exec hook there, so
the mount needs a reloader sidecar of its own, which
[`deploy/examples/configmap.yaml`](https://github.com/dennisme/clickhouse-ruler/blob/main/deploy/examples/configmap.yaml)
supplies. Without one, editing the ConfigMap changes the files and the ruler
keeps evaluating what it loaded at startup.

Both layouts hand the ruler a rules path built out of symlinks: git-sync's
`--link`, and kubelet's `..data`. The loader reads each of them exactly once.

**Two series say what is actually running.**
`clickhouse_ruler_config_last_reload_successful` at 0 means the ruler read the
new files, refused them, and kept the ones it had, which looks healthy from the
outside and is not. It is also a hazard ahead of the next restart, because files
a reload refuses are files the ruler refuses to start on. See
[a reload the ruler refused](operations.md#a-reload-the-ruler-refused). Its pair,
`clickhouse_ruler_config_last_reload_timestamp_seconds`, dates the configuration
being evaluated.

### What the chart holds and what it references

A source's address, database, table, timestamp column, caps and labels are
reviewable configuration and live in values. Its password does not: it is a
`password_file` pointing into a mounted Secret, and no value in the chart holds
one. The values schema refuses a source key the sources file does not have,
which is how a password in a values file fails at render.

There is a second path for the "ruler as a service" topology below, where the
sources file is the platform team's own artifact rather than something to
restate in values: `sourcesSecret` names a Secret holding the whole file, and
the chart templates none of it. Setting both is refused.

## One ruler, one cluster

One process, one sources file, one ClickHouse. Sources need no labels at all,
because there is nothing to select between.

Start here. Everything below is this plus a reason.

## Ruler per datacenter

One ruler in each datacenter, each holding the sources for its own clusters,
so a query reads data locally rather than across a link. Every ruler reads the
same rules repository and evaluates the subset whose selector matches the
sources it holds.

Label your sources by location and select on that:

```yaml
# rules/sources.yaml, in eu-west
sources:
  - name: traces
    labels:
      region: eu-west
```

```yaml
# a rule that only runs in eu-west
- alert: CheckoutSlow
  sources:
    region: eu-west
```

**Unmatched rules are normal here.** A ruler in `eu-west` loads the whole
repository and will never evaluate anything selecting `us-east`, which
`clickhouse_ruler_rules_unmatched` reports as a number rather than an error.
Watch that it returns to zero after a rollout, not that it is zero. See
[the operations page](operations.md#rules-that-will-never-run).

## Central rulers, highly available

Several rulers with the same sources file. It works by duplication, not by
coordination: an alert's identity is its final label set, every part of that
set comes from the files and the query result rather than from the process,
so both rulers arrive at the same fingerprint and Alertmanager deduplicates
them. No leader election, no shared state.

Three things to know before you run more than one.

**Replicas need identical source labels, and a replica label breaks
deduplication.** Source `labels` land on alerts, so two rulers meant to be
replicas of each other must carry exactly the same ones. Adding a label
naming the replica, which is the obvious thing to reach for when two
processes emit the same alert, is precisely what stops Alertmanager
deduplicating: it makes every alert two alerts, and your route tree then
routes both. If you need to tell the replicas apart, do it with the `job`
label in Prometheus, not in the sources file.

**Query load is linear in replicas.** Each replica evaluates every matched
rule against every matched source, so three rulers is three times the
queries and three times the cost caps consumed on the cluster. That is the
argument for sharding by source rather than replicating for its own sake, and
it is why the per-source query concurrency limit matters more here than
anywhere else: a cluster already carrying triple the rule traffic is the one
that starts queueing.

**Replicas can disagree about when an alert fires, never about what.** Each
ruler holds `ActiveAt` in memory, so a replica that restarts, or one added to
an existing set, re-serves every pending alert's full `for` before it will
fire. And each one computes its window from its own clock, so skew between
replicas shifts what each one reads; a source's `evaluation_delay` absorbs
the ordinary case. Neither is a reason to run one ruler instead of three.
They are reasons a deduplicated alert can arrive a little late from one
replica, and you should know that before you are paged about it.

Put the replicas behind one deployment and let `/-/ready` do its job: it
fails when the ruler has no rules loaded or no source answering, which is
what stops a rollout replacing working replicas with broken ones.

## Ruler per team

A team runs its own ruler, points it at its own sources, and consumes the
central rules repository for the checks. They own the process, the clusters
they name, and the on-call that follows from both.

## Ruler as a service

The team operating ClickHouse owns the sources file and the clusters. Other
teams contribute only rules.

This is the split worth keeping: adding a cluster or a ClickHouse user is an
admin change, and writing an alert against one is not. Rule authors never see
a password, never choose a cap, and cannot reach a table their selector's
source is not granted.

Because one user is shared across many rules here, the source's username
cannot tell you which rule sent a query. The `log_comment` on every query
can, which is what
[the ClickHouse side of the operations page](operations.md#finding-the-rulers-queries-in-clickhouse)
is for.
