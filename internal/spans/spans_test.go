package spans

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func scenario() Scenario {
	return Scenario{
		Endpoint:     "http://collector:4318",
		ServiceName:  "checkout",
		SpanName:     "GET /checkout",
		Count:        40,
		SpanDuration: 2 * time.Second,
		Start:        time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		Spread:       time.Second,
	}
}

// The scenario spec 9.2 names: exactly 40 spans over 1000ms. Exactly is the
// word that matters, since an alert count is only assertable if the number of
// spans behind it is known.
func TestCountAndSpreadAreWhatWasAskedFor(t *testing.T) {
	s := scenario()
	got := s.payload().ResourceSpans[0].ScopeSpans[0].Spans

	if len(got) != s.Count {
		t.Fatalf("emitted %d spans, want %d", len(got), s.Count)
	}

	first := unixNano(t, got[0].StartTimeUnixNano)
	last := unixNano(t, got[len(got)-1].StartTimeUnixNano)

	if first != s.Start.UnixNano() {
		t.Errorf("first span starts at %d, want %d", first, s.Start.UnixNano())
	}
	// The last span starts at the end of the spread rather than one step
	// short, so the spans cover the period a caller asked for.
	if want := s.Start.Add(s.Spread).UnixNano(); last != want {
		t.Errorf("last span starts at %d, want %d: the spans do not cover %s", last, want, s.Spread)
	}
}

// What a latency rule reads, and the one field a scenario exists to control.
func TestEverySpanLastsTheDurationAsked(t *testing.T) {
	s := scenario()

	for i, span := range s.payload().ResourceSpans[0].ScopeSpans[0].Spans {
		took := unixNano(t, span.EndTimeUnixNano) - unixNano(t, span.StartTimeUnixNano)
		if took != int64(s.SpanDuration) {
			t.Fatalf("span %d lasts %dns, want %dns", i, took, int64(s.SpanDuration))
		}
	}
}

// Spans with no spread share a start time, which is what a caller asking for a
// burst at one instant means.
func TestNoSpreadStartsEverySpanTogether(t *testing.T) {
	s := scenario()
	s.Spread = 0

	for i, span := range s.payload().ResourceSpans[0].ScopeSpans[0].Spans {
		if got := unixNano(t, span.StartTimeUnixNano); got != s.Start.UnixNano() {
			t.Fatalf("span %d starts at %d, want %d", i, got, s.Start.UnixNano())
		}
	}
}

// ServiceName is a resource attribute on the wire and a column in the table,
// and it is what every assertion against a shared table is made on.
func TestServiceNameIsAResourceAttribute(t *testing.T) {
	s := scenario()
	attrs := s.payload().ResourceSpans[0].Resource.Attributes

	if len(attrs) != 1 {
		t.Fatalf("resource carries %d attributes, want 1", len(attrs))
	}
	if attrs[0].Key != "service.name" || attrs[0].Value.StringValue != s.ServiceName {
		t.Errorf("resource attribute = %s=%s, want service.name=%s",
			attrs[0].Key, attrs[0].Value.StringValue, s.ServiceName)
	}
}

func TestSpanAttributesReachEverySpan(t *testing.T) {
	s := scenario()
	s.Attributes = map[string]string{"run": "abc"}

	for i, span := range s.payload().ResourceSpans[0].ScopeSpans[0].Spans {
		if len(span.Attributes) != 1 || span.Attributes[0].Key != "run" {
			t.Fatalf("span %d carries %v, want run=abc", i, span.Attributes)
		}
	}
}

// Ok rather than Unset for a status nobody set, since a span a test did not say
// was broken is not broken, and a rule counting errors would read Unset as one
// more row to decide about.
func TestStatusDefaultsToOk(t *testing.T) {
	for name, want := range map[string]int{"": 1, "Ok": 1, "Error": 2, "Unset": 0} {
		s := scenario()
		s.Count = 1
		s.StatusCode = name

		got := s.payload().ResourceSpans[0].ScopeSpans[0].Spans[0].Status.Code
		if got != want {
			t.Errorf("status %q encodes as %d, want %d", name, got, want)
		}
	}
}

// Ids have to be unique rather than meaningful, and a batch sharing one would
// collapse in a table ordered by it.
func TestEverySpanGetsItsOwnIds(t *testing.T) {
	s := scenario()

	seen := map[string]bool{}
	for _, span := range s.payload().ResourceSpans[0].ScopeSpans[0].Spans {
		if len(span.TraceID) != 32 {
			t.Fatalf("trace id %q is %d hex characters, want 32", span.TraceID, len(span.TraceID))
		}
		if len(span.SpanID) != 16 {
			t.Fatalf("span id %q is %d hex characters, want 16", span.SpanID, len(span.SpanID))
		}
		if seen[span.SpanID] {
			t.Fatalf("span id %s was used twice", span.SpanID)
		}
		seen[span.SpanID] = true
	}
}

// A scenario missing one of these emits something nobody asked for, and the
// table it lands in is shared, so it is refused before anything is posted.
func TestAnIncompleteScenarioIsRefused(t *testing.T) {
	for name, corrupt := range map[string]func(*Scenario){
		"no endpoint":     func(s *Scenario) { s.Endpoint = "" },
		"no service name": func(s *Scenario) { s.ServiceName = "" },
		"no spans":        func(s *Scenario) { s.Count = 0 },
		"no start time":   func(s *Scenario) { s.Start = time.Time{} },
	} {
		s := scenario()
		corrupt(&s)

		if err := s.Emit(context.Background()); err == nil {
			t.Errorf("%s: Emit returned no error", name)
		}
	}
}

func TestEmitPostsOTLPJSONToTheTracesPath(t *testing.T) {
	var gotPath, gotType string
	var body tracesRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotType = r.URL.Path, r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("the posted body is not OTLP JSON: %v", err)
		}
		_, _ = w.Write([]byte(`{"partialSuccess":{}}`))
	}))
	defer srv.Close()

	s := scenario()
	// A trailing slash is the shape a URL out of an environment variable
	// often has, and two slashes in the path is a 404 the caller then has to
	// explain.
	s.Endpoint = srv.URL + "/"
	if err := s.Emit(context.Background()); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if gotPath != "/v1/traces" {
		t.Errorf("posted to %s, want /v1/traces", gotPath)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %s, want application/json", gotType)
	}
	if got := len(body.ResourceSpans[0].ScopeSpans[0].Spans); got != s.Count {
		t.Errorf("the collector received %d spans, want %d", got, s.Count)
	}
}

// A refusal the caller is not told about is a missing row they debug at the
// other end of the path.
func TestARefusedPostIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad span"))
	}))
	defer srv.Close()

	s := scenario()
	s.Endpoint = srv.URL

	err := s.Emit(context.Background())
	if err == nil {
		t.Fatal("Emit returned no error on HTTP 400")
	}
	if !strings.Contains(err.Error(), "bad span") {
		t.Errorf("error is %q, want it to carry what the collector said", err)
	}
}

func unixNano(t *testing.T, s string) int64 {
	t.Helper()

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("timestamp %q is not an integer: %v", s, err)
	}
	return n
}
