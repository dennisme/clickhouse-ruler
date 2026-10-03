package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
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
		"--config", filepath.Join(dir, "ruler.yaml"))
	if code != exitUsage {
		t.Errorf("exit = %d, want exitUsage when --rules and --alertmanager are missing\n%s", code, stderr)
	}

	code, stderr = runRunCmd(t, "run",
		"--rules", filepath.Join(dir, "rules"),
		"--config", filepath.Join(dir, "ruler.yaml"))
	if code != exitUsage {
		t.Errorf("exit = %d, want exitUsage when --alertmanager is missing\n%s", code, stderr)
	}
}

// A file nobody can read is the whole of what stops a start. The ruler would
// otherwise come up holding a configuration nobody wrote down (spec 7.6).
func TestRunRefusesToStartOnAnUnreadableFile(t *testing.T) {
	dir := fixture(t, unreadableRule, "")

	code, stderr := runRunCmd(t, "run",
		"--rules", filepath.Join(dir, "rules"),
		"--config", filepath.Join(dir, "ruler.yaml"),
		"--alertmanager", "http://127.0.0.1:9093")

	if code != exitFinding {
		t.Errorf("exit = %d, want exitFinding\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "yaml/syntax") {
		t.Errorf("expected the yaml/syntax finding on stderr, got:\n%s", stderr)
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
			"--config", filepath.Join(dir, "ruler.yaml"),
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
		"--config", filepath.Join(dir, "ruler.yaml"),
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

// A log format nobody can parse is refused for the reason an unparseable
// level is: a pipeline that expected JSON and got text lines has no way to
// tell why.
func TestRunRejectsAnUnknownLogFormat(t *testing.T) {
	dir := fixture(t, bareRule, "")

	code, stderr := runRunCmd(t, "run",
		"--rules", filepath.Join(dir, "rules"),
		"--config", filepath.Join(dir, "ruler.yaml"),
		"--alertmanager", "http://127.0.0.1:9093",
		"--log-format", "logfmt")

	if code != exitUsage {
		t.Errorf("exit = %d, want exitUsage\n%s", code, stderr)
	}
	for _, want := range []string{"--log-format", "text", "json"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("expected the reason on stderr to name %q, got:\n%s", want, stderr)
		}
	}
}

// What --log-format=json buys is a line a log pipeline reads without parsing
// it back out of text, so the assertion is that encoding/json takes it and
// that the fields a caller passed survive under their own names.
func TestNewLogHandlerEncodesJSON(t *testing.T) {
	var out bytes.Buffer

	h, err := newLogHandler("json", &out, &slog.HandlerOptions{Level: slog.LevelInfo})
	if err != nil {
		t.Fatalf("newLogHandler: %v", err)
	}
	slog.New(h).Info("ruler running", "rules", 3, "listen", ":9090")

	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	for _, key := range []string{"time", "level", "msg", "rules", "listen"} {
		if _, ok := line[key]; !ok {
			t.Errorf("the JSON line is missing %q: %v", key, line)
		}
	}
	if line["msg"] != "ruler running" {
		t.Errorf("msg = %v, want the message unchanged", line["msg"])
	}
	if line["listen"] != ":9090" {
		t.Errorf("listen = %v, want the field unchanged", line["listen"])
	}
}

// text stays the default spelling, so the flag's other value has to leave
// today's output exactly where it was.
func TestNewLogHandlerKeepsTextLines(t *testing.T) {
	var out bytes.Buffer

	h, err := newLogHandler("text", &out, &slog.HandlerOptions{Level: slog.LevelInfo})
	if err != nil {
		t.Fatalf("newLogHandler: %v", err)
	}
	slog.New(h).Info("ruler running", "rules", 3, "listen", ":9090")

	var want bytes.Buffer
	slog.New(slog.NewTextHandler(&want, &slog.HandlerOptions{Level: slog.LevelInfo})).
		Info("ruler running", "rules", 3, "listen", ":9090")

	// The timestamp is the only part that differs between two writes.
	got := regexp.MustCompile(`time=[^ ]+ `).ReplaceAllString(out.String(), "")
	stripped := regexp.MustCompile(`time=[^ ]+ `).ReplaceAllString(want.String(), "")
	if got != stripped {
		t.Errorf("text output changed:\ngot  %q\nwant %q", got, stripped)
	}
}

func TestNewLogHandlerRejectsAnUnknownFormat(t *testing.T) {
	for _, format := range []string{"", "logfmt", "JSON "} {
		h, err := newLogHandler(format, io.Discard, nil)
		if err == nil {
			t.Errorf("newLogHandler(%q) = %v, want an error", format, h)
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
			"--config", filepath.Join(dir, "ruler.yaml"),
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

// A source that answers, or does not.
type fakePinger struct{ err error }

func (p fakePinger) Ping(context.Context) error { return p.err }

// Readiness is deliberately not a source-by-source answer. One unreachable
// cluster out of twelve is a finding for source/privileges and the evaluation
// failure counters, not a reason to declare the whole ruler unfit, and a probe
// that flaps with any cluster's availability gets disabled by whoever is on
// call (spec 8.1).
func TestReadinessNeedsRulesAndOneSource(t *testing.T) {
	down := fakePinger{err: errors.New("connection refused")}
	up := fakePinger{}

	tests := []struct {
		name    string
		rules   int
		sources map[string]pinger
		wantErr string
	}{
		{
			name:    "no rules loaded",
			rules:   0,
			sources: map[string]pinger{"a": up},
			wantErr: "no rules",
		},
		{
			name:    "rules but nothing to evaluate them against",
			rules:   3,
			sources: map[string]pinger{},
			wantErr: "no source",
		},
		{
			name:    "every source unreachable",
			rules:   3,
			sources: map[string]pinger{"a": down, "b": down},
			wantErr: "no source",
		},
		{
			name:    "one of several answering is enough",
			rules:   3,
			sources: map[string]pinger{"a": down, "b": up, "c": down},
		},
	}

	for _, tc := range tests {
		err := readiness(tc.rules, tc.sources)(context.Background())

		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: ready reported %v, want ready", tc.name, err)
		case tc.wantErr != "" && err == nil:
			t.Errorf("%s: ready reported nothing, want an error naming %q", tc.name, tc.wantErr)
		case tc.wantErr != "" && err != nil && !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("%s: ready reported %v, want it to name %q", tc.name, err, tc.wantErr)
		}
	}
}
