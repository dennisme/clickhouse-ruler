package lint

import (
	"strings"
	"testing"
)

func sample() []Problem {
	return []Problem{
		{
			File: "rules/payments/latency.yaml", Line: 12, Subject: "HighP99Latency",
			Check: "rule/expr", Severity: SeverityError,
			Text: "expr does not reference {{ .To }}, so the query has no upper time bound",
		},
		{
			File: "rules/payments/latency.yaml", Line: 5, Subject: "HighP99Latency",
			Check: "labels/required", Severity: SeverityWarning,
			Text:       `required label "team" is missing`,
			PolicyFile: "rules/ruler.yaml", PolicyLine: 4,
		},
	}
}

func TestFormatText(t *testing.T) {
	var sb strings.Builder
	if err := Format(&sb, FormatText, sample()); err != nil {
		t.Fatalf("Format: %v", err)
	}

	want := `rules/payments/latency.yaml:12 HighP99Latency: error: rule/expr: expr does not reference {{ .To }}, so the query has no upper time bound
rules/payments/latency.yaml:5 HighP99Latency: warning: labels/required: required label "team" is missing
`
	if got := sb.String(); got != want {
		t.Errorf("text output:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// GitHub renders these inline on the diff. Severity has to map to the right
// command or an error shows up as a neutral note and nobody sees it.
func TestFormatGitHub(t *testing.T) {
	var sb strings.Builder
	if err := Format(&sb, FormatGitHub, sample()); err != nil {
		t.Fatalf("Format: %v", err)
	}

	want := `::error file=rules/payments/latency.yaml,line=12,title=rule/expr::expr does not reference {{ .To }}, so the query has no upper time bound
::warning file=rules/payments/latency.yaml,line=5,title=labels/required::required label "team" is missing
`
	if got := sb.String(); got != want {
		t.Errorf("github output:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// A newline inside a workflow command truncates it, so the rest of the message
// is silently dropped from the annotation.
func TestFormatGitHubEscapesNewlines(t *testing.T) {
	var sb strings.Builder
	problems := []Problem{{
		File: "a.yaml", Line: 1, Check: "rule/expr", Severity: SeverityError,
		Text: "line one\nline two",
	}}
	if err := Format(&sb, FormatGitHub, problems); err != nil {
		t.Fatalf("Format: %v", err)
	}

	got := sb.String()
	if strings.Count(got, "\n") != 1 {
		t.Errorf("output must be one line, got:\n%s", got)
	}
	if !strings.Contains(got, "%0A") {
		t.Errorf("newline must be escaped as %%0A, got: %s", got)
	}
}

func TestFormatRejectsUnknownFormat(t *testing.T) {
	var sb strings.Builder
	if err := Format(&sb, "json", sample()); err == nil {
		t.Fatal("expected an error for an unknown format")
	}
}
