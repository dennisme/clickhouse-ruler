package policy

import (
	"sort"
	"strconv"
	"strings"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// Problems this package reports about a policy file itself.
const (
	checkPolicyUnknown  = "policy/unknown-check"
	checkPolicyFixed    = "policy/fixed-check"
	checkPolicySeverity = "policy/severity"
	checkPolicyLimit    = "policy/check-limit"
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

	// Tier 1 checks. They read the query through ClickHouse rather than the
	// file, so they need a connection (spec 7.3).
	CheckRuleSelectStar       = "rule/select-star"
	CheckRuleTableFunction    = "rule/table-function"
	CheckRuleNondeterministic = "rule/nondeterministic"
	CheckRuleForeignTable     = "rule/foreign-table"
	CheckRuleComplexity       = "rule/complexity"

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

	// A rule whose SQL will not parse cannot run, and neither can one
	// carrying a second statement. rule/inspect is fixed for a different
	// reason: it is the ruler reporting that it could not ask, not a finding
	// anyone can configure away (spec 7.3).
	"rule/syntax":  true,
	"rule/inspect": true,

	// A rule's own SETTINGS clause replaces the execution time, memory and row
	// limits the ruler sends with every evaluation, so what it overrides is
	// the thing that keeps a rule from costing the cluster whatever it likes.
	// Nothing here reasons about which settings are harmless: one clause is
	// enough, and the profile constraints are the backstop for anything that
	// gets past this (spec 6.7, 7.3).
	"rule/settings": true,

	"rule/protected-label": true,
	checkPolicyUnknown:     true,
	checkPolicyFixed:       true,
	checkPolicySeverity:    true,
	checkPolicyLimit:       true,
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

	// A rule with SELECT * evaluates correctly today and refingerprints every
	// instance the moment a column is added, which is a warning's worth of
	// wrong: nothing stops the rule working, and the author is the one who
	// can fix it.
	CheckRuleSelectStar: {Severity: lint.SeverityWarning},

	// The one check here that defaults to an error, because it is the only
	// preventive control rather than early feedback. remote() and url() are
	// refused by the grants behind them, but numbers() and generateRandom()
	// are gated by no privilege at all, and the database's only answer is to
	// time the query out once the cost is already spent (spec 6.7.1).
	//
	// The allowlist ships empty, so every table function is refused and an
	// operator permits the one they actually need.
	CheckRuleTableFunction: {Severity: lint.SeverityError},

	// Curated because there is nothing to read it from: system.functions has
	// no is_deterministic column. A list we maintain will be incomplete, so
	// an operator who finds the gap can extend it rather than wait for a
	// release (spec 6.7.1).
	CheckRuleNondeterministic: {
		Severity: lint.SeverityWarning,
		Keys:     nondeterministicFunctions,
	},

	// A warning because the grants are what stop a read outside the source's
	// database (spec 6.7.1). This check is early feedback: an author hears in
	// the pull request rather than from a rule that fails against one cluster
	// at evaluation time. It is configurable because a source whose user is
	// deliberately granted a second database is a legitimate setup, and an
	// operator running one should be able to say so.
	CheckRuleForeignTable: {Severity: lint.SeverityWarning},

	// Joins and subqueries are a cost proxy that reads no data, so this is
	// the only thing tier 1 can say about cost at all. It is a warning and
	// the ceilings are generous: a count is a crude stand-in for what a query
	// costs, and a crude measure that blocks gets switched off (spec 7.3).
	CheckRuleComplexity: {
		Severity: lint.SeverityWarning,
		Keys:     []string{LimitJoins + ":2", LimitSubqueries + ":2"},
	},
}

// The ceilings rule/complexity counts against, written into its key list as
// name:N so one setting carries both.
const (
	LimitJoins      = "max-joins"
	LimitSubqueries = "max-subqueries"
)

// limitChecks are the checks whose keys are ceilings rather than names. Their
// keys are validated when a policy file is read, because a ceiling that does
// not parse would otherwise leave an operator believing they had set a limit
// that never applied.
var limitChecks = map[string][]string{
	CheckRuleComplexity: {LimitJoins, LimitSubqueries},
}

// limited reports whether a check's keys are ceilings.
func limited(check string) bool {
	_, ok := limitChecks[check]
	return ok
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

// nondeterministicFunctions break window alignment and make a replay lie: a
// rule reading `now()` is not reading the window the ruler asked for, and the
// same query run twice over the same window does not agree with itself.
var nondeterministicFunctions = []string{
	"generateuuidv4",
	"now",
	"now64",
	"rand",
	"rand32",
	"rand64",
	"randcanonical",
	"today",
	"uptime",
	"yesterday",
}

// allowlists are the checks whose list permits rather than requires.
//
// The direction matters to the merge. A required list gets stricter as it
// grows, so scopes union it; an allowlist gets stricter as it shrinks, so
// scopes intersect it. Unioning one would let a source permit a table
// function the instance policy refused, and no scope may loosen another
// (spec 7.7).
//
// rule/complexity is neither. Its keys are ceilings, so the direction lives
// in how they are read rather than in how the lists combine: the lists union
// and Setting.Limit takes the lowest value, which is the strict one.
var allowlists = map[string]bool{
	CheckRuleTableFunction: true,
}

// Allowlist reports whether a check's list permits rather than requires.
func Allowlist(check string) bool { return allowlists[check] }

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
