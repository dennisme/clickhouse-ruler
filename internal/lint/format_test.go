package lint

import (
	"encoding/json"
	"path/filepath"
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
			PolicyFile: "rules/policy.yaml", PolicyLine: 4,
		},
	}
}

func TestFormatText(t *testing.T) {
	var sb strings.Builder
	if err := Format(&sb, FormatText, sample()); err != nil {
		t.Fatalf("Format: %v", err)
	}

	want := `rules/payments/latency.yaml:12 HighP99Latency: error: rule/expr: expr does not reference {{ .To }}, so the query has no upper time bound
  ` + DocsURL("rule/expr") + `
rules/payments/latency.yaml:5 HighP99Latency: warning: labels/required: required label "team" is missing
  ` + DocsURL("labels/required") + `
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

	want := `::error file=rules/payments/latency.yaml,line=12,title=rule/expr::expr does not reference {{ .To }}, so the query has no upper time bound (` + DocsURL("rule/expr") + `)
::warning file=rules/payments/latency.yaml,line=5,title=labels/required::required label "team" is missing (` + DocsURL("labels/required") + `)
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
	if err := Format(&sb, "xml", sample()); err == nil {
		t.Fatal("expected an error for an unknown format")
	}
}

// The feed a summary comment reads (spec 10.3). One array, so a consumer can
// stream it or slurp it, and every field the text output prints.
func TestFormatJSON(t *testing.T) {
	var sb strings.Builder
	if err := Format(&sb, FormatJSON, sample()); err != nil {
		t.Fatalf("Format: %v", err)
	}

	var got []struct {
		File       string `json:"file"`
		Line       int    `json:"line"`
		Subject    string `json:"subject"`
		Check      string `json:"check"`
		Severity   string `json:"severity"`
		Text       string `json:"text"`
		Docs       string `json:"docs"`
		PolicyFile string `json:"policy_file"`
		PolicyLine int    `json:"policy_line"`
	}
	if err := json.Unmarshal([]byte(sb.String()), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, sb.String())
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 findings:\n%s", len(got), sb.String())
	}

	first := got[0]
	if first.File != "rules/payments/latency.yaml" || first.Line != 12 {
		t.Errorf("location = %s:%d, want rules/payments/latency.yaml:12", first.File, first.Line)
	}
	if first.Subject != "HighP99Latency" || first.Check != "rule/expr" {
		t.Errorf("subject/check = %q/%q, want HighP99Latency/rule/expr", first.Subject, first.Check)
	}
	if first.Text == "" {
		t.Error("text is empty")
	}
	if first.Docs != DocsURL("rule/expr") {
		t.Errorf("docs = %q, want %q", first.Docs, DocsURL("rule/expr"))
	}

	// Where a severity was set is what --explain answers, and a comment
	// builder needs it for the same reason (spec 7.8).
	if got[1].PolicyFile != "rules/policy.yaml" || got[1].PolicyLine != 4 {
		t.Errorf("policy origin = %s:%d, want rules/policy.yaml:4", got[1].PolicyFile, got[1].PolicyLine)
	}
}

// A severity is its name. The numbers exist so policy merging can take a
// maximum across scopes (spec 7.7), and publishing them would make an
// internal ordering something a consumer parses (spec 10.3).
func TestFormatJSONNamesSeverity(t *testing.T) {
	var sb strings.Builder
	if err := Format(&sb, FormatJSON, sample()); err != nil {
		t.Fatalf("Format: %v", err)
	}

	got := sb.String()
	for _, want := range []string{`"severity": "error"`, `"severity": "warning"`} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"severity": 2`) || strings.Contains(got, `"severity": 1`) {
		t.Errorf("severity must not marshal as its number:\n%s", got)
	}
}

// No findings is the common case on a green pull request, and an empty array
// is what a consumer can iterate. `null` makes every reader special-case it.
func TestFormatJSONEmptyIsAnArray(t *testing.T) {
	var sb strings.Builder
	if err := Format(&sb, FormatJSON, nil); err != nil {
		t.Fatalf("Format: %v", err)
	}
	if got := strings.TrimSpace(sb.String()); got != "[]" {
		t.Errorf("empty output = %q, want []", got)
	}
}

// Both formats carry the link, because a finding naming a check an author
// cannot look up turns their own problem into a question for whoever owns
// policy (spec 7.8).
func TestBothFormatsLinkTheCheck(t *testing.T) {
	for _, format := range Formats {
		var sb strings.Builder
		if err := Format(&sb, format, sample()); err != nil {
			t.Fatalf("Format(%s): %v", format, err)
		}
		if !strings.Contains(sb.String(), DocsURL("rule/expr")) {
			t.Errorf("%s output does not link the check:\n%s", format, sb.String())
		}
	}
}

// dataRows counts the table's findings, leaving out the heading and the
// divider, which also begin with a pipe.
func dataRows(table string) int {
	n := 0
	for _, line := range strings.Split(table, "\n") {
		if strings.HasPrefix(line, "| error |") || strings.HasPrefix(line, "| warning |") {
			n++
		}
	}
	return n
}

// The markdown table is what a pull request comment carries, and it is built
// here rather than in the action for the reason FormatSummary already exists
// here: a table and its cell escaping are the same problem whoever reads it,
// and one written in shell is one no test can reach (spec 10.3).
func TestFormatMarkdown(t *testing.T) {
	var sb strings.Builder
	if err := FormatMarkdown(&sb, sample(), ""); err != nil {
		t.Fatalf("FormatMarkdown: %v", err)
	}
	got := sb.String()

	// The counts are the first thing read, and they are what a reader checks
	// against the build they are looking at.
	if !strings.Contains(got, "1 error, 1 warning") {
		t.Errorf("heading should count each severity, got:\n%s", got)
	}
	if rows := dataRows(got); rows != 2 {
		t.Errorf("want one row per finding, got %d:\n%s", rows, got)
	}
	if !strings.Contains(got, "`rules/payments/latency.yaml:12`") {
		t.Errorf("a location with no link prefix is plain, got:\n%s", got)
	}
	if !strings.Contains(got, "[`rule/expr`]("+DocsURL("rule/expr")+")") {
		t.Errorf("the check should link its documentation, got:\n%s", got)
	}
	if !strings.Contains(got, "HighP99Latency") {
		t.Errorf("the subject names the rule the finding belongs to, got:\n%s", got)
	}
}

// A link to the line is what saves a reader searching for the rule. The prefix
// carries the commit, which the binary cannot know: where a file is served from
// is a fact about the host, not about the rules.
func TestFormatMarkdownLinksTheLine(t *testing.T) {
	var sb strings.Builder
	prefix := "https://github.com/o/r/blob/abc123/"
	if err := FormatMarkdown(&sb, sample(), prefix); err != nil {
		t.Fatalf("FormatMarkdown: %v", err)
	}

	want := "[`rules/payments/latency.yaml:12`](" + prefix + "rules/payments/latency.yaml#L12)"
	if got := sb.String(); !strings.Contains(got, want) {
		t.Errorf("want a link to the line:\n%s\ngot:\n%s", want, got)
	}
}

// A pipe ends a cell early and shifts every value after it under the wrong
// heading. A newline ends the row.
func TestFormatMarkdownEscapesCells(t *testing.T) {
	problems := []Problem{{
		File: "a.yaml", Line: 1, Check: "rule/expr", Severity: SeverityError,
		Text: "one | two\nthree",
	}}

	var sb strings.Builder
	if err := FormatMarkdown(&sb, problems, ""); err != nil {
		t.Fatalf("FormatMarkdown: %v", err)
	}

	got := sb.String()
	if rows := dataRows(got); rows != 1 {
		t.Errorf("the finding must stay one row, got %d:\n%s", rows, got)
	}
	if !strings.Contains(got, `one \| two three`) {
		t.Errorf("a pipe should be escaped and a newline flattened, got:\n%s", got)
	}
}

// The common case on a green pull request. A comment saying nothing was found
// is how a reader tells the checks ran from the checks being skipped.
func TestFormatMarkdownNoFindings(t *testing.T) {
	var sb strings.Builder
	if err := FormatMarkdown(&sb, nil, ""); err != nil {
		t.Fatalf("FormatMarkdown: %v", err)
	}
	if got := sb.String(); !strings.Contains(got, "No findings") {
		t.Errorf("want a green result stated, got:\n%s", got)
	}
}

// A path the repository cannot spell gets no link rather than a broken one: an
// absolute path outside the working directory names a directory on one machine.
func TestFormatMarkdownSkipsALinkItCannotBuild(t *testing.T) {
	problems := []Problem{{
		File: filepath.Join(t.TempDir(), "rules", "a.yaml"), Line: 3,
		Check: "rule/expr", Severity: SeverityError, Text: "text",
	}}

	var sb strings.Builder
	if err := FormatMarkdown(&sb, problems, "https://github.com/o/r/blob/abc123/"); err != nil {
		t.Fatalf("FormatMarkdown: %v", err)
	}
	if got := sb.String(); strings.Contains(got, "https://github.com/o/r/blob") {
		t.Errorf("want no link for an unspellable path, got:\n%s", got)
	}
}
