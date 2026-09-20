package ruleset

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
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
	set, problems := Load(filepath.Join("testdata", "rules"), loadSources(t), nil)
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
	_, problems := Load(filepath.Join("testdata", "bad"), loadSources(t), nil)

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
	_, problems := Load(filepath.Join("testdata", "protected"), loadSources(t), nil)

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
	set, problems := Load(filepath.Join("testdata", "override"), loadSources(t), nil)
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
	set, problems := Load(filepath.Join("testdata", "verbatim"), loadSources(t), nil)
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

// The case this slice exists to fix. A flat directory holding a rule with no
// team, no severity and no annotations is valid: the query runs, the source
// resolves, the time bounds are there. It must load with warnings and no
// errors, because none of what it omits stops it working (spec 7.6).
func TestLoadBareRuleProducesWarningsNotErrors(t *testing.T) {
	set, problems := Load(filepath.Join("testdata", "bare"), loadSources(t), nil)

	if len(set.Rules) != 1 {
		t.Fatalf("expected the rule to load, got %d rules: %v", len(set.Rules), problems)
	}
	for _, p := range problems {
		if p.Severity == lint.SeverityError {
			t.Errorf("unexpected error, nothing here stops the rule running: %s", p)
		}
	}
	if len(problems) == 0 {
		t.Error("expected warnings so an author still learns the conventions")
	}
}

// A source that pages on-call can demand more than the baseline. The same bare
// rule that only warns under default policy becomes an error once its source
// raises the check, and the finding names the file that raised it so
// --explain can answer why (spec 7.7, 7.8).
func TestLoadMergesSourcePolicy(t *testing.T) {
	path := filepath.Join("testdata", "strict_sources.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading sources: %v", err)
	}
	sources, problems := source.Parse(path, data, func(string) (string, bool) { return "", false })
	if len(problems) != 0 {
		t.Fatalf("sources fixture problems: %v", problems)
	}

	_, problems = Load(filepath.Join("testdata", "bare"), sources, nil)

	var raised int
	for _, p := range problems {
		if p.Check != "labels/required" {
			continue
		}
		raised++
		if p.Severity != lint.SeverityError {
			t.Errorf("severity = %v, want error raised by the source", p.Severity)
		}
		if p.PolicyFile != path {
			t.Errorf("policy origin = %q, want %q", p.PolicyFile, path)
		}
		// Line 10 is the `severity: error` line itself, not the check name
		// above it: the origin points at where the severity was set.
		if p.PolicyLine != 10 {
			t.Errorf("policy line = %d, want 10", p.PolicyLine)
		}
	}
	if raised == 0 {
		t.Fatal("expected labels/required findings")
	}
}
