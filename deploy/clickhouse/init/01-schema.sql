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
