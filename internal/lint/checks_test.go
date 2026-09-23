package lint

import (
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every entry is documented and attributable. A check whose summary or spec
// section is missing produces a documentation page that says nothing, which
// is the state this table exists to make impossible (spec 7.8).
func TestEveryCheckIsDescribed(t *testing.T) {
	for _, c := range All() {
		if c.Summary == "" {
			t.Errorf("%s has no summary, so its documentation would be a heading and nothing else", c.Name)
		}
		if c.Spec == "" {
			t.Errorf("%s names no spec section, so a reader cannot find why it exists", c.Name)
		}
		if _, _, ok := strings.Cut(c.Name, "/"); !ok {
			t.Errorf("%s is not namespaced, and the namespace decides which page it lands on", c.Name)
		}
	}
}

// A fixed check has no severity to resolve, so shipping one with a default is
// a contradiction a reader of the table would have to reconcile.
func TestFixedChecksCarryNoDefault(t *testing.T) {
	for _, c := range All() {
		if !c.Fixed {
			continue
		}
		if c.Default != SeverityOff {
			t.Errorf("%s is fixed but ships severity %v, which nothing reads", c.Name, c.Default)
		}
		if len(c.Keys) > 0 || c.List != ListNone {
			t.Errorf("%s is fixed but carries a key list, which no policy can configure", c.Name)
		}
	}
}

// A configurable check that shipped at off would be a check nobody runs and
// nobody was told about.
func TestConfigurableChecksShipOnAndExplained(t *testing.T) {
	for _, c := range All() {
		if c.Fixed {
			continue
		}
		if c.Default == SeverityOff {
			t.Errorf("%s ships at off, so it reports nothing until somebody discovers it exists", c.Name)
		}
		if len(c.Keys) > 0 && c.List == ListNone {
			t.Errorf("%s ships a key list with no direction, so the merge cannot combine it", c.Name)
		}
	}
}

// The ceilings are read back out of the key list by policy, so the shipped
// ones have to be in the form it parses.
func TestCeilingKeysParse(t *testing.T) {
	for _, c := range All() {
		if c.List != ListCeiling {
			continue
		}
		for _, key := range c.Keys {
			name, value, ok := strings.Cut(key, ":")
			if !ok || name == "" {
				t.Errorf("%s ships %q, which is not name:number", c.Name, key)
				continue
			}
			if n, err := strconv.Atoi(value); err != nil || n < 0 {
				t.Errorf("%s ships %q, whose ceiling is not a whole number", c.Name, key)
			}
		}
	}
}

// Key lists are sorted so that a merged list does not depend on the order
// somebody happened to type the shipped one in.
func TestShippedKeysAreSorted(t *testing.T) {
	for _, c := range All() {
		if !sort.StringsAreSorted(c.Keys) {
			t.Errorf("%s ships keys %v out of order", c.Name, c.Keys)
		}
	}
}

// The gate. A finding built with a name the table does not know is a check
// that ships without documentation and without a resolvable severity, so it
// fails here rather than reaching output.
func TestNewProblemRejectsAnUnknownCheck(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewProblem accepted a check nobody declared")
		}
	}()

	NewProblem("f.yaml", 1, "rule/invented-here", SeverityError, "text")
}

func TestNewProblemAcceptsADeclaredCheck(t *testing.T) {
	got := NewProblem("f.yaml", 12, CheckRuleExpr, SeverityError, "expr is empty")

	if got.Check != CheckRuleExpr || got.Line != 12 || got.File != "f.yaml" {
		t.Errorf("problem = %+v, want the fields it was given", got)
	}
}

// Assertions are the keys of source/privileges, so the two have to hold the
// same set. They are separate because the report has an order worth reading
// and a key list is sorted.
func TestAssertionsAreThePrivilegeKeys(t *testing.T) {
	c, ok := Lookup(CheckSourcePrivileges)
	if !ok {
		t.Fatal("source/privileges is not in the table")
	}

	want := Assertions()
	sort.Strings(want)

	if len(c.Keys) != len(want) {
		t.Fatalf("keys = %v, assertions = %v", c.Keys, want)
	}
	for i := range want {
		if c.Keys[i] != want[i] {
			t.Fatalf("keys = %v, assertions = %v", c.Keys, want)
		}
	}
}

func TestConfigurablesAreSortedAndConfigurable(t *testing.T) {
	got := Configurables()

	if !sort.StringsAreSorted(got) {
		t.Errorf("Configurables() = %v, want sorted", got)
	}
	for _, name := range got {
		if Fixed(name) {
			t.Errorf("%s is listed as configurable and is fixed", name)
		}
	}
}
