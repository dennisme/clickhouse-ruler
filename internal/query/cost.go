package query

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// Estimate is what ClickHouse predicts one table will contribute to a query.
//
// Predicted, never measured: these come from the primary index and the part
// metadata, so they are the optimiser's guess and can be out by an order of
// magnitude on a skewed key. That is why the check reading them warns rather
// than blocks (spec 7.3).
type Estimate struct {
	Database string
	Table    string
	Parts    uint64
	Rows     uint64
	Marks    uint64
}

// CostStatus says whether a rule has a predicted cost, and when it has none,
// why. Three answers rather than a number and a zero: a rule nobody was
// allowed to estimate and a rule that reads no tracked table are different
// facts, and both are different from a rule that reads nothing.
type CostStatus int

const (
	// CostEstimated is a prediction the server made.
	CostEstimated CostStatus = iota

	// CostUntracked is a query the server estimated nothing for, which reads
	// no table it accounts for this way: a constant, or a count it answers
	// from metadata.
	CostUntracked

	// CostRefused is the source's own user not being allowed to look, which
	// is a fact about the grant rather than about the rule (spec 6.7.2).
	CostRefused
)

// CostEstimate is what one source predicts a rule reads on every evaluation.
//
// Predicted, never measured. Rows is meaningful only when Status is
// CostEstimated.
type CostEstimate struct {
	Rows   uint64
	Status CostStatus
}

// costFrom turns what the server estimated into what the caller reports.
func costFrom(est []Estimate) CostEstimate {
	if len(est) == 0 {
		return CostEstimate{Status: CostUntracked}
	}
	return CostEstimate{Rows: estimatedRows(est), Status: CostEstimated}
}

// Cost is how much a rule may read, resolved from policy by the caller.
type Cost struct {
	// MaxRows is what one evaluation may read.
	MaxRows uint64

	// MaxRowsPerSecond is the same number against the clock. It is the one
	// that matters: the same query is cheap hourly and ruinous every fifteen
	// seconds, and only the rate says which one a rule is.
	MaxRowsPerSecond float64
}

// classifyEstimate separates a source whose user cannot look from a ruler
// that could not ask.
//
// Nothing here is a finding about the SQL. By the time the estimate is asked
// for, the query has parsed and resolved, so a refusal is about the grant
// rather than the rule, which is the same answer DESCRIBE gives (spec 7.3).
func classifyEstimate(err error) (*Finding, error) {
	if err == nil {
		return nil, nil
	}

	var ex *clickhouse.Exception
	if errors.As(err, &ex) && ex.Code == codeAccessDenied {
		return &Finding{
			Check: lint.CheckRuleTableAccess,
			Detail: "this source's user cannot read what the rule asks for, so its cost was never " +
				"estimated: " + strings.TrimSpace(ex.Message),
		}, nil
	}
	return nil, fmt.Errorf("estimating the rule's cost: %w", err)
}

// explainEstimate asks what the query will read, without reading it.
//
// The third round trip, after EXPLAIN AST and DESCRIBE. Like them it executes
// nothing: ClickHouse answers from the primary index and the parts it would
// have opened, so the cost is a parse and a network hop (spec 7.3).
func (q *Querier) explainEstimate(ctx context.Context, sql string) ([]Estimate, error) {
	rows, err := q.conn.Query(ctx, "EXPLAIN ESTIMATE "+sql)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Estimate
	for rows.Next() {
		var e Estimate
		if err := rows.Scan(&e.Database, &e.Table, &e.Parts, &e.Rows, &e.Marks); err != nil {
			return nil, fmt.Errorf("estimating the rule's cost: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("estimating the rule's cost: %w", err)
	}
	return out, nil
}

// explainIndexes returns the query plan with the index sections included.
//
// The conditional fourth round trip, asked only when a rule is already over a
// ceiling. It reads no data either, and it answers the question the estimate
// raises rather than one of its own: a large number is worth acting on when
// the primary key is not excluding anything.
func (q *Querier) explainIndexes(ctx context.Context, sql string) (string, error) {
	rows, err := q.conn.Query(ctx, "EXPLAIN indexes=1 "+sql)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()

	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", fmt.Errorf("reading the rule's query plan: %w", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("reading the rule's query plan: %w", err)
	}
	return strings.Join(lines, "\n"), nil
}

// estimatedRows is what the whole query is predicted to read.
//
// Summed across tables. A rule joining two of them pays for both, and taking
// whichever row arrived first would report a join as though it read one side.
func estimatedRows(est []Estimate) uint64 {
	var total uint64
	for _, e := range est {
		total += e.Rows
	}
	return total
}

// rowsPerSecond is what an evaluation costs against the clock. Zero when no
// interval is known, which is a rule whose group did not set one: there is
// nothing to divide by, so the rate ceiling does not apply.
func rowsPerSecond(rows uint64, interval time.Duration) float64 {
	if interval <= 0 {
		return 0
	}
	return float64(rows) / interval.Seconds()
}

// overCost describes what a rule exceeds, or returns empty when it is within
// its ceilings or none are configured.
func overCost(est []Estimate, limit *Cost, interval time.Duration) string {
	if limit == nil || len(est) == 0 {
		return ""
	}
	rows := estimatedRows(est)
	rate := rowsPerSecond(rows, interval)

	var over []string
	if rows > limit.MaxRows {
		over = append(over, fmt.Sprintf("%d rows against a ceiling of %d", rows, limit.MaxRows))
	}
	if rate > limit.MaxRowsPerSecond {
		over = append(over, fmt.Sprintf("%.0f rows a second against a ceiling of %.0f",
			rate, limit.MaxRowsPerSecond))
	}
	if len(over) == 0 {
		return ""
	}

	detail := fmt.Sprintf("the query is estimated to read %s", strings.Join(over, " and "))
	if interval > 0 {
		detail += fmt.Sprintf(", every %s this group evaluates", interval)
	}
	return detail + ", and the estimate is the optimiser's prediction rather than a measurement"
}

// withPruning adds why a rule is expensive, when the plan says why.
//
// Appended to the ceiling's finding rather than reported on its own. A note
// that can never appear alone is not a check: it is this check explaining
// itself, and giving it a name of its own would need a documentation page
// saying it never fires by itself (spec 7.3).
func withPruning(detail string, tables []string) string {
	if len(tables) == 0 {
		return detail
	}
	return detail + fmt.Sprintf(". The primary key of %s excluded no granules, so the query reads "+
		"the table rather than a slice of it: a rule's time bound has to be on the key for the "+
		"key to help", strings.Join(tables, ", "))
}

// unprunedTables returns the tables whose primary key excluded nothing.
//
// `EXPLAIN indexes=1` prints one ReadFromMergeTree section per table, and a
// PrimaryKey block inside it reporting `Granules: selected/total`. Equal
// numbers mean every granule is still a candidate, so the query is reading
// the table rather than a slice of it, which is usually why the estimate is
// large and is the part an author can do something about (spec 7.3).
func unprunedTables(plan string) []string {
	var out []string
	var table string
	var inPrimaryKey bool

	for _, line := range strings.Split(plan, "\n") {
		trimmed := planLine(line)

		switch {
		case strings.HasPrefix(trimmed, "ReadFromMergeTree ("):
			table = strings.TrimSuffix(strings.TrimPrefix(trimmed, "ReadFromMergeTree ("), ")")
			inPrimaryKey = false

		case trimmed == "PrimaryKey":
			inPrimaryKey = true

		// Any other section heading ends the primary key's block. MinMax and
		// Partition report granules too, and counting those would call a
		// table unpruned because its partition key matched everything, which
		// is not the same question.
		case inPrimaryKey && strings.HasPrefix(trimmed, "Granules: "):
			selected, total, ok := granules(trimmed)
			if ok && total > 0 && selected == total && table != "" {
				out = append(out, table)
			}
			inPrimaryKey = false

		case trimmed == "MinMax" || trimmed == "Partition" || trimmed == "Skip":
			inPrimaryKey = false
		}
	}
	sort.Strings(out)

	return out
}

// planLine strips a plan line down to what it says.
//
// Indentation carries no meaning here, and newer servers draw the plan as a
// tree, so a step arrives as `└──ReadFromMergeTree (otel.otel_traces)` where an
// older one printed it indented with spaces. The glyphs are decoration around
// the same step names, and reading past them is what lets one reader answer for
// both (spec 7.2).
func planLine(line string) string {
	return strings.TrimLeft(line, " \t│└├─")
}

func granules(line string) (selected, total uint64, ok bool) {
	value := strings.TrimPrefix(line, "Granules: ")
	if _, err := fmt.Sscanf(value, "%d/%d", &selected, &total); err != nil {
		return 0, 0, false
	}
	return selected, total, true
}
