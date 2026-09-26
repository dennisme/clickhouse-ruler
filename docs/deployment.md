# Deployment topologies

Five ways to run this, all the same binary with a different sources file.
Which one you want depends on who owns the clusters and who writes the rules.

## One ruler, one cluster

One process, one sources file, one ClickHouse. Sources need no labels at all,
because there is nothing to select between.

Start here. Everything below is this plus a reason.

## Ruler per data centre

One ruler in each data centre, each holding the sources for its own clusters,
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
