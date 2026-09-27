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
// Read here rather than added to testSource: a source is one address today
// (spec 6.9 wants a list, a later slice), and the only reason a test needs the
// second node directly is to seed it. Distributed queries reach it through the
// cluster definition, not through this.
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

func shardedRule(table string) rule.Rule {
	return rule.Rule{
		Alert:  "MaxLatency",
		Expr:   fmt.Sprintf(shardedLatencyExpr, table),
		Window: 5 * time.Minute,
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
