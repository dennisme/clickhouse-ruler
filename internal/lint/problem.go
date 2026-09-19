package lint

import "fmt"

// Severity decides whether a problem blocks a deploy or only annotates it.
type Severity int

const (
	SeverityWarning Severity = iota
	SeverityError
)

func (s Severity) String() string {
	switch s {
	case SeverityWarning:
		return "warning"
	case SeverityError:
		return "error"
	default:
		return fmt.Sprintf("severity(%d)", int(s))
	}
}

// Checks that inspect the file itself rather than anything it describes.
const (
	CheckYAMLSyntax       = "yaml/syntax"
	CheckYAMLUnknownField = "yaml/unknown-field"
	CheckYAMLType         = "yaml/type"
)

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
}

func (p Problem) String() string {
	loc := fmt.Sprintf("%s:%d", p.File, p.Line)
	if p.Subject != "" {
		loc += " " + p.Subject
	}
	return fmt.Sprintf("%s: %s: %s: %s", loc, p.Severity, p.Check, p.Text)
}
