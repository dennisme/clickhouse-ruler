-- The ClickHouse user contract from spec 6.7.2, as something that runs.
--
-- Half of what this tool claims is enforced by the database rather than by any
-- check: which tables and rows a rule can read, that it cannot reach data
-- through a table function, that it cannot mutate anything, and that it cannot
-- raise the limits the ruler sends. All of it depends on the user being
-- created this way, so the integration tests connect as `ruler_payments` and
-- the contract is proven by CI instead of asserted in a README.
--
-- Like every file in this directory, it runs only on an empty data directory.
-- Editing it does nothing until the volume is destroyed: `just compose-down`
-- passes -v for that reason.
--
-- `ruler` stays the admin user. Seeding rows is not something the ruler ever
-- does, so the tests write as `ruler` and evaluate as `ruler_payments`.

-- The profile is the half a query cannot argue with. Every limit carries a
-- constraint, because a limit sent without one is a default a rule can raise
-- in a single SETTINGS clause. CONST still permits the ruler to send the same
-- value it is pinned to, and refuses any other.
CREATE SETTINGS PROFILE IF NOT EXISTS ruler SETTINGS
    readonly = 2,
    max_execution_time = 30 MAX 60,
    max_memory_usage = 1073741824 MAX 2147483648,
    max_result_rows = 1001 MAX 2000,
    result_overflow_mode = 'throw' CONST,
    timeout_overflow_mode = 'throw' CONST,
    skip_unavailable_shards = 0 CONST;

-- SELECT on one table, and nothing else. Not the database: the source names a
-- table, and a wider grant is invisible both to the checks and to a diff of
-- the rules repository.
--
-- The exception is one column pair of one system table: how many shards the
-- cluster behind a Distributed table has, so a cost prediction answers for the
-- cluster rather than for the node the ruler connected to (spec 6.9). It holds
-- no row a rule could read, which is what makes it affordable where a grant on
-- data would not be. A single node source never asks for it, and it is granted
-- here anyway because there is one contract rather than one per topology.
CREATE ROLE IF NOT EXISTS ruler_reader;
GRANT SELECT ON otel.otel_traces TO ruler_reader;
GRANT SELECT(cluster, shard_num) ON system.clusters TO ruler_reader;

CREATE USER IF NOT EXISTS ruler_payments IDENTIFIED WITH no_password SETTINGS PROFILE ruler;
GRANT ruler_reader TO ruler_payments;
ALTER USER ruler_payments DEFAULT ROLE ALL;

-- A deliberately over-privileged user, so there is something to point the
-- check at that fails it.
--
-- This is the failure the check exists for, and it is the silent one: every
-- rule evaluates perfectly as this user. It can read the whole database, reach
-- data no row policy ever sees through url() and remote(), and raise any limit
-- the ruler sends, and nothing about an evaluation looks different. Nobody
-- finds out until someone writes the query that uses it.
CREATE USER IF NOT EXISTS ruler_wide IDENTIFIED WITH no_password;
GRANT SELECT ON otel.* TO ruler_wide;
GRANT CREATE TEMPORARY TABLE ON *.* TO ruler_wide;
GRANT READ ON URL TO ruler_wide;
GRANT READ ON FILE TO ruler_wide;
GRANT READ ON S3 TO ruler_wide;
GRANT REMOTE ON *.* TO ruler_wide;

-- The second source's user, under the same contract and reading the other
-- database. Two sources are what the checks comparing sources with each other
-- need, and a source is a cluster, a user and a table, so the second one gets
-- its own user rather than widening the first's role (spec 6.10).
CREATE ROLE IF NOT EXISTS ruler_dc2_reader;
GRANT SELECT ON otel_dc2.otel_traces TO ruler_dc2_reader;
GRANT SELECT(cluster, shard_num) ON system.clusters TO ruler_dc2_reader;

CREATE USER IF NOT EXISTS ruler_dc2 IDENTIFIED WITH no_password SETTINGS PROFILE ruler;
GRANT ruler_dc2_reader TO ruler_dc2;
ALTER USER ruler_dc2 DEFAULT ROLE ALL;

-- The metrics source's user, under the same contract and reading the metrics
-- tables rather than the traces one.
--
-- Its own role rather than a second grant on ruler_reader, because the grant a
-- source holds is SELECT on the table it names and nothing else. Widening
-- ruler_reader would give every traces rule read access to a table its rules
-- never mention, which is the thing the comment above it refuses, and it would
-- make source/privileges pass for a source whose grant is wider than its rule.
--
-- Two tables in one role, because a rule that reads a counter out of
-- otel_metrics_sum and a reading out of otel_metrics_gauge is one source's
-- rule: the type decides the table, so a source reading metrics reads both or
-- is two sources for no reason a rule author would recognise.
CREATE ROLE IF NOT EXISTS ruler_metrics_reader;
GRANT SELECT ON otel.otel_metrics_sum TO ruler_metrics_reader;
GRANT SELECT ON otel.otel_metrics_gauge TO ruler_metrics_reader;
GRANT SELECT(cluster, shard_num) ON system.clusters TO ruler_metrics_reader;

CREATE USER IF NOT EXISTS ruler_metrics IDENTIFIED WITH no_password SETTINGS PROFILE ruler;
GRANT ruler_metrics_reader TO ruler_metrics;
ALTER USER ruler_metrics DEFAULT ROLE ALL;
