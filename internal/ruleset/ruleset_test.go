package ruleset

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
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

// Two rules whose static identity is the same produce one alert between them.
// Alertmanager and notify.Cadence both key on the fingerprint, so the second
// evaluation overwrites the first's state and a resolve can be lost. The
// finding names both files, because whoever sees it may own neither
// (spec 7.6).
func TestLoadReportsRulesThatProduceTheSameAlert(t *testing.T) {
	_, problems := Load(filepath.Join("testdata", "duplicate_alert"),
		loadSourcesFrom(t, "duplicate_alert_sources.yaml"), nil)

	var got []lint.Problem
	for _, p := range problems {
		if p.Check == "rule/duplicate-alert" {
			got = append(got, p)
		}
	}
	// The pair in payments/ and platform/ collides. The rule in tiered/
	// carries a label neither of them does, and the two in split/ reach
	// different sources, so neither is a collision.
	if len(got) != 1 {
		t.Fatalf("expected one collision reported once, got %d: %v", len(got), got)
	}

	p := got[0]
	if p.Severity != lint.SeverityWarning {
		t.Errorf("severity = %v, want warning", p.Severity)
	}
	if p.Subject != "HighP99Latency" {
		t.Errorf("subject = %q, want the alert name", p.Subject)
	}
	other := filepath.Join("testdata", "duplicate_alert", "payments", "latency.yaml")
	if p.File != filepath.Join("testdata", "duplicate_alert", "platform", "latency.yaml") {
		t.Errorf("file = %q, want the later of the two rules", p.File)
	}
	if !strings.Contains(p.Text, other) {
		t.Errorf("text does not name the other rule's file: %s", p.Text)
	}
}

// loadPolicy reads a fixture's instance policy the way the CLI reads the
// ruler.yaml beside the rules it was pointed at.
func loadPolicy(t *testing.T, path string) *policy.Policy {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading policy: %v", err)
	}
	p, problems := policy.Parse(path, data)
	if len(problems) != 0 {
		t.Fatalf("policy fixture problems: %v", problems)
	}
	return p
}

// severityOf finds the finding for a check against one rule file, so a test
// can say what a scope did to a check rather than counting problems.
func severityOf(t *testing.T, problems []lint.Problem, file, check string) lint.Problem {
	t.Helper()

	for _, p := range problems {
		if p.Check == check && p.File == file {
			return p
		}
	}
	t.Fatalf("no %s finding against %s, got %v", check, file, problems)
	return lint.Problem{}
}

// A team owns the directory its rules live in, so a ruler.yaml there is
// policy for those rules and no others. The merge is a maximum, so the only
// thing a team can do with it is make its own life stricter (spec 7.7).
func TestLoadReadsTeamPolicy(t *testing.T) {
	dir := filepath.Join("testdata", "team_policy")
	instance := filepath.Join(dir, "ruler.yaml")
	team := filepath.Join(dir, "payments", "ruler.yaml")
	nested := filepath.Join(dir, "payments", "critical", "ruler.yaml")

	payments := filepath.Join(dir, "payments", "latency.yaml")
	pager := filepath.Join(dir, "payments", "critical", "pager.yaml")
	search := filepath.Join(dir, "search", "errors.yaml")

	sources := loadSourcesFrom(t, filepath.Join("team_policy", "sources.yaml"))
	_, problems := Load(dir, sources, loadPolicy(t, instance))

	// The team raised labels/required for its own directory, and --explain
	// has to be able to say which file did it.
	raised := severityOf(t, problems, payments, "labels/required")
	if raised.Severity != lint.SeverityError {
		t.Errorf("labels/required in the team directory = %v, want error", raised.Severity)
	}
	if raised.PolicyFile != team || raised.PolicyLine != 5 {
		t.Errorf("policy origin = %s:%d, want %s:5", raised.PolicyFile, raised.PolicyLine, team)
	}

	// The sibling directory is untouched by another team's file.
	sibling := severityOf(t, problems, search, "labels/required")
	if sibling.Severity != lint.SeverityWarning {
		t.Errorf("labels/required in the sibling directory = %v, want the shipped warning",
			sibling.Severity)
	}
	if sibling.PolicyFile != "" {
		t.Errorf("policy origin = %q, want the shipped default", sibling.PolicyFile)
	}

	// The team set annotations/runbook off and the instance set it to error.
	// A maximum cannot go down, so the instance file is still the origin.
	lowered := severityOf(t, problems, payments, "annotations/runbook")
	if lowered.Severity != lint.SeverityError {
		t.Errorf("annotations/runbook = %v, want the instance error a team cannot lower",
			lowered.Severity)
	}
	if lowered.PolicyFile != instance || lowered.PolicyLine != 5 {
		t.Errorf("policy origin = %s:%d, want %s:5", lowered.PolicyFile, lowered.PolicyLine, instance)
	}

	// A rule below two team files answers to both.
	deep := severityOf(t, problems, pager, "annotations/required")
	if deep.Severity != lint.SeverityError || deep.PolicyFile != nested {
		t.Errorf("annotations/required = %v from %s, want error from %s",
			deep.Severity, deep.PolicyFile, nested)
	}
	if got := severityOf(t, problems, pager, "labels/required"); got.PolicyFile != team {
		t.Errorf("the directory above the nested one = %q, want %s", got.PolicyFile, team)
	}
}

// The ruler.yaml at the rules root is the instance scope, passed in already.
// Reading it again as a team file changes no severity under a maximum, but a
// finding whose origin named the wrong scope would send --explain at the
// wrong file, so it is left alone here.
func TestLoadDoesNotReadTheInstanceFileAsTeamPolicy(t *testing.T) {
	dir := filepath.Join("testdata", "team_policy")
	sources := loadSourcesFrom(t, filepath.Join("team_policy", "sources.yaml"))

	_, problems := Load(dir, sources, nil)

	got := severityOf(t, problems, filepath.Join(dir, "search", "errors.yaml"), "labels/required")
	if got.Severity != lint.SeverityWarning {
		t.Errorf("labels/required = %v, want the shipped warning: the root ruler.yaml is "+
			"the instance scope and this load was given none", got.Severity)
	}
}

// Both reserved names live in the rules tree and neither is a rule file. The
// quick start puts sources.yaml there, and a team's ruler.yaml is the point
// of this scope, so parsing either as a rule reports a pile of unknown
// fields against a file that is exactly right.
func TestLoadDoesNotParseReservedNamesAsRules(t *testing.T) {
	dir := filepath.Join("testdata", "team_policy")
	sources := loadSourcesFrom(t, filepath.Join("team_policy", "sources.yaml"))

	set, problems := Load(dir, sources, nil)

	for _, p := range problems {
		if filepath.Base(p.File) == "sources.yaml" || filepath.Base(p.File) == "ruler.yaml" {
			if p.Check == "yaml/unknown-field" {
				t.Errorf("a reserved file was parsed as a rule file: %s", p)
			}
		}
	}
	if len(set.Rules) != 3 {
		t.Fatalf("expected the three rules, got %d", len(set.Rules))
	}
}

// A deployment that publishes a revision of the rules points a symlink at it,
// so the path the ruler is handed is the symlink rather than the directory
// (spec 10.2). Walked unresolved it is one entry with no extension, which
// finds no rule files and reports nothing about having found none.
func TestLoadResolvesASymlinkedRoot(t *testing.T) {
	real, err := filepath.Abs(filepath.Join("testdata", "rules"))
	if err != nil {
		t.Fatalf("resolving the fixture: %v", err)
	}

	link := filepath.Join(t.TempDir(), "current")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("linking the revision: %v", err)
	}

	want, _ := Load(real, loadSources(t), nil)
	if len(want.Rules) == 0 {
		t.Fatalf("the fixture holds no rules, so this proves nothing")
	}

	set, problems := Load(link, loadSources(t), nil)

	if len(set.Rules) != len(want.Rules) {
		t.Errorf("rules through the symlink = %d, want %d", len(set.Rules), len(want.Rules))
	}
	for _, p := range problems {
		if p.Severity == lint.SeverityError {
			t.Errorf("unexpected error: %s", p)
		}
	}
}

// The other writer keeps the real files in a hidden directory and fills the
// root with symlinks naming them, which is how a mounted revision stays atomic
// (spec 10.2). Walked without passing over the hidden directory, every rule is
// reached twice and each is reported as the other's duplicate.
func TestLoadReadsAMountedRevisionOnce(t *testing.T) {
	dir := mountedRevision(t)

	want, _ := Load(filepath.Join("testdata", "rules"), loadSources(t), nil)
	if len(want.Rules) == 0 {
		t.Fatalf("the fixture holds no rules, so this proves nothing")
	}

	set, problems := Load(dir, loadSources(t), nil)

	if len(set.Rules) != len(want.Rules) {
		t.Errorf("rules in the mount = %d, want %d", len(set.Rules), len(want.Rules))
	}
	for _, p := range problems {
		if p.Check == "rule/duplicate-alert" {
			t.Errorf("a rule was read through the symlink and again behind it: %s", p)
		}
	}
}

// The same mount, and the identity it produces. The real files sit in a
// directory kubelet names after the moment it wrote them, so an identity
// carrying that name is renamed by an edit to an unrelated key in the same
// object (spec 8.2).
func TestGroupIDUnderAMountedRevisionNamesNeitherTheRevisionNorTheRoot(t *testing.T) {
	dir := mountedRevision(t)

	set, _ := Load(dir, loadSources(t), nil)

	want := []string{"errors.yaml:errors", "latency.yaml:api-latency"}
	if got := groupIDs(set); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("groups loaded from the mount = %v, want %v", got, want)
	}
}

// groupIDs is every group identity in a set, deduplicated and sorted, which is
// what the rule_group label on every metric carries (spec 8.2).
func groupIDs(set *Set) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range set.Rules {
		if id := r.GroupID(); !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// A rule's identity has to survive the root moving underneath it. git-sync
// publishes a revision as a symlink at a worktree named after the commit and
// the loader resolves that symlink before it walks (spec 10.2), so an identity
// built from the resolved path renames every series the ruler exposes on every
// merge (spec 8.2).
func TestGroupIDIsThePathRelativeToTheRulesRoot(t *testing.T) {
	real, err := filepath.Abs(filepath.Join("testdata", "rules"))
	if err != nil {
		t.Fatalf("resolving the fixture: %v", err)
	}

	link := filepath.Join(t.TempDir(), "current")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("linking the revision: %v", err)
	}

	want := []string{
		filepath.Join("payments", "latency.yaml") + ":api-latency",
		filepath.Join("search", "errors.yaml") + ":errors",
	}

	for _, root := range []string{real, link} {
		set, _ := Load(root, loadSources(t), nil)

		got := groupIDs(set)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("groups loaded from %s = %v, want %v", root, got, want)
		}
	}
}

// The identity a finding is addressed with is the same path, because an
// absolute path inside a container is not one the rule's author can open
// (spec 8.2).
func TestRulePathIsRelativeToTheRulesRoot(t *testing.T) {
	set, _ := Load(filepath.Join("testdata", "rules"), loadSources(t), nil)
	if len(set.Rules) == 0 {
		t.Fatal("the fixture holds no rules, so this proves nothing")
	}

	for _, r := range set.Rules {
		if filepath.IsAbs(r.Path) {
			t.Errorf("rule %s carries path %q, which names where this checkout happens to be", r.Alert, r.Path)
		}
		if !strings.HasSuffix(r.File, r.Path) {
			t.Errorf("rule %s is at %q and carries path %q", r.Alert, r.File, r.Path)
		}
	}
}

// mountedRevision builds the layout kubelet writes a ConfigMap as: the real
// files in a directory named after the moment they were written, `..data`
// pointing at it, and a symlink at the root for every key.
func mountedRevision(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	const revision = "..2026_09_28_12_00_00.1234"

	if err := os.Mkdir(filepath.Join(dir, revision), 0o750); err != nil {
		t.Fatalf("making the revision: %v", err)
	}

	// The keys of a mounted object are file names, so the tree the fixture
	// keeps in team directories arrives flat.
	for _, name := range []string{
		filepath.Join("payments", "latency.yaml"),
		filepath.Join("search", "errors.yaml"),
	} {
		data, err := os.ReadFile(filepath.Join("testdata", "rules", name))
		if err != nil {
			t.Fatalf("reading the fixture: %v", err)
		}
		base := filepath.Base(name)
		if err := os.WriteFile(filepath.Join(dir, revision, base), data, 0o600); err != nil {
			t.Fatalf("writing the revision: %v", err)
		}
		if err := os.Symlink(filepath.Join("..data", base), filepath.Join(dir, base)); err != nil {
			t.Fatalf("linking a rule file: %v", err)
		}
	}
	if err := os.Symlink(revision, filepath.Join(dir, "..data")); err != nil {
		t.Fatalf("linking the revision: %v", err)
	}
	return dir
}

// copyTree copies a rules fixture so a test can edit a file in it.
func copyTree(t *testing.T, from string) string {
	t.Helper()

	dir := t.TempDir()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, rel), 0o750)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, rel), data, 0o600)
	})
	if err != nil {
		t.Fatalf("copying the fixture: %v", err)
	}
	return dir
}

// The revision the ruler can state honestly: a hash of what it read. It has to
// be the same for the same files however the root they arrived under is
// spelled, because that is the question it answers, whether two replicas are
// evaluating the same rules (spec 8.2).
func TestRevisionIsAHashOfTheFilesThatLoaded(t *testing.T) {
	dir := copyTree(t, filepath.Join("testdata", "rules"))

	link := filepath.Join(t.TempDir(), "current")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatalf("linking the revision: %v", err)
	}

	set, _ := Load(dir, loadSources(t), nil)
	if set.Revision == "" {
		t.Fatal("the set states no revision")
	}

	through, _ := Load(link, loadSources(t), nil)
	if through.Revision != set.Revision {
		t.Errorf("revision through the symlink = %q, want %q: the files are the same files",
			through.Revision, set.Revision)
	}

	rule := filepath.Join(dir, "payments", "latency.yaml")
	data, err := os.ReadFile(rule)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rule, append(data, []byte("\n# edited\n")...), 0o600); err != nil {
		t.Fatal(err)
	}

	edited, _ := Load(dir, loadSources(t), nil)
	if edited.Revision == set.Revision {
		t.Errorf("revision after editing a rule file = %q, want it to have changed", edited.Revision)
	}
}

// A file's place in the tree is part of the revision, so moving a rule from one
// team's directory to another is a configuration change rather than the same
// one by another name.
func TestRevisionCoversWhereAFileSitsAndNotOnlyItsContents(t *testing.T) {
	dir := copyTree(t, filepath.Join("testdata", "rules"))
	set, _ := Load(dir, loadSources(t), nil)

	from := filepath.Join(dir, "payments", "latency.yaml")
	to := filepath.Join(dir, "search", "latency.yaml")
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}

	moved, _ := Load(dir, loadSources(t), nil)
	if moved.Revision == set.Revision {
		t.Errorf("revision after moving a rule file = %q, want it to have changed", moved.Revision)
	}
}
