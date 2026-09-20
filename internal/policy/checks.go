package policy

import "github.com/dennisme/clickhouse-ruler/internal/lint"

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
	CheckRuleFor             = "rule/for"
	CheckRuleWindow          = "rule/window"
)

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
	"rule/source":              true,
	"rule/source-exists":       true,
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
	CheckRuleFor:            {Severity: lint.SeverityWarning},
	CheckRuleWindow:         {Severity: lint.SeverityWarning},
}

// Fixed reports whether a check's severity is not configurable.
func Fixed(check string) bool { return fixed[check] }

// Configurable reports whether a check may appear in a policy file.
func Configurable(check string) bool {
	_, ok := defaults[check]
	return ok
}

// Defaults returns the shipped policy, used when no file configures anything.
func Defaults() *Policy {
	checks := make(map[string]Setting, len(defaults))
	for name, s := range defaults {
		checks[name] = s
	}
	return &Policy{Checks: checks}
}
