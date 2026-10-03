//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/datapoints"
	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// The three query shapes a rule over a cumulative counter can have, and the
// reason this slice has tests at all: the first one looks right, passes every
// check in the tree, and alerts on the wrong thing. They are formatted with a
// service name because every assertion in a shared table is made on one, and
// they are otherwise the SQL on the metrics page of the site.
//
// `AggregationTemporality` is in the two correct ones because it is the column
// that decides which of them is correct. A series exporting the other
// temporality under the same metric name would otherwise be read by the wrong
// arithmetic and joined into the same number.
const (
	// The wrong shape. `Value` on a cumulative sum is the total since the
	// exporting process started, so a threshold on it fires for as long as
	// the process lives and starts again from nothing when it restarts.
	rawCounterShape = `SELECT ServiceName, max(Value) AS value
FROM otel_metrics_sum
WHERE MetricName = 'http_server_errors_total'
  AND ServiceName = '%s'
  AND TimeUnix >= {{ .From }} AND TimeUnix < {{ .To }}
GROUP BY ServiceName
HAVING value > 1000`

	// Right for a cumulative sum. The delta is taken per series and summed
	// after that: subtracting across two series is nonsense, and
	// `StartTimeUnix` is in the grouping because a restart is a new series
	// under the same identity, so a subtraction spanning one counts the
	// previous run's total as if it had just arrived.
	perSeriesDeltaShape = `SELECT ServiceName, sum(delta) AS value
FROM (
  SELECT ServiceName, Attributes, StartTimeUnix, max(Value) - min(Value) AS delta
  FROM otel_metrics_sum
  WHERE MetricName = 'http_server_errors_total'
    AND ServiceName = '%s'
    AND AggregationTemporality = 2
    AND TimeUnix >= {{ .From }} AND TimeUnix < {{ .To }}
  GROUP BY ServiceName, Attributes, StartTimeUnix
)
GROUP BY ServiceName
HAVING value > 1000`

	// Right for a delta sum, and wrong for a cumulative one. Each row is
	// already an increment, so the increase over a window is their sum and a
	// subtraction would return nothing of interest.
	deltaSumShape = `SELECT ServiceName, sum(Value) AS value
FROM otel_metrics_sum
WHERE MetricName = 'http_server_errors_total'
  AND ServiceName = '%s'
  AND AggregationTemporality = 1
  AND TimeUnix >= {{ .From }} AND TimeUnix < {{ .To }}
GROUP BY ServiceName
HAVING value > 1000`
)

// The scenario both tests read, as offsets from a base the test places
// relative to the clock. Errors rise over the first two minutes, nothing
// happens for six, and then the exporting process restarts and ten more
// arrive.
//
// Every window below is counterWindow long and ends at the named offset, so
// each one holds a different part of that story while the rows never change.
const (
	counterWindow = 3 * time.Minute

	// The point cadence. A window shorter than this holds one point per
	// series, which is the trap shortWindow exists to demonstrate.
	exportInterval = time.Minute
	shortWindow    = 30 * time.Second

	riseEnd       = 3 * time.Minute   // the first three points: 0, 500, 1200
	midRise       = 150 * time.Second // one point, inside the rise
	quiet         = 9 * time.Minute   // three flat points, nothing increasing
	acrossRestart = 11 * time.Minute  // the flat tail, then the restarted series
	afterRestart  = 12 * time.Minute  // the restarted series alone: 0, 5, 10
)

// emitCumulativeCounter posts one monotonic cumulative series: 1200 errors
// over the first two minutes, six minutes of nothing, then a restart and ten
// more errors. It returns once the collector has written every point.
func emitCumulativeCounter(t *testing.T, endpoint, addr, service string, base time.Time) {
	t.Helper()

	points := []datapoints.Point{
		{Time: base, Value: 0},
		{Time: base.Add(exportInterval), Value: 500},
		{Time: base.Add(2 * exportInterval), Value: 1200},
	}
	// The counter holds its total while nothing increments it, which is what
	// keeps a threshold on the raw value firing.
	for i := 3; i <= 8; i++ {
		points = append(points, datapoints.Point{
			Time:  base.Add(time.Duration(i) * exportInterval),
			Value: 1200,
		})
	}
	// The restart. The series start moves, the total begins again from
	// nothing, and ten errors arrive that no reader of the raw value sees.
	restart := base.Add(9 * exportInterval)
	for i, v := range []float64{0, 5, 10} {
		points = append(points, datapoints.Point{
			Time:        base.Add(time.Duration(9+i) * exportInterval),
			Value:       v,
			SeriesStart: restart,
		})
	}

	s := datapoints.Scenario{
		Endpoint:    endpoint,
		ServiceName: service,
		MetricName:  "http_server_errors_total",
		Kind:        datapoints.Sum,
		Temporality: datapoints.Cumulative,
		Monotonic:   true,
		Attributes:  map[string]string{"http.route": "/pay"},
		Points:      points,
	}
	if err := s.Emit(context.Background()); err != nil {
		t.Fatalf("emitting the cumulative counter: %v", err)
	}
	waitForMetricRows(t, addr, service, len(points))
}

// emitDeltaCounter posts the same count of errors as delta temporality: three
// points of 400, each one an increment rather than a total.
func emitDeltaCounter(t *testing.T, endpoint, addr, service string, base time.Time) {
	t.Helper()

	s := datapoints.Scenario{
		Endpoint:    endpoint,
		ServiceName: service,
		MetricName:  "http_server_errors_total",
		Kind:        datapoints.Sum,
		Temporality: datapoints.Delta,
		Monotonic:   true,
		Attributes:  map[string]string{"http.route": "/pay"},
		Points: []datapoints.Point{
			{Time: base, Value: 400},
			{Time: base.Add(exportInterval), Value: 400},
			{Time: base.Add(2 * exportInterval), Value: 400},
		},
	}
	if err := s.Emit(context.Background()); err != nil {
		t.Fatalf("emitting the delta counter: %v", err)
	}
	waitForMetricRows(t, addr, service, len(s.Points))
}

// counterRule renders one rule over the metrics source, with the shape's
// service substituted in.
func counterRule(alertName, shape, service string, window time.Duration) string {
	// expr is a YAML block scalar, so every line of the shape carries the
	// block's indentation.
	expr := strings.ReplaceAll(fmt.Sprintf(shape, service), "\n", "\n          ")

	return fmt.Sprintf(`      - alert: %s
        sources:
          signal: metrics
        expr: |
          %s
        window: %s
        labels:
          team: payments
          severity: warning
        annotations:
          summary: "{{ .ServiceName }} is over the threshold at {{ .value }}"
`, alertName, expr, window)
}

// loadCounterRules writes the rules and the metrics source to a temporary
// tree and loads them the way the ruler does, so the SQL under test has been
// through every offline check before anything evaluates it.
func loadCounterRules(t *testing.T, addr string, rules ...string) map[string]ruleset.Rule {
	t.Helper()

	dir := t.TempDir()
	rulesDir := filepath.Join(dir, "rules", "payments")
	if err := os.MkdirAll(rulesDir, 0o750); err != nil {
		t.Fatal(err)
	}

	body := "groups:\n  - name: counters\n    interval: 1m\n    rules:\n" + strings.Join(rules, "")
	if err := os.WriteFile(filepath.Join(rulesDir, "counters.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(dir, "ruler.yaml")
	sources := "sources:\n" +
		"  - name: otel_metrics\n" +
		"    labels: {team: payments, signal: metrics}\n" +
		"    address: " + addr + "\n" +
		"    database: otel\n" +
		"    username: ruler_metrics\n" +
		"    table: otel_metrics_sum\n" +
		"    timestamp_column: TimeUnix\n" +
		// The rows were waited for, so there is no ingestion lag left to
		// absorb and the window a test names is the window it gets.
		"    evaluation_delay: 0s\n"
	if err := os.WriteFile(configPath, []byte(sources), 0o600); err != nil {
		t.Fatal(err)
	}

	parsed, problems := source.Parse(configPath, []byte(sources), nil)
	for _, p := range problems {
		t.Fatalf("the metrics source did not load: %s: %s", p.Check, p.Text)
	}

	set, problems := ruleset.Load(filepath.Join(dir, "rules"), parsed, nil)
	for _, p := range problems {
		// A convention warning is not what these tests are about: a counter
		// rule without a runbook is still a counter rule. An error means the
		// SQL on the metrics page would not load.
		if p.Severity == lint.SeverityError {
			t.Fatalf("rule %q: %s: %s", p.Subject, p.Check, p.Text)
		}
	}
	if len(set.Rules) != len(rules) {
		t.Fatalf("loaded %d rules, wrote %d", len(set.Rules), len(rules))
	}

	out := make(map[string]ruleset.Rule, len(set.Rules))
	for _, r := range set.Rules {
		if len(r.Sources) != 1 {
			t.Fatalf("rule %q matched %d sources, want the metrics source alone", r.Alert, len(r.Sources))
		}
		out[r.Alert] = r
	}
	return out
}

// evaluator is one rule against one source, with the alert state it
// accumulates. The state is what makes "fires and then stops" a statement
// about an alert rather than about a row count: the same instance has to be
// firing at one evaluation and resolved at the next.
type evaluator struct {
	t     *testing.T
	rule  ruleset.Rule
	q     *query.Querier
	state *alert.State
}

func newEvaluator(t *testing.T, r ruleset.Rule) *evaluator {
	t.Helper()

	src := r.Sources[0]
	q, err := query.Open(src, nil)
	if err != nil {
		t.Fatalf("opening the metrics source for %q: %v", r.Alert, err)
	}
	t.Cleanup(func() { _ = q.Close() })

	return &evaluator{
		t:     t,
		rule:  r,
		q:     q,
		state: alert.New(r.Rule, r.Labels, src, time.Hour),
	}
}

// at evaluates the rule as if now were the time, which is how a window is
// moved over rows that never change, and returns the one alert instance the
// scenario produces.
func (e *evaluator) at(now time.Time) alert.Alert {
	e.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	evaluation, err := e.q.Run(ctx, e.rule.Rule,
		query.Attribution{Group: e.rule.GroupID(), Team: e.rule.Team()}, now)
	if err != nil {
		e.t.Fatalf("evaluating %q: %v", e.rule.Alert, err)
	}

	alerts, annotationErrs, err := e.state.Eval(now, evaluation.Samples)
	if err != nil {
		e.t.Fatalf("state machine for %q: %v", e.rule.Alert, err)
	}
	if len(annotationErrs) != 0 {
		e.t.Fatalf("annotations for %q: %v", e.rule.Alert, annotationErrs)
	}

	switch len(alerts) {
	case 0:
		// Nothing returned and nothing ever had been, so there is no instance
		// to report a phase for.
		return alert.Alert{}
	case 1:
		return alerts[0]
	default:
		e.t.Fatalf("%q produced %d instances, want one: %+v", e.rule.Alert, len(alerts), alerts)
		return alert.Alert{}
	}
}

// The bug this slice exists to demonstrate, in the place where it is cheapest
// to believe and most expensive to meet: a correct-looking threshold on a
// cumulative counter.
//
// `Value` on a cumulative sum is the total since the exporting process
// started, so comparing it against a threshold asks whether the process has
// ever served that many errors rather than whether it is serving them now.
// The rows are the collector's: a scenario posted as OTLP, written by the
// exporter's own schema, read back by the restricted metrics user.
func TestARawCounterThresholdKeepsFiringAfterTheConditionPassed(t *testing.T) {
	endpoint := os.Getenv("RULER_OTLP_HTTP_URL")
	addr := os.Getenv("RULER_CLICKHOUSE_ADDR")
	if endpoint == "" || addr == "" {
		t.Fatal("RULER_OTLP_HTTP_URL and RULER_CLICKHOUSE_ADDR are not set, run these through `just integration`")
	}

	// Run-unique, so no other test's rows and no leftover of this one can be
	// mistaken for the scenario. Placed relative to the clock because the dev
	// schema TTLs at three days and a literal timestamp expires where it
	// stands.
	service := "counter-raw-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	base := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)
	emitCumulativeCounter(t, endpoint, addr, service, base)

	rules := loadCounterRules(t, addr,
		counterRule("ErrorsAreHigh", rawCounterShape, service, counterWindow))
	e := newEvaluator(t, rules["ErrorsAreHigh"])

	// The window the author had in mind. 1200 errors arrived inside it, so
	// this one is not the defect: the alert is right to fire.
	fired := e.at(base.Add(riseEnd))
	if fired.Phase != alert.PhaseFiring {
		t.Fatalf("the window the errors arrived in is %s, want firing: the rule is not reading the rows", fired.Phase)
	}
	if fired.Value != 1200 {
		t.Errorf("fired on %v, want 1200: the counter's total at the end of the rise", fired.Value)
	}

	// Six minutes later. Nothing has incremented the counter inside this
	// window: every point in it reads 1200, so the increase over it is zero
	// and there is nothing to page anybody about.
	still := e.at(base.Add(quiet))
	if still.Phase != alert.PhaseFiring {
		t.Errorf("a window where the counter did not move is %s, want firing: "+
			"this test exists to show the raw threshold still firing", still.Phase)
	}
	if still.Value != 1200 {
		t.Errorf("still firing on %v, want 1200: the total it reached six minutes earlier", still.Value)
	}

	// A window holding the restart, where the previous run's total is still
	// the largest value in it.
	spanning := e.at(base.Add(acrossRestart))
	if spanning.Phase != alert.PhaseFiring {
		t.Errorf("the window holding the restart is %s, want firing: "+
			"the previous run's total is the largest value in it", spanning.Phase)
	}

	// The other half of the same mistake. The process restarted, ten errors
	// have arrived since, and the total is back under the threshold, so the
	// alert resolves while the condition is true.
	after := e.at(base.Add(afterRestart))
	if after.Phase != alert.PhaseResolved {
		t.Errorf("the window after the restart is %s, want resolved: "+
			"a raw total starts again from nothing and goes quiet", after.Phase)
	}
}

// The shapes to write instead, over the same rows: a per-series delta for a
// cumulative sum, a plain sum for a delta one. The assertion the raw
// threshold cannot make is that the alert stops.
func TestAPerSeriesDeltaFiresAndThenStops(t *testing.T) {
	endpoint := os.Getenv("RULER_OTLP_HTTP_URL")
	addr := os.Getenv("RULER_CLICKHOUSE_ADDR")
	if endpoint == "" || addr == "" {
		t.Fatal("RULER_OTLP_HTTP_URL and RULER_CLICKHOUSE_ADDR are not set, run these through `just integration`")
	}

	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	cumulative := "counter-delta-" + run
	increments := "counter-increments-" + run
	base := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)

	emitCumulativeCounter(t, endpoint, addr, cumulative, base)
	emitDeltaCounter(t, endpoint, addr, increments, base)

	rules := loadCounterRules(t, addr,
		counterRule("ErrorsIncreasing", perSeriesDeltaShape, cumulative, counterWindow),
		counterRule("ErrorsIncreasingInHalfAMinute", perSeriesDeltaShape, cumulative, shortWindow),
		counterRule("IncrementsAreHigh", deltaSumShape, increments, counterWindow))

	delta := newEvaluator(t, rules["ErrorsIncreasing"])

	// The same window the raw threshold fired on, and the same answer, which
	// is why the wrong shape is hard to notice: 1200 errors arrived.
	fired := delta.at(base.Add(riseEnd))
	if fired.Phase != alert.PhaseFiring {
		t.Fatalf("the window the errors arrived in is %s, want firing", fired.Phase)
	}
	if fired.Value != 1200 {
		t.Errorf("fired on %v, want 1200: the increase across the series in that window", fired.Value)
	}

	// Where the two part company. The counter has not moved inside this
	// window, the delta is zero, the rule returns no row and the alert
	// resolves.
	stopped := delta.at(base.Add(quiet))
	if stopped.Phase != alert.PhaseResolved {
		t.Errorf("a window where the counter did not move is %s, want resolved", stopped.Phase)
	}

	// A window holding the restart. The previous run's total of 1200 is in
	// it, and grouping by StartTimeUnix is what keeps it out of the answer:
	// the rows describe five errors, so five is what the rule has to see.
	restart := delta.at(base.Add(acrossRestart))
	if restart.Phase == alert.PhaseFiring {
		t.Errorf("the window holding the restart fired on %v: the previous run's total is being counted as new",
			restart.Value)
	}

	// And the window after it, holding the restarted series alone. Ten errors
	// arrived, which is the number the rule has to see and is under the
	// threshold.
	beyond := delta.at(base.Add(afterRestart))
	if beyond.Phase == alert.PhaseFiring {
		t.Errorf("the window after the restart fired on %v: ten errors are under the threshold", beyond.Value)
	}

	// The window trap. Half a minute holds one point per series, nothing to
	// subtract, so a delta rule under the export interval is silent however
	// fast the counter is climbing.
	short := newEvaluator(t, rules["ErrorsIncreasingInHalfAMinute"])
	if got := short.at(base.Add(midRise)); got.Phase == alert.PhaseFiring {
		t.Errorf("a %s window over %s points fired on %v: the delta cannot have been measured",
			shortWindow, exportInterval, got.Value)
	}

	// The other correct shape, on a series exporting the same errors as
	// increments. Each row is already the increase, so summing them is the
	// query and subtracting would return nothing.
	sum := newEvaluator(t, rules["IncrementsAreHigh"])
	increased := sum.at(base.Add(riseEnd))
	if increased.Phase != alert.PhaseFiring {
		t.Fatalf("the delta series is %s, want firing: 1200 errors arrived as three increments", increased.Phase)
	}
	if increased.Value != 1200 {
		t.Errorf("fired on %v, want 1200: the sum of the increments in the window", increased.Value)
	}
}
