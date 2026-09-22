package policy

import (
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

func scope(file string, sev lint.Severity, keys ...string) *Policy {
	return &Policy{
		File: file,
		Checks: map[string]Setting{
			CheckLabelsRequired: {Severity: sev, Keys: keys, File: file, Line: 1},
		},
	}
}

// Merging takes the strictest setting, so no scope can loosen another. That is
// what makes a team-owned policy file safe to allow later: the worst it can do
// is make its own rules stricter (spec 7.7).
func TestMergeTakesStrictestSeverity(t *testing.T) {
	got := Merge(
		scope("ruler.yaml", lint.SeverityWarning),
		scope("sources.yaml", lint.SeverityError),
	)
	if s := got.For(CheckLabelsRequired).Severity; s != lint.SeverityError {
		t.Errorf("severity = %v, want error", s)
	}
}

func TestMergeCannotLowerSeverity(t *testing.T) {
	got := Merge(
		scope("sources.yaml", lint.SeverityError),
		scope("team.yaml", lint.SeverityOff),
	)
	if s := got.For(CheckLabelsRequired).Severity; s != lint.SeverityError {
		t.Errorf("severity = %v, want error: a later scope must not loosen an earlier one", s)
	}
}

// The merge is a maximum, so there is no precedence rule to remember. Feeding
// the same scopes in either order has to give the same answer, and that
// property is the reason for choosing this merge.
func TestMergeIsOrderIndependent(t *testing.T) {
	a := scope("a.yaml", lint.SeverityWarning, "team")
	b := scope("b.yaml", lint.SeverityError, "tier")

	ab := Merge(a, b).For(CheckLabelsRequired)
	ba := Merge(b, a).For(CheckLabelsRequired)

	if ab.Severity != ba.Severity {
		t.Errorf("severity depends on order: %v vs %v", ab.Severity, ba.Severity)
	}
	if len(ab.Keys) != len(ba.Keys) {
		t.Errorf("keys depend on order: %v vs %v", ab.Keys, ba.Keys)
	}
	if ab.File != ba.File {
		t.Errorf("origin depends on order: %q vs %q", ab.File, ba.File)
	}
}

func TestMergeUnionsKeys(t *testing.T) {
	got := Merge(
		scope("ruler.yaml", lint.SeverityWarning, "team", "severity"),
		scope("sources.yaml", lint.SeverityWarning, "tier", "team"),
	).For(CheckLabelsRequired)

	want := []string{"severity", "team", "tier"}
	if len(got.Keys) != len(want) {
		t.Fatalf("keys = %v, want %v", got.Keys, want)
	}
	for i, k := range want {
		if got.Keys[i] != k {
			t.Errorf("keys = %v, want %v", got.Keys, want)
			break
		}
	}
}

// The winning scope has to be identifiable, or --explain cannot say why a
// check is an error.
func TestMergeKeepsOriginOfWinningScope(t *testing.T) {
	got := Merge(
		scope("ruler.yaml", lint.SeverityWarning),
		scope("sources.yaml", lint.SeverityError),
	).For(CheckLabelsRequired)

	if got.File != "sources.yaml" {
		t.Errorf("origin = %q, want sources.yaml, the scope that raised it", got.File)
	}
}

// Merging nothing is legal and yields the defaults, which is what happens when
// no policy file exists anywhere.
func TestMergeWithNoScopesGivesDefaults(t *testing.T) {
	if s := Merge().For(CheckLabelsRequired).Severity; s != lint.SeverityWarning {
		t.Errorf("severity = %v, want the default warning", s)
	}
}

// The shipped defaults are what a check falls back to when nothing configures
// it, not a floor every scope is raised to. Treating them as a floor made
// `severity: off` unreachable: the default warning always won, so the one
// setting that means "nobody is asked" could be written and never took
// effect (spec 7.6).
func TestMergeLetsAnOperatorTurnACheckOff(t *testing.T) {
	got := Merge(scope("ruler.yaml", lint.SeverityOff)).For(CheckLabelsRequired)

	if got.Severity != lint.SeverityOff {
		t.Errorf("severity = %v, want off", got.Severity)
	}
}

// Turning one check off leaves every other check at its default.
func TestMergeLeavesUnconfiguredChecksAtTheirDefaults(t *testing.T) {
	got := Merge(scope("ruler.yaml", lint.SeverityOff))

	if s := got.For(CheckAnnotationsRunbook).Severity; s != lint.SeverityWarning {
		t.Errorf("annotations/runbook severity = %v, want the default warning", s)
	}
}

// A scope may add keys and may not drop the shipped ones, so a check turned
// up by one scope still requires everything the defaults asked for.
func TestMergeKeepsDefaultKeys(t *testing.T) {
	got := Merge(scope("ruler.yaml", lint.SeverityError, "tier")).For(CheckLabelsRequired)

	want := []string{"severity", "team", "tier"}
	if len(got.Keys) != len(want) {
		t.Fatalf("keys = %v, want %v", got.Keys, want)
	}
	for i, k := range want {
		if got.Keys[i] != k {
			t.Fatalf("keys = %v, want %v", got.Keys, want)
		}
	}
}
