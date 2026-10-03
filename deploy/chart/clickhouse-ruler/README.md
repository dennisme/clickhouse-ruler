# clickhouse-ruler

Runs [clickhouse-ruler](https://github.com/dennisme/clickhouse-ruler): alert
rules written as ClickHouse SQL, checked before they run, delivered to an
Alertmanager you already have.

The chart version is the tool version, so `--version 1.2.3` installs the chart
that runs the `1.2.3` image.

```sh
helm install ruler oci://ghcr.io/dennisme/charts/clickhouse-ruler \
  --namespace monitoring --values my-values.yaml
```

## What it installs

A Deployment with the ruler and a git-sync sidecar that clones your rules
repository and posts to the ruler's reload endpoint after each sync, a Service
on the metrics port, a ConfigMap holding the sources file, and a ServiceAccount.
A ServiceMonitor and a PodDisruptionBudget are templated too, both off until
asked for.

## What you have to set

Three things, and the render fails without them:

| Value | What it is |
| --- | --- |
| `alertmanagerURLs` | Where alerts are delivered, one entry per member of the Alertmanager cluster. Every alert is posted to every member. |
| `rules.gitSync.repo` | The repository holding the rule files. `rules.configMap.name` instead, when `rules.delivery` is `configMap`. |
| `sources` or `sourcesSecret.name` | The ClickHouse clusters to evaluate against, either templated from values or supplied whole in a Secret. Not both. |

A source's password is never a value. It is read from a Secret you create,
named per source under `passwordSecret`.

Everything else has a default that runs, and `values.yaml` says what each one
is for. `values.schema.json` refuses an unknown key at render, so a typo fails
before a pod boots.

## Where the reasoning is

- [Deployment topologies](https://dennisme.github.io/clickhouse-ruler/deployment/),
  including how a merged rule reaches the ruler and what this chart decides for
  you.
- [Operating the ruler](https://dennisme.github.io/clickhouse-ruler/operations/):
  what to watch, what the logs mean, what refuses to start.
- [`deploy/examples`](https://github.com/dennisme/clickhouse-ruler/tree/main/deploy/examples)
  for a complete values file per delivery.
