package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/scheduler"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// An unparseable --log-level exits 2 rather than defaulting, and an
// Alertmanager URL that no send could succeed against is wrong in the same
// way: silence an operator cannot account for (spec 8.1).
//
// The parsing itself is internal/source's, tested there against twelve
// refusals. What this asserts is that a refusal reaches the exit code, because
// a finding nothing acts on is a ruler that starts with nowhere to send.
func TestRunRejectsAMalformedAlertmanagerURL(t *testing.T) {
	for _, in := range []string{"localhost:9093", "ftp://localhost:9093", "http://"} {
		dir := fixture(t, bareRule, "")
		operatorFile(t, dir, alertmanagerURLs(in))

		code, stderr := runRunCmd(t, "run",
			"--rules", filepath.Join(dir, "rules"),
			"--config", filepath.Join(dir, "ruler.yaml"))

		if code == exitOK {
			t.Errorf("urls [%s]: exit = %d, want a refusal\n%s", in, code, stderr)
		}
		if !strings.Contains(stderr, "alertmanager") {
			t.Errorf("urls [%s]: expected the reason on stderr, got:\n%s", in, stderr)
		}
	}
}

// A ruler with nowhere to send refuses to start, which is what the required
// --alertmanager flag used to say. Not a finding: the file was read and
// nothing in it is malformed, there is simply no job this process could do
// (spec 6.5).
func TestRunRefusesWithNoAlertmanager(t *testing.T) {
	dir := fixture(t, bareRule, "")
	operatorFile(t, dir, alertmanagerURLs())

	code, stderr := runRunCmd(t, "run",
		"--rules", filepath.Join(dir, "rules"),
		"--config", filepath.Join(dir, "ruler.yaml"))

	if code != exitUsage {
		t.Errorf("exit = %d, want exitUsage with no alertmanagers block\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "names no alertmanager") {
		t.Errorf("expected the reason on stderr, got:\n%s", stderr)
	}
}

// probeFunc adapts a plain function to what the loop asks of a client.
type probeFunc func(context.Context) error

func (f probeFunc) Probe(ctx context.Context) error { return f(ctx) }

func testProbe(t *testing.T, p prober) (*alertmanagerProbe, *scheduler.Metrics, *bytes.Buffer) {
	t.Helper()

	var logs bytes.Buffer
	metrics := scheduler.NewMetrics(prometheus.NewRegistry())

	return &alertmanagerProbe{
		client:   p,
		url:      "http://alertmanager:9093",
		metrics:  metrics,
		log:      slog.New(slog.NewTextHandler(&logs, nil)),
		interval: time.Hour,
		timeout:  time.Second,
	}, metrics, &logs
}

func probeGauge(t *testing.T, m *scheduler.Metrics) float64 {
	t.Helper()
	return testutil.ToFloat64(m.AlertmanagerLastProbeSuccessful.WithLabelValues("http://alertmanager:9093"))
}

// Both states, because a gauge that only ever reports one of them is a gauge
// nobody can alert on (spec 8.2).
func TestAlertmanagerProbeReportsBothStates(t *testing.T) {
	var fail atomic.Bool
	p, metrics, logs := testProbe(t, probeFunc(func(context.Context) error {
		if fail.Load() {
			return errors.New("dial tcp 10.0.0.1:9093: connect: connection refused")
		}
		return nil
	}))

	ctx := context.Background()

	answering := p.probeOnce(ctx, true)
	if !answering {
		t.Error("probeOnce = false against a client that answered")
	}
	if got := probeGauge(t, metrics); got != 1 {
		t.Errorf("gauge = %v, want 1 while the alertmanager answers", got)
	}

	fail.Store(true)
	if answering = p.probeOnce(ctx, answering); answering {
		t.Error("probeOnce = true against a client that did not answer")
	}
	if got := probeGauge(t, metrics); got != 0 {
		t.Errorf("gauge = %v, want 0 once the alertmanager stopped answering", got)
	}
	if !strings.Contains(logs.String(), "did not answer its probe") {
		t.Errorf("expected a line saying the probe failed, got:\n%s", logs)
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Errorf("expected the request error on the line, got:\n%s", logs)
	}

	fail.Store(false)
	if answering = p.probeOnce(ctx, answering); !answering {
		t.Error("probeOnce = false once the alertmanager answered again")
	}
	if got := probeGauge(t, metrics); got != 1 {
		t.Errorf("gauge = %v, want 1 once the alertmanager answered again", got)
	}
	if !strings.Contains(logs.String(), "answered its probe again") {
		t.Errorf("expected a recovery line, got:\n%s", logs)
	}
}

// A probe every 30 seconds is 2,880 lines a day against an Alertmanager that
// is down, which buries the line that says when it went down (spec 8.4).
func TestAlertmanagerProbeLogsOnlyOnAChangeOfState(t *testing.T) {
	p, _, logs := testProbe(t, probeFunc(func(context.Context) error {
		return errors.New("connection refused")
	}))

	ctx := context.Background()
	answering := p.probeOnce(ctx, true)
	for i := 0; i < 5; i++ {
		answering = p.probeOnce(ctx, answering)
	}

	if got := strings.Count(logs.String(), "did not answer its probe"); got != 1 {
		t.Errorf("got %d failure lines, want 1: only the change of state is a line", got)
	}
}

// A ruler whose Alertmanager answered from the start says so without a line,
// because the gauge is the answer and nothing changed.
func TestAlertmanagerProbeIsQuietWhileItAnswers(t *testing.T) {
	p, _, logs := testProbe(t, probeFunc(func(context.Context) error { return nil }))

	answering := p.probeOnce(context.Background(), true)
	_ = p.probeOnce(context.Background(), answering)

	if logs.Len() != 0 {
		t.Errorf("expected no log lines while the alertmanager answers, got:\n%s", logs)
	}
}

// Shutdown cancels the probe's context, and a cancelled request is not an
// Alertmanager that stopped answering: logging one would put a warning and a
// zero on every clean shutdown.
func TestAlertmanagerProbeIgnoresItsOwnShutdown(t *testing.T) {
	p, _, logs := testProbe(t, probeFunc(func(ctx context.Context) error {
		return ctx.Err()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := p.probeOnce(ctx, true); !got {
		t.Error("probeOnce = false on a cancelled context, want the previous state kept")
	}
	if logs.Len() != 0 {
		t.Errorf("expected no log lines on a cancelled probe, got:\n%s", logs)
	}
}

// The loop probes once before its first tick, so the gauge has a reading from
// startup rather than one interval later.
func TestAlertmanagerProbeRunsBeforeItsFirstTick(t *testing.T) {
	var probes atomic.Int32
	p, metrics, _ := testProbe(t, probeFunc(func(context.Context) error {
		probes.Add(1)
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for probes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if probes.Load() == 0 {
		t.Fatal("the loop probed nothing before its first tick")
	}
	if got := probeGauge(t, metrics); got != 1 {
		t.Errorf("gauge = %v, want 1 after the first probe", got)
	}
}

// The wiring, against a real server: what the ruler builds from the flag is a
// client that probes the URL it was given.
func TestAlertmanagerProbeAsksTheConfiguredURL(t *testing.T) {
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, metrics, _ := testProbe(t, notify.NewClient(srv.URL))
	p.url = srv.URL

	if got := p.probeOnce(context.Background(), true); !got {
		t.Fatal("probeOnce = false against a server that answered")
	}
	if got, _ := path.Load().(string); got != "/-/ready" {
		t.Errorf("probed %q, want /-/ready", got)
	}
	if got := testutil.ToFloat64(metrics.AlertmanagerLastProbeSuccessful.WithLabelValues(srv.URL)); got != 1 {
		t.Errorf("gauge = %v, want 1", got)
	}
}

// The same address twice is one series on everything labelled `alertmanager`,
// so a failure counter would count two endpoints as one. The refusal itself is
// internal/source's; what this asserts is that it reaches the exit code
// (spec 6.5).
func TestRunRejectsADuplicateAlertmanagerURL(t *testing.T) {
	dir := fixture(t, bareRule, "")
	operatorFile(t, dir, alertmanagerURLs("http://localhost:9093", "http://localhost:9093/"))

	code, stderr := runRunCmd(t, "run",
		"--rules", filepath.Join(dir, "rules"),
		"--config", filepath.Join(dir, "ruler.yaml"))

	if code == exitOK {
		t.Errorf("exit = %d, want a refusal\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "given twice") {
		t.Errorf("expected the reason on stderr, got:\n%s", stderr)
	}
}

// One probe per endpoint and one series per endpoint, so a cluster with one
// member down says which member (spec 6.5, 8.2).
func TestEveryAlertmanagerReportsItsOwnProbeGauge(t *testing.T) {
	answering := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer answering.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()

	set := source.Alertmanager{URLs: []string{answering.URL, down.URL}}

	metrics := scheduler.NewMetrics(prometheus.NewRegistry())
	probes := newAlertmanagerProbes(alertmanagerEndpoints(set), metrics, slog.New(slog.DiscardHandler))
	if len(probes) != 2 {
		t.Fatalf("got %d probes, want one per endpoint", len(probes))
	}
	for _, p := range probes {
		p.probeOnce(context.Background(), true)
	}

	if got := testutil.ToFloat64(metrics.AlertmanagerLastProbeSuccessful.WithLabelValues(answering.URL)); got != 1 {
		t.Errorf("gauge for the endpoint that answered = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.AlertmanagerLastProbeSuccessful.WithLabelValues(down.URL)); got != 0 {
		t.Errorf("gauge for the endpoint that did not answer = %v, want 0", got)
	}
}
