package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// twoTeams is a tree with a rule under each of two teams, so a path can keep
// one team's findings and drop the other's.
func twoTeams(t *testing.T, rule string) (dir string) {
	t.Helper()

	dir = fixture(t, rule, "")
	search := filepath.Join(dir, "rules", "search")
	if err := os.MkdirAll(search, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(search, "latency.yaml"), []byte(rule), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A path reports that file's findings and leaves the rest of the tree alone,
// which is the desk question --changed-since does not answer (spec 10.3).
func TestCheckPathNarrowsToOneFile(t *testing.T) {
	dir := twoTeams(t, brokenRule)

	code, stdout, stderr := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--format", "json",
		filepath.Join(dir, "rules"),
		filepath.Join(dir, "rules", "search", "latency.yaml"))

	if code != exitFinding {
		t.Errorf("exit = %d, want exitFinding for the named file's error\n%s", code, stdout)
	}

	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	files := filesIn(t, stdout)
	if !files[filepath.Join(root, "rules", "search", "latency.yaml")] {
		t.Errorf("the named file's finding is missing: %v", files)
	}
	if files[filepath.Join(root, "rules", "payments", "latency.yaml")] {
		t.Errorf("a file nobody named should be filtered out: %v", files)
	}
	if stderr != "" {
		t.Errorf("nothing to say when every path resolved, got: %s", stderr)
	}
}

// A directory is a path too: a team checking its own subtree is the same
// request as an author checking one file (spec 10.3).
func TestCheckPathNarrowsToASubtree(t *testing.T) {
	dir := twoTeams(t, brokenRule)

	code, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--format", "json",
		filepath.Join(dir, "rules"),
		filepath.Join(dir, "rules", "search"))

	if code != exitFinding {
		t.Errorf("exit = %d, want exitFinding\n%s", code, stdout)
	}

	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	files := filesIn(t, stdout)
	if !files[filepath.Join(root, "rules", "search", "latency.yaml")] {
		t.Errorf("the named subtree's finding is missing: %v", files)
	}
	if files[filepath.Join(root, "rules", "payments", "latency.yaml")] {
		t.Errorf("a subtree nobody named should be filtered out: %v", files)
	}
}

// The severity a finding carries is a fact about the tree, so a narrowed run
// has to report what a full run reports. This is what naming a subdirectory
// as the rules root gets wrong, since the instance policy sits beside the root
// (spec 7.7, 10.3).
func TestCheckPathKeepsTheTreesPolicy(t *testing.T) {
	const raised = `checks:
  labels/required:
    severity: error
`

	dir := twoTeams(t, bareRule)
	rules := filepath.Join(dir, "rules")

	// Beside the rules root, which is where the instance policy is read from
	// when nobody passes --config.
	if err := os.WriteFile(filepath.Join(rules, "ruler.yaml"), []byte(raised), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		rules, filepath.Join(rules, "search", "latency.yaml"))
	if code != exitFinding {
		t.Errorf("exit = %d, want exitFinding: the tree's policy raises labels/required\n%s", code, stdout)
	}

	// Naming the subtree as the root is the mistake these paths replace. The
	// instance policy is no longer beside the root, so the same rule is only a
	// warning and the author is told nothing blocks.
	code, stdout, _ = runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"), filepath.Join(rules, "search"))
	if code != 0 {
		t.Errorf("exit = %d, want 0: rooting at the subtree loses the tree's policy\n%s", code, stdout)
	}
}

// Both filters answer different questions, so both apply: the changed files
// among the paths asked for (spec 10.3).
func TestCheckPathAndChangedSinceIntersect(t *testing.T) {
	dir := fixture(t, brokenRule, "")
	gitRepo(t, dir)

	search := filepath.Join(dir, "rules", "search")
	if err := os.MkdirAll(search, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(search, "latency.yaml"), []byte(brokenRule), 0o600); err != nil {
		t.Fatal(err)
	}

	// search/latency.yaml is the changed file; payments/latency.yaml is the
	// named path. Nothing is both, so nothing is reported.
	code, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--format", "json",
		"--changed-since", "HEAD",
		filepath.Join(dir, "rules"),
		filepath.Join(dir, "rules", "payments", "latency.yaml"))

	if code != 0 {
		t.Errorf("exit = %d, want 0: no file is both changed and named\n%s", code, stdout)
	}
	if files := filesIn(t, stdout); len(files) != 0 {
		t.Errorf("findings = %v, want none", files)
	}
}

// A --changed-since that widened to everything does not widen past a path the
// author asked for explicitly (spec 10.3).
func TestCheckPathHoldsWhenChangedSinceWidens(t *testing.T) {
	dir := twoTeams(t, brokenRule)
	gitRepo(t, dir)

	code, stdout, stderr := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--format", "json",
		"--changed-since", "origin/nope",
		filepath.Join(dir, "rules"),
		filepath.Join(dir, "rules", "search", "latency.yaml"))

	if code != exitFinding {
		t.Errorf("exit = %d, want exitFinding for the named file\n%s", code, stdout)
	}
	if !strings.Contains(stderr, "origin/nope") {
		t.Errorf("stderr should still name the base it could not resolve, got: %s", stderr)
	}

	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if files := filesIn(t, stdout); files[filepath.Join(root, "rules", "payments", "latency.yaml")] {
		t.Errorf("the widened run should not reach past the named path: %v", files)
	}
}

// Reporting nothing for a path nobody can match is indistinguishable from a
// clean run, which is the worst answer a check command has (spec 10.3).
func TestCheckPathUsageErrors(t *testing.T) {
	dir := twoTeams(t, brokenRule)
	rules := filepath.Join(dir, "rules")

	tests := []struct {
		name string
		path string
		says string
	}{
		{"no such file", filepath.Join(rules, "payments", "typo.yaml"), "typo.yaml"},
		{"no rule file under it", filepath.Join(rules, "payments", "latency.yaml", "deeper"), "deeper"},
		{"outside the rules directory", filepath.Join(dir, "sources.yaml"), "sources.yaml"},
		{"the policy file", filepath.Join(dir, "ruler.yaml"), "ruler.yaml"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCheck(t, "check",
				"--sources", filepath.Join(dir, "sources.yaml"), rules, tc.path)

			if code != exitUsage {
				t.Errorf("exit = %d, want exitUsage\n%s\n%s", code, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.says) {
				t.Errorf("stderr should name the path, got: %s", stderr)
			}
		})
	}
}

// With no paths the command behaves exactly as it did, which is what the
// loader does at startup (spec 10.3).
func TestCheckNoPathsReportsTheWholeTree(t *testing.T) {
	dir := twoTeams(t, brokenRule)

	code, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--format", "json",
		filepath.Join(dir, "rules"))

	if code != exitFinding {
		t.Errorf("exit = %d, want exitFinding\n%s", code, stdout)
	}

	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	files := filesIn(t, stdout)
	for _, team := range []string{"payments", "search"} {
		if !files[filepath.Join(root, "rules", team, "latency.yaml")] {
			t.Errorf("%s's finding is missing: %v", team, files)
		}
	}
}
