//go:build integration

package notify_test

import (
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

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// sinkPort matches the receiver URL in deploy/alertmanager/alertmanager.yml.
// Alertmanager reads that file at startup, so the port cannot be chosen here.
const sinkPort = 9099

// delivery is the part of Alertmanager's webhook body the assertions need.
type delivery struct {
	Status string `json:"status"`
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"alerts"`
}

// sink records what Alertmanager actually delivered. It binds all interfaces
// because the notification arrives from inside a container.
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
		t.Fatalf("listening on %d: %v, is another sink already running?", sinkPort, err)
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

// waitFor polls until cond holds, so the test does not depend on how quickly
// Alertmanager flushes its group.
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

// The whole path, with nothing stubbed: rules and sources load from files,
// the query runs against real ClickHouse, the state machine decides the alert
// fires, the notifier posts to a real Alertmanager, and Alertmanager delivers
// it to the sink. An assertion here proves the pieces agree with each other,
// which unit tests cannot.
func TestEndToEndFiringAlertReachesAlertmanager(t *testing.T) {
	amURL := os.Getenv("RULER_ALERTMANAGER_URL")
	chAddr := os.Getenv("RULER_CLICKHOUSE_ADDR")
	if amURL == "" || chAddr == "" {
		t.Fatal("RULER_ALERTMANAGER_URL and RULER_CLICKHOUSE_ADDR must be set, run `just integration`")
	}

	s := startSink(t)

	set := loadSet(t, chAddr)
	if len(set.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(set.Rules))
	}
	r := set.Rules[0]

	// The rule names no source: its team label matches both (spec 6.10).
	if len(r.Sources) != 2 {
		t.Fatalf("matched %d sources, want 2: %v", len(r.Sources), r.Sources)
	}

	now := seed(t, r.Sources[0])

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Alertmanager suppresses a repeat of a group it has already notified
	// about, so every run carries a label no previous run used. With
	// group_by: ['...'] that makes this alert its own group, which flushes
	// after group_wait instead of waiting on the next tick or being dropped
	// as a repeat. Without it the test passes once and then fails for an hour.
	runID := strconv.FormatInt(time.Now().UnixNano(), 36)
	labels := make(map[string]string, len(r.Labels)+1)
	for k, v := range r.Labels {
		labels[k] = v
	}
	labels["run_id"] = runID

	// One evaluation per matched source, each with its own state. This is the
	// loop a scheduler will run.
	var fired []alert.Alert
	for _, src := range r.Sources {
		q, err := query.Open(src)
		if err != nil {
			t.Fatalf("opening querier for %s: %v", src.Name, err)
		}
		defer func() { _ = q.Close() }()

		samples, err := q.Run(ctx, r.Rule, now)
		if err != nil {
			t.Fatalf("running rule against %s: %v", src.Name, err)
		}
		if len(samples) == 0 {
			t.Fatalf("%s returned no rows, the rule would never fire", src.Name)
		}

		// for is 0 in the fixture, so the first evaluation fires immediately.
		got := alert.New(r.Rule, labels, src).Eval(now, samples)
		if len(got) == 0 {
			t.Fatalf("state machine produced no alerts for %s", src.Name)
		}
		fired = append(fired, got...)
	}

	if len(fired) != 2 {
		t.Fatalf("got %d alerts, want one per source", len(fired))
	}
	if fired[0].Fingerprint == fired[1].Fingerprint {
		t.Fatal("same fingerprint from two sources: one would resolve the other")
	}

	client := notify.NewClient(amURL)
	if err := client.Send(ctx, fired, r.Annotations); err != nil {
		t.Fatalf("sending to alertmanager: %v", err)
	}

	// Both sources have to arrive, not just the first: the point of the run is
	// that one rule produced two alerts that Alertmanager kept apart.
	got := s.waitFor(t, 30*time.Second, func(ds []delivery) bool {
		seen := map[string]bool{}
		for _, d := range ds {
			for _, a := range d.Alerts {
				if a.Labels["run_id"] == runID {
					seen[a.Labels["source"]] = true
				}
			}
		}
		return len(seen) == 2
	})

	clusters := map[string]string{}
	var found bool
	for _, d := range got {
		for _, a := range d.Alerts {
			if a.Labels["run_id"] != runID {
				continue
			}
			found = true
			clusters[a.Labels["source"]] = a.Labels["cluster"]

			if a.Labels["alertname"] != "CheckoutIsSlow" {
				t.Errorf("delivered alertname = %q, want CheckoutIsSlow", a.Labels["alertname"])
			}

			// The team label is the point of the directory derivation: it is
			// what the route tree keys on, and nothing in the file set it.
			if a.Labels["team"] != "payments" {
				t.Errorf("delivered team = %q, want payments", a.Labels["team"])
			}
			if a.Labels["ServiceName"] != "checkout" {
				t.Errorf("delivered ServiceName = %q, want checkout", a.Labels["ServiceName"])
			}
			if a.Status != "firing" {
				t.Errorf("delivered status = %q, want firing", a.Status)
			}
			// Rendered, not the raw template.
			if want := "checkout is slow"; a.Annotations["summary"] != want {
				t.Errorf("delivered summary = %q, want %q", a.Annotations["summary"], want)
			}
		}
	}
	if !found {
		t.Fatalf("no alert for run %s delivered: %+v", runID, got)
	}

	// The source label is what kept them apart, and each carried its own
	// cluster from the source's alert_labels (spec 6.10.1).
	want := map[string]string{"otel_traces_dc1": "dc1", "otel_traces_dc2": "dc2"}
	if len(clusters) != len(want) {
		t.Fatalf("delivered sources = %v, want one alert per source %v", clusters, want)
	}
	for src, cluster := range want {
		if clusters[src] != cluster {
			t.Errorf("source %s delivered cluster %q, want %q", src, clusters[src], cluster)
		}
	}
}

func loadSet(t *testing.T, chAddr string) *ruleset.Set {
	t.Helper()

	path := filepath.Join("testdata", "sources.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading sources: %v", err)
	}
	env := func(name string) (string, bool) {
		switch name {
		case "RULER_CLICKHOUSE_ADDR":
			return chAddr, true
		case "RULER_CLICKHOUSE_PASSWORD":
			// Matches compose.yaml. Dev only, and the stack binds localhost.
			return "ruler", true
		}
		return "", false
	}
	sources, problems := source.Parse(path, data, env)
	if len(problems) != 0 {
		t.Fatalf("sources fixture problems: %v", problems)
	}

	set, problems := ruleset.Load(filepath.Join("testdata", "rules"), sources, nil)
	if len(problems) != 0 {
		t.Fatalf("rules fixture problems: %v", problems)
	}

	// Only address is overridden, and only so the justfile can point the test
	// at a different host. Everything else comes from the fixture.
	for i := range set.Rules {
		for j := range set.Rules[i].Sources {
			set.Rules[i].Sources[j].Address = chAddr
		}
	}
	return set
}

// seed writes one slow checkout span and returns the evaluation time.
//
// It opens its own connection rather than borrowing the Querier's. Writing is
// not something the ruler ever does, so Querier has no Exec and should not
// grow one just to make a test shorter.
func seed(t *testing.T, src source.Source) time.Time {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{src.Address},
		Auth: clickhouse.Auth{
			Database: src.Database,
			Username: src.Username,
			Password: src.Password,
		},
	})
	if err != nil {
		t.Fatalf("opening seed connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	now := time.Now().UTC()
	if err := conn.Exec(ctx, "TRUNCATE TABLE otel.otel_traces"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	batch, err := conn.PrepareBatch(ctx,
		"INSERT INTO otel.otel_traces (Timestamp, TraceId, SpanId, ServiceName, SpanName, Duration, StatusCode)")
	if err != nil {
		t.Fatalf("prepare batch: %v", err)
	}
	if err := batch.Append(now.Add(-30*time.Second), "trace", "span", "checkout", "GET /", uint64(5_000_000_000), "Ok"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}
	return now
}
