package scheduler

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
)

// feedLoad is the third feed into clickhouse_ruler_problem, beside the
// evaluation and the re-check timer (spec 10.4).
//
// The other two report a rule that broke after it was reviewed. This one
// reports a review that did not happen: a finding that blocks a merge no
// longer refuses a reading (spec 7.6), so a file that got past the checker
// loads, and without this it would run with nobody told.
const feedLoad = "load"

// ReportLoadFindings rebuilds the load feed's series on the problem gauge from
// one reading of the files.
//
// It owns the fixed checks that do not refuse a reading, and only those. An
// operator who raised a configurable check to error has not moved it onto this
// feed, because the checks the evaluation and the timer own are raised on
// their own clocks and a load pass knows nothing about them.
func ReportLoadFindings(m *Metrics, log *slog.Logger, set *ruleset.Set, problems []lint.Problem) {
	for _, c := range lint.All() {
		if ownedByLoad(c) {
			m.Problem.DeletePartialMatch(prometheus.Labels{"check": c.Name})
		}
	}

	teams := teamsByRule(set)
	for _, p := range problems {
		check, ok := lint.Lookup(p.Check)
		if !ok || !ownedByLoad(check) || p.Severity != lint.SeverityError {
			continue
		}

		team := teams[ruleAt{file: p.File, alert: p.Subject}]

		// No source label. A load finding is about what the file says, which
		// is true of every cluster the rule reaches, where the other two feeds
		// report what one cluster answered.
		m.Problem.WithLabelValues(p.Subject, p.Check, p.Severity.String(), team, p.File, "").Set(1)

		if log != nil {
			// A warning however severe the finding is, the same as the other
			// feeds: the rule is loaded and still paging, so nothing about the
			// ruler is failing. The severity is the check's and rides as a
			// field (spec 8.4).
			log.Warn("rule loaded with a finding that should have blocked the merge",
				"rule", p.Subject, "check", p.Check, "severity", p.Severity.String(),
				"team", team, "file", p.File, "feed", feedLoad, "problem", p.Text)
		}
	}
}

// ownedByLoad reports whether this feed is the only one that raises a check.
// Fixed and not refusing a reading is exactly that set: a fixed check cannot
// be turned off, and one that refuses a reading never reaches a running ruler.
func ownedByLoad(c lint.Check) bool {
	return c.Fixed && !c.RefusesReading
}

// ruleAt identifies a rule the way a finding names one: the file it is in and
// the alert name it carries.
type ruleAt struct {
	file  string
	alert string
}

// teamsByRule maps each loaded rule to the team that owns it, so a finding can
// be labelled with whoever has to fix it. A finding about a file rather than a
// rule matches nothing here and carries no team, which is honest: there is no
// rule to own it.
func teamsByRule(set *ruleset.Set) map[ruleAt]string {
	if set == nil {
		return nil
	}
	out := make(map[ruleAt]string, len(set.Rules))
	for _, r := range set.Rules {
		out[ruleAt{file: r.File, alert: r.Alert}] = r.Team()
	}
	return out
}
