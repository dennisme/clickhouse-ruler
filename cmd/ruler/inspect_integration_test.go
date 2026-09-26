//go:build integration

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
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

// The whole point of an exemption, end to end: a rule that really does fail a
// check against a real cluster stops being reported for it, because the
// source owner said this cluster expects it.
func TestCheckOnlineHonoursASourceExemption(t *testing.T) {
	dir := t.TempDir()
	// Selects * and still returns a value column, so the only thing wrong
	// with it is the check being exempted.
	rules := writeRule(t, dir, `SELECT *, 1 AS value FROM otel.otel_traces
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}`)

	sources := writeSources(t, dir, "ruler_payments")
	body, err := os.ReadFile(sources) //nolint:gosec // a path this test just wrote
	if err != nil {
		t.Fatalf("reading sources: %v", err)
	}
	exempting := string(body) + `    exempt:
      - check: rule/select-star
        reason: this table's schema is frozen until it is retired
        until: 2099-01-01
`
	if err := os.WriteFile(sources, []byte(exempting), 0o600); err != nil {
		t.Fatalf("writing sources: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"check", "--sources", sources, "--online", rules}, &stdout, &stderr)
	out := stdout.String() + stderr.String()

	if code != exitOK {
		t.Errorf("exit = %d, want %d: %s", code, exitOK, out)
	}
	if strings.Contains(out, "rule/select-star") {
		t.Errorf("the exempted check was still reported: %s", out)
	}
}

// The same rule, the same cluster, an expired exemption: the finding comes
// back and the expiry is reported on top of it.
func TestCheckOnlineReportsAnExpiredExemption(t *testing.T) {
	dir := t.TempDir()
	// Selects * and still returns a value column, so the only thing wrong
	// with it is the check being exempted.
	rules := writeRule(t, dir, `SELECT *, 1 AS value FROM otel.otel_traces
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}`)

	sources := writeSources(t, dir, "ruler_payments")
	body, err := os.ReadFile(sources) //nolint:gosec // a path this test just wrote
	if err != nil {
		t.Fatalf("reading sources: %v", err)
	}
	expired := string(body) + `    exempt:
      - check: rule/select-star
        reason: this table's schema was frozen while it was retired
        until: 2020-01-01
`
	if err := os.WriteFile(sources, []byte(expired), 0o600); err != nil {
		t.Fatalf("writing sources: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"check", "--sources", sources, "--online", rules}, &stdout, &stderr)
	out := stdout.String() + stderr.String()

	if code != exitFinding {
		t.Errorf("exit = %d, want %d: %s", code, exitFinding, out)
	}
	for _, want := range []string{"source/exemption", "rule/select-star"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should carry %q: %s", want, out)
		}
	}
}

// The table a pull request comment carries. One row per rule and source,
// with the estimate the cost check already asks for (spec 7.10).
func TestCheckWritesTheCostSummary(t *testing.T) {
	dir := t.TempDir()
	rules := writeRule(t, dir, workingExpr)
	sources := writeSources(t, dir, "ruler_payments")
	out := filepath.Join(dir, "summary.md")

	var stdout, stderr bytes.Buffer
	code := run([]string{"check", "--sources", sources, "--online", "--summary", out, rules},
		&stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d: %s%s", code, exitOK, stdout.String(), stderr.String())
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the summary: %v", err)
	}
	got := string(data)

	if !strings.Contains(got, "| File | Alert | Source | Rows | Interval |") {
		t.Errorf("no table heading in:\n%s", got)
	}
	if !strings.Contains(got, "| Probe | otel_traces |") {
		t.Errorf("no row for the rule against its source in:\n%s", got)
	}
	if !strings.Contains(got, "| 1m0s |") {
		t.Errorf("the group's interval is missing from:\n%s", got)
	}

	// A number, whatever it is. How much the fixture holds in the window is
	// the stack's business; that the cell is a count rather than an excuse is
	// this test's.
	if !regexp.MustCompile(`\| [0-9]+ \| 1m0s \|`).MatchString(got) {
		t.Errorf("the rule was not estimated:\n%s", got)
	}
}

// A rule within every ceiling still needs a number: the table reports what
// each rule costs, not only the ones the cost check complained about.
func TestCostSummaryReportsARuleWithNoCostCeiling(t *testing.T) {
	dir := t.TempDir()
	rules := writeRule(t, dir, workingExpr)
	sources := writeSources(t, dir, "ruler_payments")
	config := filepath.Join(dir, "ruler.yaml")
	if err := os.WriteFile(config, []byte("checks:\n  rule/cost:\n    severity: \"off\"\n"), 0o600); err != nil {
		t.Fatalf("writing policy: %v", err)
	}
	out := filepath.Join(dir, "summary.md")

	var stdout, stderr bytes.Buffer
	code := run([]string{"check", "--sources", sources, "--config", config,
		"--online", "--summary", out, rules}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d: %s%s", code, exitOK, stdout.String(), stderr.String())
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the summary: %v", err)
	}
	if !regexp.MustCompile(`\| [0-9]+ \| 1m0s \|`).MatchString(string(data)) {
		t.Errorf("the cost check being off left the row without a number:\n%s", data)
	}
}
