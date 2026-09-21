package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// Diagnostics for run live on stderr, so that is what the assertions read.
func runRunCmd(t *testing.T, args ...string) (code int, stderr string) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, errOut.String()
}

// --rules and --alertmanager are the two flags run cannot do without: no
// rules directory means nothing to evaluate, and no Alertmanager means
// nowhere to send a firing alert.
func TestRunRequiresRulesAndAlertmanagerFlags(t *testing.T) {
	dir := fixture(t, bareRule, "")

	code, stderr := runRunCmd(t, "run",
		"--sources", filepath.Join(dir, "sources.yaml"))
	if code != exitUsage {
		t.Errorf("exit = %d, want exitUsage when --rules and --alertmanager are missing\n%s", code, stderr)
	}

	code, stderr = runRunCmd(t, "run",
		"--rules", filepath.Join(dir, "rules"),
		"--sources", filepath.Join(dir, "sources.yaml"))
	if code != exitUsage {
		t.Errorf("exit = %d, want exitUsage when --alertmanager is missing\n%s", code, stderr)
	}
}

// The same correctness bar as `ruler check`: a rule that cannot run at all
// must not be allowed to start a ruler that would silently never evaluate
// it (spec 7.6).
func TestRunRefusesToStartOnAnErrorSeverityFinding(t *testing.T) {
	dir := fixture(t, brokenRule, "")

	code, stderr := runRunCmd(t, "run",
		"--rules", filepath.Join(dir, "rules"),
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--alertmanager", "http://127.0.0.1:9093")

	if code != exitFinding {
		t.Errorf("exit = %d, want exitFinding\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "rule/expr") {
		t.Errorf("expected the rule/expr finding on stderr, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "refusing to start") {
		t.Errorf("expected a refusal message, got:\n%s", stderr)
	}
}
