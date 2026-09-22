package policy

import (
	"os"
	"path/filepath"
	"testing"

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

func TestParseReadsSeverityAndKeys(t *testing.T) {
	p, problems := Parse("testdata/valid.yaml", readFixture(t, "valid.yaml"))
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got: %v", problems)
	}

	got := p.For("labels/required")
	if got.Severity != lint.SeverityError {
		t.Errorf("labels/required severity = %v, want error", got.Severity)
	}
	if len(got.Keys) != 3 {
		t.Errorf("labels/required keys = %v, want 3 keys", got.Keys)
	}
	// The origin drives --explain, so it is part of the contract.
	if got.File != "testdata/valid.yaml" {
		t.Errorf("origin file = %q, want testdata/valid.yaml", got.File)
	}
	if got.Line != 3 {
		t.Errorf("origin line = %d, want 3", got.Line)
	}

	if s := p.For("annotations/runbook").Severity; s != lint.SeverityOff {
		t.Errorf("annotations/runbook severity = %v, want off", s)
	}
}

// Naming a correctness check in the config has to fail loudly. Accepting it
// silently would leave an operator believing they had softened a check that
// still errors, which is worse than refusing.
func TestParseRejectsBadConfiguration(t *testing.T) {
	_, problems := Parse("testdata/problems.yaml", readFixture(t, "problems.yaml"))

	want := map[string]int{
		"policy/fixed-check":   2,
		"policy/unknown-check": 4,
		"policy/severity":      7,
		"yaml/unknown-field":   9,
	}
	if len(problems) != len(want) {
		t.Fatalf("got %d problems, want %d: %v", len(problems), len(want), problems)
	}
	for _, p := range problems {
		line, ok := want[p.Check]
		if !ok {
			t.Errorf("unexpected check %q: %v", p.Check, p)
			continue
		}
		if p.Line != line {
			t.Errorf("%s line = %d, want %d", p.Check, p.Line, line)
		}
		if p.Severity != lint.SeverityError {
			t.Errorf("%s severity = %v, want error", p.Check, p.Severity)
		}
	}
}

// Nothing configured means the shipped defaults apply, and they are warnings.
// A rule missing a team label should not fail to load out of the box.
func TestDefaultsAreWarnings(t *testing.T) {
	d := Defaults()

	for _, check := range []string{
		CheckLabelsRequired, CheckAnnotationsRequired,
		CheckAnnotationsRunbook, CheckAnnotationsTemplate,
		CheckRuleFor, CheckRuleWindow,
	} {
		if got := d.For(check).Severity; got != lint.SeverityWarning {
			t.Errorf("%s default severity = %v, want warning", check, got)
		}
	}
	if got := d.For("rule/expr").Severity; got != lint.SeverityError {
		t.Errorf("rule/expr severity = %v, want error, it is not configurable", got)
	}
}

// The contract in spec 6.7.2 is configurable the same way every other check
// with a list is: a severity and a list of what it requires. An operator on a
// managed cluster that will not expose settings profiles drops the one
// assertion they cannot satisfy, rather than turning the whole check off and
// losing the tenancy half with it (spec 7.6).
func TestSourcePrivilegesDefaults(t *testing.T) {
	got := Defaults().For(CheckSourcePrivileges)

	if got.Severity != lint.SeverityWarning {
		t.Errorf("severity = %v, want warning", got.Severity)
	}
	if len(got.Keys) != 4 {
		t.Errorf("keys = %v, want every assertion required by default", got.Keys)
	}
	if !Configurable(CheckSourcePrivileges) {
		t.Error("source/privileges must be configurable")
	}
	if Fixed(CheckSourcePrivileges) {
		t.Error("source/privileges must not be a correctness check")
	}
}

// Severity takes the maximum and the assertion list is unioned, so a source
// cannot drop an assertion the instance policy requires.
func TestSourcePrivilegesMergeIsStrictest(t *testing.T) {
	instance := &Policy{Checks: map[string]Setting{
		CheckSourcePrivileges: {Severity: lint.SeverityError, Keys: []string{"sources-revoked"}},
	}}
	source := &Policy{Checks: map[string]Setting{
		CheckSourcePrivileges: {Severity: lint.SeverityOff, Keys: []string{"readonly"}},
	}}

	got := Merge(instance, source).For(CheckSourcePrivileges)
	if got.Severity != lint.SeverityError {
		t.Errorf("severity = %v, want error: a source cannot soften the instance policy", got.Severity)
	}
	if len(got.Keys) != 4 {
		t.Errorf("keys = %v, want the default four unioned with both scopes", got.Keys)
	}
}
