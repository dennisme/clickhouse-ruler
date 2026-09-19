-- A trimmed version of the ClickStack OTel trace layout: enough columns for
-- the querier tests to exercise time bounds, map attributes and aggregation,
-- without carrying the full upstream schema.

CREATE DATABASE IF NOT EXISTS otel;

CREATE TABLE IF NOT EXISTS otel.otel_traces
(
    Timestamp      DateTime64(9) CODEC (Delta, ZSTD(1)),
    TraceId        String CODEC (ZSTD(1)),
    SpanId         String CODEC (ZSTD(1)),
    ServiceName    LowCardinality(String) CODEC (ZSTD(1)),
    SpanName       LowCardinality(String) CODEC (ZSTD(1)),
    Duration       UInt64 CODEC (ZSTD(1)),
    StatusCode     LowCardinality(String) CODEC (ZSTD(1)),
    SpanAttributes Map(LowCardinality(String), String) CODEC (ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toDate(Timestamp)
ORDER BY (ServiceName, SpanName, toDateTime(Timestamp))
TTL toDateTime(Timestamp) + toIntervalDay(3);
