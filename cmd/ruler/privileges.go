package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// privilegeTimeout bounds the whole contract check for one source.
//
// Wide enough for every probe to spend its own deadline and the settings
// reads to follow, so a source whose privileges are granted still produces a
// finding rather than running out of time on the way to one.
const privilegeTimeout = 30 * time.Second

// checkPrivileges runs the contract check against every source, connecting as
// the source's own user because that is the user being checked (spec 6.7.2).
//
// Each source is independent: one that cannot be reached produces an
// inconclusive finding and the rest are still checked.
func checkPrivileges(ctx context.Context, file string, sources []source.Source, root *policy.Policy) []lint.Problem {
	var problems []lint.Problem

	for _, src := range sources {
		setting := policy.Merge(root, src.Policy).For(policy.CheckSourcePrivileges)
		if !privilegesEnabled(setting) {
			continue
		}
		problems = append(problems, privilegeProblems(file, src.Name, src.Line(), setting, assertSource(ctx, src, setting))...)
	}
	return problems
}

// assertSource opens one connection, checks the contract, and closes it.
//
// It is deliberately not sharing the evaluation connections: the check runs
// in `ruler check` too, where nothing is evaluating, and a connection per
// source at load is not worth complicating either path for.
func assertSource(ctx context.Context, src source.Source, setting policy.Setting) []query.Assertion {
	q, err := query.Open(src)
	if err != nil {
		return unreachable(setting, fmt.Sprintf("connecting as %s: %s", src.Username, err))
	}
	defer func() { _ = q.Close() }()

	ctx, cancel := context.WithTimeout(ctx, privilegeTimeout)
	defer cancel()

	return q.Privileges(ctx, setting.Keys)
}

// unreachable reports every requested assertion as undecided, because a
// cluster that cannot be reached has answered none of them. Silence here
// would read as a contract that holds.
func unreachable(setting policy.Setting, detail string) []query.Assertion {
	var out []query.Assertion
	for _, name := range setting.Keys {
		out = append(out, query.Assertion{Name: name, Status: query.StatusInconclusive, Detail: detail})
	}
	return out
}

// privilegesEnabled reports whether the check runs at all. At severity off,
// or with every assertion dropped, no probe is sent: an operator who turned
// the check off should see no traffic from it, not traffic whose result is
// discarded.
func privilegesEnabled(setting policy.Setting) bool {
	return setting.Severity != lint.SeverityOff && len(setting.Keys) > 0
}

// privilegeProblems turns assertion results into findings, one per assertion
// that did not pass, so a finding names which half of the contract is missing
// rather than reporting that something about the user is wrong.
//
// A failure carries the configured severity. An inconclusive result never
// does: it says what the ruler could see rather than how the cluster is
// configured, and blocking a deploy on one would let a network blip refuse a
// source that meets the contract.
func privilegeProblems(file, name string, line int, setting policy.Setting, results []query.Assertion) []lint.Problem {
	var problems []lint.Problem

	for _, a := range results {
		var p lint.Problem
		switch a.Status {
		case query.StatusPass:
			continue
		case query.StatusFail:
			p = lint.Problem{
				Severity: setting.Severity,
				Text:     fmt.Sprintf("%s: %s", a.Name, a.Detail),
			}
		case query.StatusInconclusive:
			p = lint.Problem{
				Severity: lint.SeverityWarning,
				Text:     fmt.Sprintf("%s: inconclusive, %s", a.Name, a.Detail),
			}
		}

		p.File = file
		p.Line = line
		p.Subject = name
		p.Check = policy.CheckSourcePrivileges
		p.PolicyFile = setting.File
		p.PolicyLine = setting.Line
		problems = append(problems, p)
	}
	return problems
}

// refusedSources checks the contract for every source a rule matched and
// returns the names failing it at error severity.
//
// Findings are reported however severe they are; only the errors refuse a
// source. A warning is the operator being told, which is the whole of what
// this check does at its default severity (spec 7.6).
func refusedSources(
	ctx context.Context,
	file string,
	set *ruleset.Set,
	root *policy.Policy,
	stderr io.Writer,
	log *slog.Logger,
) map[string]bool {
	problems := checkPrivileges(ctx, file, matchedSources(set), root)
	if len(problems) == 0 {
		return nil
	}
	if err := lint.Format(stderr, lint.FormatText, problems); err != nil {
		printf(stderr, "%s\n", err)
	}

	refused := map[string]bool{}
	for _, p := range problems {
		if p.Severity == lint.SeverityError {
			refused[p.Subject] = true
			log.Warn("refusing a source that failed the user contract",
				"source", p.Subject, "check", p.Check, "problem", p.Text)
		}
	}
	return refused
}

// matchedSources lists every source a loaded rule matched, once each.
func matchedSources(set *ruleset.Set) []source.Source {
	seen := map[string]bool{}

	var out []source.Source
	for _, r := range set.Rules {
		for _, src := range r.Sources {
			if seen[src.Name] {
				continue
			}
			seen[src.Name] = true
			out = append(out, src)
		}
	}
	return out
}

// refuseSources drops refused sources from every rule that matched one, so
// rules against a source failing the contract at error severity do not
// evaluate while the rest of the ruler carries on.
func refuseSources(set *ruleset.Set, refused map[string]bool) {
	if len(refused) == 0 {
		return
	}

	for i := range set.Rules {
		kept := set.Rules[i].Sources[:0]
		for _, src := range set.Rules[i].Sources {
			if !refused[src.Name] {
				kept = append(kept, src)
			}
		}
		set.Rules[i].Sources = kept
	}
}
