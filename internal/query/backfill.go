package query

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
)

// DefaultBackfillRange is how far back a replay reaches when nobody said. A
// day covers a full diurnal cycle, which is the shortest range in which a rule
// that only fires during peak traffic shows up at all (spec 7.4).
const DefaultBackfillRange = 24 * time.Hour

// backfillTimeout bounds one rule's whole replay, every window included.
//
// Longer than sampleTimeout because this is the one check that runs more than
// one statement per rule, and still bounded: a replay runs from a continuous
// integration job somebody is waiting on, and each window carries the source's
// own execution time cap underneath this one.
//
// Spent as a cancel rather than a deadline, which is not a style choice. The
// driver turns a context deadline into max_execution_time on the wire, and the
// source's profile constrains that setting to a minute (spec 6.7), so a
// replay's own budget would arrive as a setting the server refuses rather than
// as a budget. Cancelling on a timer bounds the same thing and sends nothing.
const backfillTimeout = 5 * time.Minute

// topLabelSets is how many label sets a finding names. "Fires 4,100 times"
// does not tell anyone what to fix; "3,900 of them are one service" does, and
// the tail of that list never does (spec 7.4).
const topLabelSets = 3

// noLabels is what a rule returning nothing but a value is called in the
// report. Such a rule has one instance, and an empty label list would read as
// a bug in the report rather than as the rule's own shape.
const noLabels = "(no labels)"

// BackfillChecks is what a replay should do, resolved from policy and flags by
// the caller.
type BackfillChecks struct {
	// Range is how far back the replay reaches.
	Range time.Duration

	// Step is the gap between the evaluations it replays, which is the rule's
	// group interval unless an operator said otherwise. The caller resolves it,
	// so a zero here is a bug rather than a default.
	Step time.Duration

	// MaxAlerts is how many alert instances the range may hold before the
	// finding is worth making, zero when no ceiling is configured.
	MaxAlerts int

	// MaxRowsRead is what the whole replay may be predicted to read, which is
	// one window's prediction multiplied by every window in the range. Zero
	// when no ceiling is configured.
	MaxRowsRead uint64

	// GroupLabels are the rule's group's labels, which the replay needs for the
	// same reason an evaluation does: they are part of what each alert instance
	// is (spec 6.3.1).
	GroupLabels map[string]string
}

// LabelSetHits is one label set and how many windows it came back in.
type LabelSetHits struct {
	Labels string
	Hits   int
}

// Backfill is what replaying one rule over a past range answered.
//
// Two counts, because they differ wildly and reporting only the large one
// trains people to ignore the check: Hits is every window a row came back in,
// and Alerts is how many of those runs lasted longer than the rule's `for` and
// would have paged somebody (spec 7.4).
type Backfill struct {
	From, To time.Time
	Range    time.Duration

	// Step and Windows are what actually ran. AskedStep and AskedWindows are
	// what the operator asked for, which differ when the row ceiling raised the
	// step, and both are reported because a count over 96 windows out of 1,440
	// is a different number from a count over all of them.
	Step, AskedStep     time.Duration
	Windows, AskedCount int

	// Sampled means the step was raised to fit MaxRowsRead, so the replay
	// skipped windows and the counts below are a floor rather than the answer.
	Sampled bool

	// RowsPredicted is what the whole replay was predicted to read before any
	// of it ran, and PerWindow is one window's share of it.
	RowsPredicted, PerWindow uint64

	Alerts int
	Hits   int

	LongestStreak time.Duration

	// StillFiring means the longest streak was still over its threshold at the
	// last window, so it never resolved inside the range.
	StillFiring bool

	Top []LabelSetHits
}

// replayStrategy is how a rule's past is read back.
//
// One implementation, sequentialReplay below, and the seam is here because
// there is a second one this cannot become by configuration: rewriting the
// rule into a single bucketed GROUP BY over the whole range scans the data
// once instead of once per window, and it needs a rule whose predicate, value
// expression and label columns are separate fields rather than the free-form
// SQL rule.Rule carries. Until a rule is structured that way there is nothing
// to choose between, so nothing in a policy file chooses (spec 7.4, 11).
type replayStrategy interface {
	// evaluate returns the samples one evaluation at this instant would have
	// produced.
	evaluate(ctx context.Context, at time.Time) ([]alert.Sample, error)
}

// sequentialReplay re-renders the rule's own time bounds once per window and
// runs it, which is exactly what an evaluation does. The rule is replayable
// because rule/expr already refuses a query that does not bind {{ .From }} and
// {{ .To }}, so every rule that validates can be asked about a past window
// (spec 7.4).
//
// One query at a time, so a replay never exceeds a per-source concurrency cap
// however low it is set (spec 6.11), and every query carries the source's own
// execution time, memory and row limits because it goes through Run.
type sequentialReplay struct {
	q   *Querier
	r   rule.Rule
	who Attribution
}

func (s sequentialReplay) evaluate(ctx context.Context, at time.Time) ([]alert.Sample, error) {
	// A replay counts alerts, so it reads the samples and nothing else: what
	// the result looked like and what it cost belong to the evaluations a
	// running ruler compares against each other (spec 6.3.2).
	evaluation, err := s.q.Run(ctx, s.r, s.who, at)
	return evaluation.Samples, err
}

// Backfill replays a rule over a past range and reports how many alerts it
// would have produced.
//
// It reads rows, once per window, which is why it is a method of its own and
// behind a flag of its own: a connection is not consent, and neither is
// permission to sample once (spec 7.3).
//
// Nothing runs until the row ceiling has been checked against the whole
// replay: one window's EXPLAIN ESTIMATE multiplied by the number of windows
// the range holds. Over it, the step is raised until it fits and the answer is
// marked sampled, so a 24 hour range at a one minute step never quietly
// becomes 1,440 queries (spec 7.4).
//
// An error means the replay did not happen and is never a finding about the
// rule, the same line classifyExplain draws.
func (q *Querier) Backfill(
	ctx context.Context,
	r rule.Rule,
	who Attribution,
	c BackfillChecks,
	now time.Time,
) (*Backfill, []Finding, error) {
	if c.Range <= 0 || c.Step <= 0 {
		return nil, nil, fmt.Errorf("replaying %q: a range of %s in steps of %s is not a replay",
			r.Alert, c.Range, c.Step)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	budget := time.AfterFunc(backfillTimeout, cancel)
	defer budget.Stop()

	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(withLogComment(nil, who.Group, r.Alert)))

	// The newest window the replay will read, which is what an evaluation
	// running now reads. The prediction below is of that window rather than of
	// one ending now, because a replay asks about the past and a window the
	// rule's own bounds never reach would predict the cost of nothing.
	window := r.Window
	if window <= 0 {
		window = checkWindow
	}
	latest := now.Add(-q.src.EvaluationDelay)

	sql, err := renderBounds(r.Expr, latest.Add(-window), latest)
	if err != nil {
		return nil, nil, err
	}

	// What one window is predicted to read. A query the server would not
	// explain is already reported by Inspect, as a finding about the rule or as
	// a ruler that could not ask, so saying it again here would double the
	// output of every such rule.
	est, err := q.explainEstimate(ctx, sql)
	if finding, err := classifyEstimate(err); err != nil {
		return nil, nil, err
	} else if finding != nil {
		return nil, nil, nil
	}
	perWindow := estimatedRows(est)

	asked := replayWindows(c.Range, c.Step)
	step, sampled := stepWithinCeiling(c.Range, c.Step, perWindow, c.MaxRowsRead)
	windows := replayWindows(c.Range, step)

	instants := replayInstants(now, step, windows)

	// Resolved instances are kept for no time at all. Retention exists so a
	// failed resolve notification can be retried, and a replay sends nothing
	// (spec 6.5).
	state := alert.New(r, c.GroupLabels, q.src, 0)

	counts, err := replay(ctx, sequentialReplay{q: q, r: r, who: who}, state, instants, step)
	if err != nil {
		return nil, nil, err
	}

	out := &Backfill{
		From:          instants[0].Add(-q.src.EvaluationDelay).Add(-window),
		To:            latest,
		Range:         c.Range,
		Step:          step,
		AskedStep:     c.Step,
		Windows:       windows,
		AskedCount:    asked,
		Sampled:       sampled,
		PerWindow:     perWindow,
		RowsPredicted: saturatingMul(perWindow, windowCount(windows)),
		Alerts:        counts.Alerts,
		Hits:          counts.Hits,
		LongestStreak: counts.LongestStreak,
		StillFiring:   counts.StillFiring,
		Top:           counts.Top,
	}

	return out, q.backfillFindings(ctx, sql, *out, c), nil
}

// replayCounts is what a replay counted, before anything decides whether it is
// worth reporting.
type replayCounts struct {
	Alerts        int
	Hits          int
	LongestStreak time.Duration
	StillFiring   bool
	Top           []LabelSetHits
}

// replay runs every window in order and collapses the results through the
// alert state machine.
//
// Oldest window first, because that is the only order in which `for` means
// anything: an instance firing at one window is pending at the one before it.
// The collapse is alert.State itself rather than a count of windows over the
// threshold, so `for`, `keep_firing_for` and the resolve path all behave as
// they will in production instead of as a second implementation of them
// (spec 7.4). A gap between two firing windows is closed by keep_firing_for
// or it is a resolve, which is what those two already mean.
func replay(
	ctx context.Context,
	st replayStrategy,
	state *alert.State,
	instants []time.Time,
	step time.Duration,
) (replayCounts, error) {
	// An instance is identified by its fingerprint and when it became active,
	// so a condition that recovers and comes back is two alerts rather than
	// one, exactly as it would be two pages.
	type instanceKey struct {
		fingerprint uint64
		activeAt    time.Time
	}
	type firingSpan struct {
		firedAt, lastFiring time.Time
	}

	var out replayCounts
	spans := map[instanceKey]*firingSpan{}
	order := []instanceKey{}
	hits := map[string]int{}

	for _, at := range instants {
		samples, err := st.evaluate(ctx, at)
		if err != nil {
			return replayCounts{}, err
		}
		out.Hits += len(samples)
		for _, s := range samples {
			hits[labelSetKey(s.Labels)]++
		}

		// The annotation failures are not collected: a replay renders the same
		// templates an evaluation does, and rule/annotation-template already
		// reports one that will not render (spec 7.3).
		alerts, _, err := state.Eval(at, samples)
		if err != nil {
			return replayCounts{}, err
		}

		for _, a := range alerts {
			if a.Phase != alert.PhaseFiring {
				continue
			}
			k := instanceKey{fingerprint: a.Fingerprint, activeAt: a.ActiveAt}
			if span, ok := spans[k]; ok {
				span.lastFiring = at
				continue
			}
			spans[k] = &firingSpan{firedAt: a.FiredAt, lastFiring: at}
			order = append(order, k)
		}
	}

	out.Alerts = len(spans)
	last := instants[len(instants)-1]

	// Longest first, and ties go to whichever fired first, so the same replay
	// reports the same streak every time.
	for _, k := range order {
		span := spans[k]

		// One window's worth of time is what an evaluation covers, so an
		// instance firing at a single window fired for a step rather than for
		// no time at all.
		streak := span.lastFiring.Sub(span.firedAt) + step
		if streak <= out.LongestStreak {
			continue
		}
		out.LongestStreak = streak
		out.StillFiring = span.lastFiring.Equal(last)
	}

	out.Top = topHits(hits)
	return out, nil
}

// labelSetKey writes a result row's label set the way the report names it:
// sorted, so the same row reads the same way on every run.
//
// The columns the query returned, not the alert's full label set. A report
// naming what an operator has to change names what the query selected; the
// group, rule and source labels are the same on every row and would bury it.
func labelSetKey(labels map[string]string) string {
	if len(labels) == 0 {
		return noLabels
	}

	pairs := make([]string, 0, len(labels))
	for k, v := range labels {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// topHits orders the label sets by how many windows each came back in, and
// keeps the head of that list.
func topHits(hits map[string]int) []LabelSetHits {
	if len(hits) == 0 {
		return nil
	}

	out := make([]LabelSetHits, 0, len(hits))
	for labels, n := range hits {
		out = append(out, LabelSetHits{Labels: labels, Hits: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hits != out[j].Hits {
			return out[i].Hits > out[j].Hits
		}
		return out[i].Labels < out[j].Labels
	})

	if len(out) > topLabelSets {
		out = out[:topLabelSets]
	}
	return out
}

// replayWindows is how many evaluations a range holds at a given step.
//
// At least one, whatever the two are: a step longer than the range is an
// operator asking a coarser question rather than asking nothing, and a replay
// of no windows would report a rule that never fired.
func replayWindows(span, step time.Duration) int {
	if step <= 0 {
		return 1
	}
	n := int(span / step)
	if n < 1 {
		return 1
	}
	return n
}

// replayInstants are the evaluation instants a replay runs, oldest first.
//
// The last one is now, because that is the evaluation a live ruler would be
// running, and each earlier one is a step behind it. What each instant reads
// is the rule's own window ending there, shifted by the source's evaluation
// delay, because Run applies both exactly as it does in production.
func replayInstants(now time.Time, step time.Duration, windows int) []time.Time {
	out := make([]time.Time, 0, windows)
	for i := windows - 1; i >= 0; i-- {
		out = append(out, now.Add(-time.Duration(i)*step))
	}
	return out
}

// stepWithinCeiling raises the step until the whole replay fits the row
// ceiling, and reports whether it had to.
//
// The step gives rather than the range. A coarser replay over the range asked
// for still answers the question with less confidence; a shorter range answers
// a different question, and an operator who wanted a day is not helped by an
// hour reported as if it were one.
//
// The new step is a multiple of the old one, so the replay lands on the same
// cadence the rule evaluates at rather than on an arbitrary offset from it.
func stepWithinCeiling(span, step time.Duration, perWindow, ceiling uint64) (time.Duration, bool) {
	windows := windowCount(replayWindows(span, step))

	// Nothing to fit: no ceiling, or a query the server predicts reads no
	// tracked part at all, which is a constant or a count answered from
	// metadata.
	if ceiling == 0 || perWindow == 0 {
		return step, false
	}
	// Written as a division so a prediction large enough to overflow the
	// multiplication cannot read as a replay that fits.
	if perWindow <= ceiling/windows {
		return step, false
	}

	// How many windows the ceiling affords. One at the least: a single window
	// already over the ceiling is a rule whose cost is the cost of evaluating
	// it, which rule/cost is the check for.
	allowed := ceiling / perWindow
	if allowed < 1 {
		allowed = 1
	}

	multiple := (windows + allowed - 1) / allowed

	// A multiple too large to be a duration is a ceiling one window already
	// exceeds by an enormous margin, and one window over the whole range is
	// what that means. Guarded rather than assumed, so the conversion is
	// provably safe to a reader and to the linter.
	if multiple > uint64(math.MaxInt32) {
		return span, true
	}
	return step * time.Duration(multiple), true
}

// windowCount is a window count as the unsigned number the row arithmetic
// multiplies. Non-negative by construction, because replayWindows never
// answers less than one; the guard is here for the same reason the one above
// is.
func windowCount(n int) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

// saturatingMul multiplies without wrapping, so a prediction too large to
// represent is reported as the largest number rather than as a small one.
func saturatingMul(a, b uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > ^uint64(0)/b {
		return ^uint64(0)
	}
	return a * b
}

// backfillFindings decides what a replay is worth saying.
//
// Only a ceiling exceeded, or a caveat about the answer itself. A finding on
// every rule would read as a report, and it would mean an operator who raised
// this check to an error blocked every pull request including the rules that
// are fine (spec 7.6).
func (q *Querier) backfillFindings(ctx context.Context, sql string, b Backfill, c BackfillChecks) []Finding {
	caveats := q.backfillCaveats(ctx, sql, b)

	over := c.MaxAlerts > 0 && b.Alerts > c.MaxAlerts
	if !over && !b.Sampled && len(caveats) == 0 {
		return nil
	}

	detail := describeBackfill(b)
	if over {
		detail += fmt.Sprintf(", against a ceiling of %d", c.MaxAlerts)
	}
	if b.Sampled {
		detail += fmt.Sprintf("; the step was raised from %s to %s so the replay's predicted %d rows "+
			"fit the %d row ceiling, so this is %s of the %d the range holds and the counts are a floor",
			b.AskedStep, b.Step, saturatingMul(b.PerWindow, windowCount(b.AskedCount)), c.MaxRowsRead,
			plural(b.Windows, "window"), b.AskedCount)
	}
	for _, caveat := range caveats {
		detail += "; " + caveat
	}

	return []Finding{{Check: lint.CheckRuleAlertCount, Detail: detail}}
}

// describeBackfill writes the two numbers, the streak and the label sets
// behind them, which is the whole report in one line.
func describeBackfill(b Backfill) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "would fire %s (%s) over %s replayed in %s of %s",
		plural(b.Alerts, "alert instance"), plural(b.Hits, "evaluation hit"),
		b.Range, plural(b.Windows, "window"), b.Step)

	if b.LongestStreak > 0 {
		fmt.Fprintf(&sb, "; longest firing streak %s", b.LongestStreak)
		if b.StillFiring {
			sb.WriteString(", still firing at the end of the range")
		}
	}
	if len(b.Top) > 0 {
		sb.WriteString("; top label sets: ")
		for i, t := range b.Top {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "%s %s", t.Labels, plural(t.Hits, "hit"))
		}
	}
	return sb.String()
}

// backfillCaveats are the two ways a replay can be answering about less data
// than it asked for. Both are reported rather than hidden, because a count
// over a range the data never covered reads exactly like a rule that does not
// fire (spec 7.4).
//
// Neither is a finding of its own: a count with a caveat is one answer with a
// qualification, not two problems.
func (q *Querier) backfillCaveats(ctx context.Context, sql string, b Backfill) []string {
	var out []string

	// Which table the two questions below are asked of. Both are about stored
	// parts rather than about the rows a rule reads, and a Distributed table has
	// no parts and no TTL in its engine clause, so on a sharded cluster they are
	// asked of the local table behind it (spec 6.9).
	st := q.storageTable(ctx)
	if st.Unanswerable != "" {
		return []string{unresolvedCaveat(st)}
	}

	if ttl, ok := q.tableTTL(ctx, st.Local); ok && b.Range > ttl {
		out = append(out, ttlCaveat(ttl, st))
	}

	for _, added := range q.columnsAdded(ctx, sql, b.From, st.Local) {
		out = append(out, columnCaveat(added, st))
	}
	return out
}

// tableTTL reads how long one table keeps a row.
//
// From system.tables, which the user contract in spec 6.7.2 leaves readable,
// unlike the parts metadata columnsAdded asks for. An unreadable or
// unrecognised TTL answers nothing rather than guessing: a caveat that names a
// retention the table does not have is worse than no caveat.
//
// The table is passed in rather than taken from the source, because a TTL is a
// property of stored parts: on a sharded cluster the source names a Distributed
// table whose engine clause has no TTL at all, and the answer lives on the local
// table behind it (spec 6.9).
func (q *Querier) tableTTL(ctx context.Context, ref tableRef) (time.Duration, bool) {
	var engine string
	err := q.conn.QueryRow(ctx,
		"SELECT engine_full FROM system.tables WHERE database = ? AND name = ?",
		ref.Database, ref.Table).Scan(&engine)
	if err != nil {
		return 0, false
	}
	return parseTTL(engine)
}

// ttlUnits are the interval functions a TTL expression is written with, and
// how long each one is. A month and a year are the calendar's business and are
// approximated here, because the answer is used to say a range is longer than
// a retention rather than to compute a deletion date.
var ttlUnits = map[string]time.Duration{
	"toIntervalSecond":  time.Second,
	"toIntervalMinute":  time.Minute,
	"toIntervalHour":    time.Hour,
	"toIntervalDay":     24 * time.Hour,
	"toIntervalWeek":    7 * 24 * time.Hour,
	"toIntervalMonth":   30 * 24 * time.Hour,
	"toIntervalQuarter": 91 * 24 * time.Hour,
	"toIntervalYear":    365 * 24 * time.Hour,
}

// parseTTL reads a retention out of the engine clause, and reports whether it
// recognised one.
//
// The first TTL rule only, and the intervals inside it added up:
// `TTL ts + toIntervalDay(1) + toIntervalHour(6)` keeps a row for thirty
// hours. A later rule in the same clause moves parts between volumes rather
// than deleting them, so it says nothing about what the table still holds.
//
// Anything it does not recognise, including a TTL computed by a function or
// read from a setting, reports nothing. This exists to qualify a count, and a
// wrong qualification is worse than none.
func parseTTL(engineFull string) (time.Duration, bool) {
	_, after, found := strings.Cut(engineFull, " TTL ")
	if !found {
		return 0, false
	}
	clause, _, _ := strings.Cut(after, " SETTINGS ")
	first, _, _ := strings.Cut(clause, ",")

	var total time.Duration
	for name, unit := range ttlUnits {
		for _, n := range intervalArgs(first, name) {
			total += time.Duration(n) * unit
		}
	}
	if total <= 0 {
		return 0, false
	}
	return total, true
}

// intervalArgs returns the whole number argument of every call to one interval
// function in an expression.
func intervalArgs(expr, name string) []int {
	var out []int

	rest := expr
	for {
		_, after, found := strings.Cut(rest, name+"(")
		if !found {
			return out
		}
		arg, remainder, closed := strings.Cut(after, ")")
		rest = remainder
		if !closed {
			return out
		}
		if n, err := strconv.Atoi(strings.TrimSpace(arg)); err == nil && n > 0 {
			out = append(out, n)
		}
	}
}

// columnAddition is one column of the source's table that the data says
// appeared later than the table itself did.
type columnAddition struct {
	Column   string
	Earliest time.Time
}

// columnsAdded reports the columns the rule reads that arrived inside the
// range.
//
// It asks the parts metadata rather than the data: a column added by
// `ALTER TABLE ADD COLUMN` is absent from every part written before the alter,
// so the oldest part holding a column is when that column started existing.
//
// Reading it needs `SELECT ON system.parts_columns`, which the user contract
// in spec 6.7.2 deliberately does not grant, so under that contract this
// answers nothing and the replay reports the rest. It answers on a validation
// user with wider grants, which is the setup spec 10.3 describes, and that is
// the one where a schema change is worth catching before the rules reach the
// cluster that evaluates them.
func (q *Querier) columnsAdded(ctx context.Context, sql string, from time.Time, ref tableRef) []columnAddition {
	earliest, err := q.columnHistory(ctx, ref)
	if err != nil {
		return nil
	}

	// Which columns the rule reads, which is why the tree is parsed again: a
	// caveat naming a column the rule never mentions sends an author to read a
	// schema change that cannot affect them.
	root, err := q.explainAST(ctx, sql)
	if err != nil {
		return nil
	}
	return columnsAddedInside(earliest, identifiers(root), from)
}

// columnHistory is the oldest part each of one table's columns appears in.
//
// The table is passed in for the reason tableTTL's is: parts belong to a local
// table, and a Distributed table has none of its own (spec 6.9).
func (q *Querier) columnHistory(ctx context.Context, ref tableRef) (map[string]time.Time, error) {
	rows, err := q.conn.Query(ctx, `
SELECT column, min(min_time) AS earliest
FROM system.parts_columns
WHERE database = ? AND table = ? AND active
GROUP BY column`, ref.Database, ref.Table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]time.Time{}
	for rows.Next() {
		var (
			column   string
			earliest time.Time
		)
		if err := rows.Scan(&column, &earliest); err != nil {
			return nil, err
		}
		out[column] = earliest
	}
	return out, rows.Err()
}

// columnsAddedInside compares each column the query reads against the oldest
// data the table holds at all.
//
// Against the table rather than against the range alone, because a table that
// only started receiving data inside the range has every column appear at
// once, and that is a statement about the table which the TTL caveat and the
// count itself already make. What is worth reporting is one column arriving
// later than its neighbours, which is a schema change.
func columnsAddedInside(earliest map[string]time.Time, read []string, from time.Time) []columnAddition {
	var tableStart time.Time
	for _, at := range earliest {
		if at.IsZero() {
			continue
		}
		if tableStart.IsZero() || at.Before(tableStart) {
			tableStart = at
		}
	}
	if tableStart.IsZero() {
		return nil
	}

	var out []columnAddition
	seen := map[string]bool{}

	for _, identifier := range read {
		name, at := columnHistoryOf(identifier, earliest)
		if at.IsZero() || seen[name] {
			continue
		}
		if !at.After(from) || !at.After(tableStart) {
			continue
		}
		seen[name] = true
		out = append(out, columnAddition{Column: name, Earliest: at})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Column < out[j].Column })
	return out
}

// columnHistoryOf matches an identifier to a column, full name first and then
// the part after the last dot, for the reason columnOf does the same.
func columnHistoryOf(identifier string, earliest map[string]time.Time) (string, time.Time) {
	if at, ok := earliest[identifier]; ok {
		return identifier, at
	}
	bare := bareColumn(identifier)
	if at, ok := earliest[bare]; ok {
		return bare, at
	}
	return identifier, time.Time{}
}
