//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// Whether drift earns an integration test of its own, argued rather than
// assumed: it does, and exactly one.
//
// The unit tests drive the comparison with shapes a test handed it, which is
// every decision it makes. What none of them can show is that the shape reaching
// the comparison is the one the server produced: it comes out of the driver's
// column types on a live result, so a rule whose column is retyped underneath it
// is the one case where nothing in the process changed and the answer still has
// to differ. That is the seam this exists for, and it needs a real table a test
// can alter while the ruler is running.
//
// One test, and not a matrix. A dropped column, two sources disagreeing and a
// cost over its ceiling are the same comparison on the same shape, already
// covered without paying a container for each.
func TestRunReportsARuleTheSchemaMovedUnder(t *testing.T) {
	amURL := os.Getenv("RULER_ALERTMANAGER_URL")
	chAddr := os.Getenv("RULER_CLICKHOUSE_ADDR")
	if amURL == "" || chAddr == "" {
		t.Fatal("RULER_ALERTMANAGER_URL and RULER_CLICKHOUSE_ADDR must be set, run `just integration`")
	}

	table := "drift_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	admin := openAdmin(t, chAddr)
	seedDriftTable(t, admin, table)

	dir := t.TempDir()
	rulesDir := filepath.Join(dir, "rules")
	if err := os.MkdirAll(rulesDir, 0o750); err != nil {
		t.Fatalf("creating the rules directory: %v", err)
	}
	rulePath := filepath.Join(rulesDir, "drift.yaml")
	writeFile(t, rulePath, fmt.Sprintf(`
groups:
  - name: drift
    interval: 1s
    rules:
      - alert: DriftingRule
        sources:
          team: drift
        expr: |
          SELECT ServiceName, value
          FROM otel.%s
          WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
        window: 5m
        labels:
          team: drift
        annotations:
          summary: "still evaluating"
`, table))

	// ruler_wide is the user with SELECT on the whole database, which is what a
	// table this test created needs: the restricted user is granted one table by
	// name and granting it another would widen the contract the other tests
	// prove (spec 6.7.2).
	writeFile(t, filepath.Join(dir, "ruler.yaml"), fmt.Sprintf(`
sources:
  - name: drift_source
    labels: {team: drift}
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
			"--config", filepath.Join(dir, "ruler.yaml"),
			"--alertmanager", amURL,
			"--listen", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		}, &stdout, &stderr)
	}()

	// Two evaluations establish the baseline, so the gauge stays empty until the
	// column is retyped under the rule.
	waitForMetrics(t, port, 30*time.Second, func(body string) bool {
		return strings.Contains(body, "clickhouse_ruler_rule_evaluations_total")
	})

	if err := admin.Exec(context.Background(),
		"ALTER TABLE otel."+table+" MODIFY COLUMN value Int64 SETTINGS alter_sync = 2"); err != nil {
		t.Fatalf("retyping the column under the rule: %v", err)
	}

	body := waitForMetrics(t, port, 30*time.Second, func(body string) bool {
		return strings.Contains(body, `clickhouse_ruler_problem{check="rule/columns"`)
	})

	// The labels are what makes the signal actionable by whoever owns the rule
	// rather than by whoever operates the ruler (spec 8.2).
	// The file label is the rule's path in the rules tree, which is what
	// somebody editing the rule can open, where the path the container resolved
	// the root to is not (spec 8.2).
	for _, want := range []string{`team="drift"`, `file="drift.yaml"`, `rule="DriftingRule"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the raised gauge does not carry %s:\n%s", want, problemLines(body))
		}
	}

	// Reporting never refuses, so the rule is still being evaluated and its
	// alerts are still being sent (spec 6.3.2).
	if strings.Contains(body, "clickhouse_ruler_alerts_active{") &&
		!strings.Contains(body, `state="firing"`) {
		t.Errorf("the drifting rule stopped tracking alerts:\n%s", body)
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

// openAdmin connects as the admin user, which is who writes rows and changes
// schemas. The ruler never does either.
func openAdmin(t *testing.T, addr string) clickhouse.Conn {
	t.Helper()

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "otel", Username: "ruler", Password: "ruler"},
	})
	if err != nil {
		t.Fatalf("opening the admin connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// seedDriftTable builds a table this test owns, with one row inside the rule's
// window, so the column it retypes is one no other test reads.
func seedDriftTable(t *testing.T, conn clickhouse.Conn, table string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	create := "CREATE TABLE otel." + table + ` (
		Timestamp DateTime64(3),
		ServiceName String,
		value Float64
	) ENGINE = MergeTree ORDER BY Timestamp`
	if err := conn.Exec(ctx, create); err != nil {
		t.Fatalf("creating %s: %v", table, err)
	}
	t.Cleanup(func() {
		_ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS otel."+table)
	})

	batch, err := conn.PrepareBatch(ctx, "INSERT INTO otel."+table+" (Timestamp, ServiceName, value)")
	if err != nil {
		t.Fatalf("prepare batch: %v", err)
	}
	if err := batch.Append(time.Now().UTC().Add(-30*time.Second), "checkout", float64(1)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// freePort asks the kernel for a port and gives it straight back, so the ruler
// binds an address this test knows and can scrape.
func freePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the port: %v", err)
	}
	return port
}

// waitForMetrics scrapes until the condition holds, which is also how it waits
// for the ruler to have started at all.
func waitForMetrics(t *testing.T, port int, d time.Duration, cond func(string) bool) string {
	t.Helper()

	url := fmt.Sprintf("http://127.0.0.1:%d/metrics", port)
	deadline := time.Now().Add(d)
	var last string
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("building the scrape request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				t.Fatalf("reading /metrics: %v", readErr)
			}
			last = string(body)
			if cond(last) {
				return last
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s, last scrape had:\n%s", d, problemLines(last))
	return ""
}

// problemLines is the part of a scrape worth printing on a failure.
func problemLines(body string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "clickhouse_ruler_problem") ||
			strings.HasPrefix(line, "clickhouse_ruler_rule_evaluation") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
