//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/dennisme/clickhouse-ruler/internal/spans"
)

// How long each span the scenarios emit takes, either side of the 1000ms the
// rule in testdata holds. Both are emitted the same way, so a rule that fires
// on one and not the other is reading the data rather than reacting to its
// arrival.
const (
	slowSpan = 2 * time.Second
	fastSpan = 100 * time.Millisecond

	// 40 spans over 1000ms, the scenario spec 9.2 names as the thing
	// telemetrygen cannot express.
	spanCount  = 40
	spanSpread = time.Second
)

// waitForRows blocks until the table holds want rows for that service, which
// is the collector's half of the path: the test posted OTLP and something else
// has to have written it to the table the rules read.
func waitForRows(t *testing.T, addr, serviceName string, want int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "otel", Username: "ruler", Password: "ruler"},
	})
	if err != nil {
		t.Fatalf("opening connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(60 * time.Second)
	for {
		var got uint64
		row := conn.QueryRow(ctx,
			"SELECT count() FROM otel.otel_traces WHERE ServiceName = ?", serviceName)
		if err := row.Scan(&got); err != nil {
			t.Fatalf("counting rows for %s: %v", serviceName, err)
		}
		if got >= uint64(want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s has %d rows after 60s, want %d: the collector did not write what was posted to it",
				serviceName, got, want)
		}
		time.Sleep(time.Second)
	}
}

// The path spec 9.1 item 2 exists for, end to end: spans posted as OTLP, a real
// collector writing them into the verbatim schema, the ruler reading that table
// and Alertmanager delivering what came out.
//
// Every other integration test inserts its own rows, so nothing in the tree has
// ever proved that a rule works against what a collector actually writes. The
// schema is copied from exporter/clickhouseexporter precisely so that it does.
//
// The count is the assertion, which is what the emitter exists for (9.2). Two
// services are emitted identically and only one of them is slow, so a rule that
// fired on arrival rather than on duration fails here.
func TestCollectorWrittenSpansFireOnlyTheServiceThatIsSlow(t *testing.T) {
	otlpURL := os.Getenv("RULER_OTLP_HTTP_URL")
	amURL := os.Getenv("RULER_ALERTMANAGER_URL")
	chAddr := os.Getenv("RULER_CLICKHOUSE_ADDR")
	if otlpURL == "" || amURL == "" || chAddr == "" {
		t.Fatal("RULER_OTLP_HTTP_URL, RULER_ALERTMANAGER_URL and RULER_CLICKHOUSE_ADDR must be set, run `just integration`")
	}

	s := startSink(t)

	// Run-unique, so neither this run's leftovers nor a concurrent one can be
	// mistaken for the scenario, and so background volume never can.
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	slow := "checkout-slow-" + run
	fast := "checkout-fast-" + run

	start := time.Now().UTC().Add(-time.Minute)
	for _, sc := range []spans.Scenario{
		{ServiceName: slow, SpanDuration: slowSpan},
		{ServiceName: fast, SpanDuration: fastSpan},
	} {
		sc.Endpoint = otlpURL
		sc.SpanName = "GET /checkout"
		sc.Count = spanCount
		sc.Spread = spanSpread
		sc.Start = start
		sc.Attributes = map[string]string{"run": run}
		if err := sc.Emit(context.Background()); err != nil {
			t.Fatalf("emitting %s: %v", sc.ServiceName, err)
		}
	}

	waitForRows(t, chAddr, slow, spanCount)
	waitForRows(t, chAddr, fast, spanCount)

	configPath := filepath.Join("testdata", "sources_ingest.yaml")
	rewritten := rewriteAddress(t, configPath, chAddr, amURL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	runDone := make(chan int, 1)
	go func() {
		runDone <- runRun(ctx, []string{
			"--rules", filepath.Join("testdata", "rules"),
			"--config", rewritten,
			"--listen", ":0",
		}, &stdout, &stderr)
	}()

	got := s.waitFor(t, 60*time.Second, func(ds []delivery) bool {
		for _, d := range ds {
			for _, a := range d.Alerts {
				if a.Labels["ServiceName"] == slow {
					return true
				}
			}
		}
		return false
	})

	cancel()
	select {
	case code := <-runDone:
		if code != exitOK {
			t.Errorf("ruler run exited %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ruler run did not shut down within 10s of cancellation")
	}

	// One alert for this run and no more. A still-firing alert is re-posted, so
	// the count is of services named rather than of deliveries received.
	fired := map[string]bool{}
	for _, d := range got {
		for _, a := range d.Alerts {
			if name := a.Labels["ServiceName"]; name == slow || name == fast {
				fired[name] = true
			}
		}
	}
	if !fired[slow] {
		t.Errorf("%s emitted %s spans and no alert named it", slow, slowSpan)
	}
	if fired[fast] {
		t.Errorf("%s emitted %s spans and an alert named it anyway: the rule is firing on arrival, not on duration",
			fast, fastSpan)
	}
	if len(fired) != 1 {
		t.Errorf("this run fired %d alerts, want exactly 1: %v", len(fired), fired)
	}
}
