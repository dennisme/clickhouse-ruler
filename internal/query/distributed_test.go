package query

import (
	"strings"
	"testing"
	"time"
)

func TestDistributedTargetReadsTheEngineArguments(t *testing.T) {
	cases := []struct {
		name       string
		engineFull string
		want       tableRef
		wantOK     bool
	}{
		{
			name:       "sharding key",
			engineFull: "Distributed('ruler_shards', 'otel', 'otel_traces', rand())",
			want:       tableRef{Database: "otel", Table: "otel_traces"},
			wantOK:     true,
		},
		{
			// ClickHouse stores what the author wrote after normalising it, so
			// a table created with bare identifiers and currentDatabase() comes
			// back as three quoted literals with the database resolved. This is
			// that form, which is why nothing here has to parse an expression.
			name:       "no sharding key",
			engineFull: "Distributed('ruler_shards', 'otel', 'otel_traces')",
			want:       tableRef{Database: "otel", Table: "otel_traces"},
			wantOK:     true,
		},
		{
			name:       "policy name after the sharding key",
			engineFull: "Distributed('ruler_shards', 'otel', 'otel_traces', rand(), 'policy')",
			want:       tableRef{Database: "otel", Table: "otel_traces"},
			wantOK:     true,
		},
		{
			// A quote inside a name arrives backslash escaped, which is how
			// ClickHouse writes a string literal, and the name it stands for is
			// the one the next query has to ask about.
			name:       "quote inside a name",
			engineFull: `Distributed('ruler_shards', 'otel', 'odd\'name', rand())`,
			want:       tableRef{Database: "otel", Table: "odd'name"},
			wantOK:     true,
		},
		{
			name:       "a local table is not distributed",
			engineFull: "MergeTree PARTITION BY toDate(Timestamp) ORDER BY ServiceName",
		},
		{
			// Two arguments is not a Distributed table anybody can read, and
			// guessing which one is the table would send the next query at the
			// cluster name.
			name:       "too few arguments",
			engineFull: "Distributed('ruler_shards', 'otel')",
		},
		{
			name:       "an argument that is not a literal",
			engineFull: "Distributed('ruler_shards', currentDatabase(), 'otel_traces')",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cluster, got, ok := distributedTarget(c.engineFull)
			if ok != c.wantOK {
				t.Fatalf("distributedTarget(%q) ok = %v, want %v", c.engineFull, ok, c.wantOK)
			}
			if !ok {
				return
			}
			if got != c.want {
				t.Errorf("distributedTarget(%q) = %+v, want %+v", c.engineFull, got, c.want)
			}
			// The cluster is read for the same reason the table is: the shard
			// count behind a cost comes from `system.clusters`, and the only
			// place the cluster's name is written down is this engine clause
			// (spec 6.9).
			if want := "ruler_shards"; cluster != want {
				t.Errorf("distributedTarget(%q) cluster = %q, want %q", c.engineFull, cluster, want)
			}
		})
	}
}

// What the caveats say about which table answered. A Distributed table's parts
// live on the shards, so both caveats read this node's local copy, and a reader
// who is not told that reads a one shard answer as a cluster's (spec 6.9, 7.4).
func TestCaveatsNameTheTableTheyRead(t *testing.T) {
	local := storage{
		Source: tableRef{Database: "otel", Table: "otel_traces"},
		Local:  tableRef{Database: "otel", Table: "otel_traces"},
	}
	sharded := storage{
		Source:      tableRef{Database: "otel", Table: "otel_traces_shards"},
		Local:       tableRef{Database: "otel", Table: "otel_traces"},
		Distributed: true,
	}

	got := ttlCaveat(72*time.Hour, local)
	for _, want := range []string{"72h0m0s TTL on otel.otel_traces", "already deleted"} {
		if !strings.Contains(got, want) {
			t.Errorf("ttlCaveat on a local table = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "shard") {
		t.Errorf("ttlCaveat on a local table = %q, want nothing about shards", got)
	}

	got = ttlCaveat(72*time.Hour, sharded)
	for _, want := range []string{
		"72h0m0s TTL on otel.otel_traces",
		"otel.otel_traces_shards",
		"shard whose retention differs is not covered",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ttlCaveat on a Distributed table = %q, missing %q", got, want)
		}
	}

	got = columnCaveat(columnAddition{Column: "StatusCode", Earliest: time.Unix(0, 0).UTC()}, sharded)
	for _, want := range []string{"StatusCode", "otel.otel_traces", "another shard is not covered"} {
		if !strings.Contains(got, want) {
			t.Errorf("columnCaveat on a Distributed table = %q, missing %q", got, want)
		}
	}
}

// A Distributed table nothing can resolve a local table behind says so. Silence
// is what a replay with nothing to qualify looks like, so a caveat that stopped
// appearing would read as a clean answer (spec 6.9).
func TestUnresolvedStorageSaysSo(t *testing.T) {
	got := unresolvedCaveat(storage{
		Source:       tableRef{Database: "otel", Table: "otel_traces_shards"},
		Distributed:  true,
		Unanswerable: "its engine clause does not name a local table this can read",
	})

	for _, want := range []string{
		"otel.otel_traces_shards",
		"does not name a local table",
		"retention",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unresolvedCaveat = %q, missing %q", got, want)
		}
	}
}
