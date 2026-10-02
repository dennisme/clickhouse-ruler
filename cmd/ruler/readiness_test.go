package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dennisme/clickhouse-ruler/internal/scheduler"
)

// blockingPinger answers only when it is released, standing in for a cluster
// that has stopped answering rather than refusing.
type blockingPinger struct{ release chan struct{} }

func (p blockingPinger) Ping(ctx context.Context) error {
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A reload holds the runner's lock from the first connection it opens until
// every in-flight evaluation of the previous configuration has finished. A
// probe that waits on any of that reports on the reload rather than on the
// ruler, and kubelet gives up first, so a SIGHUP behind one slow cluster takes
// a replica out of service while it evaluates perfectly (spec 8.1).
func TestReadinessAnswersWhileAReloadHoldsTheLock(t *testing.T) {
	dir := fixture(t, bareRule, "")
	r := startRunner(t, dir)

	// What connect does for the whole of its run.
	r.mu.Lock()
	defer r.mu.Unlock()

	answered := make(chan error, 1)
	go func() { answered <- r.ready(context.Background()) }()

	select {
	case <-answered:
	case <-time.After(2 * time.Second):
		t.Fatal("readiness did not answer while a reload held the lock")
	}
}

// The probe's budget has to fit inside what the supervisor gives it, or the
// body carrying the reason never arrives and the probe may as well have
// answered a bare 503 (spec 8.1).
func TestReadinessBudgetFitsTheChartProbeTimeout(t *testing.T) {
	allowed, err := chartProbeTimeout()
	if err != nil {
		t.Fatalf("reading the chart: %v", err)
	}

	if scheduler.ReadyTimeout >= allowed {
		t.Errorf("the handler allows itself %s and the chart's probe allows %s: kubelet hangs up first, "+
			"so the reason never reaches whoever is rolling out",
			scheduler.ReadyTimeout, allowed)
	}
}

// Asked in sequence, one cluster that hangs spends the whole budget before the
// second is tried, so which source map iteration happened to reach first
// decides whether a healthy ruler reports ready (spec 8.1).
func TestReadinessPingsEverySourceAtOnce(t *testing.T) {
	hangs := blockingPinger{release: make(chan struct{})}
	defer close(hangs.release)

	sources := map[string]pinger{}
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		sources[name] = hangs
	}
	sources["answers"] = fakePinger{}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	if err := readiness(1, sources)(ctx); err != nil {
		t.Errorf("readiness = %v, want ready: one source answered and the rest only hang", err)
	}
}

// The reason is what the body exists to carry, so a ruler nothing answers for
// still has to say which of the two things is wrong.
func TestReadinessStillNamesWhyItIsNotReady(t *testing.T) {
	down := fakePinger{err: errors.New("connection refused")}

	err := readiness(2, map[string]pinger{"a": down})(context.Background())
	if err == nil {
		t.Fatal("readiness = nil against a source that refuses, want an error")
	}

	if err := readiness(0, map[string]pinger{"a": fakePinger{}})(context.Background()); err == nil {
		t.Fatal("readiness = nil with no rules loaded, want an error")
	}
}

// A probe can hold the previous state while the reload closes what the new one
// replaced, so pinging a closed connection is a normal event rather than a
// fault. It must report as a source that did not answer, and must not race.
func TestReadinessToleratesAQuerierTheReloadClosed(t *testing.T) {
	dir := fixture(t, bareRule, "")
	r := startRunner(t, dir)

	stale := toPingers(r.queriers)
	if len(stale) == 0 {
		t.Fatal("the fixture opened no sources, so there is nothing to close")
	}
	closeQueriers(r.queriers)

	if err := readiness(1, stale)(context.Background()); err == nil {
		t.Error("readiness = nil against closed connections, want the reason")
	}
}

// What the probe answers during a reload is the configuration that is running,
// so the state it reads is published when the scheduler starts evaluating it
// and not when the files were read (spec 8.1).
func TestReadinessStateIsPublishedByTheReload(t *testing.T) {
	dir := fixture(t, bareRule, reloadPolicy)
	r := startRunner(t, dir)

	before := r.readyState.Load()
	if before == nil || before.rules == 0 {
		t.Fatalf("startup published %+v, want the rules it loaded", before)
	}

	writeRuleFile(t, dir, "errors.yaml", addedRule)
	cfg, err := r.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Read but not applied: the running configuration is still the old one.
	if got := r.readyState.Load(); got != before {
		t.Error("the published state changed when the files were read, before anything ran them")
	}

	if err := r.connect(context.Background(), cfg); err != nil {
		t.Fatalf("connect: %v", err)
	}

	after := r.readyState.Load()
	if after == before {
		t.Fatal("the reload did not publish a new state")
	}
	if after.rules <= before.rules {
		t.Errorf("published %d rules after adding one to %d", after.rules, before.rules)
	}
}

// chartProbeTimeout is what the chart allows a readiness probe, read from the
// file an operator deploys rather than from a number written twice.
func chartProbeTimeout() (time.Duration, error) {
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "chart", "clickhouse-ruler", "values.yaml"))
	if err != nil {
		return 0, err
	}

	var values struct {
		ReadinessProbe struct {
			TimeoutSeconds int `yaml:"timeoutSeconds"`
		} `yaml:"readinessProbe"`
	}
	if err := yaml.Unmarshal(data, &values); err != nil {
		return 0, err
	}
	if values.ReadinessProbe.TimeoutSeconds == 0 {
		return 0, errors.New("the chart states no readinessProbe.timeoutSeconds")
	}
	return time.Duration(values.ReadinessProbe.TimeoutSeconds) * time.Second, nil
}
