// Package datapoints posts a known set of metric data points to an
// OpenTelemetry collector, so a test can state exactly what the metrics tables
// under a rule hold.
//
// The sibling of internal/spans, and separate from it for the reason the OTel
// exporter keeps metrics in their own tables: a span is an event with a
// duration and a data point is a reading on a series, so nothing about one
// describes the other. The reasoning in spec 9.2 is the same reasoning, down to
// the encoding: OTLP over HTTP with a JSON body, because that is what the
// protocol defines for a caller producing telemetry without taking on an SDK,
// and nothing was added to go.mod for it.
//
// What a scenario controls is the series identity, the kind, the two columns
// that say how a sum accumulates, and every point's time and value. Those are
// the columns a rule over a metrics table reads, and the point times matter
// more than when the post happened: a window is a range of timestamps.
package datapoints

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Kind is which table the points land in. The exporter keeps one table per
// metric type, so this decides the table as much as it decides the payload.
type Kind int

const (
	// Sum is otel_metrics_sum: a reading that accumulates, which is what a
	// counter is.
	Sum Kind = iota + 1

	// Gauge is otel_metrics_gauge: a reading that stands on its own.
	Gauge
)

// Temporality is the protocol's aggregation temporality, which ClickHouse
// holds as AggregationTemporality. It is the column that decides whether a
// rule reads a per-series delta or a sum, so a scenario states it rather than
// defaulting it.
type Temporality int32

const (
	// Delta is a point carrying the increment since the point before it.
	Delta Temporality = 1

	// Cumulative is a point carrying the total since the series started,
	// which is what a Prometheus-style counter exports.
	Cumulative Temporality = 2
)

// Point is one reading: when it was taken, what it read, and which run of the
// exporting process it belongs to.
type Point struct {
	// Time is the reading's own timestamp, which lands in TimeUnix and is
	// what a rule's window is a range of.
	Time time.Time

	// Value lands in Value. For a cumulative sum it is the total since
	// SeriesStart rather than an increment.
	Value float64

	// SeriesStart lands in StartTimeUnix. Zero means the first point's time,
	// which is one unbroken series. Moving it under a later point is how a
	// scenario says the exporting process restarted: the total starts again
	// from nothing, and a reader subtracting across that boundary gets a
	// negative number out of a monotonic counter.
	SeriesStart time.Time
}

// Scenario is one series of points to emit: what it is, how it accumulates,
// and every reading on it.
type Scenario struct {
	// Endpoint is the collector's OTLP HTTP address, e.g.
	// http://127.0.0.1:4318. The metrics path is appended.
	Endpoint string

	ServiceName string
	MetricName  string

	Kind Kind

	// Temporality and Monotonic belong to a sum and have no column on a
	// gauge's table, so a gauge carrying either is refused rather than
	// emitted with them dropped.
	Temporality Temporality
	Monotonic   bool

	// Attributes go on every point, and with ServiceName they are the series
	// identity a rule has to group by before it subtracts anything.
	Attributes map[string]string

	// Points are emitted in one request, so the table holds all of them or
	// none.
	Points []Point
}

// Emit posts the scenario and returns once the collector has accepted it.
// Accepted is not written: the collector batches, so a caller that needs the
// rows has to wait for them.
func (s Scenario) Emit(ctx context.Context) error {
	if err := s.valid(); err != nil {
		return err
	}

	body, err := json.Marshal(s.payload())
	if err != nil {
		return fmt.Errorf("encoding %d points of %s for %s: %w",
			len(s.Points), s.MetricName, s.ServiceName, err)
	}

	url := strings.TrimSuffix(s.Endpoint, "/") + "/v1/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request to %s: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("posting %d points of %s to %s: %w",
			len(s.Points), s.MetricName, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A rejection is reported in the body, and a caller left to find out from
	// a missing row would be debugging the wrong half of the path.
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("reading the collector's answer (HTTP %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the collector refused %d points of %s for %s: HTTP %d: %s",
			len(s.Points), s.MetricName, s.ServiceName, resp.StatusCode, strings.TrimSpace(string(answer)))
	}
	return nil
}

func (s Scenario) valid() error {
	switch {
	case s.Endpoint == "":
		return fmt.Errorf("no endpoint: nothing says where the collector is")
	case s.ServiceName == "":
		return fmt.Errorf("no service name: every assertion in a shared table is made on one")
	case s.MetricName == "":
		return fmt.Errorf("no metric name: a series is identified by it")
	case s.Kind != Sum && s.Kind != Gauge:
		return fmt.Errorf("kind is %d: a scenario names Sum or Gauge, which is the table it lands in", s.Kind)
	case len(s.Points) == 0:
		return fmt.Errorf("no points: a scenario emitting nothing proves nothing")
	}

	if s.Kind == Gauge && (s.Temporality != 0 || s.Monotonic) {
		return fmt.Errorf("a gauge carries neither a temporality nor monotonicity: " +
			"its table has no column for either, so a scenario setting one is describing a sum")
	}
	if s.Kind == Sum && s.Temporality != Delta && s.Temporality != Cumulative {
		return fmt.Errorf("a sum needs a temporality: a reader cannot tell a total from an increment without it")
	}

	for i, p := range s.Points {
		if p.Time.IsZero() {
			return fmt.Errorf("point %d has no time: what a rule's window holds depends on it", i)
		}
	}
	return nil
}

// OTLP's JSON encoding, as much of it as a data point needs. Timestamps are
// strings because the encoding sends a 64-bit integer that way, and a JSON
// number would lose nanoseconds to a float.
type (
	metricsRequest struct {
		ResourceMetrics []resourceMetrics `json:"resourceMetrics"`
	}

	resourceMetrics struct {
		Resource     resource       `json:"resource"`
		ScopeMetrics []scopeMetrics `json:"scopeMetrics"`
	}

	resource struct {
		Attributes []attribute `json:"attributes"`
	}

	scopeMetrics struct {
		Scope   scope    `json:"scope"`
		Metrics []metric `json:"metrics"`
	}

	scope struct {
		Name string `json:"name"`
	}

	metric struct {
		Name  string     `json:"name"`
		Sum   *sumData   `json:"sum,omitempty"`
		Gauge *gaugeData `json:"gauge,omitempty"`
	}

	sumData struct {
		DataPoints             []dataPoint `json:"dataPoints"`
		AggregationTemporality Temporality `json:"aggregationTemporality"`
		IsMonotonic            bool        `json:"isMonotonic"`
	}

	gaugeData struct {
		DataPoints []dataPoint `json:"dataPoints"`
	}

	dataPoint struct {
		Attributes        []attribute `json:"attributes"`
		StartTimeUnixNano string      `json:"startTimeUnixNano"`
		TimeUnixNano      string      `json:"timeUnixNano"`
		AsDouble          float64     `json:"asDouble"`
	}

	attribute struct {
		Key   string         `json:"key"`
		Value attributeValue `json:"value"`
	}

	attributeValue struct {
		StringValue string `json:"stringValue"`
	}
)

func (s Scenario) payload() metricsRequest {
	// The series start a point inherits when it names none. One unbroken run
	// of the exporting process is the ordinary case, and it is the case a
	// caller should not have to spell out on every point.
	seriesStart := s.Points[0].Time

	points := make([]dataPoint, 0, len(s.Points))
	for _, p := range s.Points {
		start := seriesStart
		if !p.SeriesStart.IsZero() {
			start = p.SeriesStart
		}
		points = append(points, dataPoint{
			Attributes:        attributesOf(s.Attributes),
			StartTimeUnixNano: strconv.FormatInt(start.UnixNano(), 10),
			TimeUnixNano:      strconv.FormatInt(p.Time.UnixNano(), 10),
			AsDouble:          p.Value,
		})
	}

	m := metric{Name: s.MetricName}
	switch s.Kind {
	case Sum:
		m.Sum = &sumData{
			DataPoints:             points,
			AggregationTemporality: s.Temporality,
			IsMonotonic:            s.Monotonic,
		}
	case Gauge:
		m.Gauge = &gaugeData{DataPoints: points}
	}

	return metricsRequest{ResourceMetrics: []resourceMetrics{{
		Resource: resource{Attributes: []attribute{{
			Key:   "service.name",
			Value: attributeValue{StringValue: s.ServiceName},
		}}},
		ScopeMetrics: []scopeMetrics{{
			Scope:   scope{Name: "clickhouse-ruler/datapoints"},
			Metrics: []metric{m},
		}},
	}}}
}

func attributesOf(attrs map[string]string) []attribute {
	out := make([]attribute, 0, len(attrs))
	for k, v := range attrs {
		out = append(out, attribute{Key: k, Value: attributeValue{StringValue: v}})
	}
	return out
}
