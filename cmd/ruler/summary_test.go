package main

import (
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/query"
)

// Every cell that is not a number says why it is not one. A refused estimate
// printed as 0 is the one reading nobody can act on (spec 7.10).
func TestCostCell(t *testing.T) {
	cases := []struct {
		name string
		cost *query.CostEstimate
		want string
	}{
		{"estimated", &query.CostEstimate{Rows: 4100000, Status: query.CostEstimated}, "4100000"},
		{"no parts", &query.CostEstimate{Status: query.CostNoParts}, "not estimated: the server predicts no part will be read"},
		{"refused", &query.CostEstimate{Status: query.CostRefused}, "not estimated: this source's user cannot read what the rule asks for"},
		{"unreachable", nil, "not estimated: the source could not be read"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := costCell(tc.cost); got != tc.want {
				t.Errorf("costCell = %q, want %q", got, tc.want)
			}
		})
	}
}

// A group that set no interval has no rate, and "0s" would read as one that
// evaluates constantly.
func TestIntervalCell(t *testing.T) {
	if got := intervalCell(0); got != "not set" {
		t.Errorf("intervalCell(0) = %q, want %q", got, "not set")
	}
	if got := intervalCell(30_000_000_000); got != "30s" {
		t.Errorf("intervalCell(30s) = %q, want %q", got, "30s")
	}
}
