//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// Whether the re-check pass earns an integration test of its own, argued rather
// than assumed: it does, and exactly one.
//
// The unit tests drive the pass with findings a fake handed it, which covers
// every decision it makes about the gauge, the policy and the log. What none of
// them can show is that a running ruler reads real rows on its own timer and
// reports what it found there: the map key it asks about is resolved against the
// live table's columns and counted over a window nobody in the test chose, which
// is the one thing no fake can stand in for (spec 10.4).
//
// One test, not a matrix. What sampling reports about absent keys, empty windows
// and keys it cannot check is already covered against a real cluster in
// internal/query, and paying a container again per case would buy nothing.
func TestRunReportsAMapKeyRecentDataDoesNotHave(t *testing.T) {
	amURL := os.Getenv("RULER_ALERTMANAGER_URL")
	chAddr := os.Getenv("RULER_CLICKHOUSE_ADDR")
	if amURL == "" || chAddr == "" {
		t.Fatal("RULER_ALERTMANAGER_URL and RULER_CLICKHOUSE_ADDR must be set, run `just integration`")
	}

	table := "recheck_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	admin := openAdmin(t, chAddr)
	seedRecheckTable(t, admin, table)

	dir := t.TempDir()
	rulesDir := filepath.Join(dir, "rules")
	if err := os.MkdirAll(rulesDir, 0o750); err != nil {
		t.Fatalf("creating the rules directory: %v", err)
	}

	// The rule the check exists for: the attribute was renamed, so the query
	// parses, returns the column it always did and matches nothing forever.
	rulePath := filepath.Join(rulesDir, "recheck.yaml")
	writeFile(t, rulePath, fmt.Sprintf(`
groups:
  - name: recheck
    interval: 1s
    rules:
      - alert: RenamedAttribute
        sources:
          team: recheck
        expr: |
          SELECT ServiceName, count() AS value
          FROM otel.%s
          WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
            AND SpanAttributes['http.status_code'] != ''
          GROUP BY ServiceName
        window: 5m
        labels:
          team: recheck
        annotations:
          summary: "still evaluating, still matching nothing"
`, table))

	writeFile(t, filepath.Join(dir, "sources.yaml"), fmt.Sprintf(`
sources:
  - name: recheck_source
    labels: {team: recheck}
    address: %s
    database: otel
    username: ruler_wide
    table: %s
    timestamp_column: Timestamp
    evaluation_delay: 0s
`, chAddr, table))

	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	runDone := make(chan int, 1)
	go func() {
		runDone <- runRun(ctx, []string{
			"--rules", rulesDir,
			"--sources", filepath.Join(dir, "sources.yaml"),
			"--alertmanager", amURL,
			"--recheck-interval", "1s",
			"--listen", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		}, &stdout, &stderr)
	}()

	// Both, because the pass reports and never refuses: waiting only for the
	// finding could read the gauge before the rule's first evaluation landed and
	// then prove nothing about the assertion below.
	body := waitForMetrics(t, port, 30*time.Second, func(body string) bool {
		return strings.Contains(body, `clickhouse_ruler_problem{check="rule/attribute-key"`) &&
			strings.Contains(body, "clickhouse_ruler_rule_evaluations_total{")
	})

	// Addressed to whoever owns the rule rather than to whoever operates the
	// ruler, and a warning because the answer is read through row policies
	// (spec 7.3, 8.2).
	for _, want := range []string{
		`team="recheck"`, `file="` + rulePath + `"`, `rule="RenamedAttribute"`, `severity="warning"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the raised gauge does not carry %s:\n%s", want, problemLines(body))
		}
	}

	// The pass reports and never refuses, so the rule is still being evaluated
	// on its own interval while the finding stands (spec 7.6).
	if !strings.Contains(body, `rule="RenamedAttribute"`) {
		t.Errorf("the rule stopped being evaluated:\n%s", problemLines(body))
	}

	cancel()
	select {
	case code := <-runDone:
		if code != exitOK {
			t.Errorf("ruler run exited %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ruler run did not shut down within 10s of cancellation")
	}
}

// seedRecheckTable builds a table this test owns, holding one recent row whose
// attribute map carries a different key from the one the rule reads. Rows have
// to be there: an empty window verifies nothing and is deliberately silent.
func seedRecheckTable(t *testing.T, conn clickhouse.Conn, table string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	create := "CREATE TABLE otel." + table + ` (
		Timestamp DateTime64(3),
		ServiceName String,
		SpanAttributes Map(LowCardinality(String), String),
		value Float64
	) ENGINE = MergeTree ORDER BY Timestamp`
	if err := conn.Exec(ctx, create); err != nil {
		t.Fatalf("creating %s: %v", table, err)
	}
	t.Cleanup(func() {
		_ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS otel."+table)
	})

	batch, err := conn.PrepareBatch(ctx, "INSERT INTO otel."+table)
	if err != nil {
		t.Fatalf("preparing the insert: %v", err)
	}
	if err := batch.Append(
		time.Now().Add(-time.Minute), "checkout",
		map[string]string{"http.response.status_code": "500"}, 1.0,
	); err != nil {
		t.Fatalf("appending a row: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("sending the insert: %v", err)
	}
}
