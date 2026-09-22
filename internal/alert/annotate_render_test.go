package alert

import (
	"strings"
	"testing"
)

// annotateText is annotate over raw template text, which is what these tests
// are about. State compiles the templates once at construction.
func annotateText(t *testing.T, annotations map[string]string, a Alert) (map[string]string, []AnnotationError) {
	t.Helper()

	parsed, parseErrs := parseAnnotations(annotations)
	return annotate(parsed, parseErrs, a)
}

func TestAnnotate(t *testing.T) {
	a := Alert{
		Labels: map[string]string{"alertname": "HighP99Latency", "ServiceName": "checkout"},
		Value:  1234.5,
	}

	got, failures := annotateText(t, map[string]string{
		"summary":     "{{ .ServiceName }} p99 is {{ .value }}ms",
		"runbook_url": "https://runbooks.internal/high-p99-latency",
	}, a)
	if len(failures) != 0 {
		t.Fatalf("annotate: %v", failures)
	}

	if want := "checkout p99 is 1234.5ms"; got["summary"] != want {
		t.Errorf("summary = %q, want %q", got["summary"], want)
	}
	if want := "https://runbooks.internal/high-p99-latency"; got["runbook_url"] != want {
		t.Errorf("runbook_url = %q, want %q", got["runbook_url"], want)
	}
}

// A summary referencing a label the alert does not carry must not render as
// blank. A page that reads "  p99 is  ms" wastes the responder's first minutes
// and says nothing about why. The failure goes where they will see it, and it
// is reported so the author can fix the rule.
func TestAnnotateReportsAnUnknownLabelWithoutDroppingThePage(t *testing.T) {
	a := Alert{Labels: map[string]string{"alertname": "X"}, Value: 1}

	got, failures := annotateText(t, map[string]string{"summary": "{{ .NoSuchColumn }} is bad"}, a)

	if len(failures) != 1 {
		t.Fatalf("got %d failures, want 1", len(failures))
	}
	if !strings.Contains(failures[0].Err.Error(), "NoSuchColumn") {
		t.Errorf("error should name the missing label, got: %v", failures[0].Err)
	}
	if !strings.Contains(got["summary"], "error expanding template") {
		t.Errorf("summary = %q, want the failure visible in the annotation", got["summary"])
	}
	if !strings.Contains(got["summary"], "NoSuchColumn") {
		t.Errorf("summary = %q, want it to name what was missing", got["summary"])
	}
}

// The value is the number the rule alerted on, so it has to be addressable
// alongside the labels rather than only through a label.
func TestAnnotateFormatsValueWithoutExponent(t *testing.T) {
	a := Alert{Labels: map[string]string{"alertname": "X"}, Value: 1e7}

	got, failures := annotateText(t, map[string]string{"summary": "{{ .value }}"}, a)
	if len(failures) != 0 {
		t.Fatalf("annotate: %v", failures)
	}
	if want := "10000000"; got["summary"] != want {
		t.Errorf("summary = %q, want %q", got["summary"], want)
	}
}
