package alert

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
)

func annotatedRule(annotations map[string]string) rule.Rule {
	r := testRule(0, 0)
	r.Annotations = annotations
	return r
}

// Annotations are independent, and an operator's Alertmanager templates read
// them by name. Discarding the whole set because summary named a column that is
// not there would lose the ones that rendered perfectly well, which here are
// the link to follow and the description next to it.
func TestEvalKeepsTheAnnotationsThatRendered(t *testing.T) {
	s := New(annotatedRule(map[string]string{
		"summary":     "{{ .NoSuchColumn }} is slow",
		"runbook_url": "https://runbooks.internal/high-p99",
		"description": "p99 is {{ .value }}ms",
	}), nil, testSource())

	alerts, failures, err := s.Eval(t0, []Sample{sample("checkout", 1200)})
	if err != nil {
		t.Fatalf("a broken annotation must not fail the evaluation: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1: a broken annotation must not drop the alert", len(alerts))
	}

	got := alerts[0].Annotations
	if got["runbook_url"] != "https://runbooks.internal/high-p99" {
		t.Errorf("runbook_url = %q, want it delivered untouched", got["runbook_url"])
	}
	if got["description"] != "p99 is 1200ms" {
		t.Errorf("description = %q, want it rendered", got["description"])
	}
	if !strings.Contains(got["summary"], "error expanding template") {
		t.Errorf("summary = %q, want the failure visible in the annotation", got["summary"])
	}

	// The failure still has to be reported, or an author never learns the
	// summary on their page is an error string.
	if len(failures) != 1 {
		t.Fatalf("got %d annotation failures, want 1: %v", len(failures), failures)
	}
	if failures[0].Annotation != "summary" {
		t.Errorf("failed annotation = %q, want summary", failures[0].Annotation)
	}
}

// A broken template fails on every row, so a rule returning ten thousand
// instances must report the annotation once. Spec 8.3's cardinality rule is
// about metrics and the same reasoning covers this.
func TestEvalReportsABrokenAnnotationOncePerEvaluation(t *testing.T) {
	s := New(annotatedRule(map[string]string{
		"summary": "{{ .NoSuchColumn }} is slow",
	}), nil, testSource())

	samples := make([]Sample, 0, 500)
	for i := 0; i < 500; i++ {
		samples = append(samples, sample("svc"+strconv.Itoa(i), float64(i)))
	}

	alerts, failures, err := s.Eval(t0, samples)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if len(alerts) != 500 {
		t.Fatalf("got %d alerts, want 500", len(alerts))
	}
	if len(failures) != 1 {
		t.Fatalf("got %d annotation failures for 500 instances, want 1", len(failures))
	}
}

// A resolved alert says what it said when it fired. Re-rendering it at send
// time from whatever value it last held describes a condition that has already
// gone away.
func TestResolvedAlertKeepsTheAnnotationsItFiredWith(t *testing.T) {
	s := New(annotatedRule(map[string]string{
		"summary": "p99 is {{ .value }}ms",
	}), nil, testSource())

	if _, _, err := s.Eval(t0, []Sample{sample("checkout", 1200)}); err != nil {
		t.Fatalf("Eval: %v", err)
	}

	// The instance is gone from the result, so this evaluation resolves it.
	alerts, _, err := s.Eval(t0.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}

	if len(alerts) != 1 || alerts[0].Phase != PhaseResolved {
		t.Fatalf("want one resolved alert, got %s", formatAlerts(alerts))
	}
	if got := alerts[0].Annotations["summary"]; got != "p99 is 1200ms" {
		t.Errorf("summary = %q, want the value it fired with", got)
	}
}

// While an alert is firing its annotations track the latest evaluation, because
// the value in the summary is the value the responder is being paged about.
func TestFiringAlertAnnotationsFollowTheLatestValue(t *testing.T) {
	s := New(annotatedRule(map[string]string{
		"summary": "p99 is {{ .value }}ms",
	}), nil, testSource())

	if _, _, err := s.Eval(t0, []Sample{sample("checkout", 1200)}); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	alerts, _, err := s.Eval(t0.Add(time.Minute), []Sample{sample("checkout", 1450)})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}

	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	if got := alerts[0].Annotations["summary"]; got != "p99 is 1450ms" {
		t.Errorf("summary = %q, want the current value", got)
	}
}

// A rule with no annotations must not grow an empty map, because that would
// marshal into the payload as an empty object where the field should be absent.
func TestEvalLeavesAnnotationsNilWhenTheRuleHasNone(t *testing.T) {
	s := New(testRule(0, 0), nil, testSource())

	alerts, failures, err := s.Eval(t0, []Sample{sample("checkout", 1200)})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("got %d annotation failures, want none", len(failures))
	}
	if alerts[0].Annotations != nil {
		t.Errorf("Annotations = %v, want nil", alerts[0].Annotations)
	}
}
