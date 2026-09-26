package query

import (
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// The trap this test exists for. ClickHouse sends a progress packet per block
// and each one carries what that block read, not the running total. Treating
// them as totals reports the last block; adding them to a total that was
// already a total reports something closer to the square.
func TestUsageAddsProgressDeltas(t *testing.T) {
	var m meter

	m.progress(&clickhouse.Progress{Rows: 2354724, Bytes: 18837792})
	m.progress(&clickhouse.Progress{Rows: 645276, Bytes: 5162208})
	m.progress(&clickhouse.Progress{})

	got := m.read()
	if got.ReadRows != 3000000 {
		t.Errorf("ReadRows = %d, want 3000000", got.ReadRows)
	}
	if got.ReadBytes != 24000000 {
		t.Errorf("ReadBytes = %d, want 24000000", got.ReadBytes)
	}
}

// Memory is a gauge, reported per thread, so the query's cost is the highest
// any thread reached and never the sum of them.
func TestUsageTakesPeakMemoryRatherThanSummingIt(t *testing.T) {
	var m meter

	m.profileEvents([]clickhouse.ProfileEvent{
		{Name: "SelectedRows", Value: 2420133, Type: "increment"},
		{Name: "MemoryTrackerUsage", Value: 8390608, Type: "gauge"},
		{Name: "MemoryTrackerPeakUsage", Value: 8391248, Type: "gauge"},
	})
	m.profileEvents([]clickhouse.ProfileEvent{
		{Name: "MemoryTrackerPeakUsage", Value: 9904984, Type: "gauge"},
		{Name: "MemoryTrackerPeakUsage", Value: 5190272, Type: "gauge"},
	})

	if got := m.read().PeakMemory; got != 9904984 {
		t.Errorf("PeakMemory = %d, want 9904984, the highest any thread reached", got)
	}
}

// A negative value would be the server telling us something we have no way to
// read, and a memory gauge is never negative. Dropping it beats recording a
// number that underflows the counter it lands in.
func TestUsageIgnoresANegativeMemoryEvent(t *testing.T) {
	var m meter

	m.profileEvents([]clickhouse.ProfileEvent{{Name: "MemoryTrackerPeakUsage", Value: -1, Type: "gauge"}})

	if got := m.read().PeakMemory; got != 0 {
		t.Errorf("PeakMemory = %d, want 0", got)
	}
}

// A query that failed still read rows before it died, and they cost the
// cluster exactly what a successful query's do. Whatever arrived before the
// error is what gets recorded.
func TestUsageKeepsWhatArrivedBeforeAFailure(t *testing.T) {
	var m meter

	m.progress(&clickhouse.Progress{Rows: 1000, Bytes: 8000})

	if got := m.read(); got.ReadRows != 1000 || got.ReadBytes != 8000 {
		t.Errorf("cost() = %+v, want the 1000 rows read before the failure", got)
	}
}
