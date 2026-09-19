package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

// testEnv keeps the tests hermetic. Reading the real environment would make
// them depend on whatever the developer happens to have exported.
func testEnv(pairs map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := pairs[key]
		return v, ok
	}
}

var fixtureEnv = testEnv(map[string]string{
	"RULER_PASSWORD_LOGS":  "log-env-secret",
	"RULER_PASSWORD_EMPTY": "",
})

func parseFixture(t *testing.T, name string) (*File, []lint.Problem) {
	t.Helper()
	return Parse("testdata/"+name, readFixture(t, name), fixtureEnv)
}

func TestParseValidFile(t *testing.T) {
	f, problems := parseFixture(t, "valid.yaml")

	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}
	if len(f.Sources) != 3 {
		t.Fatalf("got %d sources, want 3", len(f.Sources))
	}

	traces := f.Sources[0]
	for _, tc := range []struct{ field, got, want string }{
		{"name", traces.Name, "otel_traces"},
		{"address", traces.Address, "clickhouse:9000"},
		{"database", traces.Database, "otel"},
		{"username", traces.Username, "ruler_payments"},
		{"table", traces.Table, "otel_traces"},
		{"timestamp_column", traces.TimestampColumn, "Timestamp"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}
	if traces.EvaluationDelay != 30*time.Second {
		t.Errorf("evaluation_delay = %v, want 30s", traces.EvaluationDelay)
	}
	if traces.MaxRows != 500 {
		t.Errorf("max_rows = %d, want 500", traces.MaxRows)
	}
}

// The trailing newline `echo secret > file` leaves behind is not part of the
// password, and sending it would fail authentication for no visible reason.
func TestParseReadsPasswordFromFile(t *testing.T) {
	f, problems := parseFixture(t, "valid.yaml")
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}

	if got := f.Sources[0].Password; got != "trace-secret" {
		t.Errorf("password = %q, want %q", got, "trace-secret")
	}
}

func TestParseReadsPasswordFromEnv(t *testing.T) {
	f, problems := parseFixture(t, "valid.yaml")
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}

	if got := f.Sources[1].Password; got != "log-env-secret" {
		t.Errorf("password = %q, want %q", got, "log-env-secret")
	}
}

// No password at all is legal: local development, and mTLS where ClickHouse
// authenticates the client certificate instead.
func TestParseAllowsNoPassword(t *testing.T) {
	f, problems := parseFixture(t, "valid.yaml")
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}

	if got := f.Sources[2].Password; got != "" {
		t.Errorf("password = %q, want empty", got)
	}
}

// A source that says nothing about delay or caps still needs them, because an
// unset delay reads a half-filled window (spec 6.8) and an unset cap lets one
// rule buffer an unbounded result.
func TestParseAppliesDefaults(t *testing.T) {
	f, problems := parseFixture(t, "valid.yaml")
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}

	logs := f.Sources[1]
	if logs.EvaluationDelay != DefaultEvaluationDelay {
		t.Errorf("evaluation_delay = %v, want %v", logs.EvaluationDelay, DefaultEvaluationDelay)
	}
	if logs.MaxRows != DefaultMaxRows {
		t.Errorf("max_rows = %d, want %d", logs.MaxRows, DefaultMaxRows)
	}
	if logs.MaxExecutionTime != DefaultMaxExecutionTime {
		t.Errorf("max_execution_time = %v, want %v", logs.MaxExecutionTime, DefaultMaxExecutionTime)
	}
	if logs.MaxMemoryUsage != DefaultMaxMemoryUsage {
		t.Errorf("max_memory_usage = %d, want %d", logs.MaxMemoryUsage, DefaultMaxMemoryUsage)
	}
}

func TestParseReportsProblems(t *testing.T) {
	_, got := parseFixture(t, "problems.yaml")

	want := []lint.Problem{
		{Line: 2, Subject: "", Check: "source/name", Text: "source name is empty"},
		{
			Line: 12, Subject: "both_secrets", Check: "source/password",
			Text: "password_file and password_env are mutually exclusive, set one",
		},
		{
			Line: 20, Subject: "missing_password_file", Check: "source/password",
			Text: `cannot read password_file "testdata/secrets/nowhere": no such file or directory`,
		},
		{
			Line: 27, Subject: "empty_password_file", Check: "source/password",
			Text: `password_file "testdata/secrets/empty" is empty`,
		},
		{
			Line: 34, Subject: "missing_password_env", Check: "source/password",
			Text: "password_env references ${RULER_PASSWORD_NOWHERE}, which is not set in the environment",
		},
		{
			Line: 38, Subject: "empty_bits", Check: "source/address",
			Text: "address is empty, expected host:port",
		},
		{Line: 39, Subject: "empty_bits", Check: "source/database", Text: "database is empty"},
		{Line: 40, Subject: "empty_bits", Check: "source/username", Text: "username is empty"},
		{Line: 41, Subject: "empty_bits", Check: "source/table", Text: "table is empty"},
		{
			Line: 42, Subject: "empty_bits", Check: "source/timestamp-column",
			Text: "timestamp_column is empty",
		},
		{
			Line: 49, Subject: "bad_numbers", Check: "source/evaluation-delay",
			Text: "evaluation_delay must not be negative, got -1m0s",
		},
		{
			Line: 50, Subject: "bad_numbers", Check: "source/max-rows",
			Text: "max_rows must not be negative, got -5",
		},
		{
			Line: 57, Subject: "otel_traces", Check: "source/name",
			Text: `duplicate source name "otel_traces", first defined on line 51`,
		},
	}
	for i := range want {
		want[i].File = "testdata/problems.yaml"
		want[i].Severity = lint.SeverityError
	}

	if len(got) != len(want) {
		t.Fatalf("got %d problems, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("problem %d:\n got: %+v\nwant: %+v", i, got[i], want[i])
		}
	}
}

// Setting both is rejected rather than resolved by precedence: if the two
// differ one is stale, and picking either silently can authenticate with a
// credential that was supposed to have been rotated away.
func TestParseRejectsBothPasswordSources(t *testing.T) {
	_, got := parseFixture(t, "problems.yaml")

	var found bool
	for _, p := range got {
		if p.Subject == "both_secrets" && p.Check == "source/password" {
			found = true
		}
	}
	if !found {
		t.Error("setting password_file and password_env together must be reported")
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	data := []byte("sources:\n  - name: x\n    address: h:9000\n    database: d\n" +
		"    username: u\n    table: t\n    timestamp_column: ts\n    tabel: typo\n")

	_, got := Parse("typo.yaml", data, fixtureEnv)

	if len(got) != 1 {
		t.Fatalf("got %d problems, want 1: %v", len(got), got)
	}
	if got[0].Check != lint.CheckYAMLUnknownField {
		t.Errorf("check = %q, want %q", got[0].Check, lint.CheckYAMLUnknownField)
	}
	if got[0].Line != 8 {
		t.Errorf("line = %d, want 8", got[0].Line)
	}
}

// Printing a Source with %v must never put the password in a log.
func TestSourceStringRedactsPassword(t *testing.T) {
	s := Source{
		Name:     "otel_traces",
		Address:  "clickhouse:9000",
		Database: "otel",
		Username: "ruler_payments",
		Password: "hunter2",
	}

	text := s.String()
	if strings.Contains(text, "hunter2") {
		t.Errorf("String leaked the password: %s", text)
	}
	for _, want := range []string{"otel_traces", "ruler_payments", "clickhouse:9000", "otel"} {
		if !strings.Contains(text, want) {
			t.Errorf("String should mention %q, got: %s", want, text)
		}
	}
}

func TestByNameLooksUpSources(t *testing.T) {
	f, _ := parseFixture(t, "valid.yaml")

	s, ok := f.ByName("otel_logs")
	if !ok {
		t.Fatal("otel_logs not found")
	}
	if s.Table != "otel_logs" {
		t.Errorf("table = %q, want otel_logs", s.Table)
	}
	if _, ok := f.ByName("nope"); ok {
		t.Error("ByName found a source that does not exist")
	}
}
