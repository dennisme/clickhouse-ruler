package policy

import (
	"sort"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// Problems this package reports about a policy file itself.
const (
	checkPolicyUnknown  = "policy/unknown-check"
	checkPolicyFixed    = "policy/fixed-check"
	checkPolicySeverity = "policy/severity"
)

// Check names that can be configured. The rest are correctness checks.
const (
	CheckLabelsRequired      = "labels/required"
	CheckAnnotationsRequired = "annotations/required"
	CheckAnnotationsRunbook  = "annotations/runbook"
	CheckAnnotationsTemplate = "annotations/template"
	CheckRuleFor             = "rule/for"
	CheckRuleWindow          = "rule/window"

	// CheckSourceMatch is configurable for a different reason than the rest.
	// Whether a rule can run depends on which ruler is asking: one holding a
	// single data centre's sources will legitimately match nothing for most of
	// a shared repository (spec 6.10, 10.2).
	CheckSourceMatch = "rule/source-match"

	// CheckSourcePrivileges is a check on a source's ClickHouse user rather
	// than on any rule, and its severity governs the report and never the
	// guarantee: the grants are what stop a query, so a cluster where this is
	// off is exactly as safe as one where it passes. What changes is whether
	// anybody is told (spec 6.7.1, 7.6).
	CheckSourcePrivileges = "source/privileges"
)

// privilegeAssertions is the default requirement: the whole contract.
//
// The names are query.Assertions(), repeated here because that package cannot
// be imported from this one without a cycle. A test in the query package
// fails if the two lists ever disagree.
var privilegeAssertions = []string{
	"constraints",
	"readonly",
	"sources-revoked",
	"table-readable",
}

// fixed lists the checks whose severity nobody may change. A rule failing one
// of these cannot do its job: it will not parse, has no identity, has nothing
// to query, scans without a time bound, or breaks routing. Letting an operator
// soften them produces rules that look fine and never fire, which is the
// failure this tool exists to prevent (spec 7.6).
//
// rule/for and rule/window appear in both lists on purpose. Their negative
// cases are nonsense values and always error; their comparisons against the
// group interval are advice and are configurable.
var fixed = map[string]bool{
	lint.CheckYAMLSyntax:       true,
	lint.CheckYAMLUnknownField: true,
	lint.CheckYAMLType:         true,
	"rule/name":                true,
	"rule/group-name":          true,
	"rule/expr":                true,
	"rule/protected-label":     true,
	checkPolicyUnknown:         true,
	checkPolicyFixed:           true,
	checkPolicySeverity:        true,
}

// defaults are the shipped settings for every configurable check.
//
// They are warnings, not errors. Severity decides who has to be involved to
// unblock a contributor: a warning is theirs to act on, an error needs a repo
// owner to change policy. Nothing about a missing runbook stops a rule from
// evaluating correctly, so it does not deserve to pull a repo owner into the
// loop (spec 7.6).
var defaults = map[string]Setting{
	CheckLabelsRequired: {
		Severity: lint.SeverityWarning,
		Keys:     []string{"severity", "team"},
	},
	CheckAnnotationsRequired: {
		Severity: lint.SeverityWarning,
		Keys:     []string{"runbook_url", "summary"},
	},
	CheckAnnotationsRunbook: {Severity: lint.SeverityWarning},

	// Configurable rather than fixed, and a warning by default, because a rule
	// with an unparseable annotation still evaluates and still pages: the
	// failure lands in the annotation text rather than stopping the alert. An
	// operator who would rather a broken template never reach a pager raises
	// this to an error, which puts a repo owner on the path to unblock the
	// author (spec 7.6).
	CheckAnnotationsTemplate: {Severity: lint.SeverityWarning},
	CheckSourceMatch:         {Severity: lint.SeverityWarning},

	// A warning by default because a ruler pointed at an existing cluster
	// fails this on its first run, and a check that blocks the first run gets
	// switched off rather than fixed. The finding is about the operator's own
	// file, so the person who sees it is the person who can act on it and
	// nobody is waiting behind them (spec 7.6).
	CheckSourcePrivileges: {
		Severity: lint.SeverityWarning,
		Keys:     privilegeAssertions,
	},
	CheckRuleFor:    {Severity: lint.SeverityWarning},
	CheckRuleWindow: {Severity: lint.SeverityWarning},
}

// Fixed reports whether a check's severity is not configurable.
func Fixed(check string) bool { return fixed[check] }

// Configurable reports whether a check may appear in a policy file.
func Configurable(check string) bool {
	_, ok := defaults[check]
	return ok
}

// Names lists every configurable check, sorted, so `ruler check --explain`
// can report a rule's resolved policy in full rather than only the checks
// some file happened to mention.
func Names() []string {
	out := make([]string, 0, len(defaults))
	for name := range defaults {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Defaults returns the shipped policy, used when no file configures anything.
func Defaults() *Policy {
	checks := make(map[string]Setting, len(defaults))
	for name, s := range defaults {
		checks[name] = s
	}
	return &Policy{Checks: checks}
}
