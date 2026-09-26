package lint

import (
	"fmt"
	"io"
	"strings"
)

// SummaryRow is what one rule costs on one source, every time it evaluates.
//
// The row key is the rule and the source, never the rule alone. A rule
// matches sources by label and runs against each one, so the same SQL is
// cheap on staging and ruinous on production, and a table that averaged the
// two would hide the row somebody needs to see (spec 7.10).
//
// Every cell is text because a cell can say why there is no number. An
// estimate the source's user was not allowed to ask for is not zero, and
// printing zero for it would be the one reading nobody can act on.
type SummaryRow struct {
	File     string
	Alert    string
	Source   string
	Rows     string
	Interval string
}

// summaryColumns are the headings, in the order they are read.
//
// Rows before interval, because "4.1 M rows every 30 seconds" is the sentence
// that changes somebody's mind and the rate is the pair of them. Bytes read
// and duration are absent: both need the query to run, which is a decision an
// operator makes rather than a column a table grows (spec 7.10).
var summaryColumns = []string{"File", "Alert", "Source", "Rows", "Interval"}

// FormatSummary writes the cost table as markdown, for a comment on a pull
// request. Posting it is the workflow's job, not the ruler's.
func FormatSummary(w io.Writer, rows []SummaryRow) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "No rule was estimated: nothing matched a source.")
		return err
	}

	header := "| " + strings.Join(summaryColumns, " | ") + " |\n"
	divider := "| " + strings.Repeat("--- | ", len(summaryColumns)-1) + "--- |\n"
	if _, err := io.WriteString(w, header+divider); err != nil {
		return err
	}

	for _, r := range rows {
		cells := []string{r.File, r.Alert, r.Source, r.Rows, r.Interval}
		for i, c := range cells {
			cells[i] = escapeCell(c)
		}
		if _, err := fmt.Fprintf(w, "| %s |\n", strings.Join(cells, " | ")); err != nil {
			return err
		}
	}
	return nil
}

// escapeCell protects the column separator. A pipe inside a value ends the
// cell early and shifts every value after it under the wrong heading.
func escapeCell(s string) string {
	return strings.NewReplacer(
		"|", `\|`,
		"\r", " ",
		"\n", " ",
	).Replace(s)
}
