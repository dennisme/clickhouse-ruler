package lint

import (
	"slices"
	"sort"
)

// Every check the ruler can report, and what it ships as.
//
// One table rather than a const in each package that emits findings. The
// names used to be spelled twice, in policy and again in query, because
// policy cannot import query without a cycle, and two agreement tests existed
// to keep the copies honest. A leaf package both of them already depend on
// removes the copies rather than testing them (spec 7.8).
//
// This is also what the check documentation is generated from, so a default
// stated on a page comes from the same place the resolver reads. A second
// description of a default would make the generated pages provably consistent
// with something that is not what runs.

// Check names. The name is the finding's identity: it appears in output, in
// policy files, and as the anchor of its documentation page.
const (
	CheckYAMLSyntax       = "yaml/syntax"
	CheckYAMLUnknownField = "yaml/unknown-field"
	CheckYAMLType         = "yaml/type"

	CheckRuleName      = "rule/name"
	CheckRuleGroupName = "rule/group-name"
	CheckRuleExpr      = "rule/expr"
	CheckRuleFor       = "rule/for"
	CheckRuleWindow    = "rule/window"

	CheckLabelsRequired      = "labels/required"
	CheckAnnotationsRequired = "annotations/required"
	CheckAnnotationsRunbook  = "annotations/runbook"
	CheckAnnotationsTemplate = "annotations/template"

	CheckRuleSourceMatch    = "rule/source-match"
	CheckRuleProtectedLabel = "rule/protected-label"
	CheckRulesetDirectory   = "ruleset/directory"

	CheckRuleSyntax           = "rule/syntax"
	CheckRuleInspect          = "rule/inspect"
	CheckRuleSelectStar       = "rule/select-star"
	CheckRuleTableFunction    = "rule/table-function"
	CheckRuleNondeterministic = "rule/nondeterministic"
	CheckRuleSettings         = "rule/settings"
	CheckRuleForeignTable     = "rule/foreign-table"
	CheckRuleComplexity       = "rule/complexity"
	CheckRuleColumns          = "rule/columns"
	CheckRuleTableAccess      = "rule/table-access"

	CheckSourceName            = "source/name"
	CheckSourceAddress         = "source/address"
	CheckSourceDatabase        = "source/database"
	CheckSourceUsername        = "source/username"
	CheckSourcePassword        = "source/password"
	CheckSourceTable           = "source/table"
	CheckSourceTimestampColumn = "source/timestamp-column"
	CheckSourceEvaluationDelay = "source/evaluation-delay"
	CheckSourceMaxRows         = "source/max-rows"
	CheckSourceMaxExecution    = "source/max-execution-time"
	CheckSourceMaxMemory       = "source/max-memory-usage"
	CheckSourceExemption       = "source/exemption"
	CheckSourcePrivileges      = "source/privileges"

	CheckPolicyUnknown  = "policy/unknown-check"
	CheckPolicyFixed    = "policy/fixed-check"
	CheckPolicySeverity = "policy/severity"
	CheckPolicyLimit    = "policy/check-limit"
)

// Assertion names in the ClickHouse user contract (spec 6.7.2). They are the
// keys of source/privileges, so they live beside it: an operator on a managed
// cluster drops the one assertion they cannot satisfy and keeps the rest.
const (
	AssertionSourcesRevoked = "sources-revoked"
	AssertionReadonly       = "readonly"
	AssertionConstraints    = "constraints"
	AssertionTableReadable  = "table-readable"
)

// The ceilings rule/complexity counts against, written into its key list as
// name:N so one setting carries both.
const (
	LimitJoins      = "max-joins"
	LimitSubqueries = "max-subqueries"
)

// ListKind is what a check's key list means, which decides how scopes combine
// it. A required list gets stricter as it grows, so scopes union it; an
// allowlist gets stricter as it shrinks, so scopes intersect it; a ceiling
// gets stricter as it falls, so the lowest value binds. Each direction is a
// meet or a join, which is what keeps the merge order-independent (spec 7.7).
type ListKind int

const (
	ListNone ListKind = iota
	ListRequired
	ListAllowlist
	ListCeiling
)

// Check is one check's identity and what it ships as.
type Check struct {
	Name string

	// Summary is one line, used by the generated documentation index.
	Summary string

	// Spec is the section that decided this check exists.
	Spec string

	// Fixed means the severity is not configurable. A rule failing one of
	// these cannot do its job: it will not parse, has no identity, has
	// nothing to query, scans without a time bound, or breaks routing.
	// Letting an operator soften them produces rules that look fine and never
	// fire, which is the failure this tool exists to prevent (spec 7.6).
	Fixed bool

	// Default is the shipped severity, for the checks that have one.
	//
	// Configurable checks ship as warnings, not errors. Severity decides who
	// has to be involved to unblock a contributor: a warning is theirs to act
	// on, an error needs a repo owner to change policy. Nothing about a
	// missing runbook stops a rule evaluating correctly, so it does not
	// deserve to pull a repo owner into the loop (spec 7.6).
	Default Severity

	// Keys is the shipped list, for the checks that take one.
	Keys []string

	// List says what Keys means to the merge. ListNone for a check that takes
	// no list.
	List ListKind
}

// Configurable reports whether this check may appear in a policy file.
func (c Check) Configurable() bool { return !c.Fixed }

// checks is the table. Fixed entries carry no default: there is no severity
// to resolve for them, and Severity() answers error for anything not
// configurable.
var checks = []Check{
	{
		Name: CheckYAMLSyntax, Spec: "7.3", Fixed: true,
		Summary: "the file is not valid YAML, so nothing in it could be read",
	},
	{
		Name: CheckYAMLUnknownField, Spec: "7.3", Fixed: true,
		Summary: "a field nobody recognises, which silently drops configuration",
	},
	{
		Name: CheckYAMLType, Spec: "7.3", Fixed: true,
		Summary: "a field holding the wrong shape, such as a list where a mapping belongs",
	},

	{
		Name: CheckRuleName, Spec: "7.6", Fixed: true,
		Summary: "an alert with no name, or a duplicate within its group, which has no identity",
	},
	{
		Name: CheckRuleGroupName, Spec: "7.6", Fixed: true,
		Summary: "a group with no name, or a name repeated within one file",
	},
	{
		Name: CheckRuleExpr, Spec: "7.6", Fixed: true,
		Summary: "an empty query, or one missing the time bounds the ruler binds",
	},

	// rule/for and rule/window are configurable and still refuse nonsense.
	// A negative duration always errors in the rule package; what policy
	// governs is the comparison against the group interval, which is advice.
	{
		Name: CheckRuleFor, Spec: "7.6", Default: SeverityWarning,
		Summary: "a `for` shorter than the group interval, so the alert fires on its first evaluation",
	},
	{
		Name: CheckRuleWindow, Spec: "7.6", Default: SeverityWarning,
		Summary: "a `window` shorter than the group interval, leaving data no evaluation reads",
	},

	{
		Name: CheckLabelsRequired, Spec: "7.6", Default: SeverityWarning,
		Keys: []string{"severity", "team"}, List: ListRequired,
		Summary: "a label this repository requires on every alert is missing",
	},
	{
		Name: CheckAnnotationsRequired, Spec: "7.6", Default: SeverityWarning,
		Keys: []string{"runbook_url", "summary"}, List: ListRequired,
		Summary: "an annotation this repository requires on every alert is missing",
	},
	{
		Name: CheckAnnotationsRunbook, Spec: "7.6", Default: SeverityWarning,
		Summary: "a runbook_url that is not an absolute http or https URL",
	},

	// Configurable rather than fixed, and a warning by default, because a
	// rule with an unparseable annotation still evaluates and still pages:
	// the failure lands in the annotation text rather than stopping the
	// alert. An operator who would rather a broken template never reach a
	// pager raises this to an error, which puts a repo owner on the path to
	// unblock the author (spec 7.6).
	{
		Name: CheckAnnotationsTemplate, Spec: "7.6", Default: SeverityWarning,
		Summary: "an annotation that is not a parseable template",
	},

	// Configurable for a different reason than the rest: whether a rule can
	// run depends on which ruler is asking, so one holding a single data
	// centre's sources will legitimately match nothing for most of a shared
	// repository (spec 6.10, 10.2).
	{
		Name: CheckRuleSourceMatch, Spec: "6.10", Default: SeverityWarning,
		Summary: "a rule whose selector matches no source, so this ruler will never evaluate it",
	},

	{
		Name: CheckRuleProtectedLabel, Spec: "6.3.1", Fixed: true,
		Summary: "a rule setting a label the ruler owns, which breaks routing",
	},
	{
		Name: CheckRulesetDirectory, Spec: "7.1", Fixed: true,
		Summary: "the rules directory could not be read",
	},

	{
		Name: CheckRuleSyntax, Spec: "7.3", Fixed: true,
		Summary: "SQL ClickHouse cannot parse, or a second statement nobody reviewed",
	},

	// Not a finding about a rule. It is how the ruler says it could not ask,
	// and silence there would read as a rule that passed.
	{
		Name: CheckRuleInspect, Spec: "7.3", Fixed: true,
		Summary: "the ruler could not reach the cluster to read the rule's SQL",
	},

	// A rule with SELECT * evaluates correctly today and refingerprints every
	// instance the moment a column is added, which is a warning's worth of
	// wrong: nothing stops the rule working, and the author can fix it.
	{
		Name: CheckRuleSelectStar, Spec: "7.3", Default: SeverityWarning,
		Summary: "a query selecting *, so a schema change rewrites every alert's identity",
	},

	// The one check here defaulting to an error, because it is the only
	// preventive control rather than early feedback. remote() and url() are
	// refused by the grants behind them, but numbers() and generateRandom()
	// are gated by no privilege at all, and the database's only answer is to
	// time the query out once the cost is already spent (spec 6.7.1).
	//
	// The allowlist ships empty, so every table function is refused and an
	// operator permits the one they actually need.
	{
		Name: CheckRuleTableFunction, Spec: "7.3", Default: SeverityError,
		Keys: []string{}, List: ListAllowlist,
		Summary: "a query reading through a table function the allowlist does not permit",
	},

	// Curated because there is nothing to read it from: system.functions has
	// no is_deterministic column. A list we maintain will be incomplete, so
	// an operator who finds the gap extends it rather than waiting for a
	// release (spec 6.7.1).
	{
		Name: CheckRuleNondeterministic, Spec: "7.3", Default: SeverityWarning,
		Keys: nondeterministicFunctions, List: ListRequired,
		Summary: "a query calling a function that breaks window alignment, such as now()",
	},

	// Fixed because the clause replaces the execution time, memory and row
	// limits the ruler sends with every evaluation, so softening this check
	// softens every cost control behind it at once. The profile constraints
	// are the backstop for anything that gets past (spec 6.7, 7.3).
	{
		Name: CheckRuleSettings, Spec: "7.3", Fixed: true,
		Summary: "a query setting its own SETTINGS, overriding the limits the ruler sends",
	},

	// Early feedback: the grants are what stop a read outside the source's
	// database (spec 6.7.1). Configurable because a user deliberately granted
	// a second database is a legitimate setup, and the operator who arranged
	// it is the one who gets to say so.
	{
		Name: CheckRuleForeignTable, Spec: "7.3", Default: SeverityWarning,
		Summary: "a query reading a table outside its source's own database",
	},

	// A count of joins and subqueries is a cost proxy that reads no data, so
	// it is the only thing tier 1 can say about cost at all. A warning with
	// generous ceilings: a crude measure that blocks gets switched off.
	{
		Name: CheckRuleComplexity, Spec: "7.3", Default: SeverityWarning,
		Keys: []string{LimitJoins + ":2", LimitSubqueries + ":2"}, List: ListCeiling,
		Summary: "a query with more joins or subqueries than the configured ceiling",
	},

	// Fixed because each case means the rule cannot run: a column that is not
	// there, a table that is not there, or a result with nothing to compare
	// against a threshold. The last one is the quiet failure worth catching in
	// CI, because a rule with no value column fails at evaluation time, which
	// is after review and in front of nobody (spec 7.3).
	{
		Name: CheckRuleColumns, Spec: "7.3", Fixed: true,
		Summary: "a query naming a column or table that does not exist, or returning no value column",
	},

	// Configurable, and warn by default, because the answer differs per
	// cluster rather than per rule: an error on the cluster that will evaluate
	// the rule, and off on a validation replica whose user has no grants. A
	// source's own checks block is where that is said (spec 7.7, 10.3).
	{
		Name: CheckRuleTableAccess, Spec: "7.3", Default: SeverityWarning,
		Summary: "a source's user cannot read what the rule asks for, so nothing could be checked",
	},

	{
		Name: CheckSourceName, Spec: "6.6", Fixed: true,
		Summary: "a source with no name, or a name used twice",
	},
	{
		Name: CheckSourceAddress, Spec: "6.6", Fixed: true,
		Summary: "a source with no address to connect to",
	},
	{
		Name: CheckSourceDatabase, Spec: "6.6", Fixed: true,
		Summary: "a source naming no database",
	},
	{
		Name: CheckSourceUsername, Spec: "6.6", Fixed: true,
		Summary: "a source naming no ClickHouse user, which is the tenancy boundary",
	},
	{
		Name: CheckSourcePassword, Spec: "6.6", Fixed: true,
		Summary: "a secret that could not be read, or both secret sources set at once",
	},
	{
		Name: CheckSourceTable, Spec: "6.6", Fixed: true,
		Summary: "a source naming no table",
	},
	{
		Name: CheckSourceTimestampColumn, Spec: "6.8", Fixed: true,
		Summary: "a source naming no timestamp column, so no window can be bound",
	},
	{
		Name: CheckSourceEvaluationDelay, Spec: "6.8", Fixed: true,
		Summary: "a negative evaluation delay",
	},
	{
		Name: CheckSourceMaxRows, Spec: "6.7", Fixed: true,
		Summary: "a row cap that is not a positive number",
	},
	{
		Name: CheckSourceMaxExecution, Spec: "6.7", Fixed: true,
		Summary: "an execution time cap that is not positive",
	},
	{
		Name: CheckSourceMaxMemory, Spec: "6.7", Fixed: true,
		Summary: "a memory cap that is not positive",
	},

	// Fixed because an exemption is the one thing that loosens, so a
	// malformed one has to fail rather than be softened into a warning
	// somebody stops reading (spec 7.7).
	{
		Name: CheckSourceExemption, Spec: "7.7", Fixed: true,
		Summary: "an exemption that is malformed, names a check nobody can exempt, or has expired",
	},

	// A warning by default because a ruler pointed at an existing cluster
	// fails this on its first run, and a check that blocks the first run gets
	// switched off rather than fixed. The finding is about the operator's own
	// file, so the person who sees it is the person who can act on it and
	// nobody is waiting behind them (spec 7.6).
	{
		Name: CheckSourcePrivileges, Spec: "6.7.2", Default: SeverityWarning,
		Keys: assertions, List: ListRequired,
		Summary: "a source's ClickHouse user does not meet the contract the other checks rely on",
	},

	{
		Name: CheckPolicyUnknown, Spec: "7.6", Fixed: true,
		Summary: "a policy file configuring a check that does not exist",
	},
	{
		Name: CheckPolicyFixed, Spec: "7.6", Fixed: true,
		Summary: "a policy file configuring a correctness check, which cannot be softened",
	},
	{
		Name: CheckPolicySeverity, Spec: "7.6", Fixed: true,
		Summary: "a severity that is not off, warn or error",
	},
	{
		Name: CheckPolicyLimit, Spec: "7.7", Fixed: true,
		Summary: "a ceiling that is not written as name:number, so it would never apply",
	},
}

// nondeterministicFunctions break window alignment and make a replay lie: a
// rule reading now() is not reading the window the ruler asked for, and the
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

// assertionOrder is the whole contract, in the order a report reads best:
// the tenancy half first, then what the ruler needs to work at all.
var assertionOrder = []string{
	AssertionSourcesRevoked,
	AssertionReadonly,
	AssertionConstraints,
	AssertionTableReadable,
}

// Assertions lists every contract assertion in report order.
func Assertions() []string { return slices.Clone(assertionOrder) }

// assertions is the same list sorted, because it is the check's key list and
// every other key list is sorted.
var assertions = func() []string {
	out := slices.Clone(assertionOrder)
	sort.Strings(out)
	return out
}()

// byName indexes the table. Built once, because every finding consults it.
var byName = func() map[string]Check {
	out := make(map[string]Check, len(checks))
	for _, c := range checks {
		if _, dup := out[c.Name]; dup {
			panic("lint: check " + c.Name + " is declared twice")
		}
		out[c.Name] = c
	}
	return out
}()

// Lookup returns a check by name.
func Lookup(name string) (Check, bool) {
	c, ok := byName[name]
	return c, ok
}

// Known reports whether a name is a check this ruler can report.
func Known(name string) bool {
	_, ok := byName[name]
	return ok
}

// Fixed reports whether a check's severity is not configurable. An unknown
// name is not fixed; it is unknown, and policy reports that differently.
func Fixed(name string) bool {
	c, ok := byName[name]
	return ok && c.Fixed
}

// Configurable reports whether a check may appear in a policy file.
func Configurable(name string) bool {
	c, ok := byName[name]
	return ok && c.Configurable()
}

// Allowlist reports whether a check's list permits rather than requires.
func Allowlist(name string) bool {
	c, ok := byName[name]
	return ok && c.List == ListAllowlist
}

// Ceiling reports whether a check's list is numeric limits rather than names.
func Ceiling(name string) bool {
	c, ok := byName[name]
	return ok && c.List == ListCeiling
}

// Configurables lists every configurable check, sorted, so `ruler check
// --explain` can report a rule's resolved policy in full rather than only the
// checks some file happened to mention.
func Configurables() []string {
	out := make([]string, 0, len(checks))
	for _, c := range checks {
		if c.Configurable() {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

// All returns every check, sorted by name, for the documentation generator
// and for anything else that needs the whole catalogue.
func All() []Check {
	out := make([]Check, len(checks))
	copy(out, checks)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
