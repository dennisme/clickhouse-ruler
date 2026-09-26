package query

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// Why a key that reads as a map lookup still cannot be sampled.
const (
	skipNotAMap    = "not a Map column, so it holds no keys"
	skipOtherTable = "not a column of this source's table, so it belongs to another table in the query"
	skipNumericKey = "a numeric key, which this check does not probe for"
)

// minSampleWindow is the shortest span a sample reads.
//
// A rule alerting on the last thirty seconds would otherwise be checked against
// thirty seconds of data, where an attribute written once a minute is absent and
// the rule is fine. The floor trades a slightly wider read for not being wrong
// about the rules hardest to be sure about (spec 7.3).
const minSampleWindow = time.Hour

// SampleChecks is what a sample should do, resolved from policy by the caller.
type SampleChecks struct {
	// MaxRows bounds what one sample may read, zero when no ceiling is
	// configured.
	MaxRows int

	// RequireRows turns a window with nothing in it into a finding. Off by
	// default: a rule can land before the data does, and the operator who
	// would rather that block says so (spec 7.3).
	RequireRows bool
}

// sampleTimeout bounds one rule's sample. Longer than inspectTimeout because
// this one reads rows, and shorter than an evaluation because a check nobody is
// waiting on must not hold a continuous integration run open.
const sampleTimeout = 30 * time.Second

// sampleResult is what one sample answered: how many rows the window held, how
// many of them carried each key in order, and the oldest timestamp among them.
type sampleResult struct {
	Rows     uint64
	Present  []uint64
	Earliest time.Time
}

// Sample confirms that the map keys a rule reads are present in recent data.
//
// The only check that reads rows, which is why it is a method of its own rather
// than part of Inspect: that one is built on reading none, and a caller has to
// ask for this (spec 7.3).
//
// It parses the rule again rather than taking Inspect's tree. One more
// `EXPLAIN AST` is a parse and a network hop, and it keeps the two entry points
// independent: sampling can be turned on without threading a tree through
// everything that does not need one.
//
// An error means the sample could not be taken and is never a finding about the
// rule, the same line classifyExplain draws.
func (q *Querier) Sample(ctx context.Context, r rule.Rule, who Attribution, c SampleChecks, now time.Time) ([]Finding, error) {
	ctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()

	// The parse and the DESCRIBE below carry the comment as well as the
	// sample does. They are cheap, but they are still this rule's queries,
	// and an operator picking its work out of system.query_log wants all of
	// it (spec 8.5).
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(withLogComment(nil, who.Group, r.Alert)))

	sql, err := renderForCheck(r.Expr)
	if err != nil {
		return nil, err
	}

	root, err := q.explainAST(ctx, sql)
	if err != nil {
		// A query the server would not explain is already reported by Inspect,
		// as a finding about the rule or as a ruler that could not ask. Saying
		// it twice would double every such rule's output.
		if _, err := classifyExplain(err); err != nil {
			return nil, err
		}
		return nil, nil
	}

	keys, skipped := mapKeys(root)
	if len(keys) == 0 && len(skipped) == 0 {
		return nil, nil
	}

	cols, err := q.describeTable(ctx, q.src.Database, q.src.Table)
	if err != nil {
		return nil, fmt.Errorf("reading the columns of %s.%s: %w", q.src.Database, q.src.Table, err)
	}

	keys, unusable := resolveKeys(keys, cols)
	skipped = append(skipped, unusable...)

	from, to := sampleWindow(q.src, r, now)

	res, err := q.sample(ctx, keys, from, to, c, who.Group, r.Alert)
	if err != nil {
		return nil, err
	}

	return sampleFindings(res, keys, skipped, from, to, c), nil
}

// sample runs the one statement, or none at all when every key a rule reads
// turned out to be unanswerable.
func (q *Querier) sample(
	ctx context.Context,
	keys []MapKey,
	from, to time.Time,
	c SampleChecks,
	group, rule string,
) (sampleResult, error) {
	if len(keys) == 0 {
		return sampleResult{}, nil
	}

	table := quoteIdent(q.src.Database) + "." + quoteIdent(q.src.Table)
	sql, args := sampleQuery(table, q.src.TimestampColumn, keys)

	args = append(args, from, to)

	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(
		withLogComment(sampleSettings(c.MaxRows), group, rule)))

	var (
		res      sampleResult
		earliest time.Time
	)
	res.Present = make([]uint64, len(keys))

	dest := make([]any, 0, len(keys)+2)
	dest = append(dest, &res.Rows, &earliest)
	for i := range res.Present {
		dest = append(dest, &res.Present[i])
	}

	if err := q.conn.QueryRow(ctx, sql, args...).Scan(dest...); err != nil {
		return sampleResult{}, fmt.Errorf("sampling for map keys: %w", err)
	}
	res.Earliest = earliest

	return res, nil
}

// sampleFindings turns a sample into what to report.
//
// Fail open on an empty window. Nothing was verified either way, and a rule
// deployed before its service starts sending is ordinary rather than wrong, so
// saying anything here trains people to ignore the check on the rules that are
// fine (spec 7.3). An operator who would rather that block sets RequireRows.
//
// A skip is a fact about the query rather than about the data, so it is reported
// whatever the window held.
func sampleFindings(
	res sampleResult,
	keys []MapKey,
	skipped []MapKeySkip,
	from, to time.Time,
	c SampleChecks,
) []Finding {
	var out []Finding

	for _, s := range skipped {
		out = append(out, Finding{
			Check: lint.CheckRuleAttributeKey,
			Detail: fmt.Sprintf("a key read from %s was not checked: %s",
				s.Column, s.Reason),
		})
	}

	if res.Rows == 0 {
		if c.RequireRows && len(keys) > 0 {
			out = append(out, Finding{
				Check: lint.CheckRuleAttributeKey,
				Detail: fmt.Sprintf(
					"no rows between %s and %s, so none of the %s this rule reads could be verified",
					stamp(from), stamp(to), plural(len(keys), "key")),
			})
		}
		return out
	}

	for i, k := range keys {
		if i < len(res.Present) && res.Present[i] > 0 {
			continue
		}
		out = append(out, Finding{
			Check: lint.CheckRuleAttributeKey,
			Detail: fmt.Sprintf("%s[%q] is in none of the %d rows sampled between %s and %s, "+
				"so the rule reads a key nothing writes%s",
				k.Column, k.Key, res.Rows, stamp(from), stamp(to), shortfall(res.Earliest, from)),
		})
	}

	return out
}

// shortfall says how far back the sampled rows actually went, when that is less
// than the window asked for. A key reported absent over a span the data never
// covered is a finding about retention wearing the clothes of a finding about
// the rule.
func shortfall(earliest, from time.Time) string {
	if earliest.IsZero() || !earliest.After(from) {
		return ""
	}

	return fmt.Sprintf(" (the oldest row sampled is from %s, so the data covers less than the window)",
		stamp(earliest))
}

func stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

// resolveKeys separates the keys worth asking about from the ones that cannot
// be asked, with a reason for each.
//
// A key is answerable when its column is a Map on the source's own table. That
// is the table the window and the timestamp column describe, and by spec 7.6 a
// source names exactly one, so a key read from a joined table is out of scope
// rather than unanswerable in principle. Two columns of the same name on two
// joined tables would be read as the source's own, which is the cost of not
// resolving each column against every table the query names (spec 7.3).
//
// This is also where a numeric subscript is settled. On a column that is not a
// Map it was an array index all along, and nothing about it could go missing, so
// it is not reported either. On a Map it is a real key, and it is reported as
// unprobed rather than probed: `has()` compares against the map's own key type,
// and building that comparison is work with no OTel attribute behind it.
func resolveKeys(keys []MapKey, table []Column) ([]MapKey, []MapKeySkip) {
	types := make(map[string]string, len(table))
	for _, c := range table {
		types[c.Name] = c.Type
	}

	var (
		out     []MapKey
		skipped []MapKeySkip
	)
	for _, k := range keys {
		name, typ, onTable := columnOf(k.Column, types)
		k.Column = name
		isMap := onTable && isMapType(typ)

		switch {
		case k.Numeric && !isMap:
			// An array index, which was never a key.
		case k.Numeric:
			skipped = append(skipped, MapKeySkip{Column: k.Column, Reason: skipNumericKey})
		case !onTable:
			skipped = append(skipped, MapKeySkip{Column: k.Column, Reason: skipOtherTable})
		case !isMap:
			skipped = append(skipped, MapKeySkip{Column: k.Column, Reason: skipNotAMap})
		default:
			out = append(out, k)
		}
	}

	return out, skipped
}

// columnOf matches an identifier to a column of the table, and says which name
// matched so the sample quotes the one the table actually has.
//
// The full identifier first, then the part after the last dot. A dot means two
// different things: `t.SpanAttributes` is a column on a table alias, while
// `Events.Attributes` is the whole name of a column a Nested block flattened
// into. Trying the full name first is what keeps the second one from being
// looked up as `Attributes` and reported as belonging to another table.
func columnOf(identifier string, types map[string]string) (name, typ string, found bool) {
	if typ, ok := types[identifier]; ok {
		return identifier, typ, true
	}

	bare := bareColumn(identifier)
	if typ, ok := types[bare]; ok {
		return bare, typ, true
	}

	return identifier, "", false
}

// isMapType reports whether a column holds a map. The type arrives as
// ClickHouse writes it, so a key type of its own is nested inside:
// `Map(LowCardinality(String), String)`.
func isMapType(typ string) bool {
	return strings.HasPrefix(strings.TrimSpace(typ), "Map(")
}

// sampleWindow is the span a sample reads: the rule's own window, floored, and
// shifted back by the source's evaluation delay so the sample reads the data an
// evaluation would rather than data that has not landed yet.
func sampleWindow(src source.Source, r rule.Rule, now time.Time) (from, to time.Time) {
	span := r.Window
	if span < minSampleWindow {
		span = minSampleWindow
	}

	to = now.Add(-src.EvaluationDelay)

	return to.Add(-span), to
}

// sampleQuery asks, in one statement, how many rows in the window carry each
// key, how many rows there were at all, and how far back they went.
//
// One round trip for every key a rule reads. The row count is what separates a
// missing key from a window with nothing in it, which spec 7.3 answers by
// reporting nothing at all. The earliest timestamp is what separates a key that
// is missing from a window the data never filled: a table that TTLs, one that
// was created this morning and one whose retention is shorter than the rule's
// window all answer the same way, and all three make a clean-looking absence
// cover less ground than it claims.
//
// Keys are bound as parameters rather than written into the statement. A key is
// author-supplied text and is the one thing here that could otherwise change the
// shape of the query. Column and table names cannot be parameters, so they are
// quoted instead, and both have already been resolved against what the server
// says the table has.
func sampleQuery(table, timestampColumn string, keys []MapKey) (string, []any) {
	var (
		sql  strings.Builder
		args []any
	)

	sql.WriteString("SELECT count() AS rows, min(")
	sql.WriteString(quoteIdent(timestampColumn))
	sql.WriteString(") AS earliest")
	for i, k := range keys {
		sql.WriteString(", countIf(has(")
		sql.WriteString(quoteIdent(k.Column))
		sql.WriteString(", ?)) AS ")
		sql.WriteString(keyColumn(i))
		args = append(args, k.Key)
	}

	sql.WriteString(" FROM ")
	sql.WriteString(table)
	sql.WriteString(" WHERE ")
	sql.WriteString(quoteIdent(timestampColumn))
	sql.WriteString(" >= ? AND ")
	sql.WriteString(quoteIdent(timestampColumn))
	sql.WriteString(" < ?")

	return sql.String(), args
}

// keyColumn names the count for one key. Position rather than the key itself:
// a key is arbitrary text and a column alias is not.
func keyColumn(i int) string {
	return "key_" + strconv.Itoa(i)
}

// sampleSettings bound what the sample may read.
//
// `break` rather than the default `throw`, because a sample that overruns its
// ceiling should answer for the rows it did read. Throwing would report a rule
// nobody could check as a ruler that could not ask, which is the one answer
// that helps no one.
//
// No ceiling means nothing sent. A cap of zero is how ClickHouse spells
// unlimited, so sending it would look deliberate and mean the opposite.
func sampleSettings(maxRows int) clickhouse.Settings {
	if maxRows <= 0 {
		return clickhouse.Settings{}
	}

	return clickhouse.Settings{
		"max_rows_to_read":   maxRows,
		"read_overflow_mode": "break",
	}
}
