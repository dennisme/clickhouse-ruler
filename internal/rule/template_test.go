package rule

import (
	"slices"
	"testing"
	"text/template"
)

func TestTemplateFields(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "a bare field",
			text: "p99 is {{ .p99 }}ms",
			want: []string{"p99"},
		},
		{
			name: "a field inside a pipeline, which a regular expression over the text would miss",
			text: `{{ .service | printf "%s" }}`,
			want: []string{"service"},
		},
		{
			name: "a field inside a conditional",
			text: "{{ if .degraded }}{{ .region }}{{ else }}{{ .value }}{{ end }}",
			want: []string{"degraded", "region", "value"},
		},
		{
			name: "the first identifier only, because the data is a flat map",
			text: "{{ .labels.team }}",
			want: []string{"labels"},
		},
		{
			name: "a string that looks like a field",
			text: `{{ printf "%s" ".service" }}`,
			want: nil,
		},
		{
			name: "no fields at all",
			text: "checkout is slow",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := template.New("annotation").Parse(tc.text)
			if err != nil {
				t.Fatalf("parsing the template: %v", err)
			}
			got := TemplateFields(parsed)
			if !slices.Equal(got, tc.want) {
				t.Errorf("TemplateFields(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
