package ruleset

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

func loadSources(t *testing.T) *source.File { return loadSourcesFrom(t, "sources.yaml") }

func loadSourcesFrom(t *testing.T, name string) *source.File {
	t.Helper()

	path := filepath.Join("testdata", name)
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

// A rule whose labels no source accepts is a warning, not an error. On a
// ruler holding one datacenter's sources, most rules in a shared repository
// match nothing, and failing hard would make a shared repository unusable
// (spec 6.10).
func TestLoadWarnsWhenNoSourceMatches(t *testing.T) {
	// The source requires env=prod and the rule does not carry it, so nothing
	// accepts it.
	_, problems := Load(filepath.Join("testdata", "bad"), loadSourcesFrom(t, "labelled_sources.yaml"), nil)

	var found bool
	for _, p := range problems {
		if p.Check != "rule/source-match" {
			continue
		}
		found = true
		if p.Severity != lint.SeverityWarning {
			t.Errorf("severity = %v, want warning", p.Severity)
		}
	}
	if !found {
		t.Fatalf("expected a rule/source-match finding, got %v", problems)
	}
	for _, p := range problems {
		if p.Severity == lint.SeverityError {
			t.Errorf("unexpected error: %s", p)
		}
	}
}

// Two teams may use the same alert name. An alert's identity is its full
// label set, and rules in different files reach different sources, so their
// alerts already differ by team and source in the fingerprint. Requiring
// globally unique names would push authors into PaymentsHighErrorRate
// prefixes, re-encoding in the name what 6.3.1 says belongs in labels.
//
// Asserted rather than left unchecked, so the decision is pinned and the
// check is not quietly reintroduced later.
func TestLoadAllowsDuplicateAlertNamesAcrossFiles(t *testing.T) {
	_, problems := Load(filepath.Join("testdata", "duplicate_names"), loadSources(t), nil)

	for _, p := range problems {
		if p.Check == "rule/name" {
			t.Errorf("unexpected rule/name finding: the same name in two files is legitimate: %s", p)
		}
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
		// The `severity: error` line itself, not the check name above it: the
		// origin points at where the severity was set.
		if p.PolicyLine != 11 {
			t.Errorf("policy line = %d, want 11", p.PolicyLine)
		}
	}
	if raised == 0 {
		t.Fatal("expected labels/required findings")
	}
}

// A query may not claim a different source or contradict the cluster it ran
// on. Both are what keep two clusters' alerts apart, so a query that could
// set them could merge them (spec 6.3.1, 6.10.1).
func TestLoadRejectsQueryClaimingSourceIdentity(t *testing.T) {
	_, problems := Load(filepath.Join("testdata", "protected_source"),
		loadSourcesFrom(t, "identity_sources.yaml"), nil)

	var aliased []string
	for _, p := range problems {
		if p.Check == "rule/protected-label" {
			aliased = append(aliased, p.Text)
		}
	}
	if len(aliased) != 2 {
		t.Fatalf("expected source and cluster both rejected, got %d: %v", len(aliased), problems)
	}
}
