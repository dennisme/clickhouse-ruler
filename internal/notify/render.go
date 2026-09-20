// Package notify renders an alert's annotations and sends it to Alertmanager.
//
// Alertmanager owns grouping, silences, inhibition and routing (spec 6.5).
// This package does none of that. It turns an evaluated alert into a v2
// payload and gets it there.
package notify

import (
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

// valueKey is how a template reaches the alert's value. Lower case because it
// matches the `value` result column that produced it, so a rule author writes
// the same name in the SQL and in the summary.
const valueKey = "value"

// Render expands each annotation as a Go template over the alert's labels plus
// its value.
//
// Missing keys are an error rather than an empty string. A page that reads
// "  p99 is  ms" costs the responder their first minutes, and the failure is
// invisible until someone is already awake, so it has to surface at render
// time instead.
func Render(annotations map[string]string, a alert.Alert) (map[string]string, error) {
	if len(annotations) == 0 {
		return nil, nil
	}

	data := make(map[string]any, len(a.Labels)+1)
	for k, v := range a.Labels {
		data[k] = v
	}
	// Formatted the same way the querier formats a float label, so the number
	// in the summary matches the number in the labels.
	data[valueKey] = strconv.FormatFloat(a.Value, 'f', -1, 64)

	out := make(map[string]string, len(annotations))
	for name, text := range annotations {
		rendered, err := renderOne(name, text, data)
		if err != nil {
			return nil, err
		}
		out[name] = rendered
	}
	return out, nil
}

func renderOne(name, text string, data map[string]any) (string, error) {
	// Option "missingkey=error" only fires for a map, which is why data is a
	// map rather than a struct.
	t, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", fmt.Errorf("annotation %q: %w", name, err)
	}

	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("annotation %q: %w", name, err)
	}
	return sb.String(), nil
}
