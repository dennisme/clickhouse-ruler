//go:build integration

package query

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
)

// The map keys the seeded rows actually carry. seed writes
// `deployment.environment` into ResourceAttributes and `index` into
// SpanAttributes, so those two are present and anything else is not.
const (
	presentKey = "deployment.environment"
	absentKey  = "payment_id"
)

func sampleRule(expr string) rule.Rule {
	return rule.Rule{Alert: "Sampled", Expr: expr, Window: time.Hour}
}

// The bounds are rendered rather than bound, the same as every other check, so
// the sampled statement is the shape the evaluated one is.
func selectWithKey(column, key string) string {
	return "SELECT ServiceName, count() AS value FROM otel.otel_traces " +
		"WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }} " +
		"AND " + column + "['" + key + "'] != '' GROUP BY ServiceName"
}

func TestSampleFindsAPresentKey(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seed(t, q, []span{{at: anchor.Add(-time.Minute), service: "checkout", duration: 1}})

	got, err := q.Sample(context.Background(),
		sampleRule(selectWithKey("ResourceAttributes", presentKey)), testGroup, SampleChecks{}, anchor)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("findings = %v, want none: the key is in the seeded rows", got)
	}
}

// The finding the check exists for. A renamed OTel attribute leaves a query
// that parses, resolves and returns nothing.
func TestSampleReportsAnAbsentKey(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seed(t, q, []span{{at: anchor.Add(-time.Minute), service: "checkout", duration: 1}})

	got, err := q.Sample(context.Background(),
		sampleRule(selectWithKey("ResourceAttributes", absentKey)), testGroup, SampleChecks{}, anchor)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("findings = %v, want one", got)
	}
	// "is in none of" rather than any finding at all: a skip carries the same
	// check name, and a key reported as unreadable would otherwise pass for a key
	// reported as absent.
	for _, want := range []string{"ResourceAttributes", absentKey, "is in none of"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("detail %q is missing %q", got[0].Detail, want)
		}
	}
}

// Fail open. The window holds nothing, so nothing was verified either way, and
// a rule that lands before its data is ordinary rather than wrong (spec 7.3).
func TestSampleSaysNothingWhenTheWindowIsEmpty(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seed(t, q, []span{{at: anchor.Add(-time.Minute), service: "checkout", duration: 1}})

	// A month before the seeded rows, so the window is genuinely empty rather
	// than the key genuinely missing.
	empty := anchor.Add(-30 * 24 * time.Hour)

	got, err := q.Sample(context.Background(),
		sampleRule(selectWithKey("ResourceAttributes", absentKey)), testGroup, SampleChecks{}, empty)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("findings = %v, want none over a window with no rows in it", got)
	}
}

func TestSampleReportsAnEmptyWindowWhenRowsAreRequired(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seed(t, q, []span{{at: anchor.Add(-time.Minute), service: "checkout", duration: 1}})

	empty := anchor.Add(-30 * 24 * time.Hour)

	got, err := q.Sample(context.Background(),
		sampleRule(selectWithKey("ResourceAttributes", presentKey)),
		testGroup, SampleChecks{RequireRows: true}, empty)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("findings = %v, want one", got)
	}
	if !strings.Contains(got[0].Detail, "no rows") {
		t.Errorf("detail = %q, want it to say the window held no rows", got[0].Detail)
	}
}

// A subscript written on the table's alias is the table's column. Against the
// real schema, because whether the qualifier is an alias or part of the name is
// a question only the table can answer.
func TestSampleResolvesAKeyThroughATableAlias(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seed(t, q, []span{{at: anchor.Add(-time.Minute), service: "checkout", duration: 1}})

	expr := "SELECT t.ServiceName AS ServiceName, count() AS value " +
		"FROM otel.otel_traces AS t " +
		"WHERE t.Timestamp >= {{ .From }} AND t.Timestamp < {{ .To }} " +
		"AND t.ResourceAttributes['" + absentKey + "'] != '' GROUP BY t.ServiceName"

	got, err := q.Sample(context.Background(), sampleRule(expr), testGroup, SampleChecks{}, anchor)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("findings = %v, want the absent key found through the alias", got)
	}
	if !strings.Contains(got[0].Detail, "is in none of") {
		t.Errorf("detail = %q, want the key reported as absent rather than unchecked", got[0].Detail)
	}
	if strings.Contains(got[0].Detail, "t.ResourceAttributes") {
		t.Errorf("detail = %q, want the column the table has rather than the qualified name", got[0].Detail)
	}
}

// A Nested block flattens into Array columns whose names carry a dot, so
// `Events.Name[1]` is an index into an array rather than a key that could go
// missing. Nothing is reported, and the whole identifier has to resolve as the
// column name for that to be the answer.
func TestSampleIgnoresAnIndexIntoANestedColumn(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seed(t, q, []span{{at: anchor.Add(-time.Minute), service: "checkout", duration: 1}})

	expr := "SELECT ServiceName, count() AS value FROM otel.otel_traces " +
		"WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }} " +
		"AND length(`Events.Name`) > 0 AND `Events.Name`[1] != '' GROUP BY ServiceName"

	got, err := q.Sample(context.Background(), sampleRule(expr), testGroup, SampleChecks{}, anchor)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("findings = %v, want none: an array index was never a key", got)
	}
}

// A key on a table the source does not own cannot be sampled: the window and
// the timestamp column describe the source's own table. Reported rather than
// passed over, because a check that examined nothing must not look like one that
// found nothing wrong (spec 7.3).
func TestSampleReportsAKeyItCannotCheck(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seed(t, q, []span{{at: anchor.Add(-time.Minute), service: "checkout", duration: 1}})

	expr := "SELECT ServiceName, count() AS value FROM otel.otel_traces " +
		"WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }} " +
		"AND ResourceAttributes[concat('payment', '_id')] != '' GROUP BY ServiceName"

	got, err := q.Sample(context.Background(), sampleRule(expr), testGroup, SampleChecks{}, anchor)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("findings = %v, want the unreadable key reported", got)
	}
	if !strings.Contains(got[0].Detail, "not checked") {
		t.Errorf("detail = %q, want it to say the key went unchecked", got[0].Detail)
	}
}

// A rule reading no map at all has nothing to sample, and must not cost a query
// to find that out beyond the parse.
func TestSampleSaysNothingAboutARuleWithNoKeys(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seed(t, q, []span{{at: anchor.Add(-time.Minute), service: "checkout", duration: 1}})

	expr := "SELECT ServiceName, count() AS value FROM otel.otel_traces " +
		"WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }} GROUP BY ServiceName"

	got, err := q.Sample(context.Background(), sampleRule(expr), testGroup, SampleChecks{}, anchor)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("findings = %v, want none", got)
	}
}

// The types are what tell a Map from an Array, and they come from the server so
// that a schema change is what the check reads rather than what it assumed.
func TestDescribeTableReadsTheColumnTypes(t *testing.T) {
	q := openQuerier(t, testSource(t))

	cols, err := q.describeTable(context.Background(), "otel", "otel_traces")
	if err != nil {
		t.Fatalf("describeTable: %v", err)
	}

	types := map[string]string{}
	for _, c := range cols {
		types[c.Name] = c.Type
	}

	if !isMapType(types["ResourceAttributes"]) {
		t.Errorf("ResourceAttributes = %q, want a Map", types["ResourceAttributes"])
	}
	// A Nested block flattens into columns named with a dot, which is why the
	// whole identifier has to be tried before the part after it.
	if got := types["Events.Name"]; !strings.HasPrefix(got, "Array(") {
		t.Errorf("Events.Name = %q, want an Array", got)
	}
}
