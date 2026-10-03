# Rules over metrics tables

Every example elsewhere in this manual reads a traces table, where a row is an
event and a rule counts rows or takes a quantile of a column. The OTel
collector's ClickHouse exporter keeps metrics in tables of their own, one per
metric type, and a row there is a reading on a series. The arithmetic is
different, and a rule that uses the trace arithmetic on a counter is wrong in a
way that still parses, still returns the column it promised, and still pages
somebody.

A source is a cluster, a user and a table, so metrics are a source of their own
rather than a second table on the traces one:

```yaml
  - name: otel_metrics
    labels: {team: payments, signal: metrics}
    address: clickhouse:9000
    database: otel
    username: ruler_metrics
    table: otel_metrics_sum
    timestamp_column: TimeUnix
```

`table:` names the sum table because a counter is the shape a rule gets wrong.
A rule reading `otel_metrics_gauge` names it in its own `FROM`, the way a rule
reading any other granted table does.

## Two columns decide the query

`otel_metrics_sum` holds both kinds of counter the protocol defines, under the
same metric name and in the same table, and two columns say which a row is.

| Column | Values | What it means |
| --- | --- | --- |
| `AggregationTemporality` | `2` cumulative, `1` delta | whether `Value` is a running total or the increment since the previous point |
| `IsMonotonic` | `true`, `false` | whether the series only ever climbs, which is what makes it a counter rather than an up-down gauge |

A cumulative point's `Value` is the total since the exporting process started.
A delta point's `Value` is what arrived since the point before it. The same
question about the same metric therefore has two opposite queries, and nothing
in the SQL text tells you which one you wrote. Put the column in the `WHERE`
clause: a series that starts exporting the other temporality then stops being
read by arithmetic that does not fit it, instead of quietly joining the result.

## A cumulative counter: delta per series, then sum

```sql
SELECT ServiceName, sum(delta) AS value
FROM (
  SELECT ServiceName, Attributes, StartTimeUnix, max(Value) - min(Value) AS delta
  FROM otel_metrics_sum
  WHERE MetricName = 'http_server_errors_total'
    AND ServiceName = 'checkout'
    AND AggregationTemporality = 2
    AND TimeUnix >= {{ .From }} AND TimeUnix < {{ .To }}
  GROUP BY ServiceName, Attributes, StartTimeUnix
)
GROUP BY ServiceName
HAVING value > 1000
```

The inner query subtracts inside one series and the outer one adds the results
up. That order is the whole idiom, for two reasons.

**Subtracting across series is nonsense.** Two pods, two routes and two status
codes are different series, each with its own running total, and the difference
between one series' total and another's is not a number anybody wants.
`ServiceName` and `Attributes` are the identity, so they are in the inner
grouping.

**`StartTimeUnix` is in that grouping too, and it is the part that looks like
noise.** It is the series start each point carries, and it moves when the
exporting process restarts: the total goes back to nothing and climbs again.
Without it in the grouping, a window holding a restart subtracts the new run's
small value from the old run's large one, so a counter that has barely moved
reports the previous run's entire total. With it, the restart is two series, and
the window reports the increase on each.

## A delta counter: sum

```sql
SELECT ServiceName, sum(Value) AS value
FROM otel_metrics_sum
WHERE MetricName = 'http_server_errors_total'
  AND ServiceName = 'checkout'
  AND AggregationTemporality = 1
  AND TimeUnix >= {{ .From }} AND TimeUnix < {{ .To }}
GROUP BY ServiceName
HAVING value > 1000
```

Each row is already an increment, so the increase over a window is the sum of
the rows in it, there is nothing to subtract, and the series identity does not
have to be grouped before anything else happens. A per-series delta over these
rows would compare one increment against another and report close to zero
however many errors arrived.

## The wrong one, and what the alert does

```sql
SELECT ServiceName, max(Value) AS value
FROM otel_metrics_sum
WHERE MetricName = 'http_server_errors_total'
  AND ServiceName = 'checkout'
  AND TimeUnix >= {{ .From }} AND TimeUnix < {{ .To }}
GROUP BY ServiceName
HAVING value > 1000
```

This is what a counter rule looks like when it is written the way a traces rule
is written, and it reviews perfectly. On a cumulative series it asks whether the
process has ever served a thousand errors, not whether it is serving them now.

Take one series: 1200 errors over two minutes, then six minutes of nothing, then
a restart and ten more errors. Over a three minute window, with the threshold at
1000:

| The window | What happened in it | The raw threshold | A per-series delta |
| --- | --- | --- | --- |
| the rise | 1200 errors | fires on 1200 | fires on 1200 |
| six minutes later | nothing | still firing on 1200 | resolved |
| spanning the restart | 5 errors | still firing on 1200 | 5, under the threshold |
| after the restart | 10 errors | resolved | 10, under the threshold |

So the rule fires once and never stops, because the total it is comparing stays
where it got to for as long as the process lives. Then it resolves at a restart,
which is the moment the condition is most likely to be true, because the total
starts again from nothing. Neither of those is a threshold being too low or too
high, and no amount of tuning the number moves either one.

Every cell of that table is what the integration tests in `cmd/ruler` assert,
against data points posted as OTLP and written by the collector, so the SQL above
is the SQL that ran.

## A window shorter than the export interval

A delta needs two points to subtract. An exporter sends on an interval, so a
window shorter than that interval holds one point per series, every subtraction
is zero, and the rule is silent no matter how fast the counter is climbing. A
window shorter still holds no point at all and the rule returns nothing, which
looks exactly the same from the outside.

So `window` on a metrics rule has a floor that a traces rule does not have: it
has to be comfortably longer than the collector's export interval, and the usual
reason to set a window shorter than the group `interval` does not apply here.

## Gauges are a different table

A gauge reading stands on its own, with no accumulation and no temporality, and
the exporter writes it to `otel_metrics_gauge`. A rule asking a gauge question
of `otel_metrics_sum` parses, runs, returns no rows and never alerts, which is
the silent failure this tool exists to catch and is not one any check catches
today. The reference user in `deploy/clickhouse/init/02-ruler-user.sql` grants
`SELECT` on both tables for that reason: the metric type decides the table, so a
source reading metrics reads both.

## Nothing checks any of this yet

`ruler check` reads the SQL, and the shapes on this page are not among the
things it knows about. A raw threshold on a cumulative counter is a correct
query that answers the wrong question, and whether the ruler should learn to say
so is
[spec 12.1](https://github.com/dennisme/clickhouse-ruler/blob/main/spec.md#12-open-questions).
Until then it is on the author, which is what this page is for.
