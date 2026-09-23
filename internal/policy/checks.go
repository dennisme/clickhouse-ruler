package policy

import (
	"strconv"
	"strings"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// What a check ships as lives in the table in internal/lint, beside the check
// itself. This package owns what an operator does to it: parsing a policy
// file and merging the scopes that apply to a rule (spec 7.6, 7.7).

// defaultFor returns the shipped setting for a check, which is what applies
// when no scope configures it.
func defaultFor(check string) (Setting, bool) {
	c, ok := lint.Lookup(check)
	if !ok || !c.Configurable() {
		return Setting{}, false
	}
	return Setting{Severity: c.Default, Keys: c.Keys}, true
}

// Limit reads a ceiling from a check's key list.
//
// The lowest value wins when a name appears more than once. Merge unions key
// lists, so both scopes' ceilings survive the merge and the stricter one is
// the one that binds, which is how every other setting here behaves: no
// scope may loosen another (spec 7.7).
func (s Setting) Limit(name string) (int, bool) {
	out, found := 0, false

	for _, key := range s.Keys {
		got, value, ok := strings.Cut(key, ":")
		if !ok || got != name {
			continue
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			continue
		}
		if !found || n < out {
			out, found = n, true
		}
	}
	return out, found
}

// Defaults returns the shipped policy, used when no file configures anything.
func Defaults() *Policy {
	checks := map[string]Setting{}
	for _, name := range lint.Configurables() {
		if s, ok := defaultFor(name); ok {
			checks[name] = s
		}
	}
	return &Policy{Checks: checks}
}
