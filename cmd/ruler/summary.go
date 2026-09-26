package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// summaryRow is one rule against one source, ready for the table.
func summaryRow(r ruleset.Rule, src source.Source, cost *query.CostEstimate) lint.SummaryRow {
	return lint.SummaryRow{
		File:     r.File,
		Alert:    r.Alert,
		Source:   src.Name,
		Rows:     costCell(cost),
		Interval: intervalCell(r.Group.Interval),
	}
}

// costCell is what the rule is predicted to read, or why there is no number.
//
// A missing estimate is never printed as zero. Zero is a rule that reads
// nothing, and a reader who cannot tell that apart from a rule nobody was
// allowed to estimate learns the wrong thing from the table (spec 7.10).
func costCell(cost *query.CostEstimate) string {
	if cost == nil {
		return "not estimated: the source could not be read"
	}
	switch cost.Status {
	case query.CostRefused:
		return "not estimated: this source's user cannot read what the rule asks for"
	case query.CostNoParts:
		return "not estimated: the server predicts no part will be read"
	case query.CostEstimated:
		return strconv.FormatUint(cost.Rows, 10)
	default:
		return "not estimated"
	}
}

// intervalCell is how often the rule evaluates. A group that set none has no
// rate, and printing "0s" would claim it evaluates continuously.
func intervalCell(interval time.Duration) string {
	if interval <= 0 {
		return "not set"
	}
	return interval.String()
}

// writeSummary puts the cost table where the workflow asked for it.
//
// A path rather than a posted comment: posting one needs a token and an API
// client, and a workflow that already has both does it in a line
// (`gh pr comment --body-file`). "-" is stdout, which is where a person
// running the command by hand wants it (spec 7.10).
func writeSummary(path string, rows []lint.SummaryRow, stdout io.Writer) error {
	if path == "-" {
		if err := lint.FormatSummary(stdout, rows); err != nil {
			return fmt.Errorf("writing the cost summary: %w", err)
		}
		return nil
	}

	f, err := os.Create(path) //nolint:gosec // an operator-supplied path is the input
	if err != nil {
		return fmt.Errorf("writing the cost summary: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := lint.FormatSummary(f, rows); err != nil {
		return fmt.Errorf("writing the cost summary: %w", err)
	}
	return f.Close()
}
