package alert

import (
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// A result column named team must not decide where the page goes. team routes
// the alert and names the team on clickhouse_ruler_problem, and a value that
// only exists at query time has no route in a tree generated from the files
// (spec 6.3.1).
func TestQueryCannotSetTeam(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	smpl := sample("checkout", 1)
	smpl.Labels["team"] = "not-payments"

	alerts := evalOK(t, New(testRule(0, 0), nil, dc("traces_dc1", "dc1"), testRetention), now, []Sample{smpl})

	if len(alerts) != 1 {
		t.Fatalf("expected one alert, got %d", len(alerts))
	}
	if got := alerts[0].Labels["team"]; got != "payments" {
		t.Errorf("team = %q, want payments from the rule: a query column routed the page", got)
	}
}

// With no team in the file there is nothing for the query to override, and the
// answer is still not the query's value: the alert carries no team rather than
// one nobody reviewed.
func TestQueryTeamIsDroppedWhenTheRuleSetsNone(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	r := rule.Rule{Alert: "HighP99Latency", Labels: map[string]string{"severity": "warning"}}
	smpl := sample("checkout", 1)
	smpl.Labels["team"] = "not-payments"

	alerts := evalOK(t, New(r, nil, dc("traces_dc1", "dc1"), testRetention), now, []Sample{smpl})

	if len(alerts) != 1 {
		t.Fatalf("expected one alert, got %d", len(alerts))
	}
	if got, ok := alerts[0].Labels["team"]; ok {
		t.Errorf("team = %q, want no team label at all", got)
	}
}

// A source may still set team, because its labels are reviewed in the sources
// file and win over the query by design (spec 6.3.1, level 4).
func TestSourceMaySetTeam(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	src := source.Source{Name: "traces_dc1", Labels: map[string]string{"team": "platform"}}
	smpl := sample("checkout", 1)
	smpl.Labels["team"] = "not-payments"

	alerts := evalOK(t, New(testRule(0, 0), nil, src, testRetention), now, []Sample{smpl})

	if len(alerts) != 1 {
		t.Fatalf("expected one alert, got %d", len(alerts))
	}
	if got := alerts[0].Labels["team"]; got != "platform" {
		t.Errorf("team = %q, want platform from the source", got)
	}
}
