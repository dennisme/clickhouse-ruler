package main

import (
	"bytes"
	"log/slog"
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

// A resend interval of zero or less would post every firing alert on every
// evaluation and stamp it with an expiry already in the past, which
// Alertmanager reads as resolved. Refuse it rather than page nobody.
func TestRunRejectsANonPositiveResendInterval(t *testing.T) {
	dir := fixture(t, bareRule, "")

	for _, interval := range []string{"0", "-1m"} {
		code, stderr := runRunCmd(t, "run",
			"--rules", filepath.Join(dir, "rules"),
			"--sources", filepath.Join(dir, "sources.yaml"),
			"--alertmanager", "http://127.0.0.1:9093",
			"--resend-interval", interval)

		if code != exitUsage {
			t.Errorf("--resend-interval %s: exit = %d, want exitUsage\n%s", interval, code, stderr)
		}
		if !strings.Contains(stderr, "--resend-interval must be positive") {
			t.Errorf("--resend-interval %s: expected the reason on stderr, got:\n%s", interval, stderr)
		}
	}
}

// A log level nobody can parse has to be refused at startup rather than
// silently falling back, or an operator who asked for debug output and got
// none has no way to tell why.
func TestRunRejectsAnUnknownLogLevel(t *testing.T) {
	dir := fixture(t, bareRule, "")

	code, stderr := runRunCmd(t, "run",
		"--rules", filepath.Join(dir, "rules"),
		"--sources", filepath.Join(dir, "sources.yaml"),
		"--alertmanager", "http://127.0.0.1:9093",
		"--log-level", "chatty")

	if code != exitUsage {
		t.Errorf("exit = %d, want exitUsage\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "--log-level") {
		t.Errorf("expected the reason on stderr, got:\n%s", stderr)
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{in: "debug", want: slog.LevelDebug},
		{in: "info", want: slog.LevelInfo},
		{in: "warn", want: slog.LevelWarn},
		{in: "error", want: slog.LevelError},
		{in: "INFO", want: slog.LevelInfo},
		{in: "", wantErr: true},
		{in: "chatty", wantErr: true},
	}

	for _, tc := range tests {
		got, err := parseLogLevel(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseLogLevel(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseLogLevel(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// A tolerance of one makes a firing alert expire at the exact moment it is
// next due, so any delay at all produces a resolved notification for
// something still broken. Below two there is no headroom to tolerate
// anything, which is the whole point of the number.
func TestRunRejectsAToleranceWithNoHeadroom(t *testing.T) {
	dir := fixture(t, bareRule, "")

	for _, tolerance := range []string{"1", "0", "-1"} {
		code, stderr := runRunCmd(t, "run",
			"--rules", filepath.Join(dir, "rules"),
			"--sources", filepath.Join(dir, "sources.yaml"),
			"--alertmanager", "http://127.0.0.1:9093",
			"--resend-tolerance", tolerance)

		if code != exitUsage {
			t.Errorf("--resend-tolerance %s: exit = %d, want exitUsage\n%s", tolerance, code, stderr)
		}
		if !strings.Contains(stderr, "--resend-tolerance") {
			t.Errorf("--resend-tolerance %s: expected the reason on stderr, got:\n%s", tolerance, stderr)
		}
	}
}
