package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/scheduler"
)

// The contract check is turned off in every fixture here, so no test in this
// file sends a statement to a cluster it has not got. What a reload does with a
// source that fails the contract is the same code `ruler run` already runs at
// startup, and it needs a real ClickHouse to say anything.
const reloadPolicy = `checks:
  source/privileges:
    severity: off
`

// A second rule in a second file, so a reload has something to pick up that the
// fixture did not start with.
const addedRule = `groups:
  - name: errors
    interval: 1m
    rules:
      - alert: HighErrorRate
        sources:
          team: payments
        expr: |
          SELECT ServiceName, count() AS value
          FROM otel.otel_traces
          WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
          GROUP BY ServiceName
`

// A sources file with a second cluster nothing matches until a rule asks for
// it, which is how the reconciliation tests add and drop a connection.
const twoSourcesYAML = `sources:
  - name: otel_traces
    labels: {team: payments}
    address: 127.0.0.1:9000
    database: otel
    username: ruler
    table: otel_traces
    timestamp_column: Timestamp
  - name: otel_traces_eu
    labels: {team: checkout}
    address: 127.0.0.1:9001
    database: otel
    username: ruler
    table: otel_traces
    timestamp_column: Timestamp
`

// startRunner takes a fixture through the same three steps `ruler run` does
// before it starts ticking: load the files, connect to what the rules matched,
// and build the scheduler around both.
func startRunner(t *testing.T, dir string) *runner {
	t.Helper()

	reg := prometheus.NewRegistry()
	r := &runner{
		rulesDir:    filepath.Join(dir, "rules"),
		sourcesPath: filepath.Join(dir, "sources.yaml"),
		log:         slog.New(slog.DiscardHandler),
		stderr:      io.Discard,
		metrics:     scheduler.NewMetrics(reg),
		clock:       scheduler.NewRealClock(),
		resend:      scheduler.Resend{Interval: notify.DefaultResendInterval, Tolerance: notify.DefaultResendTolerance},
	}
	cadence := scheduler.NewCadence(
		[]scheduler.Endpoint{{Sender: notify.NewClient("http://127.0.0.1:9093"), URL: "http://127.0.0.1:9093"}},
		r.resend, r.metrics, r.clock)
	r.queue = scheduler.NewSendQueue(cadence, scheduler.DefaultNotificationQueueCapacity,
		r.metrics, r.clock, r.log)

	cfg, err := r.load()
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if err := r.connect(context.Background(), cfg); err != nil {
		t.Fatalf("initial connect: %v", err)
	}
	r.build(cfg)
	t.Cleanup(r.close)
	return r
}

// writeRuleFile adds or replaces a rule file under the fixture's rules tree.
func writeRuleFile(t *testing.T, dir, name, body string) {
	t.Helper()

	path := filepath.Join(dir, "rules", "payments", name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A rule added to the tree has to be evaluated without restarting the process,
// which is the whole of this slice.
func TestReloadPicksUpAnAddedRule(t *testing.T) {
	dir := fixture(t, bareRule, reloadPolicy)
	r := startRunner(t, dir)

	if r.rules != 1 {
		t.Fatalf("rules loaded at startup = %d, want 1", r.rules)
	}

	writeRuleFile(t, dir, "errors.yaml", addedRule)
	if err := r.reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if r.rules != 2 {
		t.Errorf("rules loaded after the reload = %d, want 2", r.rules)
	}
}

// Both gauges at startup: the ruler loaded its files, and the configuration it
// is evaluating is as old as that load (spec 8.2).
func TestStartupRecordsTheConfigReloadMetrics(t *testing.T) {
	dir := fixture(t, bareRule, reloadPolicy)
	before := time.Now().Unix()
	r := startRunner(t, dir)

	if got := testutil.ToFloat64(r.metrics.ConfigLastReloadSuccessful); got != 1 {
		t.Errorf("clickhouse_ruler_config_last_reload_successful = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.metrics.ConfigLastReloadTimestamp); got < float64(before) {
		t.Errorf("clickhouse_ruler_config_last_reload_timestamp_seconds = %v, want the time of the load", got)
	}
}

// The revision of the files being evaluated, which is the only revision the
// ruler can state: it fetches nothing and nothing hands it a commit (spec 8.2).
// One series, because it is a fact about the configuration rather than a
// dimension of anything.
func TestStartupRecordsTheConfigurationItIsRunning(t *testing.T) {
	dir := fixture(t, bareRule, reloadPolicy)
	r := startRunner(t, dir)

	cfg, err := r.load()
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	if cfg.set.Revision == "" {
		t.Fatal("the load states no revision")
	}

	if got := testutil.CollectAndCount(r.metrics.ConfigInfo); got != 1 {
		t.Errorf("clickhouse_ruler_config_info has %d series, want exactly 1", got)
	}
	got := testutil.ToFloat64(r.metrics.ConfigInfo.WithLabelValues(cfg.set.Revision, cfg.set.Dir))
	if got != 1 {
		t.Errorf("clickhouse_ruler_config_info{revision=%q, rules_root=%q} = %v, want 1",
			cfg.set.Revision, cfg.set.Dir, got)
	}
}

// A reload is a new configuration version, so the series is replaced rather
// than added to: two revisions on one ruler would read as two configurations
// running at once (spec 8.2).
func TestReloadReplacesTheConfigurationItIsRunning(t *testing.T) {
	dir := fixture(t, bareRule, reloadPolicy)
	r := startRunner(t, dir)

	before, err := r.load()
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}

	writeRuleFile(t, dir, "errors.yaml", addedRule)
	if err := r.reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	after, err := r.load()
	if err != nil {
		t.Fatalf("re-reading the fixture: %v", err)
	}
	if after.set.Revision == before.set.Revision {
		t.Fatalf("revision after a rule was added = %q, want it to have changed", after.set.Revision)
	}

	if got := testutil.CollectAndCount(r.metrics.ConfigInfo); got != 1 {
		t.Errorf("clickhouse_ruler_config_info has %d series after a reload, want exactly 1", got)
	}
	if got := testutil.ToFloat64(r.metrics.ConfigInfo.WithLabelValues(after.set.Revision, after.set.Dir)); got != 1 {
		t.Errorf("clickhouse_ruler_config_info{revision=%q} = %v, want 1 for the files now running",
			after.set.Revision, got)
	}
}

// Spec 7.6: a file nobody can read refuses the reading, and the ruler keeps
// the previous version of it. The rules that were running stay running,
// because a reload is not an opportunity to leave the ruler evaluating nothing.
func TestReloadRefusedByAnUnreadableFileKeepsTheRunningRules(t *testing.T) {
	dir := fixture(t, bareRule, reloadPolicy)
	r := startRunner(t, dir)
	stamped := testutil.ToFloat64(r.metrics.ConfigLastReloadTimestamp)

	writeRuleFile(t, dir, "latency.yaml", unreadableRule)
	err := r.reload(context.Background())

	// The reason is returned as well as logged, because a reload asked for over
	// HTTP is answered with it (spec 8.1).
	if err == nil {
		t.Fatal("reload returned no error, want the refusal's reason for whoever asked")
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("reload error = %q, want the reason the log line carries", err)
	}

	if r.rules != 1 {
		t.Errorf("rules loaded after a refused reload = %d, want the 1 that was already running", r.rules)
	}
	if got := testutil.ToFloat64(r.metrics.ConfigLastReloadSuccessful); got != 0 {
		t.Errorf("clickhouse_ruler_config_last_reload_successful = %v, want 0 after a refusal", got)
	}
	if got := testutil.ToFloat64(r.metrics.ConfigReloads.WithLabelValues("refused")); got != 1 {
		t.Errorf("clickhouse_ruler_config_reloads_total{outcome=\"refused\"} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.metrics.ConfigLastReloadTimestamp); got != stamped {
		t.Errorf("clickhouse_ruler_config_last_reload_timestamp_seconds moved to %v on a refused reload, want %v: "+
			"the timestamp is the age of what is running", got, stamped)
	}
}

// The gauge answers the last attempt, so a reload refused and then retried
// between two scrapes reads 1 and leaves no trace of the refusal. The counter
// is what carries that: attempts over time, by outcome.
func TestReloadCountsEveryAttemptByOutcome(t *testing.T) {
	dir := fixture(t, bareRule, reloadPolicy)
	r := startRunner(t, dir)

	// A startup load is a load, not a reload, so the counter starts at nothing
	// however the ruler came up.
	for _, outcome := range []string{"succeeded", "refused"} {
		if got := testutil.ToFloat64(r.metrics.ConfigReloads.WithLabelValues(outcome)); got != 0 {
			t.Errorf("clickhouse_ruler_config_reloads_total{outcome=%q} = %v at startup, want 0", outcome, got)
		}
	}

	writeRuleFile(t, dir, "latency.yaml", unreadableRule)
	if err := r.reload(context.Background()); err == nil {
		t.Fatal("reload returned no error, want the refusal")
	}

	writeRuleFile(t, dir, "latency.yaml", bareRule)
	if err := r.reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// The pair the metric exists for: the gauge is back to 1 and says nothing
	// about the refusal, and the counter still holds it.
	if got := testutil.ToFloat64(r.metrics.ConfigLastReloadSuccessful); got != 1 {
		t.Errorf("clickhouse_ruler_config_last_reload_successful = %v, want 1 after the retry", got)
	}
	if got := testutil.ToFloat64(r.metrics.ConfigReloads.WithLabelValues("refused")); got != 1 {
		t.Errorf("clickhouse_ruler_config_reloads_total{outcome=\"refused\"} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.metrics.ConfigReloads.WithLabelValues("succeeded")); got != 1 {
		t.Errorf("clickhouse_ruler_config_reloads_total{outcome=\"succeeded\"} = %v, want 1", got)
	}
}

// A finding that blocks a merge is not a finding that stops the ruler. The
// rule loads, because refusing costs every alert in the checkout and loading
// costs one rule behaving as written rather than as intended (spec 7.6).
func TestReloadLoadsARuleWhoseFindingOnlyBlocksAMerge(t *testing.T) {
	dir := fixture(t, bareRule, reloadPolicy)
	r := startRunner(t, dir)

	// A second file whose query has no time bound: rule/expr, always an error,
	// and one of the checks that no longer refuses a reading.
	writeRuleFile(t, dir, "unbounded.yaml", brokenRule)

	if err := r.reload(context.Background()); err != nil {
		t.Fatalf("reload = %v, want the rule loaded: rule/expr blocks a merge, not a start", err)
	}
	if r.rules != 2 {
		t.Errorf("rules loaded = %d, want 2: the finding should not have dropped the rule", r.rules)
	}
	if got := testutil.ToFloat64(r.metrics.ConfigLastReloadSuccessful); got != 1 {
		t.Errorf("clickhouse_ruler_config_last_reload_successful = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.metrics.ConfigReloads.WithLabelValues("succeeded")); got != 1 {
		t.Errorf("clickhouse_ruler_config_reloads_total{outcome=\"succeeded\"} = %v, want 1", got)
	}
}

// The other half of loading a rule that should not have merged: the ruler has
// to say so. Without this the rule runs and looks healthy (spec 7.6, 10.4).
func TestReloadRaisesTheLoadFindingOnTheProblemGauge(t *testing.T) {
	dir := fixture(t, bareRule, reloadPolicy)
	r := startRunner(t, dir)

	writeRuleFile(t, dir, "unbounded.yaml", brokenRule)
	if err := r.reload(context.Background()); err != nil {
		t.Fatalf("reload = %v", err)
	}

	// The label is the rule's path in the tree, not where the tree happens to
	// be mounted: the loader resolves the rules root, and nothing a deployment
	// does to that root may rename the series (spec 8.2).
	//
	// No team: this fixture's rule carries no team label, and `sources` is a
	// selector rather than one. An empty team is the honest answer when no file
	// says who owns the rule.
	got := testutil.ToFloat64(r.metrics.Problem.WithLabelValues(
		"HighLatency", lint.CheckRuleExpr, "error", "",
		filepath.Join("payments", "unbounded.yaml"), ""))
	if got != 1 {
		t.Errorf("clickhouse_ruler_problem for rule/expr = %v, want 1: the rule loaded with nobody told", got)
	}
}

// A rules file that cannot be read at all is refused the same way a finding
// refuses one: nothing about the running configuration changes.
func TestReloadRefusesAnUnreadableSourcesFile(t *testing.T) {
	dir := fixture(t, bareRule, reloadPolicy)
	r := startRunner(t, dir)

	if err := os.Remove(filepath.Join(dir, "sources.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := r.reload(context.Background()); err == nil {
		t.Error("reload returned no error, want the reason the sources file could not be read")
	}

	if r.rules != 1 {
		t.Errorf("rules loaded after an unreadable sources file = %d, want 1", r.rules)
	}
	if got := testutil.ToFloat64(r.metrics.ConfigLastReloadSuccessful); got != 0 {
		t.Errorf("clickhouse_ruler_config_last_reload_successful = %v, want 0", got)
	}
}

// A connection per source the rules reached, and no more: one opened for a
// source a reload brought in, one closed for a source nothing matches any
// more, and the connection of an unchanged source left exactly as it was.
func TestReloadReconcilesTheOpenConnections(t *testing.T) {
	dir := fixture(t, bareRule, reloadPolicy)
	if err := os.WriteFile(filepath.Join(dir, "sources.yaml"), []byte(twoSourcesYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	r := startRunner(t, dir)
	if len(r.queriers) != 1 {
		t.Fatalf("connections at startup = %d, want 1: only otel_traces is matched", len(r.queriers))
	}
	kept := r.queriers["otel_traces"]

	// A rule for the other team, which brings the second cluster in while the
	// first stays matched.
	writeRuleFile(t, dir, "errors.yaml", addedRule)
	writeRuleFile(t, dir, "checkout.yaml", checkoutRule)
	if err := r.reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if len(r.queriers) != 2 {
		t.Fatalf("connections after the reload = %d, want 2: %v", len(r.queriers), r.queriers)
	}
	if r.queriers["otel_traces"] != kept {
		t.Error("the connection to an unchanged source was replaced, so it was closed and reopened for nothing")
	}
	if r.queriers["otel_traces_eu"] == nil {
		t.Error("no connection opened for the source the reload brought in")
	}

	// Now drop both rules that reached the second cluster.
	if err := os.Remove(filepath.Join(dir, "rules", "payments", "checkout.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := r.reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if _, held := r.queriers["otel_traces_eu"]; held {
		t.Error("the connection to a source nothing matches any more is still held")
	}
	if r.queriers["otel_traces"] != kept {
		t.Error("the connection to an unchanged source was replaced by the second reload")
	}
}

// A source whose definition changed needs a new connection: the address, the
// user and the password are read from the file when the connection is opened,
// so keeping the old one would leave the ruler connected to what the file used
// to say.
func TestReloadReopensASourceWhoseDefinitionChanged(t *testing.T) {
	dir := fixture(t, bareRule, reloadPolicy)
	r := startRunner(t, dir)
	before := r.queriers["otel_traces"]

	moved := `sources:
  - name: otel_traces
    labels: {team: payments}
    address: 127.0.0.1:9002
    database: otel
    username: ruler
    table: otel_traces
    timestamp_column: Timestamp
`
	if err := os.WriteFile(filepath.Join(dir, "sources.yaml"), []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if r.queriers["otel_traces"] == before {
		t.Error("the source moved to another address and the ruler is still on the connection it opened for the old one")
	}
}

// The rule that reaches the second cluster in the reconciliation test.
const checkoutRule = `groups:
  - name: checkout
    interval: 1m
    rules:
      - alert: CheckoutIsSlow
        sources:
          team: checkout
        expr: |
          SELECT ServiceName, max(Duration) AS value
          FROM otel.otel_traces
          WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
          GROUP BY ServiceName
`
