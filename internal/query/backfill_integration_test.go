//go:build integration

package query

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
)

// The rule the replays below count. One row per service whose spans are slow in
// the window, which is a rule that fires when a service is slow and stops when
// it is not.
const slowSpansExpr = `
SELECT ServiceName, count() AS value
FROM otel.otel_traces
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
  AND Duration > 1000
GROUP BY ServiceName
HAVING value > 0`

func backfillRule(pending time.Duration) rule.Rule {
	return rule.Rule{
		Alert:  "SlowSpans",
		Expr:   slowSpansExpr,
		Window: time.Minute,
		For:    pending,
	}
}

// One span per minute for a service, placed around anchor rather than a date in
// this file, for the reason anchor itself is read from the clock.
func slowSpans(service string, minutesBack ...int) []span {
	out := make([]span, 0, len(minutesBack))
	for _, m := range minutesBack {
		out = append(out, span{
			at:       anchor.Add(-time.Duration(m) * time.Minute),
			service:  service,
			duration: 2000,
		})
	}
	return out
}

func backfill(t *testing.T, q *Querier, r rule.Rule, c BackfillChecks) (*Backfill, []Finding) {
	t.Helper()

	got, findings, err := q.Backfill(context.Background(), r, testGroup, c, anchor)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if got == nil {
		t.Fatal("Backfill answered nothing, want a replay of the seeded range")
	}
	return got, findings
}

// The two numbers the check exists for, against real data: every window a row
// came back in, and the runs of them longer than `for` that would have paged
// somebody (spec 7.4).
func TestBackfillCountsAlertsAndHits(t *testing.T) {
	q := openQuerier(t, testSource(t))

	// checkout is slow for six consecutive minutes, so its windows are the six
	// evaluations from anchor-5m to anchor. cart is slow in one window only,
	// which never outlasts `for`.
	seed(t, q, append(slowSpans("checkout", 6, 5, 4, 3, 2, 1), slowSpans("cart", 8)...))

	got, findings := backfill(t, q, backfillRule(2*time.Minute), BackfillChecks{
		Range: 10 * time.Minute,
		Step:  time.Minute,
	})

	if got.Windows != 10 {
		t.Errorf("windows = %d, want 10: a ten minute range in minute steps", got.Windows)
	}
	if got.Hits != 7 {
		t.Errorf("hits = %d, want 7: six windows of checkout and one of cart", got.Hits)
	}
	if got.Alerts != 1 {
		t.Errorf("alerts = %d, want 1: only checkout stayed slow for longer than `for`", got.Alerts)
	}
	if !got.StillFiring {
		t.Error("still firing = false, but checkout was slow in the last window")
	}
	if want := 4 * time.Minute; got.LongestStreak != want {
		t.Errorf("longest streak = %s, want %s", got.LongestStreak, want)
	}
	if len(got.Top) != 2 || got.Top[0].Labels != "ServiceName=checkout" || got.Top[0].Hits != 6 {
		t.Errorf("top label sets = %+v, want checkout first with six hits", got.Top)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none: the rule is within every ceiling", findings)
	}
}

// The count is only worth reporting against something, and the ceiling is what
// separates a rule that pages twice a day from one that pages every minute.
func TestBackfillReportsMoreAlertsThanTheCeiling(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seed(t, q, append(slowSpans("checkout", 3, 2, 1), slowSpans("cart", 3, 2, 1)...))

	_, findings := backfill(t, q, backfillRule(0), BackfillChecks{
		Range:     5 * time.Minute,
		Step:      time.Minute,
		MaxAlerts: 1,
	})

	if len(findings) != 1 {
		t.Fatalf("findings = %v, want the ceiling reported", findings)
	}
	if got := findings[0].Check; got != "rule/alert-count" {
		t.Errorf("check = %q, want rule/alert-count", got)
	}
	detail := findings[0].Detail
	for _, want := range []string{"2 alert instances", "ceiling of 1", "evaluation hits", "ServiceName="} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, want it to carry %q", detail, want)
		}
	}
}

// Nothing runs until the whole replay fits the row ceiling. The step gives, the
// range does not, and the answer says it is a floor rather than the count.
func TestBackfillRaisesTheStepToFitTheRowCeiling(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seed(t, q, slowSpans("checkout", 6, 5, 4, 3, 2, 1))

	got, findings := backfill(t, q, backfillRule(0), BackfillChecks{
		Range:       10 * time.Minute,
		Step:        time.Minute,
		MaxRowsRead: 1,
	})

	if !got.Sampled {
		t.Fatal("sampled = false, but one window alone was predicted over the ceiling")
	}
	if got.Windows != 1 {
		t.Errorf("windows = %d, want 1: the ceiling affords one window", got.Windows)
	}
	if got.Step != 10*time.Minute {
		t.Errorf("step = %s, want the whole range", got.Step)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want the sampled replay reported", findings)
	}
	if !strings.Contains(findings[0].Detail, "the step was raised") {
		t.Errorf("detail = %q, want it to say the step was raised", findings[0].Detail)
	}
}

// A range longer than the table keeps reads windows whose rows have been
// deleted, and the count then under reports. The TTL is read from
// system.tables, which the user contract leaves readable (spec 6.7.2, 7.4).
func TestBackfillWarnsWhenTheRangeExceedsTheTTL(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seed(t, q, slowSpans("checkout", 1))

	// The schema in deploy/clickhouse/init TTLs at three days.
	_, findings := backfill(t, q, backfillRule(0), BackfillChecks{
		Range: 96 * time.Hour,
		Step:  48 * time.Hour,
	})

	if len(findings) != 1 {
		t.Fatalf("findings = %v, want the TTL reported", findings)
	}
	if !strings.Contains(findings[0].Detail, "TTL") {
		t.Errorf("detail = %q, want it to name the table's TTL", findings[0].Detail)
	}
}

// The TTL is the table's own, read through the source's user rather than
// written down here, so a retention change is what the caveat reads.
func TestTableTTLReadsTheSchema(t *testing.T) {
	q := openQuerier(t, testSource(t))

	got, ok := q.tableTTL(context.Background(), q.storageTable(context.Background()).Local)
	if !ok {
		t.Fatal("tableTTL answered nothing, but the schema sets one")
	}
	if want := 72 * time.Hour; got != want {
		t.Errorf("tableTTL = %s, want %s", got, want)
	}
}

// The parts metadata is not part of the contract in spec 6.7.2, so under it the
// drift caveat answers nothing rather than failing the replay. It answers on a
// validation user with wider grants, which is the setup that catches a schema
// change before the rules reach the cluster evaluating them (spec 10.3).
func TestColumnsAddedIsSilentWithoutTheGrant(t *testing.T) {
	q := openQuerier(t, testSource(t))
	seed(t, q, slowSpans("checkout", 1))

	sql, err := renderForCheck(slowSpansExpr, time.Minute)
	if err != nil {
		t.Fatalf("renderForCheck: %v", err)
	}

	local := q.storageTable(context.Background()).Local
	if got := q.columnsAdded(context.Background(), sql, anchor.Add(-time.Hour), local); len(got) != 0 {
		t.Errorf("columns = %v, want none: this user cannot read system.parts_columns", got)
	}
}
