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
		lint.CheckLabelsRequired, lint.CheckAnnotationsRequired,
		lint.CheckAnnotationsRunbook, lint.CheckAnnotationsTemplate,
		lint.CheckRuleFor, lint.CheckRuleWindow,
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
	got := Defaults().For(lint.CheckSourcePrivileges)

	if got.Severity != lint.SeverityWarning {
		t.Errorf("severity = %v, want warning", got.Severity)
	}
	if len(got.Keys) != 4 {
		t.Errorf("keys = %v, want every assertion required by default", got.Keys)
	}
	if !lint.Configurable(lint.CheckSourcePrivileges) {
		t.Error("source/privileges must be configurable")
	}
	if lint.Fixed(lint.CheckSourcePrivileges) {
		t.Error("source/privileges must not be a correctness check")
	}
}

// Severity takes the maximum and the assertion list is unioned, so a source
// cannot drop an assertion the instance policy requires.
func TestSourcePrivilegesMergeIsStrictest(t *testing.T) {
	instance := &Policy{Checks: map[string]Setting{
		lint.CheckSourcePrivileges: {Severity: lint.SeverityError, Keys: []string{"sources-revoked"}},
	}}
	source := &Policy{Checks: map[string]Setting{
		lint.CheckSourcePrivileges: {Severity: lint.SeverityOff, Keys: []string{"readonly"}},
	}}

	got := Merge(instance, source).For(lint.CheckSourcePrivileges)
	if got.Severity != lint.SeverityError {
		t.Errorf("severity = %v, want error: a source cannot soften the instance policy", got.Severity)
	}
	if len(got.Keys) != 4 {
		t.Errorf("keys = %v, want the default four unioned with both scopes", got.Keys)
	}
}

// The ceilings ride on the same key list every other configurable check
// uses, written as name:N so one Setting carries both.
func TestComplexityLimits(t *testing.T) {
	got := Defaults().For(lint.CheckRuleComplexity)

	if got.Severity != lint.SeverityWarning {
		t.Errorf("severity = %v, want warning", got.Severity)
	}
	for _, name := range []string{lint.LimitJoins, lint.LimitSubqueries} {
		if _, ok := got.Limit(name); !ok {
			t.Errorf("%s has no default ceiling", name)
		}
	}
	if _, ok := got.Limit("max-something-else"); ok {
		t.Error("a ceiling nobody set was read from the key list")
	}
}

// Merge unions key lists, so both scopes' ceilings survive and the lowest is
// the one that binds. That is what keeps a scope from loosening another.
func TestComplexityLimitTakesTheLowest(t *testing.T) {
	instance := &Policy{Checks: map[string]Setting{
		lint.CheckRuleComplexity: {Severity: lint.SeverityWarning, Keys: []string{lint.LimitJoins + ":1"}},
	}}
	source := &Policy{Checks: map[string]Setting{
		lint.CheckRuleComplexity: {Severity: lint.SeverityWarning, Keys: []string{lint.LimitJoins + ":9"}},
	}}

	got, ok := Merge(instance, source).For(lint.CheckRuleComplexity).Limit(lint.LimitJoins)
	if !ok {
		t.Fatal("the merged policy has no join ceiling")
	}
	if got != 1 {
		t.Errorf("joins ceiling = %d, want 1: a scope cannot raise another's ceiling", got)
	}
}

// A ceiling that does not parse is not a ceiling. Reading it as the default
// would leave an operator believing they had set a limit that never applied.
func TestParseRejectsAMalformedLimit(t *testing.T) {
	in := []byte("checks:\n  rule/complexity:\n    keys: [max-joins, max-subqueries:many]\n")

	_, problems := Parse("ruler.yaml", in)
	if len(problems) != 2 {
		t.Fatalf("got %d problems, want one per malformed key: %v", len(problems), problems)
	}
	for _, p := range problems {
		if p.Check != lint.CheckPolicyLimit {
			t.Errorf("check = %q, want %s", p.Check, lint.CheckPolicyLimit)
		}
		if p.Severity != lint.SeverityError {
			t.Errorf("severity = %v, want error", p.Severity)
		}
	}
}

// The shipped ceilings apply when nobody sets any. They are not a maximum a
// scope may not raise: an operator who means to permit a fourth join says so
// and that is what binds.
func TestComplexityScopeMayRaiseTheShippedCeiling(t *testing.T) {
	p := &Policy{Checks: map[string]Setting{
		lint.CheckRuleComplexity: {Severity: lint.SeverityWarning, Keys: []string{lint.LimitJoins + ":4"}},
	}}

	got, ok := Merge(p).For(lint.CheckRuleComplexity).Limit(lint.LimitJoins)
	if !ok {
		t.Fatal("the merged policy has no join ceiling")
	}
	if got != 4 {
		t.Errorf("joins ceiling = %d, want 4", got)
	}
}

// A flag is a bare name the check declares, so it parses where a ceiling
// without its number does not. Reading any bare name as a flag would let
// `max-sample-rows` written without a value merge and quietly do nothing.
func TestParseAcceptsADeclaredFlag(t *testing.T) {
	in := []byte("checks:\n  rule/attribute-key:\n    keys: [require-rows]\n")

	p, problems := Parse("ruler.yaml", in)
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if !p.For(lint.CheckRuleAttributeKey).Flag(lint.FlagRequireRows) {
		t.Error("require-rows did not survive parsing")
	}
}

func TestParseRejectsACeilingWrittenAsAFlag(t *testing.T) {
	in := []byte("checks:\n  rule/attribute-key:\n    keys: [max-sample-rows]\n")

	_, problems := Parse("ruler.yaml", in)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want one", problems)
	}
	if problems[0].Check != lint.CheckPolicyLimit {
		t.Errorf("check = %q, want %s", problems[0].Check, lint.CheckPolicyLimit)
	}
}

// A flag no check declared is a typo, and silence about it is a setting an
// operator believes they made.
func TestParseRejectsAnUndeclaredFlag(t *testing.T) {
	in := []byte("checks:\n  rule/attribute-key:\n    keys: [require-row]\n")

	if _, problems := Parse("ruler.yaml", in); len(problems) != 1 {
		t.Fatalf("problems = %v, want one", problems)
	}
}

// A flag merges by union, which is the only direction that works for a setting
// making a check stricter: one scope can add it and none can take it away.
func TestFlagSurvivesTheMerge(t *testing.T) {
	instance := &Policy{Checks: map[string]Setting{
		lint.CheckRuleAttributeKey: {Severity: lint.SeverityWarning},
	}}
	datasource := &Policy{Checks: map[string]Setting{
		lint.CheckRuleAttributeKey: {Severity: lint.SeverityWarning, Keys: []string{lint.FlagRequireRows}},
	}}

	if !Merge(instance, datasource).For(lint.CheckRuleAttributeKey).Flag(lint.FlagRequireRows) {
		t.Error("a scope's flag did not survive the merge")
	}
	if !Merge(datasource, instance).For(lint.CheckRuleAttributeKey).Flag(lint.FlagRequireRows) {
		t.Error("the merge depends on the order the scopes were given")
	}
}
