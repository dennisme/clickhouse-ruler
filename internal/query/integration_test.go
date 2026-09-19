//go:build integration

package query

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// anchor is the fixed point the seeded rows are placed around. Using a fixed
// instant rather than time.Now keeps the assertions exact.
var anchor = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

func testSource(t *testing.T) source.Source {
	t.Helper()

	address := os.Getenv("RULER_CLICKHOUSE_ADDR")
	if address == "" {
		t.Fatal("RULER_CLICKHOUSE_ADDR is not set, run these through `just integration`")
	}
	// Credentials match compose.yaml. They are dev-only and the compose stack
	// is bound to localhost.
	return source.Source{
		Name:             "otel_traces",
		Address:          address,
		Database:         "otel",
		Username:         "ruler",
		Password:         "ruler",
		Table:            "otel_traces",
		TimestampColumn:  "Timestamp",
		EvaluationDelay:  0,
		MaxRows:          source.DefaultMaxRows,
		MaxExecutionTime: source.DefaultMaxExecutionTime,
		MaxMemoryUsage:   source.DefaultMaxMemoryUsage,
	}
}

// openQuerier connects and gives each test its own table so the tests do not
// interfere with one another.
func openQuerier(t *testing.T, src source.Source) *Querier {
	t.Helper()

	q, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { q.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := q.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v, is the stack up? run `just compose-up`", err)
	}
	return q
}

type span struct {
	at       time.Time
	service  string
	duration uint64
}

func seed(t *testing.T, q *Querier, spans []span) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := q.conn.Exec(ctx, "TRUNCATE TABLE otel.otel_traces"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	batch, err := q.conn.PrepareBatch(ctx,
		"INSERT INTO otel.otel_traces (Timestamp, TraceId, SpanId, ServiceName, SpanName, Duration, StatusCode, SpanAttributes)")
	if err != nil {
		t.Fatalf("prepare batch: %v", err)
	}
	for i, s := range spans {
		err := batch.Append(
			s.at, "trace", "span", s.service, "GET /", s.duration, "Ok",
			map[string]string{"index": string(rune('a' + i))},
		)
		if err != nil {
			t.Fatalf("append row %d: %v", i, err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send batch: %v", err)
	}
}

func run(t *testing.T, q *Querier, r rule.Rule, now time.Time) []alertSample {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := q.Run(ctx, r, now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	out := make([]alertSample, 0, len(got))
	for _, s := range got {
		out = append(out, alertSample{service: s.Labels["ServiceName"], value: s.Value})
	}
	return out
}

type alertSample struct {
	service string
	value   float64
}

const latencyExpr = `
SELECT ServiceName, max(Duration) AS value
FROM otel.otel_traces
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
GROUP BY ServiceName
ORDER BY ServiceName`

// The bounds are half open: From is included, To is not. A row exactly on To
// belongs to the next evaluation, and counting it in both would double alert.
func TestRunSelectsOnlyTheWindow(t *testing.T) {
	src := testSource(t)
	q := openQuerier(t, src)

	seed(t, q, []span{
		{at: anchor.Add(-10 * time.Minute), service: "too-old", duration: 1},
		{at: anchor.Add(-5 * time.Minute), service: "on-from", duration: 2},
		{at: anchor.Add(-3 * time.Minute), service: "inside", duration: 3},
		{at: anchor, service: "on-to", duration: 4},
		{at: anchor.Add(time.Minute), service: "future", duration: 5},
	})

	r := rule.Rule{Alert: "MaxLatency", Source: "otel_traces", Expr: latencyExpr, Window: 5 * time.Minute}
	got := run(t, q, r, anchor)

	want := []alertSample{
		{service: "inside", value: 3},
		{service: "on-from", value: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// evaluation_delay exists because the freshest rows are still arriving. This
// proves the delay really moves the upper bound back rather than being
// cosmetic (spec 6.8).
func TestRunEvaluationDelayExcludesFreshestRows(t *testing.T) {
	src := testSource(t)
	src.EvaluationDelay = 2 * time.Minute
	q := openQuerier(t, src)

	seed(t, q, []span{
		{at: anchor.Add(-4 * time.Minute), service: "settled", duration: 10},
		{at: anchor.Add(-1 * time.Minute), service: "still-arriving", duration: 20},
	})

	r := rule.Rule{Alert: "MaxLatency", Source: "otel_traces", Expr: latencyExpr, Window: 5 * time.Minute}
	got := run(t, q, r, anchor)

	if len(got) != 1 {
		t.Fatalf("got %v, want only the settled row", got)
	}
	if got[0].service != "settled" {
		t.Errorf("got %q, want settled", got[0].service)
	}
}

// Timestamps reach ClickHouse as bound parameters. Proving a quote in a label
// survives shows nothing is being pasted into SQL text.
func TestRunBindsParametersRatherThanInterpolating(t *testing.T) {
	src := testSource(t)
	q := openQuerier(t, src)

	seed(t, q, []span{
		{at: anchor.Add(-time.Minute), service: "o'brien', 1) --", duration: 7},
	})

	r := rule.Rule{Alert: "MaxLatency", Source: "otel_traces", Expr: latencyExpr, Window: 5 * time.Minute}
	got := run(t, q, r, anchor)

	if len(got) != 1 {
		t.Fatalf("got %v, want 1 sample", got)
	}
	if got[0].service != "o'brien', 1) --" {
		t.Errorf("label = %q, want it to survive unchanged", got[0].service)
	}
}

func TestRunRequiresValueColumn(t *testing.T) {
	src := testSource(t)
	q := openQuerier(t, src)
	seed(t, q, []span{{at: anchor.Add(-time.Minute), service: "checkout", duration: 1}})

	r := rule.Rule{
		Alert:  "NoValue",
		Source: "otel_traces",
		Expr:   "SELECT ServiceName FROM otel.otel_traces WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}",
		Window: 5 * time.Minute,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := q.Run(ctx, r, anchor); err == nil {
		t.Fatal("expected an error when the query returns no value column")
	} else if !strings.Contains(err.Error(), "value") {
		t.Errorf("error should mention the value column, got: %v", err)
	}
}

// A rule returning more rows than the cap has to fail rather than buffer
// them. The server side max_result_rows should stop it before the client
// side check ever sees the rows (spec 6.7).
func TestRunEnforcesMaxRows(t *testing.T) {
	src := testSource(t)
	src.MaxRows = 3
	q := openQuerier(t, src)

	spans := make([]span, 10)
	for i := range spans {
		spans[i] = span{
			at:       anchor.Add(-time.Minute),
			service:  "svc-" + string(rune('a'+i)),
			duration: uint64(i + 1),
		}
	}
	seed(t, q, spans)

	r := rule.Rule{Alert: "TooMany", Source: "otel_traces", Expr: latencyExpr, Window: 5 * time.Minute}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := q.Run(ctx, r, anchor); err == nil {
		t.Fatal("expected an error when the row cap is exceeded")
	}
}

// Map columns cannot be labels, and the failure has to be a clear error
// rather than a silently dropped label.
func TestRunRejectsMapColumnAsLabel(t *testing.T) {
	src := testSource(t)
	q := openQuerier(t, src)
	seed(t, q, []span{{at: anchor.Add(-time.Minute), service: "checkout", duration: 1}})

	r := rule.Rule{
		Alert:  "MapLabel",
		Source: "otel_traces",
		Expr: `SELECT SpanAttributes, max(Duration) AS value FROM otel.otel_traces
		       WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }} GROUP BY SpanAttributes`,
		Window: 5 * time.Minute,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := q.Run(ctx, r, anchor); err == nil {
		t.Fatal("expected an error for a map column used as a label")
	}
}
