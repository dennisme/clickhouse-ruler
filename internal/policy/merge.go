package policy

import (
	"slices"
	"sort"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// Merge combines policy from every scope that applies to a rule, taking the
// strictest setting for each check.
//
// Severity is the maximum over off < warn < error, and key lists are unioned.
// Nothing can lower a severity or drop a key, which is what lets a scope be
// owned by the people it governs: the worst a team can do to its own rules is
// make them stricter.
//
// Because the merge is a maximum it is commutative, so there is no precedence
// between scopes to define and none for a reader to memorise. Any new setting
// added here has to have an unambiguous stricter direction, or that property
// is lost (spec 7.7).
//
// Scopes are variadic because how many apply to a rule is not fixed: the
// instance file, every team file above the rule in the tree, and one per
// source its selector reached.
//
// The shipped defaults are not a scope. They are what a check falls back to
// when no scope configures it, which is what makes `severity: off` reachable:
// as a scope they would be a floor, the default warning would win every
// merge, and the one setting meaning "nobody is asked" could be written and
// never take effect. Their key lists do still apply, so a scope can add a
// required key and cannot drop one (spec 7.6).
func Merge(scopes ...*Policy) *Policy {
	out := &Policy{Checks: map[string]Setting{}}

	for _, scope := range scopes {
		if scope == nil {
			continue
		}
		for name, incoming := range scope.Checks {
			current, ok := out.Checks[name]
			if !ok {
				// An allowlist starts from the first scope that sets one, not
				// from the shipped empty list: intersecting with empty would
				// refuse everything however the scopes were configured.
				//
				// A ceiling list starts there too. The shipped ceilings are
				// what applies when nobody configures any, not a maximum
				// nobody may raise: carrying them into the merge would make
				// the default the strictest entry for ever, and an operator
				// who means to permit a fourth join could not say so.
				current = Setting{}
				if d, ok := defaultFor(name); ok {
					current.Keys = d.Keys
				}
				if lint.Allowlist(name) || lint.Ceiling(name) {
					current.Keys = incoming.Keys
				}
			}
			out.Checks[name] = strictest(current, incoming, lint.Allowlist(name))
		}
	}
	return out
}

// strictest returns whichever of two settings binds harder.
//
// When two scopes set the same severity the earlier one keeps the origin.
// Effective policy is identical either way; only which scope --explain names
// differs, and naming one that genuinely set it is accurate enough.
func strictest(current, incoming Setting, allowlist bool) Setting {
	merged := current

	// The origin has to follow the severity, otherwise --explain would name a
	// scope that lost.
	if incoming.Severity > current.Severity {
		merged.Severity = incoming.Severity
		merged.File = incoming.File
		merged.Line = incoming.Line
	}
	if allowlist {
		merged.Keys = intersect(current.Keys, incoming.Keys)
	} else {
		merged.Keys = union(current.Keys, incoming.Keys)
	}
	return merged
}

// intersect keeps only the entries both lists permit, sorted so the result
// does not depend on map iteration order.
func intersect(a, b []string) []string {
	in := make(map[string]bool, len(a))
	for _, k := range a {
		in[k] = true
	}

	out := make([]string, 0, len(b))
	for _, k := range b {
		if in[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// union merges two key lists, sorted so the result does not depend on map
// iteration order.
func union(a, b []string) []string {
	if len(b) == 0 {
		return a
	}

	seen := make(map[string]bool, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, k := range list {
			seen[k] = true
		}
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
