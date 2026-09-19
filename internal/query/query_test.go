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
			got, err := render(tc.expr)
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
	_, err := render("WHERE ts >= {{ .From }} AND host = {{ .Hostname }}")
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
