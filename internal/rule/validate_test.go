package rule

import (
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
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
					Line:     25,
					Subject:  "DuplicateName",
					Check:    "rule/name",
					Severity: lint.SeverityError,
					Text:     `duplicate alert name "DuplicateName" in group "api-latency", first defined on line 15`,
				},
			},
		},
		{
			// A group's identity is (file, name), which is what the
			// scheduler keys on and what the rule_group metric label
			// carries. Two groups sharing a name in one file collapse onto
			// one series (spec 7.6, 8.2).
			fixture: "group_name.yaml",
			want: []lint.Problem{
				{
					File:     "testdata/group_name.yaml",
					Line:     15,
					Subject:  "api-latency",
					Check:    "rule/group-name",
					Severity: lint.SeverityError,
					Text:     `duplicate group name "api-latency", first defined on line 2`,
				},
			},
		},
		{
			// An alert's identity is its full label set, not its name, so
			// the same name in two groups is allowed: group labels are part
			// of that set and the two are genuinely different alerts. This
			// pins the decision rather than leaving it merely unchecked.
			fixture: "rule_name_across_groups.yaml",
			want:    nil,
		},
		{
			fixture: "rule_expr.yaml",
			want: []lint.Problem{
				{
					File:     "testdata/rule_expr.yaml",
					Line:     8,
					Subject:  "EmptyExpr",
					Check:    "rule/expr",
					Severity: lint.SeverityError,
					Text:     "expr is empty",
				},
				{
					File:     "testdata/rule_expr.yaml",
					Line:     28,
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
					Severity: lint.SeverityWarning,
					Text:     `required label "severity" is missing`,
				},
				{
					File:     "testdata/labels_required.yaml",
					Line:     5,
					Subject:  "NoLabels",
					Check:    "labels/required",
					Severity: lint.SeverityWarning,
					Text:     `required label "team" is missing`,
				},
				{
					File:     "testdata/labels_required.yaml",
					Line:     17,
					Subject:  "EmptyTeam",
					Check:    "labels/required",
					Severity: lint.SeverityWarning,
					Text:     `required label "team" is empty`,
				},
				{
					File:     "testdata/labels_required.yaml",
					Line:     26,
					Subject:  "MissingSeverity",
					Check:    "labels/required",
					Severity: lint.SeverityWarning,
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
					Severity: lint.SeverityWarning,
					Text:     `required annotation "runbook_url" is missing`,
				},
				{
					File:     "testdata/annotations_required.yaml",
					Line:     5,
					Subject:  "NoAnnotations",
					Check:    "annotations/required",
					Severity: lint.SeverityWarning,
					Text:     `required annotation "summary" is missing`,
				},
				{
					File:     "testdata/annotations_required.yaml",
					Line:     19,
					Subject:  "MissingRunbook",
					Check:    "annotations/required",
					Severity: lint.SeverityWarning,
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
					Line:     14,
					Subject:  "RelativeRunbook",
					Check:    "annotations/runbook",
					Severity: lint.SeverityWarning,
					Text:     `runbook_url must be an absolute http or https URL, got "/runbooks/relative"`,
				},
				{
					File:     "testdata/annotations_runbook.yaml",
					Line:     24,
					Subject:  "NoScheme",
					Check:    "annotations/runbook",
					Severity: lint.SeverityWarning,
					Text:     `runbook_url must be an absolute http or https URL, got "runbooks.internal/no-scheme"`,
				},
			},
		},
		{
			// UnknownColumn must not be reported: whether the query returns
			// the column a variable names is only knowable from the result,
			// which is tier 1 (spec 7.3). This check covers parsing.
			fixture: "annotations_template.yaml",
			want: []lint.Problem{
				{
					File:     "testdata/annotations_template.yaml",
					Line:     13,
					Subject:  "UnclosedAction",
					Check:    "annotations/template",
					Severity: lint.SeverityWarning,
					Text: `annotation "summary" is not a valid template: ` +
						`template: summary:1: unexpected "}" in operand`,
				},
				{
					File:     "testdata/annotations_template.yaml",
					Line:     23,
					Subject:  "UnknownFunction",
					Check:    "annotations/template",
					Severity: lint.SeverityWarning,
					Text: `annotation "summary" is not a valid template: ` +
						`template: summary:1: function "humanize" not defined`,
				},
				{
					File:     "testdata/annotations_template.yaml",
					Line:     34,
					Subject:  "BrokenRunbookTemplate",
					Check:    "annotations/template",
					Severity: lint.SeverityWarning,
					Text: `annotation "runbook_url" is not a valid template: ` +
						`template: runbook_url:1: unclosed action`,
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
					Line:     9,
					Subject:  "NegativeFor",
					Check:    "rule/for",
					Severity: lint.SeverityError,
					Text:     "for must not be negative, got -5m0s",
				},
				{
					File:     "testdata/rule_for.yaml",
					Line:     20,
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
					Line:     9,
					Subject:  "NegativeWindow",
					Check:    "rule/window",
					Severity: lint.SeverityError,
					Text:     "window must not be negative, got -5m0s",
				},
				{
					File:     "testdata/rule_window.yaml",
					Line:     20,
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

			got := Validate(f, nil)
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

// A rule may take team from its group rather than repeating it (spec 6.3.1).
// labels/required has to check the labels the alert actually ends up with, not
// just the ones written on the rule, or group labels are useless.
func TestValidateRequiredLabelsSatisfiedByGroup(t *testing.T) {
	f, problems := Parse("testdata/group_labels.yaml", readFixture(t, "group_labels.yaml"))
	if len(problems) != 0 {
		t.Fatalf("parse problems: %v", problems)
	}

	if got := Validate(f, nil); len(got) != 0 {
		t.Fatalf("expected no problems, got %d: %v", len(got), got)
	}
}

// An operator who would rather a broken annotation template never reach a pager
// raises the check to an error, which refuses to start the ruler and blocks the
// merge. Soft is the shipped default; hard is theirs to choose (spec 7.6).
func TestAnnotationsTemplateSeverityIsTheOperatorsChoice(t *testing.T) {
	f, problems := Parse("testdata/annotations_template.yaml", readFixture(t, "annotations_template.yaml"))
	if len(problems) != 0 {
		t.Fatalf("parse problems: %v", problems)
	}

	for _, tc := range []struct {
		name     string
		severity lint.Severity
		want     int
	}{
		{name: "shipped default", severity: lint.SeverityWarning, want: 3},
		{name: "raised to error", severity: lint.SeverityError, want: 3},
		{name: "turned off", severity: lint.SeverityOff, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &policy.Policy{Checks: map[string]policy.Setting{
				lint.CheckAnnotationsTemplate: {Severity: tc.severity},
			}}

			var got []lint.Problem
			for _, problem := range Validate(f, func(Rule) *policy.Policy { return p }) {
				if problem.Check == "annotations/template" {
					got = append(got, problem)
				}
			}

			if len(got) != tc.want {
				t.Fatalf("got %d annotations/template problems, want %d", len(got), tc.want)
			}
			for _, problem := range got {
				if problem.Severity != tc.severity {
					t.Errorf("severity = %v, want %v", problem.Severity, tc.severity)
				}
			}
		})
	}
}
