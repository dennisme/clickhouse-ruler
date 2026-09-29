package lint

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Output formats for a set of findings.
const (
	FormatText   = "text"
	FormatGitHub = "github"
	FormatJSON   = "json"
)

// Formats lists every supported format, for flag help and validation.
var Formats = []string{FormatText, FormatGitHub, FormatJSON}

// Format writes findings in the named format.
func Format(w io.Writer, format string, problems []Problem) error {
	switch format {
	case FormatText:
		return formatText(w, problems)
	case FormatGitHub:
		return formatGitHub(w, problems)
	case FormatJSON:
		return formatJSON(w, problems)
	default:
		return fmt.Errorf("unknown format %q, want one of %s", format, strings.Join(Formats, ", "))
	}
}

// formatText prints one finding per line, with its documentation on the next
// one. Indented under the finding rather than appended to it: the finding is
// what a reader scans and the link is what they follow once they have decided
// to (spec 7.8).
func formatText(w io.Writer, problems []Problem) error {
	for _, p := range problems {
		if _, err := fmt.Fprintln(w, p.String()); err != nil {
			return err
		}
		if url := DocsURL(p.Check); url != "" {
			if _, err := fmt.Fprintf(w, "  %s\n", url); err != nil {
				return err
			}
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

		// The link goes in the message rather than the title. GitHub renders
		// the title as a short label and the message as the annotation body,
		// and an author reading the annotation on their diff is the person
		// who needs somewhere to go next.
		text := p.Text
		if url := DocsURL(p.Check); url != "" {
			text += " (" + url + ")"
		}

		_, err := fmt.Fprintf(w, "::%s file=%s,line=%d,title=%s::%s\n",
			command,
			escapeProperty(p.File),
			p.Line,
			escapeProperty(p.Check),
			escapeData(text),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// jsonProblem is the published shape of a finding, which is deliberately not
// the Problem struct itself.
//
// Two differences, and both are the point. A severity is its name: the numbers
// order a policy merge (spec 7.7) and are ours to rearrange, which stops being
// true the moment a consumer parses them. And the documentation link is
// carried rather than left to be constructed, the same link the other two
// formats print, because a check name a reader cannot look up turns their own
// finding into a question for whoever owns policy (spec 7.8).
type jsonProblem struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	Subject    string `json:"subject,omitempty"`
	Check      string `json:"check"`
	Severity   string `json:"severity"`
	Text       string `json:"text"`
	Docs       string `json:"docs,omitempty"`
	PolicyFile string `json:"policy_file,omitempty"`
	PolicyLine int    `json:"policy_line,omitempty"`
}

// formatJSON writes the findings as one array, which is what the summary
// comment in spec 10.3 reads and what anyone integrating the checks elsewhere
// gets for free (spec 10.1).
func formatJSON(w io.Writer, problems []Problem) error {
	// Never nil. An empty array is something a consumer can iterate, where
	// `null` makes every reader special-case the green build.
	out := make([]jsonProblem, 0, len(problems))
	for _, p := range problems {
		out = append(out, jsonProblem{
			File:       p.File,
			Line:       p.Line,
			Subject:    p.Subject,
			Check:      p.Check,
			Severity:   p.Severity.String(),
			Text:       p.Text,
			Docs:       DocsURL(p.Check),
			PolicyFile: p.PolicyFile,
			PolicyLine: p.PolicyLine,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
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

// findingColumns are the headings of the markdown table, in the order they are
// read: what it is, where it is, what raised it, and what to do.
var findingColumns = []string{"Severity", "Rule", "Check", "Finding"}

// FormatMarkdown writes the findings as a markdown table, for a pull request
// comment. Posting it is the workflow's job, not the ruler's, the same split
// FormatSummary makes for the cost table (spec 7.10, 10.3).
//
// linkPrefix is a URL a finding's path is appended to, such as
// https://github.com/owner/repo/blob/<commit>/, which turns each location into
// a link to the line. Empty prints the location plainly, which is what a run on
// a laptop wants: where a file is served from is a fact about the host, and the
// ruler knows nothing about hosts.
func FormatMarkdown(w io.Writer, problems []Problem, linkPrefix string) error {
	var errors, warnings int
	for _, p := range problems {
		switch p.Severity {
		case SeverityError:
			errors++
		case SeverityWarning:
			warnings++
		case SeverityOff:
		}
	}

	if len(problems) == 0 {
		// Stated rather than left blank. An empty comment reads as a checker
		// that did not run, which is the one thing a green result must not
		// look like.
		_, err := fmt.Fprintln(w, "No findings.")
		return err
	}

	header := fmt.Sprintf("%s, %s\n\n", plural(errors, "error"), plural(warnings, "warning"))
	header += "| " + strings.Join(findingColumns, " | ") + " |\n"
	header += "| " + strings.Repeat("--- | ", len(findingColumns)-1) + "--- |\n"
	if _, err := io.WriteString(w, header); err != nil {
		return err
	}

	for _, p := range problems {
		location := fmt.Sprintf("%s:%d", p.File, p.Line)
		rule := "`" + location + "`"
		if path, ok := repoPath(p.File); ok && linkPrefix != "" {
			rule = fmt.Sprintf("[`%s`](%s%s#L%d)", location, linkPrefix, path, p.Line)
		}
		if p.Subject != "" {
			rule += " " + escapeCell(p.Subject)
		}

		check := "`" + p.Check + "`"
		if url := DocsURL(p.Check); url != "" {
			check = fmt.Sprintf("[`%s`](%s)", p.Check, url)
		}

		cells := []string{p.Severity.String(), rule, check, escapeCell(p.Text)}
		if _, err := fmt.Fprintf(w, "| %s |\n", strings.Join(cells, " | ")); err != nil {
			return err
		}
	}
	return nil
}

// repoPath is the finding's path as the repository spells it, which is what a
// link appends to its prefix.
//
// A finding carries the path the loader walked, so a rules argument given as an
// absolute path produces one, and a URL built from that names a directory on
// the runner rather than a file anybody can open. Such a path is linked only
// when it sits under the working directory, which is the checkout when a
// workflow runs it; anything else is printed without a link rather than with a
// broken one.
func repoPath(file string) (string, bool) {
	if !filepath.IsAbs(file) {
		return filepath.ToSlash(strings.TrimPrefix(file, "./")), true
	}

	wd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(wd, file)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// plural counts a severity the way the heading reads it.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
