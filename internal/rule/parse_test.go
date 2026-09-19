package rule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func TestParseValidFile(t *testing.T) {
	f, problems := Parse("testdata/valid.yaml", readFixture(t, "valid.yaml"))

	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %d: %v", len(problems), problems)
	}
	if len(f.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(f.Groups))
	}

	g := f.Groups[0]
	if g.Name != "api-latency" {
		t.Errorf("group name = %q, want %q", g.Name, "api-latency")
	}
	if g.Interval != time.Minute {
		t.Errorf("group interval = %v, want %v", g.Interval, time.Minute)
	}
	if len(g.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(g.Rules))
	}

	r := g.Rules[0]
	if r.Alert != "HighP99Latency" {
		t.Errorf("alert = %q, want %q", r.Alert, "HighP99Latency")
	}
	if r.Source != "otel_traces" {
		t.Errorf("source = %q, want %q", r.Source, "otel_traces")
	}
	if r.For != 5*time.Minute {
		t.Errorf("for = %v, want %v", r.For, 5*time.Minute)
	}
	if r.KeepFiringFor != 0 {
		t.Errorf("keep_firing_for = %v, want 0", r.KeepFiringFor)
	}
	// The file sets no window, so it takes the group's interval.
	if r.Window != time.Minute {
		t.Errorf("window = %v, want %v", r.Window, time.Minute)
	}
	if want := "payments"; r.Labels["team"] != want {
		t.Errorf("labels[team] = %q, want %q", r.Labels["team"], want)
	}
	if want := "warning"; r.Labels["severity"] != want {
		t.Errorf("labels[severity] = %q, want %q", r.Labels["severity"], want)
	}
	if want := "https://runbooks.internal/high-p99-latency"; r.Annotations["runbook_url"] != want {
		t.Errorf("annotations[runbook_url] = %q, want %q", r.Annotations["runbook_url"], want)
	}
	if !strings.Contains(r.Expr, "{{ .From }}") || !strings.Contains(r.Expr, "{{ .To }}") {
		t.Errorf("expr did not round-trip the time bound template vars: %q", r.Expr)
	}
}

// A misspelled field would otherwise be silently dropped, leaving a rule that
// parses cleanly and never alerts. The file is the only source of truth, so
// that failure mode has to be loud.
func TestParseRejectsUnknownFields(t *testing.T) {
	_, got := Parse("testdata/unknown_field.yaml", readFixture(t, "unknown_field.yaml"))

	assertProblems(t, got, []lint.Problem{
		{
			File:     "testdata/unknown_field.yaml",
			Line:     4,
			Subject:  "",
			Check:    "yaml/unknown-field",
			Severity: lint.SeverityError,
			Text:     `unknown field "labels" in group`,
		},
		{
			File:     "testdata/unknown_field.yaml",
			Line:     10,
			Subject:  "TypoField",
			Check:    "yaml/unknown-field",
			Severity: lint.SeverityError,
			Text:     `unknown field "sevrity" in rule`,
		},
	})
}

// window defaults to the group interval so that a rule which says nothing
// about it examines exactly the data produced since the previous evaluation.
func TestParseDefaultsWindowToGroupInterval(t *testing.T) {
	f, problems := Parse("testdata/rule_window.yaml", readFixture(t, "rule_window.yaml"))
	if len(problems) != 0 {
		t.Fatalf("fixture should parse cleanly, got %v", problems)
	}

	got := map[string]time.Duration{}
	for _, r := range f.Groups[0].Rules {
		got[r.Alert] = r.Window
	}

	for alert, want := range map[string]time.Duration{
		"NegativeWindow": -5 * time.Minute,
		"ShortWindow":    30 * time.Second,
		"DefaultWindow":  time.Minute,
	} {
		if got[alert] != want {
			t.Errorf("%s window = %v, want %v", alert, got[alert], want)
		}
	}
}

func TestParseReportsSyntaxError(t *testing.T) {
	_, got := Parse("bad.yaml", []byte("groups:\n  - name: x\n   interval: 1m\n"))

	if len(got) != 1 {
		t.Fatalf("got %d problems, want 1: %v", len(got), got)
	}
	if got[0].Check != lint.CheckYAMLSyntax {
		t.Errorf("check = %q, want %q", got[0].Check, lint.CheckYAMLSyntax)
	}
	if got[0].Severity != lint.SeverityError {
		t.Errorf("severity = %v, want error", got[0].Severity)
	}
}

// Line numbers drive inline CI comments, so they are part of the contract and
// are asserted directly rather than left to the checks that consume them.
func TestParseRecordsLineNumbers(t *testing.T) {
	f, problems := Parse("testdata/valid.yaml", readFixture(t, "valid.yaml"))
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}

	r := f.Groups[0].Rules[0]
	for _, tc := range []struct {
		key  string
		want int
	}{
		{"alert", 5},
		{"source", 6},
		{"expr", 7},
		{"for", 15},
		{"labels", 16},
		{"labels.team", 17},
		{"labels.severity", 18},
		{"annotations", 19},
		{"annotations.summary", 20},
		{"annotations.runbook_url", 21},
	} {
		if got := r.lineOf(tc.key); got != tc.want {
			t.Errorf("lineOf(%q) = %d, want %d", tc.key, got, tc.want)
		}
	}

	if got := f.Groups[0].lineOf("name"); got != 2 {
		t.Errorf("group lineOf(name) = %d, want 2", got)
	}
	if r.Line() != 5 {
		t.Errorf("rule Line = %d, want 5", r.Line())
	}
}
