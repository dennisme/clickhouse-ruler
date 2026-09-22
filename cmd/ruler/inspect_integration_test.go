//go:build integration

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRule writes a one-rule directory, so a test can point `ruler check`
// at a specific query rather than at the shared fixture.
func writeRule(t *testing.T, dir, expr string) string {
	t.Helper()

	rules := filepath.Join(dir, "rules")
	if err := os.MkdirAll(rules, 0o750); err != nil {
		t.Fatalf("creating rules dir: %v", err)
	}

	body := "groups:\n" +
		"  - name: probe\n" +
		"    interval: 1m\n" +
		"    rules:\n" +
		"      - alert: Probe\n" +
		"        sources: {team: payments}\n" +
		"        expr: |\n"
	for _, line := range strings.Split(strings.TrimSpace(expr), "\n") {
		body += "          " + line + "\n"
	}
	body += "        labels: {team: payments, severity: warning}\n" +
		"        annotations: {summary: probe, runbook_url: 'https://example.com/r'}\n"

	if err := os.WriteFile(filepath.Join(rules, "probe.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing rule: %v", err)
	}
	return rules
}

func checkRule(t *testing.T, expr string, args ...string) (int, string) {
	t.Helper()

	dir := t.TempDir()
	rules := writeRule(t, dir, expr)
	sources := writeSources(t, dir, "ruler_payments")

	var stdout, stderr bytes.Buffer
	argv := append([]string{"check", "--sources", sources, "--online"}, args...)
	argv = append(argv, rules)

	code := run(argv, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

const workingExpr = `SELECT ServiceName, max(Duration) AS value
FROM otel.otel_traces
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
GROUP BY ServiceName`

func TestCheckOnlineAcceptsAWorkingRule(t *testing.T) {
	code, out := checkRule(t, workingExpr)

	if code != exitOK {
		t.Errorf("exit = %d, want %d: %s", code, exitOK, out)
	}
	for _, check := range []string{"rule/select-star", "rule/table-function", "rule/nondeterministic", "rule/syntax"} {
		if strings.Contains(out, check) {
			t.Errorf("output reports %s for a working rule: %s", check, out)
		}
	}
}

// A table function blocks by default, because it is the only preventive
// control for the ones no privilege gates (spec 6.7.1).
func TestCheckOnlineBlocksATableFunction(t *testing.T) {
	code, out := checkRule(t, `SELECT number AS value FROM numbers(10) WHERE {{ .From }} <= {{ .To }}`)

	if code != exitFinding {
		t.Errorf("exit = %d, want %d: %s", code, exitFinding, out)
	}
	if !strings.Contains(out, "rule/table-function") || !strings.Contains(out, "numbers") {
		t.Errorf("output does not report numbers(): %s", out)
	}
	// The source is named because a rule can match several and fail against
	// only one of them.
	if !strings.Contains(out, "otel_traces") {
		t.Errorf("output does not name the source: %s", out)
	}
}

// An operator permits the one function they actually need, and then the same
// rule passes.
func TestCheckOnlineHonoursTheAllowlist(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "ruler.yaml")
	body := "checks:\n  rule/table-function:\n    keys: [numbers]\n"
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatalf("writing policy: %v", err)
	}

	code, out := checkRule(t,
		`SELECT number AS value FROM numbers(10) WHERE {{ .From }} <= {{ .To }}`,
		"--config", config)

	if code != exitOK {
		t.Errorf("exit = %d, want %d: %s", code, exitOK, out)
	}
}

// SELECT * and now() warn rather than block: both rules evaluate, they just
// evaluate badly.
func TestCheckOnlineWarnsAboutSelectStarAndNondeterminism(t *testing.T) {
	code, out := checkRule(t, `SELECT *, now() AS value FROM otel.otel_traces WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}`)

	if code != exitOK {
		t.Errorf("exit = %d, want %d at the default severities: %s", code, exitOK, out)
	}
	for _, want := range []string{"rule/select-star", "rule/nondeterministic", "warning"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q: %s", want, out)
		}
	}
}

// A rule whose SQL will not parse cannot run, so it blocks whatever policy
// says, and nothing else is reported about it.
func TestCheckOnlineBlocksUnparseableSQL(t *testing.T) {
	code, out := checkRule(t, `SELECT * FROM WHERE {{ .From }} {{ .To }}`)

	if code != exitFinding {
		t.Errorf("exit = %d, want %d: %s", code, exitFinding, out)
	}
	if !strings.Contains(out, "rule/syntax") {
		t.Errorf("output does not report rule/syntax: %s", out)
	}
	if strings.Contains(out, "rule/select-star") {
		t.Errorf("output reports other checks about an unparseable rule: %s", out)
	}
}

// Without --online nothing reads the query, so CI with no cluster is
// unchanged.
func TestCheckStaysOfflineForQueryChecks(t *testing.T) {
	dir := t.TempDir()
	rules := writeRule(t, dir, `SELECT number AS value FROM numbers(10) WHERE {{ .From }} <= {{ .To }}`)
	sources := writeSources(t, dir, "ruler_payments")

	var stdout, stderr bytes.Buffer
	code := run([]string{"check", "--sources", sources, rules}, &stdout, &stderr)

	out := stdout.String() + stderr.String()
	if code != exitOK {
		t.Errorf("exit = %d, want %d: %s", code, exitOK, out)
	}
	if strings.Contains(out, "rule/table-function") {
		t.Errorf("output reports a query check without --online: %s", out)
	}
}
