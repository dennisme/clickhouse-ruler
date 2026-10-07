package docs

import (
	"flag"
	"fmt"
	"regexp"
	"strings"

	"github.com/dennisme/clickhouse-ruler/internal/cli"
)

// FlagsPage is the page carrying the flag reference. It sits beside the
// check pages rather than under them, because flags are what the binary takes
// rather than what it reports.
const FlagsPage = "running.md"

// Flags renders a table per command of what the binary takes.
//
// Generated from the flag set for the reason the check pages are generated
// from the check table: the usage strings are what `--help` prints, so a
// generated page and a terminal cannot disagree, and a hand-written one goes
// stale the first time a default moves (spec 14).
func Flags() string {
	var sb strings.Builder

	for i, c := range cli.Commands() {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "### `ruler %s`\n\n", c.Name)
		sb.WriteString("| Flag | Default | What it does |\n")
		sb.WriteString("| --- | --- | --- |\n")

		c.Flags.VisitAll(func(f *flag.Flag) {
			fmt.Fprintf(&sb, "| `--%s` | %s | %s |\n", f.Name, defaultValue(f), usage(f))
		})
	}
	return sb.String()
}

// defaultValue prints what the flag is worth when nobody sets it.
//
// An empty string reads as "none" rather than as an empty cell, because an
// empty cell is also what a bug in this function looks like. A required flag
// says so, since "none" there would suggest the ruler runs without it.
func defaultValue(f *flag.Flag) string {
	if f.DefValue != "" {
		return "`" + f.DefValue + "`"
	}
	if strings.Contains(f.Usage, "(required)") {
		return "none, required"
	}
	return "none"
}

// usage is the flag's own usage string, rendered so that a markdown table
// survives it.
//
// A URL goes in a code span, because a bare one is an MD034 failure and
// because a placeholder inside it is then left alone: `<commit>` in a code
// span is the text somebody has to replace, where outside one it is read as a
// tag and disappears. Everything else is escaped, since a pipe would end the
// cell and an angle bracket would vanish the same way.
func usage(f *flag.Flag) string {
	var sb strings.Builder

	for rest := f.Usage; ; {
		at := url.FindStringIndex(rest)
		if at == nil {
			sb.WriteString(escape(rest))
			return sb.String()
		}
		sb.WriteString(escape(rest[:at[0]]))
		sb.WriteString("`" + rest[at[0]:at[1]] + "`")
		rest = rest[at[1]:]
	}
}

// url matches a URL in a usage string, stopping at the comma or space that
// ends the clause it sits in rather than swallowing it.
var url = regexp.MustCompile(`https?://[^\s,]+`)

func escape(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}
