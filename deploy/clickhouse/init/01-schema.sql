-- The OpenTelemetry Collector's ClickHouse exporter trace schema, reproduced
-- so the tests run against the layout real OTel data actually lands in.
--
-- Upstream is open-telemetry/opentelemetry-collector-contrib, at
-- exporter/clickhouseexporter/internal/sqltemplates/traces_table.sql
--
-- Columns, types, codecs, indexes, PARTITION BY and ORDER BY are verbatim.
-- Two deliberate deviations, both local-development concerns:
--   * ENGINE is plain MergeTree. Upstream templates the engine so a cluster
--     can use a Replicated one. See spec 12 on sharded testing.
--   * TTL is 3 days rather than the upstream default, to keep the dev volume
--     small. `just compose-down` deletes the volume anyway.

CREATE DATABASE IF NOT EXISTS otel;

CREATE TABLE IF NOT EXISTS otel.otel_traces
(
    Timestamp          DateTime64(9) CODEC (Delta, ZSTD(1)),
    TraceId            String CODEC (ZSTD(1)),
    SpanId             String CODEC (ZSTD(1)),
    ParentSpanId       String CODEC (ZSTD(1)),
    TraceState         String CODEC (ZSTD(1)),
    SpanName           LowCardinality(String) CODEC (ZSTD(1)),
    SpanKind           LowCardinality(String) CODEC (ZSTD(1)),
    ServiceName        LowCardinality(String) CODEC (ZSTD(1)),
    ResourceAttributes Map(LowCardinality(String), String) CODEC (ZSTD(1)),
    ScopeName          String CODEC (ZSTD(1)),
    ScopeVersion       String CODEC (ZSTD(1)),
    SpanAttributes     Map(LowCardinality(String), String) CODEC (ZSTD(1)),
    Duration           UInt64 CODEC (ZSTD(1)),
    StatusCode         LowCardinality(String) CODEC (ZSTD(1)),
    StatusMessage      String CODEC (ZSTD(1)),
    Events Nested (
        Timestamp DateTime64(9),
        Name LowCardinality(String),
        Attributes Map(LowCardinality(String), String)
    ) CODEC (ZSTD(1)),
    Links Nested (
        TraceId String,
        SpanId String,
        TraceState String,
        Attributes Map(LowCardinality(String), String)
    ) CODEC (ZSTD(1)),
    INDEX idx_trace_id TraceId TYPE bloom_filter(0.001) GRANULARITY 1,
    INDEX idx_res_attr_key mapKeys(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_res_attr_value mapValues(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_span_attr_key mapKeys(SpanAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_span_attr_value mapValues(SpanAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_duration Duration TYPE minmax GRANULARITY 1
)
ENGINE = MergeTree
PARTITION BY toDate(Timestamp)
ORDER BY (ServiceName, SpanName, toDateTime(Timestamp))
TTL toDateTime(Timestamp) + toIntervalDay(3)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

-- A second source's table: the same table with one column of another type.
--
-- A rule selects sources rather than naming one, so every cluster its selector
-- reaches has to carry the table the SQL reads with the columns it reads
-- (spec 6.10). A single-node stack cannot be two clusters and does not have to
-- be: a source is a cluster, a user and a table, so two sources reading this
-- node through different databases disagree exactly the way two clusters do,
-- because a rule's unqualified table name resolves in each source's own
-- database.
--
-- Duration is Float64 here and UInt64 in otel.otel_traces, so one rule returns
-- a value of a different type per source. That is the disagreement
-- rule/source-schema exists to report, and it is the one worth reproducing
-- against a real server: both sources resolve the query, so every other tier 1
-- check passes against both and nothing else says the two answers differ.
CREATE DATABASE IF NOT EXISTS otel_dc2;

CREATE TABLE IF NOT EXISTS otel_dc2.otel_traces
(
    Timestamp   DateTime64(9) CODEC (Delta, ZSTD(1)),
    ServiceName LowCardinality(String) CODEC (ZSTD(1)),
    Duration    Float64 CODEC (ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toDate(Timestamp)
ORDER BY (ServiceName, toDateTime(Timestamp))
TTL toDateTime(Timestamp) + toIntervalDay(3)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

-- The same exporter's metrics tables, reproduced the same way and for the same
-- reason: a rule over a metrics table is proven against the layout real OTel
-- metrics land in rather than against one trimmed to what a test needs.
--
-- Upstream is open-telemetry/opentelemetry-collector-contrib, at
-- exporter/clickhouseexporter/internal/sqltemplates/metrics_sum_table.sql and
-- metrics_gauge_table.sql. Columns, types, codecs, indexes, PARTITION BY,
-- ORDER BY and SETTINGS are verbatim, with the same two local deviations the
-- trace table takes: plain MergeTree, and a 3 day TTL.
--
-- Verbatim matters more here than it does for traces. The collector runs with
-- create_schema off, so a column that differs from the template is an insert
-- the exporter fails: the rows never arrive, and a test waiting for them waits
-- rather than failing with a reason.
--
-- Two tables, not five. A sum is where a counter lands and a gauge is where a
-- reading lands, which is the pair a rule can get wrong: the sum's
-- AggregationTemporality and IsMonotonic decide whether a correct rule reads a
-- per-series delta or a plain sum, and a gauge read out of the sum table parses
-- and returns nothing forever. Histograms, summaries and exponential
-- histograms land in their own tables and no rule in the tree reads one, so
-- they are not here until one does.
--
-- Only in otel. otel_dc2 exists to disagree with otel about a trace column
-- (see above), and a second metrics table nothing reads would be a table
-- nothing reads.

CREATE TABLE IF NOT EXISTS otel.otel_metrics_sum
(
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ResourceSchemaUrl String CODEC(ZSTD(1)),
    ScopeName String CODEC(ZSTD(1)),
    ScopeVersion String CODEC(ZSTD(1)),
    ScopeAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ScopeDroppedAttrCount UInt32 CODEC(ZSTD(1)),
    ScopeSchemaUrl String CODEC(ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    MetricName LowCardinality(String) CODEC(ZSTD(1)),
    MetricDescription String CODEC(ZSTD(1)),
    MetricUnit String CODEC(ZSTD(1)),
    Attributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    StartTimeUnix DateTime CODEC(Delta, ZSTD(1)),
    TimeUnix DateTime CODEC(Delta, ZSTD(1)),
    Value Float64 CODEC(ZSTD(1)),
    Flags UInt32  CODEC(ZSTD(1)),
    Exemplars Nested (
        FilteredAttributes Map(LowCardinality(String), String),
        TimeUnix DateTime,
        Value Float64,
        SpanId String,
        TraceId String
    ) CODEC(ZSTD(1)),
    AggregationTemporality Int32 CODEC(ZSTD(1)),
    IsMonotonic Boolean CODEC(Delta, ZSTD(1)),
    INDEX idx_res_attr_key mapKeys(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_res_attr_value mapValues(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_scope_attr_key mapKeys(ScopeAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_scope_attr_value mapValues(ScopeAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_attr_key mapKeys(Attributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_attr_value mapValues(Attributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_time_minmax TimeUnix TYPE minmax GRANULARITY 1
)
ENGINE = MergeTree
PARTITION BY toDate(TimeUnix)
ORDER BY (ServiceName, MetricName, toStartOfHour(TimeUnix), cityHash64(Attributes), TimeUnix)
TTL toDateTime(TimeUnix) + toIntervalDay(3)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

CREATE TABLE IF NOT EXISTS otel.otel_metrics_gauge
(
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ResourceSchemaUrl String CODEC(ZSTD(1)),
    ScopeName String CODEC(ZSTD(1)),
    ScopeVersion String CODEC(ZSTD(1)),
    ScopeAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ScopeDroppedAttrCount UInt32 CODEC(ZSTD(1)),
    ScopeSchemaUrl String CODEC(ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    MetricName LowCardinality(String) CODEC(ZSTD(1)),
    MetricDescription String CODEC(ZSTD(1)),
    MetricUnit String CODEC(ZSTD(1)),
    Attributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    StartTimeUnix DateTime CODEC(Delta, ZSTD(1)),
    TimeUnix DateTime CODEC(Delta, ZSTD(1)),
    Value Float64 CODEC(ZSTD(1)),
    Flags UInt32 CODEC(ZSTD(1)),
    Exemplars Nested (
        FilteredAttributes Map(LowCardinality(String), String),
        TimeUnix DateTime,
        Value Float64,
        SpanId String,
        TraceId String
    ) CODEC(ZSTD(1)),
    INDEX idx_res_attr_key mapKeys(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_res_attr_value mapValues(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_scope_attr_key mapKeys(ScopeAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_scope_attr_value mapValues(ScopeAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_attr_key mapKeys(Attributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_attr_value mapValues(Attributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_time_minmax TimeUnix TYPE minmax GRANULARITY 1
)
ENGINE = MergeTree
PARTITION BY toDate(TimeUnix)
ORDER BY (ServiceName, MetricName, toStartOfHour(TimeUnix), cityHash64(Attributes), TimeUnix)
TTL toDateTime(TimeUnix) + toIntervalDay(3)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;
