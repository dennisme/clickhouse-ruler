//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// sinkPort matches the receiver URL in deploy/alertmanager/alertmanager.yml.
const sinkPort = 9099

type delivery struct {
	Status string `json:"status"`
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"alerts"`
}

type sink struct {
	mu   sync.Mutex
	got  []delivery
	srv  *http.Server
	done chan struct{}
}

func startSink(t *testing.T) *sink {
	t.Helper()

	s := &sink{done: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var d delivery
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
			t.Errorf("decoding webhook body: %v", err)
		}
		s.mu.Lock()
		s.got = append(s.got, d)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(sinkPort)))
	if err != nil {
		t.Fatalf("listening on %d: %v, is another sink already running? (`just integration` needs -p 1)", sinkPort, err)
	}
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		defer close(s.done)
		_ = s.srv.Serve(ln)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
		<-s.done
	})
	return s
}

func (s *sink) waitFor(t *testing.T, d time.Duration, cond func([]delivery) bool) []delivery {
	t.Helper()

	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		got := append([]delivery(nil), s.got...)
		s.mu.Unlock()

		if cond(got) {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	t.Fatalf("condition not met within %s, deliveries so far: %+v", d, s.got)
	return nil
}

// seed writes one slow checkout span under a run-unique ServiceName, so this
// run's alert cannot be confused with another test's leftover data or with a
// concurrent run of this same test.
func seed(t *testing.T, addr, serviceName string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "otel", Username: "ruler", Password: "ruler"},
	})
	if err != nil {
		t.Fatalf("opening seed connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	batch, err := conn.PrepareBatch(ctx,
		"INSERT INTO otel.otel_traces (Timestamp, TraceId, SpanId, ServiceName, SpanName, Duration, StatusCode)")
	if err != nil {
		t.Fatalf("prepare batch: %v", err)
	}
	now := time.Now().UTC()
	if err := batch.Append(now.Add(-30*time.Second), "trace", "span", serviceName, "GET /", uint64(5_000_000_000), "Ok"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// This is the scheduler slice's own end to end test: unlike
// internal/notify's, which drives ruleset.Load, query.Run, alert.State.Eval
// and notify.Send by hand, this one starts `ruler run` as a subprocess of the
// test binary and never touches evaluation itself. The scheduler has to tick
// on its own, find the seeded row, and deliver the alert through the real
// HTTP surface end to end.
func TestRunEndToEndFiringAlertReachesAlertmanager(t *testing.T) {
	amURL := os.Getenv("RULER_ALERTMANAGER_URL")
	chAddr := os.Getenv("RULER_CLICKHOUSE_ADDR")
	if amURL == "" || chAddr == "" {
		t.Fatal("RULER_ALERTMANAGER_URL and RULER_CLICKHOUSE_ADDR must be set, run `just integration`")
	}
	t.Setenv("RULER_CLICKHOUSE_PASSWORD", "ruler")

	s := startSink(t)

	serviceName := "checkout-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seed(t, chAddr, serviceName)

	sourcesPath := filepath.Join("testdata", "sources.yaml")
	rewritten := rewriteAddress(t, sourcesPath, chAddr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	runDone := make(chan int, 1)
	go func() {
		runDone <- runRun(ctx, []string{
			"--rules", filepath.Join("testdata", "rules"),
			"--sources", rewritten,
			"--alertmanager", amURL,
			"--listen", ":0",
		}, &stdout, &stderr)
	}()

	got := s.waitFor(t, 30*time.Second, func(ds []delivery) bool {
		for _, d := range ds {
			for _, a := range d.Alerts {
				if a.Labels["ServiceName"] == serviceName {
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

	var found bool
	for _, d := range got {
		for _, a := range d.Alerts {
			if a.Labels["ServiceName"] != serviceName {
				continue
			}
			found = true

			// The annotation is rendered from the rule's template, not
			// passed through: proving the scheduler's send path renders it
			// needs the expanded text, and the run-unique service name is
			// what makes the expansion visible rather than merely present.
			if want := serviceName + " is slow"; a.Annotations["summary"] != want {
				t.Errorf("delivered summary = %q, want %q", a.Annotations["summary"], want)
			}

			if a.Labels["alertname"] != "CheckoutIsSlow" {
				t.Errorf("delivered alertname = %q, want CheckoutIsSlow", a.Labels["alertname"])
			}
			if a.Labels["team"] != "payments" {
				t.Errorf("delivered team = %q, want payments", a.Labels["team"])
			}
			if a.Labels["source"] != "otel_traces" {
				t.Errorf("delivered source = %q, want otel_traces", a.Labels["source"])
			}
			if a.Status != "firing" {
				t.Errorf("delivered status = %q, want firing", a.Status)
			}
		}
	}
	if !found {
		t.Fatalf("no alert for %s delivered: %+v", serviceName, got)
	}
}

// rewriteAddress copies the sources fixture into the test's temp directory
// with its address overridden, the same trick the check tests use, so the
// checked-in fixture stays reviewable without a live cluster address in it.
func rewriteAddress(t *testing.T, path, addr string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	out := filepath.Join(t.TempDir(), "sources.yaml")
	rewritten := bytes.ReplaceAll(data, []byte("address: 127.0.0.1:9000"), []byte("address: "+addr))
	if err := os.WriteFile(out, rewritten, 0o600); err != nil {
		t.Fatalf("writing %s: %v", out, err)
	}
	return out
}
