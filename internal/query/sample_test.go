package query

import (
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

var tracesColumns = []Column{
	{Name: "Timestamp", Type: "DateTime64(9)"},
	{Name: "ServiceName", Type: "LowCardinality(String)"},
	{Name: "ResourceAttributes", Type: "Map(LowCardinality(String), String)"},
	{Name: "SpanAttributes", Type: "Map(LowCardinality(String), String)"},
	{Name: "Durations", Type: "Array(UInt64)"},
}

func TestResolveKeysKeepsMapColumnsOnTheSourceTable(t *testing.T) {
	keys := []MapKey{
		{Column: "SpanAttributes", Key: "tenant"},
		{Column: "ResourceAttributes", Key: "service.name"},
	}

	got, skipped := resolveKeys(keys, tracesColumns)

	if len(got) != 2 {
		t.Fatalf("resolveKeys = %v, want both map keys kept", got)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped %v, want none", skipped)
	}
}

// A column that is not a Map has no keys, so there is nothing to sample for.
// Probing it anyway would ask ClickHouse a question it cannot answer.
func TestResolveKeysSkipsAColumnThatIsNotAMap(t *testing.T) {
	got, skipped := resolveKeys([]MapKey{{Column: "ServiceName", Key: "checkout"}}, tracesColumns)

	if len(got) != 0 {
		t.Errorf("resolveKeys = %v, want nothing kept", got)
	}
	if len(skipped) != 1 || skipped[0].Reason != skipNotAMap {
		t.Fatalf("skipped = %v, want ServiceName skipped as %q", skipped, skipNotAMap)
	}
}

// A key read from a joined table is out of scope: the window and the timestamp
// column only answer for the source's own table (spec 7.3).
func TestResolveKeysSkipsAColumnTheSourceTableDoesNotHave(t *testing.T) {
	got, skipped := resolveKeys([]MapKey{{Column: "LogAttributes", Key: "payment_id"}}, tracesColumns)

	if len(got) != 0 {
		t.Errorf("resolveKeys = %v, want nothing kept", got)
	}
	if len(skipped) != 1 || skipped[0].Reason != skipOtherTable {
		t.Fatalf("skipped = %v, want LogAttributes skipped as %q", skipped, skipOtherTable)
	}
}

// An array index is not a map lookup, so it is neither probed nor reported.
// The author wrote nothing that could go missing.
func TestResolveKeysIgnoresAnArrayIndex(t *testing.T) {
	got, skipped := resolveKeys([]MapKey{{Column: "Durations", Key: "1", Numeric: true}}, tracesColumns)

	if len(got) != 0 {
		t.Errorf("resolveKeys = %v, want nothing probed", got)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want nothing reported: an array index is not a key", skipped)
	}
}

// A map can be keyed by a number, so this one is a real lookup, and saying
// nothing about it would be claiming it was checked. Reported instead, because
// what a numeric key needs is a typed comparison this check does not build.
func TestResolveKeysReportsANumericMapKey(t *testing.T) {
	table := []Column{{Name: "Counters", Type: "Map(UInt64, UInt64)"}}

	got, skipped := resolveKeys([]MapKey{{Column: "Counters", Key: "7", Numeric: true}}, table)

	if len(got) != 0 {
		t.Errorf("resolveKeys = %v, want nothing probed", got)
	}
	if len(skipped) != 1 || skipped[0].Reason != skipNumericKey {
		t.Fatalf("skipped = %v, want Counters as %q", skipped, skipNumericKey)
	}
}

// Every skip is reported. A check that examined nothing must not read the same
// as a check that found nothing wrong (spec 7.3).
func TestResolveKeysReportsEverySkip(t *testing.T) {
	keys := []MapKey{
		{Column: "SpanAttributes", Key: "tenant"},
		{Column: "ServiceName", Key: "checkout"},
		{Column: "LogAttributes", Key: "payment_id"},
	}

	got, skipped := resolveKeys(keys, tracesColumns)

	if len(got) != 1 {
		t.Errorf("resolveKeys = %v, want the one map key on this table", got)
	}
	if len(skipped) != 2 {
		t.Errorf("skipped = %v, want both unusable columns reported", skipped)
	}
}

// The window is the rule's own, so the sample reads the data an evaluation
// would, shifted back by the source's evaluation delay for the same reason the
// evaluation is.
func TestSampleWindowFollowsTheRule(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	src := source.Source{EvaluationDelay: time.Minute}
	r := rule.Rule{Window: time.Hour}

	from, to := sampleWindow(src, r, now)

	if want := now.Add(-time.Minute); !to.Equal(want) {
		t.Errorf("to = %s, want %s", to, want)
	}
	if want := now.Add(-time.Minute - time.Hour); !from.Equal(want) {
		t.Errorf("from = %s, want %s", from, want)
	}
}

// A rule asking about thirty seconds would sample thirty seconds and call a
// live key missing. The floor is what stops the check being wrong about the
// rules that are hardest to be sure about.
func TestSampleWindowIsFloored(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	r := rule.Rule{Window: 30 * time.Second}

	from, to := sampleWindow(source.Source{}, r, now)

	if got := to.Sub(from); got != minSampleWindow {
		t.Errorf("window = %s, want the %s floor", got, minSampleWindow)
	}
}

// A rule with no window of its own still gets sampled over something.
func TestSampleWindowWithNoRuleWindow(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

	from, to := sampleWindow(source.Source{}, rule.Rule{}, now)

	if got := to.Sub(from); got != minSampleWindow {
		t.Errorf("window = %s, want the %s floor", got, minSampleWindow)
	}
}

// One statement for every key, not one per key. A rule with a dozen subscripts
// is one round trip.
func TestSampleQueryAsksForEveryKeyAtOnce(t *testing.T) {
	keys := []MapKey{
		{Column: "SpanAttributes", Key: "tenant"},
		{Column: "ResourceAttributes", Key: "quote'd"},
	}

	sql, args := sampleQuery("otel.otel_traces", "Timestamp", keys)

	for _, want := range []string{
		"countIf(has(`SpanAttributes`, ?))",
		"countIf(has(`ResourceAttributes`, ?))",
		"count() AS rows",
		"min(`Timestamp`) AS earliest",
		"FROM otel.otel_traces",
		"`Timestamp` >= ?",
		"`Timestamp` < ?",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("sample query is missing %q:\n%s", want, sql)
		}
	}

	// Keys travel as parameters, never as text in the statement: a key is
	// author-supplied and the one place a quote in it could change the query.
	if len(args) != 2 || args[0] != "tenant" || args[1] != "quote'd" {
		t.Errorf("args = %v, want both keys bound as parameters", args)
	}
}

// The ceiling bounds what the sample reads, and it has to stop rather than
// fail: an overrun that throws is a rule nobody could check, reported as a
// ruler that could not ask, where stopping short still answers for the rows it
// did read.
func TestSampleSettingsStopAtTheCeiling(t *testing.T) {
	got := sampleSettings(1000)

	if got["max_rows_to_read"] != 1000 {
		t.Errorf("max_rows_to_read = %v, want 1000", got["max_rows_to_read"])
	}
	if got["read_overflow_mode"] != "break" {
		t.Errorf("read_overflow_mode = %v, want break", got["read_overflow_mode"])
	}
}

// No ceiling configured means no cap sent, rather than a cap of zero, which
// ClickHouse reads as unlimited and would only look deliberate by accident.
func TestSampleSettingsWithNoCeiling(t *testing.T) {
	if got := sampleSettings(0); len(got) != 0 {
		t.Errorf("sampleSettings(0) = %v, want nothing sent", got)
	}
}

// A key no row carries is the finding the whole check exists for.
func TestSampleFindingsReportsAnAbsentKey(t *testing.T) {
	from := time.Date(2026, 9, 25, 11, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	keys := []MapKey{
		{Column: "SpanAttributes", Key: "tenant"},
		{Column: "SpanAttributes", Key: "payment_id"},
	}
	res := sampleResult{Rows: 4127, Present: []uint64{4127, 0}, Earliest: from}

	got := sampleFindings(res, keys, nil, from, to, SampleChecks{})

	if len(got) != 1 {
		t.Fatalf("findings = %v, want the absent key alone", got)
	}
	if got[0].Check != lint.CheckRuleAttributeKey {
		t.Errorf("check = %q, want %q", got[0].Check, lint.CheckRuleAttributeKey)
	}
	for _, want := range []string{"SpanAttributes", "payment_id", "4127"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("detail %q is missing %q", got[0].Detail, want)
		}
	}
}

// Fail open. A rule can land before the data does, and a cluster that has not
// started receiving a service's telemetry is the normal case for a new rule
// rather than a mistake (spec 7.3).
func TestSampleFindingsSaysNothingWhenTheWindowIsEmpty(t *testing.T) {
	from := time.Date(2026, 9, 25, 11, 0, 0, 0, time.UTC)
	keys := []MapKey{{Column: "SpanAttributes", Key: "tenant"}}
	res := sampleResult{Rows: 0, Present: []uint64{0}}

	if got := sampleFindings(res, keys, nil, from, from.Add(time.Hour), SampleChecks{}); len(got) != 0 {
		t.Errorf("findings = %v, want none: an empty window verifies nothing either way", got)
	}
}

// The opt-in hard fail, for an operator who would rather an unverifiable rule
// block than ship unchecked.
func TestSampleFindingsReportsAnEmptyWindowWhenRowsAreRequired(t *testing.T) {
	from := time.Date(2026, 9, 25, 11, 0, 0, 0, time.UTC)
	keys := []MapKey{{Column: "SpanAttributes", Key: "tenant"}}
	res := sampleResult{Rows: 0, Present: []uint64{0}}

	got := sampleFindings(res, keys, nil, from, from.Add(time.Hour), SampleChecks{RequireRows: true})

	if len(got) != 1 {
		t.Fatalf("findings = %v, want one", got)
	}
	if !strings.Contains(got[0].Detail, "no rows") {
		t.Errorf("detail = %q, want it to say the window held no rows", got[0].Detail)
	}
}

// The sample can cover less than it asked for, because the table's TTL, its
// retention or its age all cut the window short. Reporting a key as absent over
// a span the data never covered is the finding being wrong (spec 7.3).
func TestSampleFindingsSaysWhenTheDataCoversLessThanTheWindow(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	keys := []MapKey{{Column: "SpanAttributes", Key: "tenant"}}
	res := sampleResult{Rows: 10, Present: []uint64{0}, Earliest: to.Add(-time.Hour)}

	got := sampleFindings(res, keys, nil, from, to, SampleChecks{})

	if len(got) != 1 {
		t.Fatalf("findings = %v, want the absent key", got)
	}
	if !strings.Contains(got[0].Detail, "oldest row") {
		t.Errorf("detail = %q, want it to say how far back the data actually went", got[0].Detail)
	}
}

// A lookup that went unchecked is reported on its own, whatever the sample
// found, because silence about it reads as a key that passed (spec 7.3).
func TestSampleFindingsReportsSkips(t *testing.T) {
	from := time.Date(2026, 9, 25, 11, 0, 0, 0, time.UTC)
	skips := []MapKeySkip{{Column: "LogAttributes", Reason: skipDynamicKey}}
	res := sampleResult{Rows: 10}

	got := sampleFindings(res, nil, skips, from, from.Add(time.Hour), SampleChecks{})

	if len(got) != 1 {
		t.Fatalf("findings = %v, want the skip reported", got)
	}
	if !strings.Contains(got[0].Detail, "LogAttributes") || !strings.Contains(got[0].Detail, skipDynamicKey) {
		t.Errorf("detail = %q, want the column and the reason", got[0].Detail)
	}
}

// A skip is a fact about the query, not about the data, so an empty window does
// not silence it.
func TestSampleFindingsReportsSkipsWhenTheWindowIsEmpty(t *testing.T) {
	from := time.Date(2026, 9, 25, 11, 0, 0, 0, time.UTC)
	skips := []MapKeySkip{{Column: "LogAttributes", Reason: skipOtherTable}}

	got := sampleFindings(sampleResult{}, nil, skips, from, from.Add(time.Hour), SampleChecks{})

	if len(got) != 1 {
		t.Fatalf("findings = %v, want the skip reported", got)
	}
}

// A Nested block flattens into columns whose names contain a dot, so the whole
// identifier is the column name. Reading the part after the dot would look up a
// column that does not exist and report the key as another table's.
func TestResolveKeysResolvesANestedColumn(t *testing.T) {
	table := []Column{
		{Name: "Events.Attributes", Type: "Array(Map(LowCardinality(String), String))"},
		{Name: "Attributes", Type: "Map(LowCardinality(String), String)"},
	}

	got, skipped := resolveKeys([]MapKey{{Column: "Events.Attributes", Key: "code", Numeric: true}}, table)

	if len(got) != 0 || len(skipped) != 0 {
		t.Fatalf("resolveKeys = %v, %v: an index into an Array column is not a key", got, skipped)
	}
}

// The qualifier on a table alias is not part of the column name, so it is
// dropped once the table says there is no such column.
func TestResolveKeysResolvesThroughATableAlias(t *testing.T) {
	got, skipped := resolveKeys([]MapKey{{Column: "t.SpanAttributes", Key: "tenant"}}, tracesColumns)

	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", skipped)
	}
	if len(got) != 1 || got[0].Column != "SpanAttributes" {
		t.Errorf("resolveKeys = %v, want the column the table has", got)
	}
}
