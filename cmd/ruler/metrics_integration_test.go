//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/dennisme/clickhouse-ruler/internal/datapoints"
)

// A counter rule, written the way a counter has to be read: the delta is taken
// per series and summed after that, because subtracting across series is
// nonsense and subtracting across a restart is negative. The threshold is far
// out of reach, since what this test proves is that the rule reads the rows at
// all rather than what it decides about them.
const metricsRule = `groups:
  - name: metrics
    interval: 1m
    rules:
      - alert: ErrorsIncreasing
        sources:
          signal: metrics
        expr: |
          SELECT ServiceName, sum(delta) AS value
          FROM (
            SELECT ServiceName, Attributes, max(Value) - min(Value) AS delta
            FROM otel_metrics_sum
            WHERE MetricName = 'http_server_errors_total'
              AND TimeUnix >= {{ .From }} AND TimeUnix < {{ .To }}
            GROUP BY ServiceName, Attributes
          )
          GROUP BY ServiceName
          HAVING value > 1000000
        labels:
          severity: warning
        annotations:
          summary: errors rose on {{ .Labels.ServiceName }}
`

// waitForMetricRows blocks until the sum table holds want points for that
// service, which is the collector's half of the path: the test posted OTLP and
// something else has to have written it to the table a rule reads.
func waitForMetricRows(t *testing.T, addr, serviceName string, want int) {
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
			"SELECT count() FROM otel.otel_metrics_sum WHERE ServiceName = ?", serviceName)
		if err := row.Scan(&got); err != nil {
			t.Fatalf("counting points for %s: %v", serviceName, err)
		}
		if got >= uint64(want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("otel_metrics_sum holds %d points for %s after 60s, want %d: "+
				"the collector accepted the post, so this is the exporter's write",
				got, serviceName, want)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// The whole chain this slice exists to prove: the emitter posts data points,
// the collector writes them to the verbatim metrics table, and a rule reads
// them back as the restricted user whose grant is those tables and nothing
// else (spec 9.1, 9.2).
func TestAMetricsRuleReadsWhatTheCollectorWrote(t *testing.T) {
	endpoint := os.Getenv("RULER_OTLP_HTTP_URL")
	addr := os.Getenv("RULER_CLICKHOUSE_ADDR")
	if endpoint == "" || addr == "" {
		t.Fatal("RULER_OTLP_HTTP_URL and RULER_CLICKHOUSE_ADDR are not set, run these through `just integration`")
	}

	// Its own service name, so this test asserts on points nothing else wrote.
	// Placed relative to the clock, because the dev schema TTLs at three days
	// and a literal timestamp expires where it stands.
	service := fmt.Sprintf("metrics-chain-%d", time.Now().UnixNano())
	start := time.Now().Add(-3 * time.Minute).Truncate(time.Second)

	s := datapoints.Scenario{
		Endpoint:    endpoint,
		ServiceName: service,
		MetricName:  "http_server_errors_total",
		Kind:        datapoints.Sum,
		Temporality: datapoints.Cumulative,
		Monotonic:   true,
		Attributes:  map[string]string{"http.route": "/pay"},
		Points: []datapoints.Point{
			{Time: start, Value: 10},
			{Time: start.Add(time.Minute), Value: 25},
			{Time: start.Add(2 * time.Minute), Value: 40},
		},
	}
	if err := s.Emit(context.Background()); err != nil {
		t.Fatalf("emitting data points: %v", err)
	}

	waitForMetricRows(t, addr, service, len(s.Points))

	dir := t.TempDir()
	rules := filepath.Join(dir, "rules")
	if err := os.MkdirAll(rules, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rules, "errors.yaml"), []byte(metricsRule), 0o600); err != nil {
		t.Fatal(err)
	}

	sources := filepath.Join(dir, "ruler.yaml")
	body := "sources:\n" +
		"  - name: otel_metrics\n" +
		"    labels: {signal: metrics}\n" +
		"    address: " + addr + "\n" +
		"    database: otel\n" +
		"    username: ruler_metrics\n" +
		"    table: otel_metrics_sum\n" +
		"    timestamp_column: TimeUnix\n"
	if err := os.WriteFile(sources, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// --online is the tier that sends the statement to the cluster, so a pass
	// here is the restricted user reading the table rather than the query
	// merely parsing. --sample reads the rows, which is the half that would
	// stay quiet if the collector had written nothing.
	code, stdout, stderr := runCheck(t, "check", "--config", sources, "--online", "--sample", rules)
	out := stdout + stderr
	if code != exitOK {
		t.Errorf("exit = %d, want %d: a metrics rule is readable by its own user\n%s", code, exitOK, out)
	}
	if strings.Contains(out, "source/privileges") {
		t.Errorf("the metrics user does not meet the contract:\n%s", out)
	}
}
