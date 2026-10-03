package lint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo builds a git repository with a base commit, so each test can branch
// from it. Real git rather than a stub: what is being tested is which commit
// git calls the merge base, and a stub would only assert our own arithmetic.
func repo(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	dir := t.TempDir()

	gitIn(t, dir, "init", "--initial-branch=main")
	writeFile(t, dir, "rules/payments/latency.yaml", "base\n")
	writeFile(t, dir, "rules/search/latency.yaml", "base\n")
	writeFile(t, dir, "sources.yaml", "base\n")
	writeFile(t, dir, "rules/payments/ruler.yaml", "base\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "base")
	return dir
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// problemsIn names the files a filter kept, so a test reads as a set of paths
// rather than as a list of findings.
func problemsIn(dir string, rels ...string) []Problem {
	var out []Problem
	for _, rel := range rels {
		out = append(out, Problem{
			File: filepath.Join(dir, rel), Line: 1,
			Check: "rule/expr", Severity: SeverityError, Text: "text",
		})
	}
	return out
}

func keptFiles(dir string, f Filter, rels ...string) map[string]bool {
	kept := make(map[string]bool)
	for _, p := range f.Keep(problemsIn(dir, rels...)) {
		rel, err := filepath.Rel(dir, p.File)
		if err != nil {
			rel = p.File
		}
		kept[rel] = true
	}
	return kept
}

// The base is the merge base, not the previous commit. A branch with several
// commits, or a base that has moved on, gets the wrong answer from HEAD~1
// (spec 10.3).
func TestChangedSinceUsesTheMergeBase(t *testing.T) {
	dir := repo(t)

	// The base branch moves on after the branch point. A file only it touched
	// is not this pull request's to answer for.
	writeFile(t, dir, "rules/search/latency.yaml", "main moved on\n")
	gitIn(t, dir, "commit", "-am", "on main")
	gitIn(t, dir, "branch", "base-tip")
	gitIn(t, dir, "checkout", "-b", "work", "HEAD~1")

	// Two commits on the branch, so HEAD~1 would only see the second.
	writeFile(t, dir, "rules/payments/first.yaml", "one\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "first")
	writeFile(t, dir, "rules/payments/second.yaml", "two\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "second")

	f := ChangedSince(dir, "base-tip", nil)
	if f.Note != "" {
		t.Fatalf("Note = %q, want no note when the base resolved", f.Note)
	}

	kept := keptFiles(dir, f,
		"rules/payments/first.yaml",
		"rules/payments/second.yaml",
		"rules/search/latency.yaml")

	for _, want := range []string{"rules/payments/first.yaml", "rules/payments/second.yaml"} {
		if !kept[want] {
			t.Errorf("%s was dropped, both branch commits are in scope: %v", want, kept)
		}
	}
	if kept["rules/search/latency.yaml"] {
		t.Errorf("a file only the base branch changed is not in scope: %v", kept)
	}
}

// A change to the sources file can move a source's labels, so a rule in an
// untouched file stops matching, starts matching, or matches a different
// cluster. The filter widens rather than guessing which rules it reached
// (spec 10.3).
func TestChangedSinceWidensOnTheSourcesFile(t *testing.T) {
	dir := repo(t)
	gitIn(t, dir, "branch", "base-tip")
	writeFile(t, dir, "sources.yaml", "changed\n")

	f := ChangedSince(dir, "base-tip", []string{filepath.Join(dir, "sources.yaml")})
	if f.Note == "" {
		t.Error("widening to a full run must say why")
	}

	kept := keptFiles(dir, f, "rules/search/latency.yaml")
	if !kept["rules/search/latency.yaml"] {
		t.Errorf("an untouched rule stays in scope once sources changed: %v", kept)
	}
}

// A policy file can raise a check, so a rule that warned yesterday blocks
// today (spec 7.7, 10.3). Any ruler.yaml, not only the one named by --config.
func TestChangedSinceWidensOnAPolicyFile(t *testing.T) {
	dir := repo(t)
	gitIn(t, dir, "branch", "base-tip")
	writeFile(t, dir, "rules/payments/ruler.yaml", "changed\n")

	f := ChangedSince(dir, "base-tip", nil)
	if f.Note == "" {
		t.Error("widening to a full run must say why")
	}

	kept := keptFiles(dir, f, "rules/search/latency.yaml")
	if !kept["rules/search/latency.yaml"] {
		t.Errorf("an untouched rule stays in scope once policy changed: %v", kept)
	}
}

// A base that will not resolve is a reason to do more work, never less.
// Filtering that silently checks nothing is what makes a green build
// meaningless (spec 10.3).
func TestChangedSinceUnresolvableBaseChecksEverything(t *testing.T) {
	dir := repo(t)

	f := ChangedSince(dir, "origin/nope", nil)
	if f.Note == "" {
		t.Error("an unresolvable base must say so")
	}

	kept := keptFiles(dir, f, "rules/search/latency.yaml")
	if !kept["rules/search/latency.yaml"] {
		t.Errorf("everything is checked when the base will not resolve: %v", kept)
	}
}

// Not a repository at all lands in the same place, for the same reason.
func TestChangedSinceOutsideARepositoryChecksEverything(t *testing.T) {
	dir := t.TempDir()

	f := ChangedSince(dir, "main", nil)
	if f.Note == "" {
		t.Error("a directory that is not a repository must say so")
	}
	if kept := keptFiles(dir, f, "rules/a.yaml"); !kept["rules/a.yaml"] {
		t.Errorf("everything is checked outside a repository: %v", kept)
	}
}

// A rule written and not yet committed is exactly the rule its author wants
// checked, so the untracked files count as changed.
func TestChangedSinceIncludesUntrackedFiles(t *testing.T) {
	dir := repo(t)
	gitIn(t, dir, "branch", "base-tip")
	writeFile(t, dir, "rules/payments/draft.yaml", "draft\n")

	f := ChangedSince(dir, "base-tip", nil)
	kept := keptFiles(dir, f, "rules/payments/draft.yaml", "rules/search/latency.yaml")

	if !kept["rules/payments/draft.yaml"] {
		t.Errorf("an uncommitted rule file is in scope: %v", kept)
	}
	if kept["rules/search/latency.yaml"] {
		t.Errorf("an untouched rule is not: %v", kept)
	}
}

func TestPathsMatchesFilesAndSubtrees(t *testing.T) {
	dir := t.TempDir()
	payments := writeRule(t, dir, "payments", "latency.yaml")
	search := writeRule(t, dir, "search", "latency.yaml")
	errors := writeRule(t, dir, "search", "errors.yaml")
	files := []string{payments, search, errors}

	problems := []Problem{
		{File: payments, Check: "rule/expr"},
		{File: search, Check: "rule/expr"},
		{File: errors, Check: "rule/expr"},
	}

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"one file", []string{search}, []string{search}},
		{"a subtree", []string{filepath.Join(dir, "search")}, []string{search, errors}},
		{"two paths", []string{payments, errors}, []string{payments, errors}},
		{"the root", []string{dir}, []string{payments, search, errors}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			filter, err := Paths(files, tc.args)
			if err != nil {
				t.Fatalf("Paths: %v", err)
			}

			kept := map[string]bool{}
			for _, p := range filter.Keep(problems) {
				kept[p.File] = true
			}
			if len(kept) != len(tc.want) {
				t.Fatalf("kept %v, want %v", kept, tc.want)
			}
			for _, path := range tc.want {
				if !kept[path] {
					t.Errorf("%s was dropped, kept %v", path, kept)
				}
			}
		})
	}
}

// writeRule puts a file where the loader would have walked one, so that a
// path and a file resolve the same way: a temporary directory on macOS is
// reached through a symlink, and a path that does not exist cannot resolve it.
func writeRule(t *testing.T, dir, team, name string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(dir, team), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, team, name)
	if err := os.WriteFile(path, []byte("groups: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A path matching nothing the loader read can only report nothing, which is
// indistinguishable from a clean run, so it is an error rather than a silent
// narrowing (spec 10.3).
func TestPathsRefusesWhatTheLoaderNeverRead(t *testing.T) {
	dir := t.TempDir()
	files := []string{writeRule(t, dir, "payments", "latency.yaml")}

	for _, arg := range []string{
		filepath.Join(dir, "payments", "typo.yaml"),
		filepath.Join(dir, "search"),
		filepath.Join(dir, "sources.yaml"),
		filepath.Join(dir, "..", "elsewhere"),
	} {
		if _, err := Paths(files, []string{arg}); err == nil {
			t.Errorf("Paths(%q) = nil error, want one", arg)
		} else if !strings.Contains(err.Error(), arg) {
			t.Errorf("error should name the path, got: %v", err)
		}
	}
}

// No paths is the zero filter, which keeps everything: that is what the
// loader reads at startup (spec 10.3).
func TestPathsWithNoArgumentsKeepsEverything(t *testing.T) {
	problems := []Problem{{File: "rules/payments/latency.yaml", Check: "rule/expr"}}

	filter, err := Paths(nil, nil)
	if err != nil {
		t.Fatalf("Paths: %v", err)
	}
	if got := filter.Keep(problems); len(got) != 1 {
		t.Errorf("kept %d problems, want 1", len(got))
	}
}
