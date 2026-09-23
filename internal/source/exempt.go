package source

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// Exemption is a source owner saying that a finding is expected on this
// cluster, for a stated reason, until a stated date.
//
// It is not a severity and takes no part in the policy merge. A severity that
// could be lowered per source would make "the strictest scope wins" untrue,
// which is what 7.7 rests on; an exemption sits outside the merge and drops
// the finding afterwards, for one check on one source.
//
// The rule file cannot carry one. That is the difference from `pint`'s
// `# pint disable promql/series(staging)`: the author who is blocked would be
// the author who unblocks themselves, and the only thing between a repository
// and a silently disabled check would be whether a reviewer read a comment
// (spec 7.9).
type Exemption struct {
	Check  string
	Reason string

	// Until is when it stops applying. Required, because an exemption with no
	// expiry is a check quietly deleted: the reason it was granted stops
	// being true long before anyone thinks to look at the file again.
	//
	// The instant itself is already expired, so `until: 2026-12-01` covers
	// November and not December. A bare date is midnight UTC, whatever the
	// machine running the check is set to: an exemption that expired at a
	// different moment in CI than on a laptop would be a finding that depends
	// on where it was run. An operator who needs a specific local moment
	// writes the RFC3339 form with its offset.
	Until time.Time

	// Line is where it was written, so an expiry finding points at the
	// exemption rather than at the source.
	Line int
}

// exemptionFormats are what `until` may be written as. A date is what an
// operator reaches for and is read as midnight UTC; a timestamp carries its
// own offset, so one can expire at a deploy rather than at midnight.
var exemptionFormats = []string{time.DateOnly, time.RFC3339}

// Exempts reports whether this source has an unexpired exemption for a check.
func (s Source) Exempts(check string, now time.Time) bool {
	for _, e := range s.Exemptions {
		if e.Check == check && now.Before(e.Until) {
			return true
		}
	}
	return false
}

// ExpiredExemptions reports every exemption that has run out.
//
// An error rather than a warning, and a finding rather than silence: the date
// is what makes an exemption a decision with an end, so the file failing on
// the day it was granted until is the mechanism working. Whoever renews it
// has to state the reason again, in front of a reviewer.
func (f *File) ExpiredExemptions(now time.Time) []lint.Problem {
	var out []lint.Problem

	for _, s := range f.Sources {
		for _, e := range s.Exemptions {
			if now.Before(e.Until) {
				continue
			}
			p := lint.NewProblem(f.File, e.Line, lint.CheckSourceExemption, lint.SeverityError,
				fmt.Sprintf("the exemption for %s expired on %s: %s",
					e.Check, e.Until.Format(time.RFC3339), e.Reason))
			p.Subject = s.Name
			out = append(out, p)
		}
	}
	return out
}

// parseExemptions reads a source's exempt list.
func parseExemptions(r *lint.Reader, n *yaml.Node) []Exemption {
	if !r.Sequence(n, "exempt") {
		return nil
	}

	out := make([]Exemption, 0, len(n.Content))
	for _, item := range n.Content {
		if e, ok := parseExemption(r, item); ok {
			out = append(out, e)
		}
	}
	return out
}

func parseExemption(r *lint.Reader, n *yaml.Node) (Exemption, bool) {
	if !r.Mapping(n, "exemption") {
		return Exemption{}, false
	}
	e := Exemption{Line: n.Line}

	var until string
	lines := lint.NewLines(n.Line)

	for _, entry := range lint.Entries(n) {
		lines.Set(entry.Key.Value, entry.Key.Line)

		switch entry.Key.Value {
		case "check":
			e.Check, _ = r.Scalar(entry.Value, "check")
		case "reason":
			e.Reason, _ = r.Scalar(entry.Value, "reason")
		case "until":
			until, _ = r.Scalar(entry.Value, "until")
		default:
			r.UnknownField(entry.Key, "exemption")
		}
	}

	ok := checkExemptionCheck(r, e.Check, lines.Of("check"))

	// Every field is required. An exemption is a decision somebody has to be
	// able to review later, and two of the three things a reviewer needs are
	// why it was granted and when it ends.
	if e.Reason == "" {
		r.Add(lines.Of("reason"), lint.CheckSourceExemption, lint.SeverityError,
			"reason is empty: an exemption nobody explained cannot be reviewed")
		ok = false
	}
	if until == "" {
		r.Add(lines.Of("until"), lint.CheckSourceExemption, lint.SeverityError,
			"until is empty: an exemption with no expiry is a check quietly deleted")
		return e, false
	}

	parsed, err := parseUntil(until)
	if err != nil {
		r.Add(lines.Of("until"), lint.CheckSourceExemption, lint.SeverityError,
			"until must be a date such as 2026-12-01 or an RFC3339 timestamp, got %q", until)
		return e, false
	}
	e.Until = parsed

	return e, ok
}

// checkExemptionCheck refuses an exemption for a check that does not exist or
// cannot be softened.
func checkExemptionCheck(r *lint.Reader, check string, line int) bool {
	switch {
	case check == "":
		r.Add(line, lint.CheckSourceExemption, lint.SeverityError,
			"check is empty: an exemption has to name what it exempts")
	case lint.Fixed(check):
		// The same refusal policy gives a fixed check in a `checks:` block. A
		// rule failing one cannot do its job, so exempting it produces a rule
		// that looks fine and never fires.
		r.Add(line, lint.CheckSourceExemption, lint.SeverityError,
			"%q is a correctness check and cannot be exempted: a rule that fails it cannot run", check)
	case !lint.Known(check):
		r.Add(line, lint.CheckSourceExemption, lint.SeverityError, "unknown check %q", check)
	default:
		return true
	}
	return false
}

func parseUntil(s string) (time.Time, error) {
	for _, layout := range exemptionFormats {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q", s)
}
