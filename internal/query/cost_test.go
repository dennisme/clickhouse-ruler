package query

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readEstimate reads a captured `EXPLAIN ESTIMATE` result: one line per table,
// database, table, parts, rows and marks. The driver decodes these as rows in
// production, so the fixture is here for the shape of a real answer rather
// than to exercise a second decoder.
func readEstimate(t *testing.T, name string) []Estimate {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}

	var out []Estimate
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 5 {
			t.Fatalf("fixture %s line %q has %d fields, want 5", name, line, len(fields))
		}
		out = append(out, Estimate{
			Database: fields[0],
			Table:    fields[1],
			Parts:    atoiOrFail(t, fields[2]),
			Rows:     atoiOrFail(t, fields[3]),
			Marks:    atoiOrFail(t, fields[4]),
		})
	}
	return out
}

func atoiOrFail(t *testing.T, s string) uint64 {
	t.Helper()

	var v uint64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		t.Fatalf("fixture field %q is not a number: %v", s, err)
	}
	return v
}

func readPlan(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return string(data)
}

// A rule reading two tables costs what both of them cost, so the estimate is
// summed rather than taken from whichever row came first.
func TestEstimatedRows(t *testing.T) {
	if got := estimatedRows(readEstimate(t, "estimate_bounded.txt")); got != 126272 {
		t.Errorf("rows = %d, want 126272", got)
	}
	if got := estimatedRows(readEstimate(t, "estimate_two_tables.txt")); got != 201000 {
		t.Errorf("rows = %d, want both tables summed", got)
	}
	if got := estimatedRows(nil); got != 0 {
		t.Errorf("rows = %d, want 0 when the server estimated nothing", got)
	}
}

// The rate is what the ceiling is really about: the same query is cheap
// hourly and ruinous every fifteen seconds.
func TestRowsPerSecond(t *testing.T) {
	if got := RowsPerSecond(120000, 30*time.Second); got != 4000 {
		t.Errorf("rate = %v, want 4000", got)
	}
	// No interval is a rule whose group did not set one. Nothing to divide
	// by, so the rate ceiling does not apply rather than dividing by zero.
	if got := RowsPerSecond(120000, 0); got != 0 {
		t.Errorf("rate = %v, want 0 when no interval is known", got)
	}
}

// Granules selected equal to granules total means the primary key excluded
// nothing, which is usually why an estimate is large.
func TestUnprunedTables(t *testing.T) {
	if got := unprunedTables(readPlan(t, "plan_unpruned.txt")); len(got) != 1 || got[0] != "otel.otel_traces" {
		t.Errorf("got %v, want otel.otel_traces: the key pruned nothing", got)
	}
	if got := unprunedTables(readPlan(t, "plan_pruned.txt")); len(got) != 0 {
		t.Errorf("got %v, want none: 16 granules of 25 were selected", got)
	}
}

func TestOverCost(t *testing.T) {
	est := readEstimate(t, "estimate_bounded.txt")

	tests := []struct {
		name     string
		cost     *Cost
		interval time.Duration
		want     bool
	}{
		{name: "no ceiling configured", cost: nil, want: false},
		{
			name:     "under both",
			cost:     &Cost{MaxRows: 1_000_000, MaxRowsPerSecond: 1_000_000},
			interval: time.Minute,
		},
		{
			name: "over the row ceiling",
			cost: &Cost{MaxRows: 1000, MaxRowsPerSecond: 1_000_000},
			want: true,
		},
		{
			// The row count is fine; running it every second is not.
			name:     "over the rate ceiling alone",
			cost:     &Cost{MaxRows: 1_000_000, MaxRowsPerSecond: 1000},
			interval: time.Second,
			want:     true,
		},
		{
			// Same query, same ceilings, an interval that makes it affordable.
			name:     "the interval is what saves it",
			cost:     &Cost{MaxRows: 1_000_000, MaxRowsPerSecond: 1000},
			interval: time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cost := costFrom(est, fanout{Shards: 1, Counted: true})
			if got := overCost(cost, tt.cost, tt.interval) != ""; got != tt.want {
				t.Errorf("overCost = %v, want %v", got, tt.want)
			}
		})
	}
}

// The numbers an author acts on: what it reads, how often, and what that
// comes to per second.
func TestOverCostDetail(t *testing.T) {
	cost := costFrom(readEstimate(t, "estimate_bounded.txt"), fanout{Shards: 1, Counted: true})
	got := overCost(cost, &Cost{MaxRows: 1000, MaxRowsPerSecond: 10}, 30*time.Second)

	for _, want := range []string{"126272", "30s", "estimate"} {
		if !contains(got, want) {
			t.Errorf("detail = %q, want it to carry %q", got, want)
		}
	}
}

// The plan explains the estimate rather than replacing it, so the pruning
// note is appended to the finding the ceiling produced.
func TestWithPruning(t *testing.T) {
	detail := "the query is estimated to read 200000 rows against a ceiling of 1000"

	got := withPruning(detail, []string{"otel.otel_traces"})
	if !contains(got, detail) {
		t.Errorf("detail = %q, want it to keep what the ceiling said", got)
	}
	for _, want := range []string{"otel.otel_traces", "primary key"} {
		if !contains(got, want) {
			t.Errorf("detail = %q, want it to carry %q", got, want)
		}
	}

	// A rule whose key did prune is expensive for some other reason, and
	// saying nothing is better than inventing a cause.
	if got := withPruning(detail, nil); got != detail {
		t.Errorf("detail = %q, want it unchanged when every table pruned", got)
	}
}

// The same answer from the tree-drawn plan a newer server prints. `EXPLAIN PLAN`
// output has no stability guarantee, and 26.9 wraps it in box-drawing glyphs
// with extra lines per step, so a reader anchored on the bare step name stops
// finding the table and the finding quietly loses its explanation (spec 7.2).
func TestUnprunedTablesReadsATreeDrawnPlan(t *testing.T) {
	got := unprunedTables(readPlan(t, "plan_unpruned_tree.txt"))

	if len(got) != 1 || got[0] != "otel.otel_traces" {
		t.Errorf("got %v, want otel.otel_traces: the key pruned nothing", got)
	}
}

// The table on a pull request wants a number for a cheap rule too, so the
// estimate is carried out of the inspection whether or not a ceiling was
// exceeded (spec 7.10).
func TestCostFrom(t *testing.T) {
	est := readEstimate(t, "estimate_two_tables.txt")

	got := costFrom(est, fanout{Shards: 1, Counted: true})
	if got.Status != CostEstimated {
		t.Errorf("status = %v, want estimated", got.Status)
	}
	if want := estimatedRows(est); got.Rows != want {
		t.Errorf("rows = %d, want %d", got.Rows, want)
	}
}

// A server that estimated nothing predicts no part will be read: an empty
// table, a window everything pruned out of, or a count answered from
// metadata. Zero rows would read as a measurement of a free query rather than
// the absence of one.
func TestCostFromWithNoParts(t *testing.T) {
	if got := costFrom(nil, fanout{Shards: 1, Counted: true}); got.Status != CostNoParts {
		t.Errorf("status = %v, want no parts", got.Status)
	}
}

// A sharded rule's cost is the cluster's, not the coordinator's. EXPLAIN
// ESTIMATE answers for the parts on the node it was asked, so an unscaled
// number on two shards is half of what every evaluation reads (spec 6.9).
func TestCostFromScalesByTheShardCount(t *testing.T) {
	est := readEstimate(t, "estimate_bounded.txt")

	got := costFrom(est, fanout{Shards: 3, Counted: true})

	if got.Status != CostEstimated {
		t.Errorf("status = %v, want estimated", got.Status)
	}
	if want := estimatedRows(est) * 3; got.Rows != want {
		t.Errorf("rows = %d, want %d: one shard's estimate times the shard count", got.Rows, want)
	}
	if got.Shards != 3 {
		t.Errorf("shards = %d, want 3: the number says how it was arrived at", got.Shards)
	}
}

// A shard count nobody could read turns the ceiling off rather than comparing a
// shard's number against a cluster's, which is the reading that lets a rule
// through (spec 6.9).
func TestCostFromWithAnUncountedFanout(t *testing.T) {
	got := costFrom(readEstimate(t, "estimate_bounded.txt"), fanout{})

	if got.Status != CostShardsUnknown {
		t.Errorf("status = %v, want the shard count reported unreadable", got.Status)
	}
	if got.Rows != 0 {
		t.Errorf("rows = %d, want none: a number here would be one shard's read as a cluster's", got.Rows)
	}
}

// Multiplication that wrapped would report the largest read there is as a small
// one, which is the one rule this check must not let through.
func TestCostFromSaturatesRatherThanWrapping(t *testing.T) {
	huge := []Estimate{{Database: "otel", Table: "otel_traces", Rows: math.MaxUint64 / 2}}

	got := costFrom(huge, fanout{Shards: 4, Counted: true})

	if got.Rows != math.MaxUint64 {
		t.Errorf("rows = %d, want %d: the product does not fit and the ceiling is exceeded either way",
			got.Rows, uint64(math.MaxUint64))
	}
}

// The finding says the number is a multiplication, because a reader who takes a
// scaled prediction for a measured one acts on a precision it does not have.
func TestOverCostSaysTheNumberWasScaled(t *testing.T) {
	cost := costFrom(readEstimate(t, "estimate_bounded.txt"), fanout{Shards: 2, Counted: true})

	got := overCost(cost, &Cost{MaxRows: 1000, MaxRowsPerSecond: 10}, 30*time.Second)

	for _, want := range []string{"252544", "2 shards", "connected to"} {
		if !contains(got, want) {
			t.Errorf("detail = %q, want it to carry %q", got, want)
		}
	}
}

// A single node source says nothing about shards. There is one, the estimate is
// the whole answer, and a sentence about fanout would be noise on every rule.
func TestOverCostIsSilentAboutOneShard(t *testing.T) {
	cost := costFrom(readEstimate(t, "estimate_bounded.txt"), fanout{Shards: 1, Counted: true})

	got := overCost(cost, &Cost{MaxRows: 1000, MaxRowsPerSecond: 10}, 30*time.Second)

	if contains(got, "shard") {
		t.Errorf("detail = %q, want no mention of shards on a single node source", got)
	}
}

// No count, no ceiling. Reporting the breach of a ceiling the number was never
// comparable to is the false finding the fallback exists to avoid (spec 6.9).
func TestOverCostAppliesNoCeilingWithoutAShardCount(t *testing.T) {
	cost := costFrom(readEstimate(t, "estimate_bounded.txt"), fanout{})

	if got := overCost(cost, &Cost{MaxRows: 1, MaxRowsPerSecond: 1}, time.Second); got != "" {
		t.Errorf("detail = %q, want nothing: the estimate covers one shard of an unknown number", got)
	}
}
