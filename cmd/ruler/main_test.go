package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture builds a rules tree on disk so the CLI is exercised the way a user
// runs it, through arguments and an exit code, rather than through internals.
func fixture(t *testing.T, rule, config string) (dir string) {
	t.Helper()

	dir = t.TempDir()
	rules := filepath.Join(dir, "rules", "payments")
	if err := os.MkdirAll(rules, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		if content == "" {
			return
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(rules, "latency.yaml"), rule)
	write(filepath.Join(dir, "sources.yaml"), sourcesYAML)
	write(filepath.Join(dir, "ruler.yaml"), config)
	return dir
}

const bareRule = `groups:
  - name: latency
    interval: 1m
    rules:
      - alert: HighLatency
        sources:
          team: payments
        expr: |
          SELECT ServiceName, max(Duration) AS value
          FROM otel.otel_traces
          WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
          GROUP BY ServiceName
`

const brokenRule = `groups:
  - name: latency
    interval: 1m
    rules:
      - alert: HighLatency
        sources:
          team: payments
        expr: "SELECT 1 AS value FROM t"
`

// Two groups sharing a name in one file. Valid YAML and valid rules; the
// only thing wrong is that the group identity is no longer unique.
const repeatedGroupRule = `groups:
  - name: latency
    interval: 1m
    rules:
      - alert: HighLatency
        sources:
          team: payments
        expr: "SELECT 1 AS value FROM t WHERE ts >= {{ .From }} AND ts < {{ .To }}"
  - name: latency
    interval: 1m
    rules:
      - alert: HighErrorRate
        sources:
          team: payments
        expr: "SELECT 1 AS value FROM t WHERE ts >= {{ .From }} AND ts < {{ .To }}"
`

const sourcesYAML = `sources:
  - name: otel_traces
    labels: {team: payments}
    address: 127.0.0.1:9000
    database: otel
    username: ruler
    table: otel_traces
    timestamp_column: Timestamp
`

func runCheck(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// The case the configurable checks exist for. A rule with no team, severity or
// annotations is valid, so warnings must not fail the build: a warning is the
// contributor's to act on and does not need a repo owner.
func TestCheckWarningsExitZero(t *testing.T) {
	dir := fixture(t, bareRule, "")

	code, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		filepath.Join(dir, "rules"))

	if code != 0 {
		t.Errorf("exit = %d, want 0 when only warnings exist\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "warning") {
		t.Errorf("warnings must still be visible, got:\n%s", stdout)
	}
}

// A query with no time bound scans without limit on every evaluation. That is
// correctness, not convention, so it blocks regardless of configuration.
func TestCheckErrorsExitNonZero(t *testing.T) {
	dir := fixture(t, brokenRule, "")

	code, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		filepath.Join(dir, "rules"))

	if code == 0 {
		t.Errorf("exit = 0, want non-zero when an error exists\n%s", stdout)
	}
	if !strings.Contains(stdout, "rule/expr") {
		t.Errorf("expected a rule/expr finding, got:\n%s", stdout)
	}
}

// Raising a check in policy turns the same warning into a blocking error.
func TestCheckPolicyRaisesSeverity(t *testing.T) {
	config := `checks:
  labels/required:
    severity: error
`
	dir := fixture(t, bareRule, config)

	code, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--config", filepath.Join(dir, "ruler.yaml"),
		filepath.Join(dir, "rules"))

	if code == 0 {
		t.Errorf("exit = 0, want non-zero once policy raises the check\n%s", stdout)
	}
}

func TestCheckGitHubFormat(t *testing.T) {
	dir := fixture(t, brokenRule, "")

	_, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--format", "github",
		filepath.Join(dir, "rules"))

	if !strings.HasPrefix(stdout, "::error file=") {
		t.Errorf("expected workflow commands, got:\n%s", stdout)
	}
	// Anything else on stdout would be rendered as a stray annotation.
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if !strings.HasPrefix(line, "::") {
			t.Errorf("non-command line on stdout in github mode: %q", line)
		}
	}
}

// Two teams may use the same alert name in their own files. Their alerts
// differ by team and source in the fingerprint, so this is not a clash, and
// blocking it would push authors into prefixing names with what already
// lives in labels (spec 6.3.1, 7.6).
func TestCheckAllowsDuplicateAlertNamesAcrossFiles(t *testing.T) {
	dir := fixture(t, bareRule, "")

	// A second file under a different directory, reusing the same name.
	other := filepath.Join(dir, "rules", "search")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "latency.yaml"), []byte(bareRule), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		filepath.Join(dir, "rules"))

	if code != exitOK {
		t.Errorf("exit = %d, want 0: the same alert name in two files is legitimate\n%s", code, stdout)
	}
	if strings.Contains(stdout, "rule/name") {
		t.Errorf("unexpected rule/name finding:\n%s", stdout)
	}
}

// A group name repeated inside one file is an error. The scheduler keys a
// group by (file, name), so two of them collapse onto one rule_group metric
// label and two goroutines report into a single series (spec 7.6, 8.2).
func TestCheckRejectsRepeatedGroupNameInOneFile(t *testing.T) {
	dir := fixture(t, repeatedGroupRule, "")

	code, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		filepath.Join(dir, "rules"))

	if code != exitFinding {
		t.Errorf("exit = %d, want exitFinding for a repeated group name\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "rule/group-name") {
		t.Errorf("expected a rule/group-name finding, got:\n%s", stdout)
	}
}

// --explain answers "why is this an error", which is the whole point of
// carrying the policy origin.
func TestCheckExplainNamesPolicyOrigin(t *testing.T) {
	config := `checks:
  labels/required:
    severity: error
`
	dir := fixture(t, bareRule, config)

	_, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--config", filepath.Join(dir, "ruler.yaml"),
		"--explain",
		filepath.Join(dir, "rules"))

	if !strings.Contains(stdout, "labels/required") {
		t.Errorf("explain should list the check, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "ruler.yaml") {
		t.Errorf("explain should name the policy file that set it, got:\n%s", stdout)
	}
}

func TestCheckRejectsUnknownFormat(t *testing.T) {
	dir := fixture(t, bareRule, "")

	code, _, stderr := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--format", "json",
		filepath.Join(dir, "rules"))

	if code == 0 {
		t.Error("exit = 0, want non-zero for an unknown format")
	}
	if !strings.Contains(stderr, "json") {
		t.Errorf("error should name the bad format, got: %s", stderr)
	}
}

func TestNoSubcommandFails(t *testing.T) {
	code, _, stderr := runCheck(t)
	if code == 0 {
		t.Error("exit = 0, want non-zero with no subcommand")
	}
	if !strings.Contains(stderr, "check") {
		t.Errorf("usage should mention the check subcommand, got: %s", stderr)
	}
}

// In github mode stdout is consumed by the workflow runner, so the human
// readable explanation belongs on stderr. Leaving it on stdout turns each of
// its lines into a stray annotation on the diff.
func TestCheckExplainStaysOffStdoutInGitHubMode(t *testing.T) {
	dir := fixture(t, bareRule, "")

	_, stdout, stderr := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--format", "github",
		"--explain",
		filepath.Join(dir, "rules"))

	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line != "" && !strings.HasPrefix(line, "::") {
			t.Errorf("non-command line on stdout in github mode: %q", line)
		}
	}
	if !strings.Contains(stderr, "labels/required") {
		t.Errorf("explanation should be on stderr, got: %s", stderr)
	}
}

// An exemption that has run out fails the build. The date is what makes an
// exemption a decision with an end: whoever renews it states the reason
// again, in front of a reviewer (spec 7.7).
func TestCheckBlocksAnExpiredExemption(t *testing.T) {
	dir := fixture(t, bareRule, "")

	expired := sourcesYAML + `    exempt:
      - check: rule/select-star
        reason: the schema here was frozen while the table was retired
        until: 2020-01-01
`
	if err := os.WriteFile(filepath.Join(dir, "sources.yaml"), []byte(expired), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		filepath.Join(dir, "rules"))

	if code == 0 {
		t.Errorf("exit = 0, want non-zero for an expired exemption\n%s", stdout)
	}
	if !strings.Contains(stdout, "source/exemption") {
		t.Errorf("expected a source/exemption finding, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "rule/select-star") {
		t.Errorf("the finding must name the check it exempted, got:\n%s", stdout)
	}
}

// An exemption is invisible until it drops a finding somebody expected, so
// --explain names it alongside the policy it sits beside (spec 7.8).
func TestExplainListsExemptions(t *testing.T) {
	dir := fixture(t, bareRule, "")

	exempting := sourcesYAML + `    exempt:
      - check: rule/select-star
        reason: the schema here is frozen until the table is retired
        until: 2099-01-01
`
	if err := os.WriteFile(filepath.Join(dir, "sources.yaml"), []byte(exempting), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--explain",
		filepath.Join(dir, "rules"))

	for _, want := range []string{"exempt", "rule/select-star", "2099-01-01", "frozen"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("explain should carry %q, got:\n%s", want, stdout)
		}
	}
}
