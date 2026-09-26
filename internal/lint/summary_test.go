package lint

import (
	"strings"
	"testing"
)

func summaryRows() []SummaryRow {
	return []SummaryRow{
		{
			File: "rules/payments/latency.yaml", Alert: "HighP99Latency",
			Source: "payments_prod", Rows: "4100000", Interval: "30s",
		},
		{
			File: "rules/payments/latency.yaml", Alert: "HighP99Latency",
			Source: "payments_staging", Rows: "90000", Interval: "30s",
		},
	}
}

// The same rule against two sources is two rows. A rule that is cheap on
// staging and pathological on prod is the row worth surfacing, so the table
// must never collapse them (spec 7.10).
func TestFormatSummary(t *testing.T) {
	var sb strings.Builder
	if err := FormatSummary(&sb, summaryRows()); err != nil {
		t.Fatalf("FormatSummary: %v", err)
	}

	want := `| File | Alert | Source | Rows | Interval |
| --- | --- | --- | --- | --- |
| rules/payments/latency.yaml | HighP99Latency | payments_prod | 4100000 | 30s |
| rules/payments/latency.yaml | HighP99Latency | payments_staging | 90000 | 30s |
`
	if got := sb.String(); got != want {
		t.Errorf("summary output:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// A pipe in a cell would end the column early and shift every value after it
// into the wrong heading, which is a table that lies rather than one that
// looks wrong.
func TestFormatSummaryEscapesPipes(t *testing.T) {
	var sb strings.Builder
	rows := []SummaryRow{{File: "rules/a|b.yaml", Alert: "A", Source: "s", Rows: "1", Interval: "30s"}}
	if err := FormatSummary(&sb, rows); err != nil {
		t.Fatalf("FormatSummary: %v", err)
	}

	if !strings.Contains(sb.String(), `rules/a\|b.yaml`) {
		t.Errorf("pipe not escaped in:\n%s", sb.String())
	}
}

// An empty table is a heading with nothing under it, which reads as though
// the numbers were lost. Say that nothing was estimated instead.
func TestFormatSummaryWithNoRows(t *testing.T) {
	var sb strings.Builder
	if err := FormatSummary(&sb, nil); err != nil {
		t.Fatalf("FormatSummary: %v", err)
	}

	want := "No rule was estimated: nothing matched a source.\n"
	if got := sb.String(); got != want {
		t.Errorf("summary output:\ngot:\n%q\nwant:\n%q", got, want)
	}
}
