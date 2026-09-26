//go:build integration

package query

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
)

// recorded is one call to the Recorder, kept so a test can assert on what a
// query reported rather than on a metric it would have to scrape back.
type recorded struct {
	rule  string
	team  string
	usage Usage
}

type captureRecorder struct {
	mu   sync.Mutex
	took []recorded
}

func (c *captureRecorder) QueryCost(rule, team string, u Usage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.took = append(c.took, recorded{rule: rule, team: team, usage: u})
}

func (c *captureRecorder) calls() []recorded {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recorded(nil), c.took...)
}

// recordingQuerier is openQuerier with somewhere for the cost to go.
func recordingQuerier(t *testing.T) (*Querier, *captureRecorder) {
	t.Helper()

	rec := &captureRecorder{}
	q, err := Open(testSource(t), rec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q, rec
}

// serverCost is what ClickHouse itself recorded for a rule's last query,
// found through the log_comment from spec 8.5.
func serverCost(t *testing.T, address, ruleName string) (readRows, readBytes, memory uint64) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := adminConn(t, address)
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flushing the query log: %v", err)
	}

	err := conn.QueryRow(ctx, `
		SELECT read_rows, read_bytes, toUInt64(greatest(memory_usage, 0))
		FROM system.query_log
		WHERE type = 'QueryFinish'
		  AND event_time >= now() - INTERVAL 1 HOUR
		  AND JSONExtractString(log_comment, 'rule') = ?
		ORDER BY event_time DESC
		LIMIT 1`, ruleName).Scan(&readRows, &readBytes, &memory)
	if err != nil {
		t.Fatalf("reading system.query_log: %v", err)
	}
	return readRows, readBytes, memory
}

// The claim spec 8.2 makes is that the driver's callbacks can replace a
// follow-up query against system.query_log. Two independent accounts of one
// query, which is the only way to know the cheaper one is right.
func TestRecordedCostAgreesWithTheQueryLog(t *testing.T) {
	q, rec := recordingQuerier(t)

	// Enough rows that the server reads more than one block and sends more
	// than one progress packet, which is where treating a delta as a total
	// would show up as a number far too small.
	spans := make([]span, 0, 500)
	for i := range 500 {
		spans = append(spans, span{at: anchor.Add(-time.Duration(i) * time.Second), service: "checkout", duration: uint64(i)})
	}
	seed(t, q, spans)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r := rule.Rule{
		Alert:  "CostedRule",
		Expr:   latencyExpr,
		Window: time.Hour,
	}
	if _, err := q.Run(ctx, r, Attribution{Group: "rules/payments.yaml:latency", Team: "payments"}, anchor); err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := rec.calls()
	if len(calls) != 1 {
		t.Fatalf("recorder was called %d times, want once per evaluation", len(calls))
	}
	got := calls[0]

	if got.rule != "CostedRule" || got.team != "payments" {
		t.Errorf("recorded rule=%q team=%q, want CostedRule and payments", got.rule, got.team)
	}

	rows, bytes, memory := serverCost(t, q.src.Address, "CostedRule")

	if got.usage.ReadRows != rows {
		t.Errorf("read rows: driver says %d, the server recorded %d", got.usage.ReadRows, rows)
	}
	if got.usage.ReadBytes != bytes {
		t.Errorf("read bytes: driver says %d, the server recorded %d", got.usage.ReadBytes, bytes)
	}
	if got.usage.ReadRows == 0 {
		t.Error("read rows is zero over a window with 500 seeded rows in it")
	}

	// Peak memory is sampled per thread as the query runs, so the two will
	// not agree to the byte. Same order of magnitude is the claim.
	if got.usage.PeakMemory == 0 {
		t.Error("no peak memory reported")
	}
	if memory > 0 && (got.usage.PeakMemory > memory*4 || memory > got.usage.PeakMemory*4) {
		t.Errorf("peak memory: driver says %d, the server recorded %d, which is not the same query",
			got.usage.PeakMemory, memory)
	}

	if got.usage.Duration <= 0 {
		t.Errorf("duration = %s, want the time the ruler waited", got.usage.Duration)
	}
}

// A failed evaluation is still recorded, because a rule that trips a cap is
// exactly the rule an operator is looking for and one that reported nothing
// would be invisible.
//
// What it reports is whatever arrived before the failure, which here is
// nothing: the server refuses the result before it sends a progress packet,
// so the row count is zero and only the duration is real. A query that runs
// long enough to send progress and then throws reports what it read. Both
// are the same rule, which is why the assertion is on the call rather than
// on the numbers.
func TestAFailedQueryStillReportsWhatItRead(t *testing.T) {
	q, rec := recordingQuerier(t)

	// Two rows over the source's max_rows, which is one past the
	// max_result_rows the ruler sends, so ClickHouse itself throws the
	// result overflow the caps in 6.7 configure rather than the client
	// catching it after the fact.
	spans := make([]span, 0, q.src.MaxRows+2)
	for i := range q.src.MaxRows + 2 {
		// From the second before the anchor, because the window is half
		// open and a row exactly on it belongs to the next evaluation.
		spans = append(spans, span{
			at:       anchor.Add(-time.Duration(i+1) * time.Second),
			service:  fmt.Sprintf("svc%04d", i),
			duration: uint64(i),
		})
	}
	seed(t, q, spans)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r := rule.Rule{Alert: "OverflowingRule", Expr: latencyExpr, Window: 2 * time.Hour}
	if _, err := q.Run(ctx, r, Attribution{Group: "g.yaml:g", Team: "payments"}, anchor); err == nil {
		t.Fatal("want the result overflow to fail the evaluation")
	}

	calls := rec.calls()
	if len(calls) != 1 {
		t.Fatalf("recorder was called %d times for a failed query, want once", len(calls))
	}
	if calls[0].usage.Duration <= 0 {
		t.Error("a failed query reported no duration")
	}
}
