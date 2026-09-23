package lint

import "fmt"

// Severity decides whether a problem blocks a deploy or only annotates it.
type Severity int

// Ordered from least to most severe. Policy merging takes the maximum across
// every scope that applies to a rule, so the numeric order is meaningful and
// must not be rearranged (spec 7.7).
const (
	SeverityOff Severity = iota
	SeverityWarning
	SeverityError
)

func (s Severity) String() string {
	switch s {
	case SeverityOff:
		return "off"
	case SeverityWarning:
		return "warning"
	case SeverityError:
		return "error"
	default:
		return fmt.Sprintf("severity(%d)", int(s))
	}
}

// Problem is one validation finding. File and Line are always populated so
// that CI can attach the finding to a line of the diff.
type Problem struct {
	File string
	Line int

	// Subject is the named thing the finding belongs to, such as an alert
	// name or a source name. Empty when the finding is about the file itself.
	Subject string

	Check    string
	Severity Severity
	Text     string

	// PolicyFile and PolicyLine are where this finding's severity was set.
	// Empty when it came from the shipped default. `ruler check --explain`
	// reads them so an author can see which policy file raised a check
	// instead of guessing (spec 7.8).
	PolicyFile string
	PolicyLine int
}

// NewProblem builds a finding, refusing a check name the table does not know.
//
// Every finding is constructed here, which is what makes the check table a
// complete catalogue rather than a list somebody remembers to extend: a name
// written inline at a call site fails the first test that exercises it, and
// the documentation generated from the table cannot be missing a page for a
// check that ships (spec 7.8).
//
// A panic rather than a returned error. An unknown check name is a mistake in
// this repository's own source, not a condition a caller can handle or an
// operator can cause.
func NewProblem(file string, line int, check string, sev Severity, text string) Problem {
	if !Known(check) {
		panic("lint: unknown check " + check + ", every check belongs in the table in checks.go")
	}
	return Problem{
		File:     file,
		Line:     line,
		Check:    check,
		Severity: sev,
		Text:     text,
	}
}

func (p Problem) String() string {
	loc := fmt.Sprintf("%s:%d", p.File, p.Line)
	if p.Subject != "" {
		loc += " " + p.Subject
	}
	return fmt.Sprintf("%s: %s: %s: %s", loc, p.Severity, p.Check, p.Text)
}
