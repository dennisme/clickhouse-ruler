package datapoints

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The anchor every case below is written around. Seeded rows are placed
// relative to the clock rather than to a date, because the dev schema TTLs at
// three days and a literal date expires where it stands.
func anchor() time.Time { return time.Now().Add(-5 * time.Minute).Truncate(time.Second) }

// A cumulative monotonic sum is the shape the counter idioms turn on, so the
// payload has to carry both columns that say which it is: ClickHouse reads
// them into AggregationTemporality and IsMonotonic, and a rule's correctness
// depends on them.
func TestCumulativeSumCarriesTemporalityAndMonotonicity(t *testing.T) {
	start := anchor()
	s := Scenario{
		Endpoint:    "http://127.0.0.1:4318",
		ServiceName: "checkout",
		MetricName:  "http_server_errors_total",
		Kind:        Sum,
		Temporality: Cumulative,
		Monotonic:   true,
		Attributes:  map[string]string{"http.route": "/pay"},
		Points: []Point{
			{Time: start, Value: 0},
			{Time: start.Add(time.Minute), Value: 7},
		},
	}

	body, err := json.Marshal(s.payload())
	if err != nil {
		t.Fatalf("encoding the payload: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the payload is not JSON: %v\n%s", err, body)
	}

	metric := soleMetric(t, got)
	sum, ok := metric["sum"].(map[string]any)
	if !ok {
		t.Fatalf("no sum on the metric: %v", metric)
	}
	if sum["isMonotonic"] != true {
		t.Errorf("isMonotonic = %v, want true", sum["isMonotonic"])
	}
	if sum["aggregationTemporality"] != float64(Cumulative) {
		t.Errorf("aggregationTemporality = %v, want %d", sum["aggregationTemporality"], Cumulative)
	}
	if _, ok := metric["gauge"]; ok {
		t.Error("a sum carries a gauge as well, which would write the point into both tables")
	}

	points, ok := sum["dataPoints"].([]any)
	if !ok || len(points) != 2 {
		t.Fatalf("dataPoints = %v, want the 2 the scenario named", sum["dataPoints"])
	}
	first, _ := points[0].(map[string]any)
	if first["timeUnixNano"] != strconv.FormatInt(start.UnixNano(), 10) {
		t.Errorf("timeUnixNano = %v, want the point's own time", first["timeUnixNano"])
	}
	if first["startTimeUnixNano"] != strconv.FormatInt(start.UnixNano(), 10) {
		t.Errorf("startTimeUnixNano = %v, want the first point's time by default", first["startTimeUnixNano"])
	}
	if first["asDouble"] != float64(0) {
		t.Errorf("asDouble = %v, want 0", first["asDouble"])
	}
}

// A cumulative counter resets when the process exporting it restarts, which is
// the case that makes `max(Value) - min(Value)` across a restart read as
// negative. A scenario says so by moving the series start under later points.
func TestARestartMovesTheSeriesStartUnderTheLaterPoints(t *testing.T) {
	start := anchor()
	restart := start.Add(2 * time.Minute)

	s := Scenario{
		Endpoint:    "http://127.0.0.1:4318",
		ServiceName: "checkout",
		MetricName:  "http_server_errors_total",
		Kind:        Sum,
		Temporality: Cumulative,
		Monotonic:   true,
		Points: []Point{
			{Time: start, Value: 10},
			{Time: start.Add(time.Minute), Value: 40},
			{Time: restart, Value: 2, SeriesStart: restart},
		},
	}

	body, err := json.Marshal(s.payload())
	if err != nil {
		t.Fatalf("encoding the payload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the payload is not JSON: %v", err)
	}

	sum := soleMetric(t, got)["sum"].(map[string]any)
	points := sum["dataPoints"].([]any)
	if len(points) != 3 {
		t.Fatalf("dataPoints = %d, want 3", len(points))
	}

	before := points[1].(map[string]any)
	after := points[2].(map[string]any)
	if before["startTimeUnixNano"] != strconv.FormatInt(start.UnixNano(), 10) {
		t.Errorf("the point before the restart starts at %v, want the first point's time", before["startTimeUnixNano"])
	}
	if after["startTimeUnixNano"] != strconv.FormatInt(restart.UnixNano(), 10) {
		t.Errorf("the point after the restart starts at %v, want the restart", after["startTimeUnixNano"])
	}
}

// A gauge has neither column, so the table it lands in has neither. Sending a
// gauge as a sum is one of the mistakes a rule over these tables makes, and an
// emitter that quietly allowed it could not describe the mistake.
func TestAGaugeCarriesNeitherSumColumn(t *testing.T) {
	start := anchor()
	s := Scenario{
		Endpoint:    "http://127.0.0.1:4318",
		ServiceName: "checkout",
		MetricName:  "queue_depth",
		Kind:        Gauge,
		Points:      []Point{{Time: start, Value: 12}},
	}

	body, err := json.Marshal(s.payload())
	if err != nil {
		t.Fatalf("encoding the payload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the payload is not JSON: %v", err)
	}

	metric := soleMetric(t, got)
	if _, ok := metric["gauge"]; !ok {
		t.Fatalf("no gauge on the metric: %v", metric)
	}
	if _, ok := metric["sum"]; ok {
		t.Error("a gauge carries a sum as well, which would write the point into the sum table")
	}
}

func TestScenarioRefusesWhatCannotBeAsserted(t *testing.T) {
	start := anchor()
	ok := Scenario{
		Endpoint:    "http://127.0.0.1:4318",
		ServiceName: "checkout",
		MetricName:  "http_server_errors_total",
		Kind:        Sum,
		Temporality: Cumulative,
		Points:      []Point{{Time: start, Value: 1}},
	}

	tests := []struct {
		name  string
		edit  func(*Scenario)
		wants string
	}{
		{name: "no endpoint", edit: func(s *Scenario) { s.Endpoint = "" }, wants: "endpoint"},
		{name: "no service name", edit: func(s *Scenario) { s.ServiceName = "" }, wants: "service name"},
		{name: "no metric name", edit: func(s *Scenario) { s.MetricName = "" }, wants: "metric name"},
		{name: "no points", edit: func(s *Scenario) { s.Points = nil }, wants: "point"},
		{name: "a point with no time", edit: func(s *Scenario) { s.Points = []Point{{Value: 1}} }, wants: "time"},
		{name: "a sum with no temporality", edit: func(s *Scenario) { s.Temporality = 0 }, wants: "temporality"},
		{
			name:  "a gauge given a temporality",
			edit:  func(s *Scenario) { s.Kind = Gauge; s.Monotonic = true },
			wants: "gauge",
		},
	}

	for _, tc := range tests {
		s := ok
		tc.edit(&s)
		err := s.valid()
		if err == nil {
			t.Errorf("%s: valid() returned nothing, want a refusal", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%s: valid() = %q, want it to name %q", tc.name, err, tc.wants)
		}
	}

	if err := ok.valid(); err != nil {
		t.Errorf("valid() refused a scenario that is fine: %v", err)
	}
}

// One request per scenario, for the reason spec 9.2 gives: the table holds
// every point or none of them, so a test that found fewer rows than it asked
// for is looking at a collector that dropped them.
func soleMetric(t *testing.T, got map[string]any) map[string]any {
	t.Helper()

	resource, ok := got["resourceMetrics"].([]any)
	if !ok || len(resource) != 1 {
		t.Fatalf("resourceMetrics = %v, want exactly one", got["resourceMetrics"])
	}
	scopes, ok := resource[0].(map[string]any)["scopeMetrics"].([]any)
	if !ok || len(scopes) != 1 {
		t.Fatalf("scopeMetrics = %v, want exactly one", resource[0])
	}
	metrics, ok := scopes[0].(map[string]any)["metrics"].([]any)
	if !ok || len(metrics) != 1 {
		t.Fatalf("metrics = %v, want exactly one", scopes[0])
	}
	return metrics[0].(map[string]any)
}
