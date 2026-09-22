//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
)

// writeSources writes a sources file naming one ClickHouse user, so a test
// can point the contract check at a user that meets it and one that does not.
func writeSources(t *testing.T, dir, username string) string {
	t.Helper()

	addr := os.Getenv("RULER_CLICKHOUSE_ADDR")
	if addr == "" {
		t.Fatal("RULER_CLICKHOUSE_ADDR is not set, run these through `just integration`")
	}

	path := filepath.Join(dir, "sources.yaml")
	body := "sources:\n" +
		"  - name: otel_traces\n" +
		"    labels: {team: payments}\n" +
		"    address: " + addr + "\n" +
		"    database: otel\n" +
		"    username: " + username + "\n" +
		"    table: otel_traces\n" +
		"    timestamp_column: Timestamp\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing sources: %v", err)
	}
	return path
}

func checkOnline(t *testing.T, sourcesPath string, args ...string) (int, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	argv := append([]string{"check", "--sources", sourcesPath, "--online"}, args...)
	argv = append(argv, filepath.Join("testdata", "rules"))

	code := run(argv, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// The reference user meets the contract, so the check that exists to report a
// gap has nothing to say about it.
func TestCheckOnlineSaysNothingAboutACompliantUser(t *testing.T) {
	path := writeSources(t, t.TempDir(), "ruler_payments")

	code, out := checkOnline(t, path)
	if code != exitOK {
		t.Errorf("exit = %d, want %d: %s", code, exitOK, out)
	}
	if strings.Contains(out, "source/privileges") {
		t.Errorf("output mentions the check, want silence: %s", out)
	}
}

// The silent failure, reported. Every assertion that fails is named, so an
// operator reads which half of the contract is missing rather than that
// something about the user is wrong.
func TestCheckOnlineReportsAnOverPrivilegedUser(t *testing.T) {
	path := writeSources(t, t.TempDir(), "ruler_wide")

	// A warning by default, so the check reports and the deploy carries on.
	code, out := checkOnline(t, path)
	if code != exitOK {
		t.Errorf("exit = %d, want %d at the default severity: %s", code, exitOK, out)
	}

	for _, want := range []string{"sources-revoked", "readonly", "constraints", "warning"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q: %s", want, out)
		}
	}
	// The grant that is there is not a finding. Too many privileges and too
	// few are different problems.
	if strings.Contains(out, "table-readable") {
		t.Errorf("output reports table-readable, which this user satisfies: %s", out)
	}
}

// An operator who wants the contract enforced raises the severity, and then
// the same finding blocks (spec 7.6).
func TestCheckOnlineBlocksWhenPolicyRaisesTheSeverity(t *testing.T) {
	dir := t.TempDir()
	path := writeSources(t, dir, "ruler_wide")

	config := filepath.Join(dir, "ruler.yaml")
	body := "checks:\n  source/privileges:\n    severity: error\n"
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatalf("writing policy: %v", err)
	}

	code, out := checkOnline(t, path, "--config", config)
	if code != exitFinding {
		t.Errorf("exit = %d, want %d: %s", code, exitFinding, out)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("output does not report an error: %s", out)
	}
}

// Off means no probe is sent at all, not that results are discarded.
func TestCheckOnlineSendsNothingWhenOff(t *testing.T) {
	dir := t.TempDir()
	path := writeSources(t, dir, "ruler_wide")

	config := filepath.Join(dir, "ruler.yaml")
	body := "checks:\n  source/privileges:\n    severity: off\n"
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatalf("writing policy: %v", err)
	}

	code, out := checkOnline(t, path, "--config", config)
	if code != exitOK {
		t.Errorf("exit = %d, want %d: %s", code, exitOK, out)
	}
	if strings.Contains(out, "source/privileges") {
		t.Errorf("output mentions the check, want silence: %s", out)
	}
}

// Without --online the check is not run, so `ruler check` still works in CI
// with no cluster to reach.
func TestCheckStaysOfflineByDefault(t *testing.T) {
	path := writeSources(t, t.TempDir(), "ruler_wide")

	var stdout, stderr bytes.Buffer
	code := run([]string{"check", "--sources", path, filepath.Join("testdata", "rules")}, &stdout, &stderr)

	out := stdout.String() + stderr.String()
	if code != exitOK {
		t.Errorf("exit = %d, want %d: %s", code, exitOK, out)
	}
	if strings.Contains(out, "source/privileges") {
		t.Errorf("output mentions the check, want it not to have run: %s", out)
	}
}

// `ruler run` refuses a source that fails the contract at error severity, and
// keeps running for every other source. The rules against the refused one
// stop evaluating; nothing else changes (spec 6.7.3).
func TestRunRefusesASourceFailingTheContract(t *testing.T) {
	dir := t.TempDir()
	path := writeSources(t, dir, "ruler_wide")

	sources, problems, err := loadSources(path)
	if err != nil {
		t.Fatalf("loading sources: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("sources fixture problems: %v", problems)
	}

	root, _ := policy.Parse("ruler.yaml", []byte("checks:\n  source/privileges:\n    severity: error\n"))
	// The fixture rule warns about a missing runbook; only errors matter here.
	set, problems := ruleset.Load(filepath.Join("testdata", "rules"), sources, root)
	for _, p := range problems {
		if p.Severity == lint.SeverityError {
			t.Fatalf("rules fixture problems: %v", problems)
		}
	}
	if len(set.Rules) == 0 || len(set.Rules[0].Sources) == 0 {
		t.Fatal("fixture rule matched no source, so there is nothing to refuse")
	}

	var stderr bytes.Buffer
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	refused := refusedSources(context.Background(), path, set, root, &stderr, log)
	if !refused["otel_traces"] {
		t.Fatalf("refused = %v, want the over-privileged source: %s", refused, stderr.String())
	}

	refuseSources(set, refused)
	for _, r := range set.Rules {
		if len(r.Sources) != 0 {
			t.Errorf("rule %q still has sources %v, want none", r.Alert, r.Sources)
		}
	}
}
