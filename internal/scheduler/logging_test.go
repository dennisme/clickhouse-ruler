package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// logBuffer hands slog a handler writing to memory. Test output has to stay
// clean, so no assertion here is made by letting a real log line through.
func logBuffer() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), &buf
}

// logLines decodes what was logged, one record per line.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// wantFields fails unless every field is present with that exact value.
func wantFields(t *testing.T, rec map[string]any, fields map[string]string) {
	t.Helper()

	for k, want := range fields {
		got, ok := rec[k].(string)
		if !ok {
			t.Errorf("log record has no string field %q: %v", k, rec)
			continue
		}
		if got != want {
			t.Errorf("field %q = %q, want %q", k, got, want)
		}
	}
}

func oneRuleSet(alertName string, sources ...source.Source) *ruleset.Set {
	return &ruleset.Set{Rules: []ruleset.Rule{{
		Rule:    rule.Rule{Alert: alertName},
		File:    "f.yaml",
		Group:   testGroup("g1", time.Minute),
		Labels:  map[string]string{},
		Sources: sources,
	}}}
}

// A counter moving is not an operational signal on its own: it says something
// failed, not which rule, which source, or what the database said.
func TestEvalGroupLogsWhichRuleAndSourceFailed(t *testing.T) {
	log, buf := logBuffer()
	q := &fakeQuerier{err: errors.New("connection refused")}

	sched := New(oneRuleSet("Broken", source.Source{Name: "src1"}), map[string]Querier{"src1": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		NewMetrics(prometheus.NewRegistry()), newFakeClock(time.Unix(0, 0)), 0, log)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %v", len(lines), lines)
	}
	wantFields(t, lines[0], map[string]string{
		"level":      "ERROR",
		"rule_group": "f.yaml:g1",
		"rule":       "Broken",
		"source":     "src1",
	})
	if got, _ := lines[0]["error"].(string); !strings.Contains(got, "connection refused") {
		t.Errorf("error field = %q, want it to carry what the query said", got)
	}
}

// Result.SendError was set and then dropped, so an operator reading
// ruler_alerts_send_failures_total had nothing saying which rule could not be
// delivered.
func TestEvalGroupLogsASendFailure(t *testing.T) {
	log, buf := logBuffer()
	sender := &recordingSender{err: errors.New("alertmanager unreachable")}

	sched := New(oneRuleSet("Undeliverable", source.Source{Name: "src1"}),
		map[string]Querier{"src1": &fakeQuerier{samples: oneSample()}},
		notify.NewCadence(sender, time.Minute, notify.DefaultResendTolerance),
		NewMetrics(prometheus.NewRegistry()), newFakeClock(time.Unix(0, 0)), 0, log)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %v", len(lines), lines)
	}
	wantFields(t, lines[0], map[string]string{
		"level":      "ERROR",
		"rule_group": "f.yaml:g1",
		"rule":       "Undeliverable",
	})
	if got, _ := lines[0]["error"].(string); !strings.Contains(got, "alertmanager unreachable") {
		t.Errorf("error field = %q, want it to carry what the send said", got)
	}
}

// Spec 8.3's cardinality rule is about metrics, and the same reasoning applies
// to logs: a rule returning ten thousand rows must not write ten thousand
// lines. One line per rule and per source, never per alert instance.
func TestEvalGroupLogsOncePerRuleRegardlessOfInstanceCount(t *testing.T) {
	log, buf := logBuffer()

	samples := make([]alert.Sample, 0, 500)
	for i := 0; i < 500; i++ {
		samples = append(samples, alert.Sample{
			Labels: map[string]string{"ServiceName": "svc" + strconv.Itoa(i)},
			Value:  1,
		})
	}
	sender := &recordingSender{err: errors.New("alertmanager unreachable")}

	sched := New(oneRuleSet("Noisy", source.Source{Name: "src1"}),
		map[string]Querier{"src1": &fakeQuerier{samples: samples}},
		notify.NewCadence(sender, time.Minute, notify.DefaultResendTolerance),
		NewMetrics(prometheus.NewRegistry()), newFakeClock(time.Unix(0, 0)), 0, log)

	sched.groups[0].Eval(context.Background(), time.Unix(0, 0))

	if lines := logLines(t, buf); len(lines) != 1 {
		t.Fatalf("got %d log lines for 500 instances, want 1", len(lines))
	}
}

// A shutdown that gives up on work still running is the only moment an
// operator can learn that a query or a send was cut off part way through.
func TestShutdownLogsWhenTheTimeoutExpires(t *testing.T) {
	log, buf := logBuffer()

	started := make(chan struct{})
	release := make(chan struct{})
	q := &blockingQuerier{started: started, release: release}

	clock := newFakeClock(time.Unix(0, 0))
	sched := New(oneRuleSet("Slow", source.Source{Name: "src1"}), map[string]Querier{"src1": q},
		notify.NewCadence(&recordingSender{}, time.Minute, notify.DefaultResendTolerance),
		NewMetrics(prometheus.NewRegistry()), clock, 0, log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	// Drive the clock until the group's first tick lands and its query blocks.
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case <-started:
		case <-time.After(time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("the group never ticked")
			}
			clock.Advance(time.Minute)
			continue
		}
		break
	}

	sched.Shutdown(50 * time.Millisecond)
	close(release)

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %v", len(lines), lines)
	}
	if got, _ := lines[0]["level"].(string); got != "WARN" {
		t.Errorf("level = %q, want WARN", got)
	}
	if got, _ := lines[0]["msg"].(string); !strings.Contains(got, "shutdown") {
		t.Errorf("msg = %q, want it to name shutdown", got)
	}
}

// blockingQuerier holds an evaluation open until the test lets it go, so a
// shutdown can be made to time out without waiting on a real duration.
type blockingQuerier struct {
	started chan struct{}
	release chan struct{}
	once    bool
}

func (q *blockingQuerier) Run(context.Context, rule.Rule, time.Time) ([]alert.Sample, error) {
	if !q.once {
		q.once = true
		close(q.started)
	}
	<-q.release
	return nil, nil
}
