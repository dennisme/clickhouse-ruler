package ruleset

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/source"
)

func loadSources(t *testing.T) *source.File {
	t.Helper()

	path := filepath.Join("testdata", "sources.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading sources: %v", err)
	}
	f, problems := source.Parse(path, data, func(string) (string, bool) { return "", false })
	if len(problems) != 0 {
		t.Fatalf("sources fixture has problems: %v", problems)
	}
	return f
}

// team comes from the directory so it is not author-supplied, matching the
// ClickHouse user derived the same way in spec 6.6. Neither fixture writes a
// team label; both get one anyway.
func TestLoadDerivesTeamFromDirectory(t *testing.T) {
	set, problems := Load(filepath.Join("testdata", "rules"), loadSources(t))
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got: %v", problems)
	}

	got := map[string]string{}
	for _, r := range set.Rules {
		got[r.Alert] = r.Labels["team"]
	}

	want := map[string]string{
		"HighP99Latency": "payments",
		"ErrorRate":      "search",
	}
	if len(got) != len(want) {
		t.Fatalf("loaded %d rules, want %d: %v", len(got), len(want), got)
	}
	for alert, team := range want {
		if got[alert] != team {
			t.Errorf("%s team = %q, want %q", alert, got[alert], team)
		}
	}
}

// A rule naming a source that does not exist parses perfectly well and can
// never run. Catching it needs both files, which is why it lives here rather
// than in the rule package.
func TestLoadRejectsUnknownSource(t *testing.T) {
	_, problems := Load(filepath.Join("testdata", "bad"), loadSources(t))

	if len(problems) != 1 {
		t.Fatalf("expected 1 problem, got %d: %v", len(problems), problems)
	}
	p := problems[0]
	if p.Check != "rule/source-exists" {
		t.Errorf("check = %q, want rule/source-exists", p.Check)
	}
	if p.Subject != "PointsAtNothing" {
		t.Errorf("subject = %q, want PointsAtNothing", p.Subject)
	}
	if p.Line != 5 {
		t.Errorf("line = %d, want 5", p.Line)
	}
}

// A query may not set team or alertname. Not because overriding team is
// forbidden, a rule may do that in its labels, but because a value arriving
// from a result column cannot be enumerated when the Alertmanager route tree
// is generated (spec 6.3.1, 6.5).
func TestLoadRejectsProtectedLabels(t *testing.T) {
	_, problems := Load(filepath.Join("testdata", "protected"), loadSources(t))

	var got []string
	for _, p := range problems {
		if p.Check == "rule/protected-label" {
			got = append(got, p.Subject+": "+p.Text)
		}
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 protected-label problems, got %d: %v", len(got), problems)
	}
}

// One team running operations for another team's service is a real
// arrangement, so an explicit team label beats the directory. The value is in
// the file, so it shows up in a diff and the route tree can still enumerate
// it (spec 6.3.1).
func TestLoadRuleLabelOverridesDerivedTeam(t *testing.T) {
	set, problems := Load(filepath.Join("testdata", "override"), loadSources(t))
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got: %v", problems)
	}
	if len(set.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(set.Rules))
	}

	r := set.Rules[0]
	if r.Team != "search" {
		t.Errorf("team = %q, want search (the label, not the platform directory)", r.Team)
	}
	if r.Labels["team"] != "search" {
		t.Errorf("labels[team] = %q, want search", r.Labels["team"])
	}
	// The resolved source is what the querier needs; a loaded rule that did
	// not carry one would fail only at evaluation time.
	if r.Source.Name != "otel_traces" {
		t.Errorf("source = %q, want otel_traces resolved from the sources file", r.Source.Name)
	}
	if r.Source.Table != "otel_traces" {
		t.Errorf("source table = %q, want otel_traces", r.Source.Table)
	}
}

// The directory name is the team, with nothing stripped from it. A tool that
// quietly rewrites part of a path makes the mapping from directory to team
// something you have to know rather than something you can read.
func TestLoadUsesDirectoryNameVerbatim(t *testing.T) {
	set, problems := Load(filepath.Join("testdata", "verbatim"), loadSources(t))
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got: %v", problems)
	}
	if len(set.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(set.Rules))
	}
	if got := set.Rules[0].Team; got != "team-payments" {
		t.Errorf("team = %q, want team-payments taken from the directory as written", got)
	}
}
