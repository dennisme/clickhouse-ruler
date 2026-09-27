package main

import (
	"strings"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
)

// sourceColumns is what one source said a rule's result looks like.
//
// A rule does not name a source, it selects them, and it writes its own
// `FROM otel.otel_traces`, so every cluster the selector reaches has to carry
// that table with those columns (spec 6.10). Only the cluster can say whether
// it does, which is why this is collected per source during the online pass
// rather than read off the file.
type sourceColumns struct {
	source  string
	columns []query.Column
}

// schemaDisagreements compares what each source said the rule returns.
//
// Every source is compared against the first that answered rather than against
// every other one. Pairwise would report the same difference once per pair, so
// an estate of five clusters where one has drifted reads as four problems
// instead of one; and the first answer is as good a reference as any, because
// nothing here knows which cluster is the correct one. A source that reported
// the same difference as another still gets its own line, since which cluster
// disagrees is the part an operator acts on.
func schemaDisagreements(answered []sourceColumns) []string {
	// One answer cannot disagree with anything. A rule matching a single
	// source arrives here that way, and so does a rule whose second source
	// could not be reached or whose user cannot read the table: those are
	// already rule/inspect and rule/table-access, and a second finding saying
	// it differently is noise.
	if len(answered) < 2 {
		return nil
	}

	reference := answered[0]

	var out []string
	for _, other := range answered[1:] {
		out = append(out, query.Differences(
			reference.columns, other.columns,
			"source "+reference.source, "source "+other.source)...)
	}
	return out
}

// schemaProblems reports the disagreement as one problem about the rule.
//
// One problem however many sources differed, because the rule is what is
// wrong: reporting it per source would say the rule is broken against the
// cluster that has drifted and correct against the one that has not, which is
// exactly the confusing answer this check exists to replace.
//
// The severity comes from the rule's own scopes and never from a source's,
// unlike every other finding in the online pass. A finding naming two clusters
// has no one source to take a setting from, and letting either decide would
// mean the same disagreement reported differently depending on which source
// answered first. The same reasoning rules out a source exemption here.
func schemaProblems(r ruleset.Rule, answered []sourceColumns, rulePolicy *policy.Policy) []lint.Problem {
	setting := rulePolicy.For(lint.CheckRuleSourceSchema)
	if setting.Severity == lint.SeverityOff {
		return nil
	}

	disagreements := schemaDisagreements(answered)
	if len(disagreements) == 0 {
		return nil
	}

	p := lint.NewProblem(r.File, r.Line(), lint.CheckRuleSourceSchema, setting.Severity,
		"the sources this rule matched do not agree on what it returns, so it cannot mean the same "+
			"thing on all of them: "+strings.Join(disagreements, "; "))
	p.Subject = r.Alert
	p.PolicyFile, p.PolicyLine = setting.File, setting.Line

	return []lint.Problem{p}
}
