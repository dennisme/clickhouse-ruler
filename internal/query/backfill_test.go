package query

import (
	"context"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// The range and the step decide how many evaluations a replay runs, and the
// arithmetic is what stops a 24 hour range at a one minute step from running
// 1,440 queries nobody asked for (spec 7.4).
func TestReplayWindows(t *testing.T) {
	cases := []struct {
		name string
		span time.Duration
		step time.Duration
		want int
	}{
		{"a day of minutes", 24 * time.Hour, time.Minute, 1440},
		{"a day of quarter hours", 24 * time.Hour, 15 * time.Minute, 96},
		{"a step the range divides unevenly", time.Hour, 25 * time.Minute, 2},
		{"a step longer than the range still evaluates once", time.Hour, 2 * time.Hour, 1},
		{"a step the length of the range", time.Hour, time.Hour, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := replayWindows(tc.span, tc.step); got != tc.want {
				t.Errorf("replayWindows(%s, %s) = %d, want %d", tc.span, tc.step, got, tc.want)
			}
		})
	}
}

// The last evaluation is the one a live ruler would have run just now, and the
// first is one step inside the range, because an evaluation reads the window
// behind it.
func TestReplayInstants(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	got := replayInstants(now, time.Hour, 3)
	want := []time.Time{
		now.Add(-2 * time.Hour),
		now.Add(-time.Hour),
		now,
	}

	if len(got) != len(want) {
		t.Fatalf("replayInstants gave %d instants, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("instant %d = %s, want %s", i, got[i], want[i])
		}
	}
}

// Nothing runs until the predicted total fits the ceiling, and what gives is
// the step rather than the range: a coarser replay over the whole range still
// answers the question, where a shorter range answers a different one.
func TestStepWithinTheRowCeiling(t *testing.T) {
	cases := []struct {
		name        string
		perWindow   uint64
		ceiling     uint64
		span        time.Duration
		step        time.Duration
		wantStep    time.Duration
		wantSampled bool
	}{
		{
			name:      "within the ceiling, left alone",
			perWindow: 1000, ceiling: 10_000_000,
			span: 24 * time.Hour, step: time.Minute,
			wantStep: time.Minute,
		},
		{
			name:      "no ceiling, left alone",
			perWindow: 1_000_000_000, ceiling: 0,
			span: 24 * time.Hour, step: time.Minute,
			wantStep: time.Minute,
		},
		{
			name:      "nothing predicted, left alone",
			perWindow: 0, ceiling: 100,
			span: 24 * time.Hour, step: time.Minute,
			wantStep: time.Minute,
		},
		{
			name:      "raised until the windows fit",
			perWindow: 1_000_000, ceiling: 100_000_000,
			span: 24 * time.Hour, step: time.Minute,
			wantStep: 15 * time.Minute, wantSampled: true,
		},
		{
			name:      "one window is already over, so one window is what runs",
			perWindow: 1_000_000_000, ceiling: 1000,
			span: 24 * time.Hour, step: time.Minute,
			wantStep: 24 * time.Hour, wantSampled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step, sampled := stepWithinCeiling(tc.span, tc.step, tc.perWindow, tc.ceiling)
			if step != tc.wantStep {
				t.Errorf("step = %s, want %s", step, tc.wantStep)
			}
			if sampled != tc.wantSampled {
				t.Errorf("sampled = %v, want %v", sampled, tc.wantSampled)
			}
		})
	}
}

// canned is a replay whose answers are written down, so the collapse is tested
// without a database: what the `for` and the resolve path do with a sequence of
// windows is arithmetic, and it belongs in a unit test (spec 7.4).
type canned struct {
	samples map[time.Time][]alert.Sample
	asked   []time.Time
}

func (c *canned) evaluate(_ context.Context, at time.Time) ([]alert.Sample, error) {
	c.asked = append(c.asked, at)
	return c.samples[at], nil
}

func sampleFor(service string, value float64) alert.Sample {
	return alert.Sample{Labels: map[string]string{"ServiceName": service}, Value: value}
}

// Two numbers, and they differ wildly: every window a row came back in is an
// evaluation hit, and only a run of them longer than `for` is an alert somebody
// would have been paged about (spec 7.4).
func TestReplayCountsAlertsAndHits(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	step := time.Minute
	instants := replayInstants(now, step, 10)

	// checkout is over the threshold for the first six windows and recovers;
	// cart is over it in one window only, which is shorter than `for`.
	c := &canned{samples: map[time.Time][]alert.Sample{}}
	for i, at := range instants {
		if i < 6 {
			c.samples[at] = append(c.samples[at], sampleFor("checkout", 1))
		}
		if i == 8 {
			c.samples[at] = append(c.samples[at], sampleFor("cart", 1))
		}
	}

	r := rule.Rule{Alert: "HighLatency", For: 2 * time.Minute}
	state := alert.New(r, nil, source.Source{Name: "payments"}, 0)

	got, err := replay(context.Background(), c, state, instants, step)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if got.Hits != 7 {
		t.Errorf("hits = %d, want 7, one per window a row came back in", got.Hits)
	}
	if got.Alerts != 1 {
		t.Errorf("alerts = %d, want 1: only checkout stayed over the threshold for `for`", got.Alerts)
	}
	// Pending at the first two windows, firing from the third to the sixth,
	// and one window's worth of time is what an evaluation covers.
	if want := 4 * time.Minute; got.LongestStreak != want {
		t.Errorf("longest streak = %s, want %s", got.LongestStreak, want)
	}
	if got.StillFiring {
		t.Error("still firing, but checkout recovered before the range ended")
	}
	if len(got.Top) != 2 {
		t.Fatalf("top label sets = %v, want two of them", got.Top)
	}
	if got.Top[0].Labels != "ServiceName=checkout" || got.Top[0].Hits != 6 {
		t.Errorf("top label set = %+v, want checkout with 6 hits", got.Top[0])
	}
	if got.Top[1].Labels != "ServiceName=cart" || got.Top[1].Hits != 1 {
		t.Errorf("second label set = %+v, want cart with 1 hit", got.Top[1])
	}
}

// An instance still over its threshold at the last window never resolved, and
// saying so is the difference between a rule that flaps and one that has been
// paging somebody all day (spec 7.4).
func TestReplayReportsAnAlertThatNeverResolved(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	step := time.Minute
	instants := replayInstants(now, step, 4)

	c := &canned{samples: map[time.Time][]alert.Sample{}}
	for _, at := range instants {
		c.samples[at] = []alert.Sample{sampleFor("checkout", 1)}
	}

	state := alert.New(rule.Rule{Alert: "HighLatency"}, nil, source.Source{Name: "payments"}, 0)

	got, err := replay(context.Background(), c, state, instants, step)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !got.StillFiring {
		t.Error("still firing = false, but the instance was over its threshold at the last window")
	}
	if want := 4 * time.Minute; got.LongestStreak != want {
		t.Errorf("longest streak = %s, want %s", got.LongestStreak, want)
	}
}

// A replay asks for every window in order, oldest first, because the `for`
// timer of a later window is measured from an earlier one.
func TestReplayEvaluatesEveryWindowInOrder(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	instants := replayInstants(now, time.Minute, 5)

	c := &canned{samples: map[time.Time][]alert.Sample{}}
	state := alert.New(rule.Rule{Alert: "Quiet"}, nil, source.Source{Name: "payments"}, 0)

	if _, err := replay(context.Background(), c, state, instants, time.Minute); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(c.asked) != len(instants) {
		t.Fatalf("asked %d windows, want %d", len(c.asked), len(instants))
	}
	for i := range instants {
		if !c.asked[i].Equal(instants[i]) {
			t.Fatalf("window %d asked for %s, want %s", i, c.asked[i], instants[i])
		}
	}
}

// A range longer than the table keeps reads windows whose data has been
// deleted, and the count then under reports without saying so (spec 7.4).
func TestParseTTL(t *testing.T) {
	cases := []struct {
		name   string
		engine string
		want   time.Duration
		wantOK bool
	}{
		{
			name:   "the trace table",
			engine: "MergeTree PARTITION BY toDate(Timestamp) ORDER BY (ServiceName) TTL toDateTime(Timestamp) + toIntervalDay(3) SETTINGS index_granularity = 8192",
			want:   72 * time.Hour, wantOK: true,
		},
		{
			name:   "hours",
			engine: "MergeTree ORDER BY x TTL ts + toIntervalHour(36)",
			want:   36 * time.Hour, wantOK: true,
		},
		{
			name:   "two intervals in one expression",
			engine: "MergeTree ORDER BY x TTL ts + toIntervalDay(1) + toIntervalHour(6)",
			want:   30 * time.Hour, wantOK: true,
		},
		{
			name:   "the delete rule rather than the one moving parts about",
			engine: "MergeTree ORDER BY x TTL ts + toIntervalDay(7) DELETE, ts + toIntervalDay(1) TO VOLUME 'cold'",
			want:   7 * 24 * time.Hour, wantOK: true,
		},
		{
			name:   "no TTL at all",
			engine: "MergeTree PARTITION BY toDate(ts) ORDER BY x SETTINGS index_granularity = 8192",
		},
		{
			name:   "a TTL nothing here can read",
			engine: "MergeTree ORDER BY x TTL ts + interval_from_a_function(retention)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseTTL(tc.engine)
			if ok != tc.wantOK {
				t.Fatalf("parseTTL ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("parseTTL = %s, want %s", got, tc.want)
			}
		})
	}
}

// A column added inside the range is the quiet one: the query still resolves,
// the older windows read the column's default, and the replay reports a rule
// that never fired (spec 7.4).
func TestColumnsAddedInside(t *testing.T) {
	from := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	tableStart := from.Add(-48 * time.Hour)
	added := from.Add(6 * time.Hour)

	earliest := map[string]time.Time{
		"Timestamp":   tableStart,
		"ServiceName": tableStart,
		"Duration":    added,
		"Unread":      added,
	}

	got := columnsAddedInside(earliest, []string{"Duration", "ServiceName"}, from)
	if len(got) != 1 {
		t.Fatalf("columns = %v, want only Duration", got)
	}
	if got[0].Column != "Duration" || !got[0].Earliest.Equal(added) {
		t.Errorf("column = %+v, want Duration at %s", got[0], added)
	}
}

// A table that only started holding data inside the range has every column
// appear at once, which is a statement about the table rather than about a
// schema change, and the range caveat already says it.
func TestColumnsAddedInsideIgnoresANewTable(t *testing.T) {
	from := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	started := from.Add(time.Hour)

	earliest := map[string]time.Time{"Timestamp": started, "Duration": started}

	if got := columnsAddedInside(earliest, []string{"Duration"}, from); len(got) != 0 {
		t.Errorf("columns = %v, want none: every column arrived with the table", got)
	}
}
