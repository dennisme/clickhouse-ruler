package query

import (
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// valueColumn is the one result column that is not a label. See spec 6.3.
const valueColumn = "value"

// timeBounds are what {{ .From }} and {{ .To }} render to: ClickHouse named
// parameter placeholders, not timestamps.
//
// Rendering a timestamp into the SQL text would mean formatting and timezone
// decisions inside a string, which is how a window silently shifts by hours.
// Binding leaves both to the driver.
var timeBounds = struct {
	From string
	To   string
}{
	From: "{from:DateTime64(3)}",
	To:   "{to:DateTime64(3)}",
}

// render replaces the time bound actions with parameter placeholders. Any
// other template variable is an error, because the ruler supplies only these
// two and a typo must not reach ClickHouse as empty text.
func render(expr string) (string, error) {
	t, err := template.New("expr").Option("missingkey=error").Parse(expr)
	if err != nil {
		return "", fmt.Errorf("parsing expr template: %w", err)
	}

	var out strings.Builder
	if err := t.Execute(&out, timeBounds); err != nil {
		return "", fmt.Errorf("rendering expr: %w", err)
	}
	return out.String(), nil
}

// window is the span an evaluation examines. The newest data is skipped
// because it is still arriving, see spec 6.8.
func window(src source.Source, r rule.Rule, now time.Time) (from, to time.Time) {
	to = now.Add(-src.EvaluationDelay)
	return to.Add(-r.Window), to
}

// toSamples turns result rows into alert samples. One row is one alert
// instance: the value column becomes the value and every other column becomes
// a label.
func toSamples(columns []string, rows [][]any, maxRows int) ([]alert.Sample, error) {
	valueAt := -1
	for i, name := range columns {
		if name == valueColumn {
			valueAt = i
			break
		}
	}
	if valueAt < 0 {
		return nil, fmt.Errorf("query returned no %q column, so there is nothing to compare against a threshold", valueColumn)
	}
	if maxRows > 0 && len(rows) > maxRows {
		return nil, fmt.Errorf("query returned %d rows, which exceeds the source max_rows of %d", len(rows), maxRows)
	}

	samples := make([]alert.Sample, 0, len(rows))
	for i, row := range rows {
		if len(row) != len(columns) {
			return nil, fmt.Errorf("row %d has %d values for %d columns", i, len(row), len(columns))
		}

		s := alert.Sample{Labels: make(map[string]string, len(columns)-1)}
		for j, name := range columns {
			if j == valueAt {
				v, err := toFloat(row[j])
				if err != nil {
					return nil, fmt.Errorf("row %d: column %q: %w", i, name, err)
				}
				s.Value = v
				continue
			}

			label, err := toLabel(row[j])
			if err != nil {
				return nil, fmt.Errorf("row %d: column %q: %w", i, name, err)
			}
			s.Labels[name] = label
		}
		samples = append(samples, s)
	}

	return samples, nil
}

// toLabel converts a result value to label text.
//
// The conversion has to be stable: labels feed the alert fingerprint, so a
// number formatted two different ways would split one alert instance into
// two. FormatFloat with precision -1 gives the shortest text that round-trips,
// which is the same for a given value every time.
func toLabel(v any) (string, error) {
	switch v := v.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	case bool:
		return strconv.FormatBool(v), nil
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32), nil
	case int:
		return strconv.FormatInt(int64(v), 10), nil
	case int8:
		return strconv.FormatInt(int64(v), 10), nil
	case int16:
		return strconv.FormatInt(int64(v), 10), nil
	case int32:
		return strconv.FormatInt(int64(v), 10), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case uint:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint64:
		return strconv.FormatUint(v, 10), nil
	case *string:
		if v == nil {
			return "", nil
		}
		return *v, nil
	}

	// Nullable columns arrive as pointers, so one level of indirection is
	// unwrapped before giving up.
	if unwrapped, ok := derefPointer(v); ok {
		return toLabel(unwrapped)
	}
	return "", fmt.Errorf("cannot use a %T column as a label, select a scalar instead", v)
}

func toFloat(v any) (float64, error) {
	switch v := v.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int8:
		return float64(v), nil
	case int16:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case uint:
		return float64(v), nil
	case uint8:
		return float64(v), nil
	case uint16:
		return float64(v), nil
	case uint32:
		return float64(v), nil
	case uint64:
		return float64(v), nil
	}

	if unwrapped, ok := derefPointer(v); ok {
		return toFloat(unwrapped)
	}
	return 0, fmt.Errorf("the %s column must be a number, got %T", valueColumn, v)
}
