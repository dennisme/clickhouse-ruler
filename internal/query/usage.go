package query

import (
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// peakMemoryEvent is the profile event carrying how much memory a query
// reached. ClickHouse sends it as a gauge, once per thread per batch, so the
// query's cost is the highest value any of them reported.
const peakMemoryEvent = "MemoryTrackerPeakUsage"

// Usage is what one query actually cost the cluster (spec 8.2). Cost, above,
// is the ceiling it was measured against.
//
// Read from the driver's callbacks during the query rather than from
// system.query_log afterwards: the numbers arrive with the result, so there
// is no follow-up query and no dependency on how long that table is kept.
type Usage struct {
	ReadRows  uint64
	ReadBytes uint64

	// PeakMemory is the highest any thread serving this query reached, which
	// is the number the memory cap in 6.7 is compared against.
	PeakMemory uint64

	// Duration is what the ruler waited, from sending the query to the last
	// row arriving. Longer than what ClickHouse spent, and the one that
	// turns into evaluation duration and then into a missed iteration.
	Duration time.Duration
}

// Recorder is told what a rule's query cost, once per evaluation.
//
// An interface declared here and satisfied by the caller, because
// internal/query has no business knowing about a metrics registry and the
// registry has no business knowing about a driver callback.
type Recorder interface {
	QueryCost(rule, team string, u Usage)
}

// usage accumulates one query's callbacks.
//
// The driver calls these from the goroutine reading the connection while the
// caller iterates rows on its own, so the lock is not decoration.
type meter struct {
	mu    sync.Mutex
	usage Usage
}

// progress adds one packet.
//
// Each packet carries what that block read, not the running total, so these
// are added.
func (m *meter) progress(p *clickhouse.Progress) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.usage.ReadRows += p.Rows
	m.usage.ReadBytes += p.Bytes
}

// profileEvents reads the memory gauge out of a batch of events.
func (m *meter) profileEvents(events []clickhouse.ProfileEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, e := range events {
		if e.Name != peakMemoryEvent || e.Value < 0 {
			continue
		}
		if peak := uint64(e.Value); peak > m.usage.PeakMemory {
			m.usage.PeakMemory = peak
		}
	}
}

// usage is what arrived, which is everything the query read whether or not
// it finished: rows read before a cap threw cost the cluster the same as
// rows read by a query that returned.
func (m *meter) read() Usage {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.usage
}
