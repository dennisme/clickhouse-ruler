package lint

import (
	"fmt"
	"io"
	"strings"
)

// Output formats for a set of findings.
const (
	FormatText   = "text"
	FormatGitHub = "github"
)

// Formats lists every supported format, for flag help and validation.
var Formats = []string{FormatText, FormatGitHub}

// Format writes findings in the named format.
func Format(w io.Writer, format string, problems []Problem) error {
	switch format {
	case FormatText:
		return formatText(w, problems)
	case FormatGitHub:
		return formatGitHub(w, problems)
	default:
		return fmt.Errorf("unknown format %q, want one of %s", format, strings.Join(Formats, ", "))
	}
}

func formatText(w io.Writer, problems []Problem) error {
	for _, p := range problems {
		if _, err := fmt.Fprintln(w, p.String()); err != nil {
			return err
		}
	}
	return nil
}

// formatGitHub writes workflow commands, which GitHub renders inline on the
// diff. That inline rendering is the reason every Problem carries a file and a
// line: a finding attached to the changed line is one a contributor acts on,
// and a finding in a log is one they scroll past.
func formatGitHub(w io.Writer, problems []Problem) error {
	for _, p := range problems {
		command := "warning"
		if p.Severity == SeverityError {
			command = "error"
		}

		_, err := fmt.Fprintf(w, "::%s file=%s,line=%d,title=%s::%s\n",
			command,
			escapeProperty(p.File),
			p.Line,
			escapeProperty(p.Check),
			escapeData(p.Text),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// escapeData protects the message body. A literal newline would end the
// command early and silently drop the rest of the annotation.
func escapeData(s string) string {
	return strings.NewReplacer(
		"%", "%25",
		"\r", "%0D",
		"\n", "%0A",
	).Replace(s)
}

// escapeProperty protects a command property, which additionally cannot
// contain the separators that delimit the properties themselves.
func escapeProperty(s string) string {
	return strings.NewReplacer(
		"%", "%25",
		"\r", "%0D",
		"\n", "%0A",
		":", "%3A",
		",", "%2C",
	).Replace(s)
}
