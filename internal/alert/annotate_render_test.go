package alert

import (
	"slices"
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
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
	if got["summary"] == "" {
		t.Error("summary is empty, want the failure visible on the page")
	}
	if !strings.Contains(got[rule.ErrorAnnotation], "NoSuchColumn") {
		t.Errorf("%s = %q, want it to name what was missing", rule.ErrorAnnotation, got[rule.ErrorAnnotation])
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

// A consumer builds a PagerDuty title out of summary, so what lands there when
// a template fails has to be short and the same every time. The error itself is
// what an author needs and a responder does not, so it goes in the annotation
// the ruler owns (spec 6.5).
func TestAnnotateMarksTheFailedAnnotationAndCarriesTheErrorItself(t *testing.T) {
	a := Alert{Labels: map[string]string{"alertname": "X"}, Value: 1}

	got, failures := annotateText(t, map[string]string{
		"summary":     "{{ .NoSuchColumn }} is bad",
		"runbook_url": "https://runbooks.internal/high-p99-latency",
	}, a)

	if len(failures) != 1 {
		t.Fatalf("got %d failures, want 1: %v", len(failures), failures)
	}
	if want := `<ruler: annotation "summary" failed: no label "NoSuchColumn">`; got["summary"] != want {
		t.Errorf("summary = %q, want %q", got["summary"], want)
	}
	if want := "https://runbooks.internal/high-p99-latency"; got["runbook_url"] != want {
		t.Errorf("runbook_url = %q, want it delivered untouched", got["runbook_url"])
	}

	err := got[rule.ErrorAnnotation]
	if !strings.Contains(err, `annotation "summary"`) {
		t.Errorf("%s = %q, want it to name the annotation", rule.ErrorAnnotation, err)
	}
	if !strings.Contains(err, "NoSuchColumn") {
		t.Errorf("%s = %q, want it to name what was missing", rule.ErrorAnnotation, err)
	}
}

// Two failures are one field with both errors in it, in annotation name order,
// so a machine reading the errors has one name to know and the order does not
// change between evaluations.
func TestAnnotateCarriesEveryErrorInOneField(t *testing.T) {
	a := Alert{Labels: map[string]string{"alertname": "X"}, Value: 1}

	got, failures := annotateText(t, map[string]string{
		"summary":     "{{ .NoSuchColumn }} is bad",
		"description": "{{ .AlsoMissing }}",
	}, a)

	if len(failures) != 2 {
		t.Fatalf("got %d failures, want 2: %v", len(failures), failures)
	}
	err := got[rule.ErrorAnnotation]
	if !strings.Contains(err, "AlsoMissing") || !strings.Contains(err, "NoSuchColumn") {
		t.Errorf("%s = %q, want both errors", rule.ErrorAnnotation, err)
	}
	if strings.Index(err, "AlsoMissing") > strings.Index(err, "NoSuchColumn") {
		t.Errorf("%s = %q, want description before summary", rule.ErrorAnnotation, err)
	}
	if want := 1; strings.Count(err, "; ") != want {
		t.Errorf("%s = %q, want the two errors joined by one separator", rule.ErrorAnnotation, err)
	}
}

// A rule whose annotations all rendered carries no field from the ruler, so an
// Alertmanager template testing for one is testing for a real failure.
func TestAnnotateAddsNothingWhenEveryTemplateRenders(t *testing.T) {
	a := Alert{Labels: map[string]string{"alertname": "X"}, Value: 1}

	got, failures := annotateText(t, map[string]string{"summary": "value is {{ .value }}"}, a)
	if len(failures) != 0 {
		t.Fatalf("annotate: %v", failures)
	}
	if _, ok := got[rule.ErrorAnnotation]; ok {
		t.Errorf("%s = %q, want it absent", rule.ErrorAnnotation, got[rule.ErrorAnnotation])
	}
}

// The finding the ruler raises on a rule's owner names what the template asked
// for and the alert did not carry, and the raw Go error is not what it carries
// (spec 6.5). So the failure has to say which keys were missing, structurally,
// rather than leaving a caller to read them back out of the error text.
func TestAnnotateNamesTheMissingKeys(t *testing.T) {
	a := Alert{Labels: map[string]string{"alertname": "X", "ServiceName": "checkout"}, Value: 1}

	_, failures := annotateText(t, map[string]string{
		"summary": "{{ .ServiceName }} p99 is {{ .p99 }} over {{ .window }}",
	}, a)

	if len(failures) != 1 {
		t.Fatalf("got %d failures, want 1", len(failures))
	}
	want := []string{"p99", "window"}
	if !slices.Equal(failures[0].MissingKeys, want) {
		t.Errorf("MissingKeys = %v, want %v: sorted, and only the keys the alert did not carry",
			failures[0].MissingKeys, want)
	}
}

// A template that never parsed reached no data, so nothing about it says which
// key was missing and an empty list is the honest answer.
func TestAnnotateNamesNoMissingKeysForATemplateThatDidNotParse(t *testing.T) {
	a := Alert{Labels: map[string]string{"alertname": "X"}, Value: 1}

	_, failures := annotateText(t, map[string]string{"summary": "{{ .Unclosed "}, a)

	if len(failures) != 1 {
		t.Fatalf("got %d failures, want 1", len(failures))
	}
	if len(failures[0].MissingKeys) != 0 {
		t.Errorf("MissingKeys = %v, want none for a template that did not parse", failures[0].MissingKeys)
	}
}

// The marker is the one thing a responder is guaranteed to read, because summary
// is what a PagerDuty title and a Slack message are built from. `ruler_error`
// carries the whole error and only reaches a human if the operator's receiver
// templates it, so the reason has to be in the marker itself or the page says
// something is broken without saying what (spec 6.5).
func TestAnnotateMarkerNamesTheMissingLabel(t *testing.T) {
	a := Alert{Labels: map[string]string{"alertname": "X", "service_name": "checkout"}, Value: 1}

	got, _ := annotateText(t, map[string]string{
		"summary": "{{ .ServiceName }} p99 is {{ .value }}ms",
	}, a)

	want := `<ruler: annotation "summary" failed: no label "ServiceName">`
	if got["summary"] != want {
		t.Errorf("summary = %q, want %q", got["summary"], want)
	}
}

// Two missing labels read as a list, because they are one edit for the author
// and a marker naming half of them sends them back for a second look.
func TestAnnotateMarkerNamesTwoMissingLabels(t *testing.T) {
	a := Alert{Labels: map[string]string{"alertname": "X"}, Value: 1}

	got, _ := annotateText(t, map[string]string{
		"summary": "{{ .p99 }} in {{ .region }}",
	}, a)

	want := `<ruler: annotation "summary" failed: no labels "p99" and "region">`
	if got["summary"] != want {
		t.Errorf("summary = %q, want %q", got["summary"], want)
	}
}

// Bounded, because summary is a notification field and a template reading a
// dozen labels must not turn a PagerDuty title into a list of them.
func TestAnnotateMarkerCapsTheLabelsItNames(t *testing.T) {
	a := Alert{Labels: map[string]string{"alertname": "X"}, Value: 1}

	got, _ := annotateText(t, map[string]string{
		"summary": "{{ .a }}{{ .b }}{{ .c }}{{ .d }}{{ .e }}",
	}, a)

	want := `<ruler: annotation "summary" failed: no labels "a", "b", "c" and 2 more>`
	if got["summary"] != want {
		t.Errorf("summary = %q, want %q", got["summary"], want)
	}
}

// A template that never parsed reached no data, so there is no missing label to
// name and the marker says what it can.
func TestAnnotateMarkerForATemplateThatDidNotParse(t *testing.T) {
	a := Alert{Labels: map[string]string{"alertname": "X"}, Value: 1}

	got, _ := annotateText(t, map[string]string{"summary": "{{ .Unclosed "}, a)

	want := `<ruler: annotation "summary" failed>`
	if got["summary"] != want {
		t.Errorf("summary = %q, want %q", got["summary"], want)
	}
}
