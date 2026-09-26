package query

import (
	"fmt"
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
	if got := rowsPerSecond(120000, 30*time.Second); got != 4000 {
		t.Errorf("rate = %v, want 4000", got)
	}
	// No interval is a rule whose group did not set one. Nothing to divide
	// by, so the rate ceiling does not apply rather than dividing by zero.
	if got := rowsPerSecond(120000, 0); got != 0 {
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
			if got := overCost(est, tt.cost, tt.interval) != ""; got != tt.want {
				t.Errorf("overCost = %v, want %v", got, tt.want)
			}
		})
	}
}

// The numbers an author acts on: what it reads, how often, and what that
// comes to per second.
func TestOverCostDetail(t *testing.T) {
	got := overCost(readEstimate(t, "estimate_bounded.txt"),
		&Cost{MaxRows: 1000, MaxRowsPerSecond: 10}, 30*time.Second)

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

	got := costFrom(est)
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
	if got := costFrom(nil); got.Status != CostNoParts {
		t.Errorf("status = %v, want no parts", got.Status)
	}
}
