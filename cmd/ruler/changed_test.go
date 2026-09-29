package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo turns a fixture tree into a repository with everything committed, so
// a test can then change one file and ask what the branch touched.
func gitRepo(t *testing.T, dir string) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"add", "."},
		{"commit", "-m", "base"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// filesIn is the set of files the findings are anchored on. Read from the JSON
// feed rather than from the text output, because a finding's message can name
// another file: rule/duplicate-alert names the rule it collides with.
func filesIn(t *testing.T, stdout string) map[string]bool {
	t.Helper()

	var findings []struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal([]byte(stdout), &findings); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}

	files := make(map[string]bool)
	for _, f := range findings {
		files[f.File] = true
	}
	return files
}

// --changed-since reports the findings in the files a branch touched, and
// leaves the ones an author did not write alone (spec 10.3).
func TestCheckChangedSinceNarrowsToChangedFiles(t *testing.T) {
	dir := fixture(t, brokenRule, "")
	gitRepo(t, dir)

	other := filepath.Join(dir, "rules", "search")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "latency.yaml"), []byte(brokenRule), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--format", "json",
		"--changed-since", "HEAD",
		filepath.Join(dir, "rules"))

	if code != exitFinding {
		t.Errorf("exit = %d, want exitFinding for the changed file's error\n%s", code, stdout)
	}

	// The loader resolves the rules root, so a finding carries the real path
	// and a temporary directory on macOS is reached through a symlink.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	files := filesIn(t, stdout)
	if !files[filepath.Join(root, "rules", "search", "latency.yaml")] {
		t.Errorf("the changed file's finding is missing: %v", files)
	}
	if files[filepath.Join(root, "rules", "payments", "latency.yaml")] {
		t.Errorf("an untouched file's finding should be filtered out: %v", files)
	}
	if stderr != "" {
		t.Errorf("nothing to say when the base resolved, got: %s", stderr)
	}
}

// An unresolvable base checks everything and says so. Filtering that silently
// checks nothing is what makes a green build meaningless (spec 10.3).
func TestCheckChangedSinceUnresolvableBaseChecksEverything(t *testing.T) {
	dir := fixture(t, brokenRule, "")
	gitRepo(t, dir)

	code, stdout, stderr := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--changed-since", "origin/nope",
		filepath.Join(dir, "rules"))

	if code != exitFinding {
		t.Errorf("exit = %d, want exitFinding: everything is checked\n%s", code, stdout)
	}
	if !strings.Contains(stdout, filepath.Join("payments", "latency.yaml")+":") {
		t.Errorf("every finding is reported when the base will not resolve:\n%s", stdout)
	}
	if !strings.Contains(stderr, "origin/nope") {
		t.Errorf("stderr should name the base it could not resolve, got: %s", stderr)
	}
}

// A policy file named by --config widens the run, whatever it is called: it
// can raise a check for rules the diff does not name (spec 7.7, 10.3).
func TestCheckChangedSinceWidensOnThePolicyFile(t *testing.T) {
	dir := fixture(t, brokenRule, "")
	policyPath := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(policyPath, []byte("checks: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRepo(t, dir)

	if err := os.WriteFile(policyPath, []byte("checks:\n  rule/select-star:\n    severity: error\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--config", policyPath,
		"--changed-since", "HEAD",
		filepath.Join(dir, "rules"))

	if code != exitFinding {
		t.Errorf("exit = %d, want exitFinding: the policy change widens the run\n%s", code, stdout)
	}
	if !strings.Contains(stdout, filepath.Join("payments", "latency.yaml")+":") {
		t.Errorf("an untouched rule stays in scope once policy changed:\n%s", stdout)
	}
	if !strings.Contains(stderr, "policy.yaml") {
		t.Errorf("stderr should name the file that widened the run, got: %s", stderr)
	}
}
