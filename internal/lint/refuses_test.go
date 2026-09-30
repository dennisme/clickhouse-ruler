package lint

import "testing"

// Only a file nobody can read refuses a reading. Every other finding blocks a
// merge and lets the ruler load, because a ruler that will not start pages
// nobody while a rule loaded against its author's intent pages somebody (spec
// 7.6).
func TestOnlyUnreadableFilesRefuseAReading(t *testing.T) {
	want := map[string]bool{
		CheckYAMLSyntax:       true,
		CheckRulesetDirectory: true,
	}

	for _, c := range All() {
		if got := c.RefusesReading; got != want[c.Name] {
			t.Errorf("%s: RefusesReading = %v, want %v", c.Name, got, want[c.Name])
		}
	}
}

// A check that refuses a reading has to be one an operator cannot soften,
// since a severity nobody may change is the only way the refusal is reachable
// at all.
func TestARefusingCheckIsFixedAtError(t *testing.T) {
	for _, c := range All() {
		if !c.RefusesReading {
			continue
		}
		if !c.Fixed {
			t.Errorf("%s refuses a reading but its severity is configurable", c.Name)
		}
		if c.Always != SeverityError {
			t.Errorf("%s refuses a reading at severity %v, want error", c.Name, c.Always)
		}
	}
}

// Being fixed at error is the normal case and says nothing about refusing, so
// the two properties have to be separable in the table rather than one
// implying the other.
func TestMostFixedErrorChecksDoNotRefuse(t *testing.T) {
	var fixedErrors, refusing int
	for _, c := range All() {
		if c.Fixed && c.Always == SeverityError {
			fixedErrors++
			if c.RefusesReading {
				refusing++
			}
		}
	}
	if fixedErrors == refusing {
		t.Fatalf("every fixed error check refuses a reading (%d of %d): the axes are still fused", refusing, fixedErrors)
	}
}
