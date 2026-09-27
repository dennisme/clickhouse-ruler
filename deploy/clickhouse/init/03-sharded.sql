-- The Distributed tables the sharded tests read, over the clusters in
-- deploy/clickhouse/cluster.xml.
--
-- A rule author writes their own FROM clause, so on a sharded cluster they name
-- the Distributed table and the ruler never has to know the difference (spec
-- 6.9). These are that table: one over both shards, and one whose second shard
-- does not resolve, which is the only way to find out what an evaluation does
-- when part of the cluster is gone.
--
-- Both are created on both nodes, because this file runs on both. A Distributed
-- table is a definition rather than data and contacts nothing when it is
-- created, so the second node carrying one it never uses costs nothing and
-- keeps the two nodes' init identical.
--
-- The structure is copied from otel.otel_traces so the columns a rule reads are
-- the same ones whether it reads a shard or the cluster. rand() as the sharding
-- key is deliberate: nothing here inserts through these tables, and a test
-- seeds each shard directly so it knows which rows live where.
CREATE TABLE IF NOT EXISTS otel.otel_traces_shards AS otel.otel_traces
ENGINE = Distributed(ruler_shards, otel, otel_traces, rand());

CREATE TABLE IF NOT EXISTS otel.otel_traces_dead_shard AS otel.otel_traces
ENGINE = Distributed(ruler_dead_shard, otel, otel_traces, rand());

-- SELECT on the Distributed tables and nothing more. A Distributed table needs
-- no other grant, which is the point worth proving: the ruler's user fans a
-- query out across the cluster while still refused remote() and url(), so the
-- contract in spec 6.7.2 holds on a sharded cluster exactly as it does on one
-- node.
GRANT SELECT ON otel.otel_traces_shards TO ruler_reader;
GRANT SELECT ON otel.otel_traces_dead_shard TO ruler_reader;
