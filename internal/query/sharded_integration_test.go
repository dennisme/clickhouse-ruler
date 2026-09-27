//go:build integration

package query

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// What a dead shard does to an evaluation, against a real two node cluster.
//
// The ruler sends skip_unavailable_shards = 0 with every query (spec 6.9), and
// a unit test can read that out of the settings map. What a unit test cannot
// show is the failure the setting exists to prevent: with it at 1, ClickHouse
// answers a distributed query whose shard is unreachable by returning the rows
// the surviving shards held, with no error at all. Those missing rows are
// indistinguishable from a recovered condition, so instances leave the state
// machine and their alerts resolve in the middle of the outage that should have
// paged someone.
//
// One node cannot produce that answer, which is why the compose stack has two
// and the clusters in deploy/clickhouse/cluster.xml exist. The three tests here
// are one argument: a query fans out for real, an unreachable shard fails the
// evaluation under the ruler's settings, and the same query returns a partial
// result when the setting is raised. The third is the bug, reproduced, and it is
// what makes the second one proof rather than an assertion about a string.

// deadShardHost is the address the dead shard cluster points at, matching
// deploy/clickhouse/cluster.xml. It is in the range RFC 5737 reserves for
// documentation, so nothing answers on it anywhere. That is what makes the
// failure static: no container is stopped, so the stack a later test finds is
// the stack this one was handed.
const deadShardHost = "192.0.2.1"

// shardTwoAddress is the second shard, as `just integration` passes it.
//
// Read here rather than added to testSource: a source is one address and stays
// one (spec 6.9), and the only reason a test needs the second node directly is to
// seed it. Distributed queries reach it through the cluster definition, not
// through this.
func shardTwoAddress(t *testing.T) string {
	t.Helper()

	address := os.Getenv("RULER_CLICKHOUSE_ADDR_2")
	if address == "" {
		t.Fatal("RULER_CLICKHOUSE_ADDR_2 is not set, run these through `just integration`")
	}
	return address
}

// shardedSource reads one of the Distributed tables from deploy/clickhouse/init.
//
// Still the restricted user from spec 6.7.2. A Distributed table needs nothing
// more than SELECT on itself, so the contract that refuses remote() and url()
// is intact while the query fans out, which is the point: the ruler reaches
// other nodes the way an author's own FROM clause does and no other way.
func shardedSource(t *testing.T, table string) source.Source {
	t.Helper()

	src := testSource(t)
	src.Name = table
	src.Table = table
	return src
}

const shardedLatencyExpr = `
SELECT ServiceName, max(Duration) AS value
FROM otel.%s
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
GROUP BY ServiceName
ORDER BY ServiceName`

// shardedKeyRule reads one map key, which is what the sample check probes for.
const shardedKeyExpr = `
SELECT ServiceName, count() AS value
FROM otel.%s
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
AND SpanAttributes['%s'] != ''
GROUP BY ServiceName`

func shardedKeyRule(table, key string) rule.Rule {
	return rule.Rule{
		Alert:  "KeyPresent",
		Expr:   fmt.Sprintf(shardedKeyExpr, table, key),
		Window: time.Hour,
	}
}

func shardedRule(table string) rule.Rule {
	return rule.Rule{
		Alert:  "MaxLatency",
		Expr:   fmt.Sprintf(shardedLatencyExpr, table),
		Window: 5 * time.Minute,
	}
}

// shardedCostRule is the same rule over a window wide enough to hold the rows a
// cost test seeded.
//
// A cost check renders its bounds ending at now and spanning the rule's own
// window, because those bounds are what the optimiser prunes on. The fixture is
// written around anchor, which is an hour behind, so a five minute rule estimates
// correctly at nothing and there is no number to scale.
func shardedCostRule(table string) rule.Rule {
	r := shardedRule(table)
	r.Window = 24 * time.Hour
	return r
}

// seedKeyAt writes one row to one node carrying one map key, so a key exists on
// that shard and nowhere else.
//
// seedAt cannot express this: it writes the same attribute names to every row it
// is given, and what the sample check asks about is a key name rather than a
// value. Which shard a key lives on is the whole question here, so the rows are
// written shard by shard for the reason seedAt itself is: a Distributed table is
// what a rule reads, not what a test writes through.
func seedKeyAt(t *testing.T, address, service, key string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := adminConn(t, address)
	if err := conn.Exec(ctx, "TRUNCATE TABLE otel.otel_traces"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	err := conn.Exec(ctx,
		"INSERT INTO otel.otel_traces (Timestamp, ServiceName, SpanName, Duration, SpanAttributes) "+
			"VALUES (?, ?, 'GET /', 1, map(?, '1'))",
		anchor.Add(-time.Minute), service, key)
	if err != nil {
		t.Fatalf("seeding %s on %s: %v", key, address, err)
	}
}

// Both shards answering, so the tests that follow are about the dead shard
// rather than about a cluster that was never fanning out.
func TestRunReadsEveryShard(t *testing.T) {
	src := shardedSource(t, "otel_traces_shards")
	q := openQuerier(t, src)

	seedAt(t, src.Address, []span{
		{at: anchor.Add(-time.Minute), service: "shard-one", duration: 11},
	})
	seedAt(t, shardTwoAddress(t), []span{
		{at: anchor.Add(-time.Minute), service: "shard-two", duration: 22},
	})

	got := run(t, q, shardedRule("otel_traces_shards"), anchor)

	want := []alertSample{
		{service: "shard-one", value: 11},
		{service: "shard-two", value: 22},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want a row from each shard: %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The evaluation fails rather than resolving every alert the dead shard held.
func TestRunFailsWhenAShardIsUnreachable(t *testing.T) {
	src := shardedSource(t, "otel_traces_dead_shard")
	q := openQuerier(t, src)

	seedAt(t, src.Address, []span{
		{at: anchor.Add(-time.Minute), service: "shard-one", duration: 11},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := q.Run(ctx, shardedRule("otel_traces_dead_shard"), testGroup, anchor)
	if err == nil {
		t.Fatalf("Run succeeded with %d samples from the surviving shard, want an error", len(got.Samples))
	}

	// Which error matters as much as that there was one. A query refused
	// because the profile would not take the setting is also an error, and it
	// would make this test pass while proving nothing about a dead shard, so
	// the failure has to be about reaching the node. ClickHouse words that
	// differently depending on how far the connection got, and the host name
	// is the part every wording carries.
	if !strings.Contains(err.Error(), deadShardHost) {
		t.Errorf("Run failed with %v, want a failure naming the unreachable shard %q", err, deadShardHost)
	}
}

// The bug itself, so the test above is proof rather than an assertion about a
// string: the same unreachable shard, one setting different, and ClickHouse
// answers with half the cluster's rows and no error.
//
// It runs as ruler_wide, the deliberately over-privileged user in
// deploy/clickhouse/init/02-ruler-user.sql. The ruler's own user cannot
// reproduce this at all: skip_unavailable_shards is pinned CONST in its
// profile, so ClickHouse refuses the query instead of answering it partially.
// That is the contract in spec 6.7.2 doing its half of the job, and this is
// what it is protecting against.
func TestRaisedSkipUnavailableShardsHidesTheDeadShard(t *testing.T) {
	src := shardedSource(t, "otel_traces_dead_shard")

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{src.Address},
		Auth: clickhouse.Auth{Database: "otel", Username: "ruler_wide"},
		Settings: clickhouse.Settings{
			"skip_unavailable_shards": 1,
		},
	})
	if err != nil {
		t.Fatalf("opening the unconstrained connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	seedAt(t, src.Address, []span{
		{at: anchor.Add(-time.Minute), service: "shard-one", duration: 11},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := conn.Query(ctx,
		"SELECT DISTINCT ServiceName FROM otel.otel_traces_dead_shard ORDER BY ServiceName")
	if err != nil {
		t.Fatalf("the query was expected to succeed and return a partial result: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var services []string
	for rows.Next() {
		var service string
		if err := rows.Scan(&service); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		services = append(services, service)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("the query was expected to succeed and return a partial result: %v", err)
	}

	if len(services) != 1 || services[0] != "shard-one" {
		t.Fatalf("got %v, want only the surviving shard's row, which is the silent failure this reproduces", services)
	}
}

// What `table:` names on a sharded cluster, proven against both tables of that
// name.
//
// `table:` is the table a rule reads, so it is the Distributed one (spec 6.9).
// The tests below are the argument for that answer and for what it costs: the
// sample sees every shard where the local table sees one, the grant assertion
// reaches every node, and the two questions only a local MergeTree can answer
// resolve through the Distributed engine's own arguments.

// The local table behind a Distributed one, read off the server rather than
// written down, because the caveats that read parts metadata ask about it.
func TestStorageTableResolvesTheLocalTable(t *testing.T) {
	q := openQuerier(t, shardedSource(t, "otel_traces_shards"))

	got := q.storageTable(context.Background())

	if !got.Distributed {
		t.Errorf("storageTable = %+v, want it to know the source's table is Distributed", got)
	}
	want := tableRef{Database: "otel", Table: "otel_traces"}
	if got.Local != want {
		t.Errorf("local table = %+v, want %+v", got.Local, want)
	}
	if got.Unanswerable != "" {
		t.Errorf("unanswerable = %q, want the local table resolved", got.Unanswerable)
	}
}

// A local table is its own storage, so nothing resolves through on a single
// node and the caveats read the table the source names.
func TestStorageTableLeavesALocalTableAlone(t *testing.T) {
	q := openQuerier(t, testSource(t))

	got := q.storageTable(context.Background())

	if got.Distributed {
		t.Errorf("storageTable = %+v, want a MergeTree source to resolve to itself", got)
	}
	if want := (tableRef{Database: "otel", Table: "otel_traces"}); got.Local != want {
		t.Errorf("local table = %+v, want %+v", got.Local, want)
	}
}

// The key check against both readings of `table:`, which is the whole decision
// in one test.
//
// The key is written only by the rows on the second shard. Read through the
// Distributed table the sample sees it and says nothing; read through the
// coordinator's local table it sees one shard, calls a live attribute missing,
// and reports a rule that works as a rule that reads a key nothing writes.
func TestSampleReadsEveryShard(t *testing.T) {
	const onSecondShard = "shard_two_only"

	seedKeyAt(t, testSource(t).Address, "shard-one", "shard_one_only")
	seedKeyAt(t, shardTwoAddress(t), "shard-two", onSecondShard)

	sharded := openQuerier(t, shardedSource(t, "otel_traces_shards"))
	got, err := sharded.Sample(context.Background(),
		shardedKeyRule("otel_traces_shards", onSecondShard), testGroup, SampleChecks{}, anchor)
	if err != nil {
		t.Fatalf("Sample through the Distributed table: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("findings = %v, want none: the key is in the second shard's rows", got)
	}

	// The same key, the same cluster, asked of the table a rule does not read.
	local := openQuerier(t, testSource(t))
	got, err = local.Sample(context.Background(),
		shardedKeyRule("otel_traces", onSecondShard), testGroup, SampleChecks{}, anchor)
	if err != nil {
		t.Fatalf("Sample through the local table: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("findings = %v, want the false absence this decision avoids", got)
	}
	if !strings.Contains(got[0].Detail, "is in none of") {
		t.Errorf("detail = %q, want the key reported absent", got[0].Detail)
	}
}

// The grant assertion fans out, which is more than it does on one node: a read
// of a Distributed table contacts every shard even at LIMIT 0, so the assertion
// covers the grant on each node rather than one row on the coordinator.
func TestTableReadableReachesEveryShard(t *testing.T) {
	q := openQuerier(t, shardedSource(t, "otel_traces_shards"))

	got := q.Privileges(context.Background(), []string{lint.AssertionTableReadable})

	if len(got) != 1 {
		t.Fatalf("assertions = %v, want one", got)
	}
	if got[0].Status != StatusPass {
		t.Errorf("status = %s (%s), want pass: the user is granted the Distributed table",
			got[0].Status, got[0].Detail)
	}
}

// A shard that cannot be reached leaves the question open rather than answered.
// Reporting a missing grant would send an operator to fix an access problem
// that is an outage, and reporting a pass would claim a table was readable on a
// node nothing could reach.
func TestTableReadableIsInconclusiveWhenAShardIsUnreachable(t *testing.T) {
	q := openQuerier(t, shardedSource(t, "otel_traces_dead_shard"))

	got := q.Privileges(context.Background(), []string{lint.AssertionTableReadable})

	if len(got) != 1 {
		t.Fatalf("assertions = %v, want one", got)
	}
	if got[0].Status != StatusInconclusive {
		t.Errorf("status = %s (%s), want inconclusive", got[0].Status, got[0].Detail)
	}
	if !strings.Contains(got[0].Detail, deadShardHost) {
		t.Errorf("detail = %q, want it to name the unreachable shard %q", got[0].Detail, deadShardHost)
	}
}

// The retention caveat on a sharded cluster, which is the question a Distributed
// table has no answer to: its engine clause carries no TTL, so asked naively the
// caveat disappears and a replay reaching past the retention reads as clean.
func TestBackfillTTLCaveatResolvesTheLocalTable(t *testing.T) {
	src := shardedSource(t, "otel_traces_shards")
	q := openQuerier(t, src)

	seedAt(t, src.Address, []span{{at: anchor.Add(-time.Minute), service: "checkout", duration: 1}})
	seedAt(t, shardTwoAddress(t), []span{{at: anchor.Add(-time.Minute), service: "other", duration: 2}})

	// The schema in deploy/clickhouse/init TTLs at three days, on the local
	// table, which is the one the caveat has to find.
	_, findings := backfill(t, q, shardedRule("otel_traces_shards"), BackfillChecks{
		Range: 96 * time.Hour,
		Step:  48 * time.Hour,
	})

	if len(findings) != 1 {
		t.Fatalf("findings = %v, want the TTL reported", findings)
	}
	for _, want := range []string{
		"TTL on otel.otel_traces",
		"the local table behind the Distributed otel.otel_traces_shards",
		"a shard whose retention differs is not covered",
	} {
		if !strings.Contains(findings[0].Detail, want) {
			t.Errorf("detail = %q, missing %q", findings[0].Detail, want)
		}
	}
}

// How many shards the source's table fans a query out across, read off the
// cluster definition rather than written into a test.
//
// The dead shard cluster is counted too, and that is the point of asserting it:
// `system.clusters` is a definition, so the count survives the node behind it
// being gone. A prediction scaled by two on a cluster that answers from one is
// the right number for the cluster the rule is pointed at, and the evaluation
// fails on the missing shard anyway.
func TestCountShardsReadsTheClusterDefinition(t *testing.T) {
	cases := []struct {
		name string
		src  source.Source
		want fanout
	}{
		{
			name: "both shards",
			src:  shardedSource(t, "otel_traces_shards"),
			want: fanout{Shards: 2, Counted: true},
		},
		{
			name: "a shard nothing answers on is still a shard",
			src:  shardedSource(t, "otel_traces_dead_shard"),
			want: fanout{Shards: 2, Counted: true},
		},
		{
			// A local table is its own single shard, so nothing is asked of
			// system.clusters and a single node source never needs the grant.
			name: "a local table",
			src:  testSource(t),
			want: fanout{Shards: 1, Counted: true},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := openQuerier(t, c.src).countShards(context.Background()); got != c.want {
				t.Errorf("countShards = %+v, want %+v", got, c.want)
			}
		})
	}
}

// The predicted cost of a sharded rule is the cluster's, measured against the
// coordinator's own answer to the same question.
//
// Both numbers come from the same server on the same rows, so the assertion is
// the arithmetic and nothing else: `EXPLAIN ESTIMATE` over the Distributed table
// answers for the parts on the node it was asked, which is what the local table
// answers on its own, and the cluster's number is that times the two shards the
// definition has (spec 6.9).
func TestCostScalesToTheCluster(t *testing.T) {
	sharded := openQuerier(t, shardedSource(t, "otel_traces_shards"))
	local := openQuerier(t, testSource(t))

	seedManySpans(t, local)
	seedManySpansAt(t, shardTwoAddress(t))

	onNode := estimatedCost(t, local, shardedCostRule("otel_traces"))
	onCluster := estimatedCost(t, sharded, shardedCostRule("otel_traces_shards"))

	if onNode.Rows == 0 {
		t.Fatalf("the coordinator estimated %+v, want rows to scale", onNode)
	}
	if onCluster.Shards != 2 {
		t.Errorf("shards = %d, want 2: the number says what it was multiplied by", onCluster.Shards)
	}
	if want := onNode.Rows * 2; onCluster.Rows != want {
		t.Errorf("rows = %d, want %d: the coordinator's %d parts across two shards",
			onCluster.Rows, want, onNode.Rows)
	}
}

// A finding on a sharded rule says the number is a multiplication, because a
// reader who takes it for a measurement acts on a precision it does not have.
func TestCostFindingSaysItWasScaled(t *testing.T) {
	q := openQuerier(t, shardedSource(t, "otel_traces_shards"))

	seedManySpans(t, openQuerier(t, testSource(t)))
	seedManySpansAt(t, shardTwoAddress(t))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	got, err := q.Inspect(ctx, shardedCostRule("otel_traces_shards"), testGroup,
		costChecks(100, 1_000_000, time.Minute))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(got.Findings) != 1 || got.Findings[0].Check != lint.CheckRuleCost {
		t.Fatalf("findings = %v, want only %s", got.Findings, lint.CheckRuleCost)
	}
	for _, want := range []string{"2 shards", "connected to"} {
		if !strings.Contains(got.Findings[0].Detail, want) {
			t.Errorf("detail = %q, want it to carry %q", got.Findings[0].Detail, want)
		}
	}
}

// A user that cannot count the cluster is told its cost was not estimated,
// rather than having one shard's number compared against the cluster's ceiling.
//
// This is the fallback, and what it costs is visible here: the ceiling is one
// hundred rows, the rows are there to exceed it, and no finding is reported
// because the number the server handed back was never comparable to it. Wrong in
// the other direction is worse, because it passes a rule reading twice what the
// operator allowed and says nothing (spec 6.9).
func TestCostIsUnestimatedWithoutTheClusterCount(t *testing.T) {
	src := shardedSource(t, "otel_traces_shards")
	src.Username = "ruler_uncounted_shards"
	q := openQuerier(t, src)

	seedManySpans(t, openQuerier(t, testSource(t)))
	seedManySpansAt(t, shardTwoAddress(t))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	got, err := q.Inspect(ctx, shardedCostRule("otel_traces_shards"), testGroup,
		costChecks(100, 1_000_000, time.Minute))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got.Cost == nil {
		t.Fatal("cost = nil, want the estimate reported as unscaled")
	}
	if got.Cost.Status != CostShardsUnknown {
		t.Errorf("cost = %+v, want the shard count reported unreadable", *got.Cost)
	}
	if len(got.Findings) != 0 {
		t.Errorf("findings = %v, want none: no ceiling applies to a number covering one shard of "+
			"an unknown count", got.Findings)
	}
}

// estimatedCost asks one source what a rule reads every time it evaluates.
func estimatedCost(t *testing.T, q *Querier, r rule.Rule) CostEstimate {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c := describeChecks()
	c.ReportCost = true

	got, err := q.Inspect(ctx, r, testGroup, c)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got.Cost == nil {
		t.Fatal("cost = nil, want an estimate")
	}
	if got.Cost.Status != CostEstimated {
		t.Fatalf("cost = %+v, want an estimate the optimiser made", *got.Cost)
	}
	return *got.Cost
}
