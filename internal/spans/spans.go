// Package spans posts a known set of spans to an OpenTelemetry collector, so
// a test can state exactly what the table under a rule holds.
//
// This is the deterministic half of spec 9.2. `telemetrygen` gives volume and
// cannot express "exactly 40 spans over 1000ms on ServiceName=checkout
// starting at T+30s", and without that control no test can assert an exact
// alert count.
//
// It speaks OTLP over HTTP with JSON bodies, which is the encoding the protocol
// defines for exactly this: a caller that wants to produce telemetry without
// taking on an SDK. The alternative was the OpenTelemetry Go SDK and its OTLP
// exporter, which is three modules and their dependencies to build a payload
// this package writes in a struct literal.
package spans

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Scenario is one set of spans to emit: what they are, how many, how long each
// one took, and when the first of them started.
type Scenario struct {
	// Endpoint is the collector's OTLP HTTP address, e.g.
	// http://127.0.0.1:4318. The traces path is appended.
	Endpoint string

	ServiceName string
	SpanName    string

	// Count is how many spans to emit. They are posted in one request, so the
	// table either holds all of them or none.
	Count int

	// SpanDuration is how long each span took, which is the column a latency
	// rule reads.
	SpanDuration time.Duration

	// Start is when the first span started. A window is a range of timestamps,
	// so what a rule sees depends on this more than on when the post happened.
	Start time.Time

	// Spread is the period the spans' start times are spaced evenly across. A
	// zero Spread starts them all at Start.
	Spread time.Duration

	// Attributes go on every span, which is what a rule grouping or filtering
	// by one reads.
	Attributes map[string]string

	// StatusCode is the span's status: "Unset", "Ok" or "Error". Empty means
	// "Ok", since a span a test did not say was broken is not.
	StatusCode string
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
		return fmt.Errorf("encoding %d spans for %s: %w", s.Count, s.ServiceName, err)
	}

	url := strings.TrimSuffix(s.Endpoint, "/") + "/v1/traces"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request to %s: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("posting %d spans to %s: %w", s.Count, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A rejection is reported in the body, and a caller left to find out from
	// a missing row would be debugging the wrong half of the path.
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("reading the collector's answer (HTTP %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the collector refused %d spans for %s: HTTP %d: %s",
			s.Count, s.ServiceName, resp.StatusCode, strings.TrimSpace(string(answer)))
	}
	return nil
}

func (s Scenario) valid() error {
	switch {
	case s.Endpoint == "":
		return fmt.Errorf("no endpoint: nothing says where the collector is")
	case s.ServiceName == "":
		return fmt.Errorf("no service name: every assertion in a shared table is made on one")
	case s.Count < 1:
		return fmt.Errorf("count is %d: a scenario emitting nothing proves nothing", s.Count)
	case s.Start.IsZero():
		return fmt.Errorf("no start time: what a rule's window holds depends on it")
	}
	return nil
}

// OTLP's JSON encoding, as much of it as a span needs. Timestamps are strings
// because the encoding sends a 64-bit integer that way, and a JSON number would
// lose nanoseconds to a float.
type (
	tracesRequest struct {
		ResourceSpans []resourceSpans `json:"resourceSpans"`
	}

	resourceSpans struct {
		Resource   resource     `json:"resource"`
		ScopeSpans []scopeSpans `json:"scopeSpans"`
	}

	resource struct {
		Attributes []attribute `json:"attributes"`
	}

	scopeSpans struct {
		Scope scope  `json:"scope"`
		Spans []span `json:"spans"`
	}

	scope struct {
		Name string `json:"name"`
	}

	span struct {
		TraceID           string      `json:"traceId"`
		SpanID            string      `json:"spanId"`
		Name              string      `json:"name"`
		Kind              int         `json:"kind"`
		StartTimeUnixNano string      `json:"startTimeUnixNano"`
		EndTimeUnixNano   string      `json:"endTimeUnixNano"`
		Attributes        []attribute `json:"attributes"`
		Status            status      `json:"status"`
	}

	attribute struct {
		Key   string         `json:"key"`
		Value attributeValue `json:"value"`
	}

	attributeValue struct {
		StringValue string `json:"stringValue"`
	}

	status struct {
		Code int `json:"code"`
	}
)

// SPAN_KIND_SERVER, which is what an inbound request is and so what a latency
// rule is written about.
const kindServer = 2

func (s Scenario) payload() tracesRequest {
	out := make([]span, 0, s.Count)
	for i := range s.Count {
		start := s.Start.Add(s.offset(i))
		out = append(out, span{
			TraceID:           randomID(16),
			SpanID:            randomID(8),
			Name:              s.SpanName,
			Kind:              kindServer,
			StartTimeUnixNano: strconv.FormatInt(start.UnixNano(), 10),
			EndTimeUnixNano:   strconv.FormatInt(start.Add(s.SpanDuration).UnixNano(), 10),
			Attributes:        attributesOf(s.Attributes),
			Status:            status{Code: statusCode(s.StatusCode)},
		})
	}

	return tracesRequest{ResourceSpans: []resourceSpans{{
		Resource: resource{Attributes: []attribute{{
			Key:   "service.name",
			Value: attributeValue{StringValue: s.ServiceName},
		}}},
		ScopeSpans: []scopeSpans{{
			Scope: scope{Name: "clickhouse-ruler/spans"},
			Spans: out,
		}},
	}}}
}

// offset spaces the spans evenly across Spread. The last one starts at the end
// of it rather than one step short, so "40 spans over 1000ms" covers the 1000ms
// a caller asked for.
func (s Scenario) offset(i int) time.Duration {
	if s.Spread == 0 || s.Count == 1 {
		return 0
	}
	return time.Duration(int64(s.Spread) * int64(i) / int64(s.Count-1))
}

func attributesOf(attrs map[string]string) []attribute {
	out := make([]attribute, 0, len(attrs))
	for k, v := range attrs {
		out = append(out, attribute{Key: k, Value: attributeValue{StringValue: v}})
	}
	return out
}

// The protocol's status codes. An unrecognised name is sent as Unset rather
// than rejected, which is what the protocol does with a status nobody set.
func statusCode(name string) int {
	switch name {
	case "Error":
		return 2
	case "Unset":
		return 0
	default:
		return 1
	}
}

// randomID is a trace or span id, which the protocol sends as hex and requires
// to be unique rather than meaningful.
func randomID(size int) string {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		panic("reading random bytes for a span id: " + err.Error())
	}
	return hex.EncodeToString(b)
}
