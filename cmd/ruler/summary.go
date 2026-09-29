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
	case query.CostShardsUnknown:
		return "not estimated: this source's shard count could not be read, so what the server " +
			"answered covers the coordinator's parts alone"
	case query.CostEstimated:
		return rowsCell(cost)
	default:
		return "not estimated"
	}
}

// rowsCell is the number, and how it was arrived at when a cluster's was scaled
// from a node's.
//
// This table is the only place a sharded rule inside its ceilings is told that.
// `rule/cost` reports on breach, so a comfortable rule raises no finding, and a
// finding is the only thing a caveat can hang from (spec 6.9, 7.10).
func rowsCell(cost *query.CostEstimate) string {
	rows := strconv.FormatUint(cost.Rows, 10)
	if cost.Shards <= 1 {
		return rows
	}
	return fmt.Sprintf("%s (%d shards)", rows, cost.Shards)
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

// writeReport puts the findings table where the workflow asked for it, the same
// way writeSummary puts the cost table: a path rather than a posted comment,
// because posting needs a token and an API and the workflow already has both.
// "-" is stdout, for a person running the command by hand (spec 10.3).
func writeReport(path, linkPrefix string, problems []lint.Problem, stdout io.Writer) error {
	if path == "-" {
		if err := lint.FormatMarkdown(stdout, problems, linkPrefix); err != nil {
			return fmt.Errorf("writing the findings report: %w", err)
		}
		return nil
	}

	f, err := os.Create(path) //nolint:gosec // an operator-supplied path is the input
	if err != nil {
		return fmt.Errorf("writing the findings report: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := lint.FormatMarkdown(f, problems, linkPrefix); err != nil {
		return fmt.Errorf("writing the findings report: %w", err)
	}
	return f.Close()
}
