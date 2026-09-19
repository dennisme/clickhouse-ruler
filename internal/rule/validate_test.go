package rule

import (
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// Each fixture is otherwise valid so that exactly one check fires, which keeps
// the expected sets stable as further checks are added.
func TestValidate(t *testing.T) {
	tests := []struct {
		fixture string
		want    []lint.Problem
	}{
		{
			fixture: "valid.yaml",
			want:    nil,
		},
		{
			fixture: "rule_name.yaml",
			want: []lint.Problem{
				{
					File:     "testdata/rule_name.yaml",
					Line:     5,
					Subject:  "",
					Check:    "rule/name",
					Severity: lint.SeverityError,
					Text:     "alert name is empty",
				},
				{
					File:     "testdata/rule_name.yaml",
					Line:     23,
					Subject:  "DuplicateName",
					Check:    "rule/name",
					Severity: lint.SeverityError,
					Text:     `duplicate alert name "DuplicateName" in group "api-latency", first defined on line 14`,
				},
			},
		},
		{
			fixture: "rule_source.yaml",
			want: []lint.Problem{
				{
					File:     "testdata/rule_source.yaml",
					Line:     6,
					Subject:  "NoSource",
					Check:    "rule/source",
					Severity: lint.SeverityError,
					Text:     "source is empty",
				},
			},
		},
		{
			fixture: "rule_expr.yaml",
			want: []lint.Problem{
				{
					File:     "testdata/rule_expr.yaml",
					Line:     7,
					Subject:  "EmptyExpr",
					Check:    "rule/expr",
					Severity: lint.SeverityError,
					Text:     "expr is empty",
				},
				{
					File:     "testdata/rule_expr.yaml",
					Line:     25,
					Subject:  "MissingTo",
					Check:    "rule/expr",
					Severity: lint.SeverityError,
					Text:     "expr does not reference {{ .To }}, so the query has no upper time bound",
				},
			},
		},
		{
			fixture: "labels_required.yaml",
			want: []lint.Problem{
				{
					File:     "testdata/labels_required.yaml",
					Line:     5,
					Subject:  "NoLabels",
					Check:    "labels/required",
					Severity: lint.SeverityError,
					Text:     `required label "team" is missing`,
				},
				{
					File:     "testdata/labels_required.yaml",
					Line:     5,
					Subject:  "NoLabels",
					Check:    "labels/required",
					Severity: lint.SeverityError,
					Text:     `required label "severity" is missing`,
				},
				{
					File:     "testdata/labels_required.yaml",
					Line:     15,
					Subject:  "EmptyTeam",
					Check:    "labels/required",
					Severity: lint.SeverityError,
					Text:     `required label "team" is empty`,
				},
				{
					File:     "testdata/labels_required.yaml",
					Line:     23,
					Subject:  "MissingSeverity",
					Check:    "labels/required",
					Severity: lint.SeverityError,
					Text:     `required label "severity" is missing`,
				},
			},
		},
		{
			fixture: "annotations_required.yaml",
			want: []lint.Problem{
				{
					File:     "testdata/annotations_required.yaml",
					Line:     5,
					Subject:  "NoAnnotations",
					Check:    "annotations/required",
					Severity: lint.SeverityError,
					Text:     `required annotation "summary" is missing`,
				},
				{
					File:     "testdata/annotations_required.yaml",
					Line:     5,
					Subject:  "NoAnnotations",
					Check:    "annotations/required",
					Severity: lint.SeverityError,
					Text:     `required annotation "runbook_url" is missing`,
				},
				{
					File:     "testdata/annotations_required.yaml",
					Line:     17,
					Subject:  "MissingRunbook",
					Check:    "annotations/required",
					Severity: lint.SeverityError,
					Text:     `required annotation "runbook_url" is missing`,
				},
			},
		},
		{
			// TemplatedRunbook must not be reported: annotations are rendered
			// per alert instance, so a template action in the URL is legal.
			fixture: "annotations_runbook.yaml",
			want: []lint.Problem{
				{
					File:     "testdata/annotations_runbook.yaml",
					Line:     13,
					Subject:  "RelativeRunbook",
					Check:    "annotations/runbook",
					Severity: lint.SeverityError,
					Text:     `runbook_url must be an absolute http or https URL, got "/runbooks/relative"`,
				},
				{
					File:     "testdata/annotations_runbook.yaml",
					Line:     22,
					Subject:  "NoScheme",
					Check:    "annotations/runbook",
					Severity: lint.SeverityError,
					Text:     `runbook_url must be an absolute http or https URL, got "runbooks.internal/no-scheme"`,
				},
			},
		},
		{
			// NoFor and ExplicitZeroFor must not be reported. Firing on the
			// first evaluation is a legitimate choice, and warning about it
			// would fire on most rules in a repository.
			fixture: "rule_for.yaml",
			want: []lint.Problem{
				{
					File:     "testdata/rule_for.yaml",
					Line:     8,
					Subject:  "NegativeFor",
					Check:    "rule/for",
					Severity: lint.SeverityError,
					Text:     "for must not be negative, got -5m0s",
				},
				{
					File:     "testdata/rule_for.yaml",
					Line:     18,
					Subject:  "ShortFor",
					Check:    "rule/for",
					Severity: lint.SeverityWarning,
					Text:     "for (30s) is shorter than the group interval (1m0s), so it rounds up to one interval",
				},
			},
		},
		{
			// DefaultWindow must not be reported. Taking the group interval
			// is the intended behaviour, not something to warn about.
			fixture: "rule_window.yaml",
			want: []lint.Problem{
				{
					File:     "testdata/rule_window.yaml",
					Line:     8,
					Subject:  "NegativeWindow",
					Check:    "rule/window",
					Severity: lint.SeverityError,
					Text:     "window must not be negative, got -5m0s",
				},
				{
					File:     "testdata/rule_window.yaml",
					Line:     18,
					Subject:  "ShortWindow",
					Check:    "rule/window",
					Severity: lint.SeverityWarning,
					Text:     "window (30s) is shorter than the group interval (1m0s), so data between evaluations is never examined",
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			path := "testdata/" + tc.fixture
			f, parseProblems := Parse(path, readFixture(t, tc.fixture))
			if len(parseProblems) != 0 {
				t.Fatalf("fixture should parse cleanly, got %v", parseProblems)
			}

			got := Validate(f)
			assertProblems(t, got, tc.want)
		})
	}
}

func assertProblems(t *testing.T, got, want []lint.Problem) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d problems, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("problem %d:\n got: %+v\nwant: %+v", i, got[i], want[i])
		}
	}
}
