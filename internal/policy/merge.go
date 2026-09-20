package policy

import "sort"

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
// Scopes are variadic rather than a fixed pair so that adding team-level
// policy later changes call sites and nothing else.
func Merge(scopes ...*Policy) *Policy {
	out := Defaults()

	for _, scope := range scopes {
		if scope == nil {
			continue
		}
		for name, incoming := range scope.Checks {
			out.Checks[name] = strictest(out.For(name), incoming)
		}
	}
	return out
}

// strictest returns whichever of two settings binds harder.
//
// When two scopes set the same severity the earlier one keeps the origin.
// Effective policy is identical either way; only which scope --explain names
// differs, and naming one that genuinely set it is accurate enough.
func strictest(current, incoming Setting) Setting {
	merged := current

	// The origin has to follow the severity, otherwise --explain would name a
	// scope that lost.
	if incoming.Severity > current.Severity {
		merged.Severity = incoming.Severity
		merged.File = incoming.File
		merged.Line = incoming.Line
	}
	merged.Keys = union(current.Keys, incoming.Keys)
	return merged
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
