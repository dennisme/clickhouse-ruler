package query

import (
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

func TestRenderBindsTimeBoundsAsParameters(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want string
	}{
		{
			name: "spaced",
			expr: "WHERE ts >= {{ .From }} AND ts < {{ .To }}",
			want: "WHERE ts >= {from:DateTime64(3)} AND ts < {to:DateTime64(3)}",
		},
		{
			// {{.From}} is a valid template action, so it must render too.
			name: "tight braces and trim markers",
			expr: "WHERE ts >= {{.From}} AND ts < {{- .To }}",
			want: "WHERE ts >= {from:DateTime64(3)} AND ts <{to:DateTime64(3)}",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := render(tc.expr, source.Source{})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got != tc.want {
				t.Errorf("render =\n %q\nwant\n %q", got, tc.want)
			}
		})
	}
}

// A timestamp rendered into SQL text would invite injection and timezone
// bugs, so nothing but the placeholder may reach the query string.
func TestRenderRejectsUnknownVariables(t *testing.T) {
	_, err := render("WHERE ts >= {{ .From }} AND host = {{ .Hostname }}", source.Source{})
	if err == nil {
		t.Fatal("expected an error for an unknown template variable")
	}
	if !strings.Contains(err.Error(), "Hostname") {
		t.Errorf("error should name the unknown variable, got: %v", err)
	}
}

func TestWindowAppliesEvaluationDelay(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	src := source.Source{EvaluationDelay: 90 * time.Second}
	r := rule.Rule{Window: 5 * time.Minute}

	from, to := window(src, r, now)

	// The newest 90 seconds are still filling, so they are excluded.
	if want := now.Add(-90 * time.Second); !to.Equal(want) {
		t.Errorf("to = %v, want %v", to, want)
	}
	if want := now.Add(-90 * time.Second).Add(-5 * time.Minute); !from.Equal(want) {
		t.Errorf("from = %v, want %v", from, want)
	}
}

func TestToSamplesSplitsValueFromLabels(t *testing.T) {
	got, err := toSamples(
		[]string{"ServiceName", "StatusCode", "value"},
		[][]any{
			{"checkout", "Error", float64(1200)},
			{"cart", "Error", float64(900)},
		},
		10,
	)
	if err != nil {
		t.Fatalf("toSamples: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2", len(got))
	}

	if got[0].Value != 1200 {
		t.Errorf("value = %v, want 1200", got[0].Value)
	}
	if got[0].Labels["ServiceName"] != "checkout" {
		t.Errorf("ServiceName = %q, want checkout", got[0].Labels["ServiceName"])
	}
	if _, ok := got[0].Labels["value"]; ok {
		t.Error("value must not also appear as a label")
	}
}

// Labels feed the fingerprint, so the same number has to produce the same
// text on every evaluation or an instance would change identity.
func TestToSamplesConvertsLabelsDeterministically(t *testing.T) {
	ts := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	str := "pointer"

	got, err := toSamples(
		[]string{"s", "i64", "u64", "f64", "f32", "b", "t", "p", "value"},
		[][]any{{
			"plain", int64(-7), uint64(7), float64(1.5), float32(2.5),
			true, ts, &str, float64(1),
		}},
		10,
	)
	if err != nil {
		t.Fatalf("toSamples: %v", err)
	}

	for key, want := range map[string]string{
		"s":   "plain",
		"i64": "-7",
		"u64": "7",
		"f64": "1.5",
		"f32": "2.5",
		"b":   "true",
		"t":   "2026-09-19T12:00:00Z",
		"p":   "pointer",
	} {
		if got[0].Labels[key] != want {
			t.Errorf("label %q = %q, want %q", key, got[0].Labels[key], want)
		}
	}
}

func TestToSamplesRequiresValueColumn(t *testing.T) {
	_, err := toSamples([]string{"ServiceName"}, [][]any{{"checkout"}}, 10)
	if err == nil {
		t.Fatal("expected an error when no value column is returned")
	}
	if !strings.Contains(err.Error(), "value") {
		t.Errorf("error should mention the value column, got: %v", err)
	}
}

// One rule returning millions of rows must fail rather than buffer them all
// (spec 6.7).
func TestToSamplesEnforcesMaxRows(t *testing.T) {
	rows := make([][]any, 5)
	for i := range rows {
		rows[i] = []any{"svc", float64(i)}
	}

	_, err := toSamples([]string{"ServiceName", "value"}, rows, 3)
	if err == nil {
		t.Fatal("expected an error when the row cap is exceeded")
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("error should name the cap, got: %v", err)
	}
}

func TestToSamplesRejectsUnsupportedLabelType(t *testing.T) {
	_, err := toSamples(
		[]string{"attrs", "value"},
		[][]any{{map[string]string{"a": "b"}, float64(1)}},
		10,
	)
	if err == nil {
		t.Fatal("expected an error for a map column used as a label")
	}
}

func TestToSamplesAcceptsNumericValueTypes(t *testing.T) {
	for _, v := range []any{float64(5), float32(5), int64(5), uint64(5), int32(5), uint32(5)} {
		got, err := toSamples([]string{"value"}, [][]any{{v}}, 10)
		if err != nil {
			t.Fatalf("value of type %T: %v", v, err)
		}
		if got[0].Value != 5 {
			t.Errorf("value of type %T = %v, want 5", v, got[0].Value)
		}
	}
}

// A distributed query against a cluster with an unreachable shard succeeds by
// default on some configurations, returning only the rows the surviving shards
// held. Missing rows are indistinguishable from a recovered condition, so
// instances vanish from the state machine and their alerts resolve. Pinning
// this means the evaluation fails loudly instead (spec 6.9).
func TestSettingsPinSkipUnavailableShards(t *testing.T) {
	got := settings(source.Source{
		MaxExecutionTime: 30 * time.Second,
		MaxMemoryUsage:   1 << 30,
		MaxRows:          1000,
	})

	if got["skip_unavailable_shards"] != 0 {
		t.Errorf("skip_unavailable_shards = %v, want 0 so a dead shard fails the query",
			got["skip_unavailable_shards"])
	}
}

// A rule names no cluster, so one expr has to be able to run against sources
// that spell their table and timestamp column differently (spec 6.4.1).
func TestRenderSubstitutesSourceIdentifiers(t *testing.T) {
	const expr = "SELECT 1 AS value FROM {{ .Table }} " +
		"WHERE {{ .TimestampColumn }} >= {{ .From }} AND {{ .TimestampColumn }} < {{ .To }}"

	dc1 := source.Source{Table: "otel_traces", TimestampColumn: "Timestamp"}
	dc2 := source.Source{Table: "traces", TimestampColumn: "ts"}

	got1, err := render(expr, dc1)
	if err != nil {
		t.Fatalf("render against dc1: %v", err)
	}
	got2, err := render(expr, dc2)
	if err != nil {
		t.Fatalf("render against dc2: %v", err)
	}

	want1 := "SELECT 1 AS value FROM otel_traces " +
		"WHERE Timestamp >= {from:DateTime64(3)} AND Timestamp < {to:DateTime64(3)}"
	want2 := "SELECT 1 AS value FROM traces " +
		"WHERE ts >= {from:DateTime64(3)} AND ts < {to:DateTime64(3)}"

	if got1 != want1 {
		t.Errorf("render against dc1 =\n %q\nwant\n %q", got1, want1)
	}
	if got2 != want2 {
		t.Errorf("render against dc2 =\n %q\nwant\n %q", got2, want2)
	}
}

// The identifiers are optional: a rule author whose clusters agree on the
// names keeps writing them literally (spec 6.4.1).
func TestRenderLeavesLiteralIdentifiersAlone(t *testing.T) {
	const expr = "SELECT 1 AS value FROM otel_traces WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}"

	got, err := render(expr, source.Source{Table: "traces", TimestampColumn: "ts"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	want := "SELECT 1 AS value FROM otel_traces " +
		"WHERE Timestamp >= {from:DateTime64(3)} AND Timestamp < {to:DateTime64(3)}"
	if got != want {
		t.Errorf("render =\n %q\nwant\n %q", got, want)
	}
}

// The check-time render substitutes the same identifiers, so the statement
// that is explained and described is the one that will be evaluated.
func TestRenderForCheckSubstitutesSourceIdentifiers(t *testing.T) {
	src := source.Source{Table: "traces", TimestampColumn: "ts"}

	got, err := renderForCheck("SELECT 1 AS value FROM {{ .Table }} WHERE {{ .TimestampColumn }} >= {{ .From }} AND {{ .TimestampColumn }} < {{ .To }}", src, time.Minute)
	if err != nil {
		t.Fatalf("renderForCheck: %v", err)
	}
	if !strings.Contains(got, "FROM traces") {
		t.Errorf("rendered SQL should read the source's table, got: %q", got)
	}
	if strings.Contains(got, "{{") {
		t.Errorf("rendered SQL still carries a template action, got: %q", got)
	}
}
