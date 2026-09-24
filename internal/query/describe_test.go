package query

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// readColumns reads a captured `DESCRIBE (SELECT ...)` result, name and type
// per line. The server sends these as rows and the driver decodes them, so
// this parses only what the fixtures hold: the point of the fixture is the
// shape of a real answer, not a second decoder.
func readColumns(t *testing.T, name string) []Column {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}

	var out []Column
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		field, kind, _ := strings.Cut(line, "\t")
		out = append(out, Column{Name: strings.TrimSpace(field), Type: strings.TrimSpace(kind)})
	}
	return out
}

// The value column is what a threshold is compared against, so a result
// without one produces no alert however many rows it returns.
func TestHasValueColumn(t *testing.T) {
	if !hasValueColumn(readColumns(t, "describe_plain.txt")) {
		t.Error("hasValueColumn = false for a result carrying value")
	}
	if hasValueColumn(readColumns(t, "describe_no_value.txt")) {
		t.Error("hasValueColumn = true for a result whose numeric column is named total")
	}
}

// The case the tier 0 check cannot see: the alias is produced inside a
// subquery, so the outer query's text never says `AS team`.
func TestProducedProtectedLabels(t *testing.T) {
	protected := []string{"alertname", "source", "team"}

	got := producedProtectedLabels(readColumns(t, "describe_protected_label.txt"), protected)
	if len(got) != 1 || got[0] != "team" {
		t.Fatalf("got %v, want team alone", got)
	}

	if got := producedProtectedLabels(readColumns(t, "describe_plain.txt"), protected); len(got) != 0 {
		t.Errorf("got %v, want none: this query produces no protected label", got)
	}
}

// SELECT * produces whatever the table has, which is how a protected label
// arrives without anybody writing its name.
func TestProducedProtectedLabelsThroughSelectStar(t *testing.T) {
	cols := readColumns(t, "describe_star.txt")

	if len(producedProtectedLabels(cols, []string{"ServiceName"})) != 1 {
		t.Errorf("a column reached through SELECT * was not seen in %v", cols)
	}
}

func TestUnresolvedFields(t *testing.T) {
	cols := readColumns(t, "describe_plain.txt")
	known := []string{"team", "severity"}

	annotations := map[string]string{
		"summary":     "{{ .ServiceName }} is at {{ .value }}",
		"description": "owned by {{ .team }}",
		"wrong":       "{{ .SeviceName }} typo, and {{ .Missing }}",
	}

	got := unresolvedFields(annotations, cols, known)
	if len(got) != 1 {
		t.Fatalf("got %v, want the one annotation with unresolvable fields", got)
	}
	if got[0].Annotation != "wrong" {
		t.Errorf("annotation = %q, want wrong", got[0].Annotation)
	}
	if len(got[0].Fields) != 2 || got[0].Fields[0] != "Missing" || got[0].Fields[1] != "SeviceName" {
		t.Errorf("fields = %v, want [Missing SeviceName] sorted", got[0].Fields)
	}
}

// The ruler sets these itself, so an annotation reading them resolves at
// evaluation time even though no column produces them.
func TestUnresolvedFieldsKnowsTheRulersOwnLabels(t *testing.T) {
	annotations := map[string]string{"summary": "{{ .alertname }} on {{ .source }}"}

	got := unresolvedFields(annotations, readColumns(t, "describe_plain.txt"), []string{"alertname", "source"})
	if len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

// An unparseable template is tier 0's finding, reported as
// annotations/template against the file. Reporting it again here would give
// one mistake two findings, and the second one would be less useful.
func TestUnresolvedFieldsSkipsAnUnparseableTemplate(t *testing.T) {
	annotations := map[string]string{"summary": "{{ .ServiceName "}

	if got := unresolvedFields(annotations, readColumns(t, "describe_plain.txt"), nil); len(got) != 0 {
		t.Errorf("got %v, want none: the parse failure is already reported", got)
	}
}

// A template action that reads nothing, or reads the whole context, names no
// field and cannot be checked against columns.
func TestUnresolvedFieldsIgnoresNonFieldActions(t *testing.T) {
	annotations := map[string]string{
		"summary": `{{ if .value }}high{{ end }} {{ printf "%s" .ServiceName }}`,
	}

	if got := unresolvedFields(annotations, readColumns(t, "describe_plain.txt"), nil); len(got) != 0 {
		t.Errorf("got %v, want none: every field named is a real column", got)
	}
}

func describeErr(code int32, msg string) error {
	return &clickhouse.Exception{Code: code, Message: msg}
}

// What the server says about a DESCRIBE separates three things: a rule that
// cannot run, a source whose user cannot look, and a ruler that could not ask.
func TestClassifyDescribe(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantCheck string
		wantErr   bool
		wantIn    string
	}{
		{name: "described", err: nil},
		{
			name:      "unknown column",
			err:       describeErr(codeUnknownIdentifier, "Unknown expression identifier `Duration_ms` in scope SELECT"),
			wantCheck: lint.CheckRuleColumns,
			wantIn:    "Duration_ms",
		},
		{
			name:      "unknown table",
			err:       describeErr(codeUnknownTable, "Unknown table expression identifier 'otel.gone' in scope SELECT"),
			wantCheck: lint.CheckRuleColumns,
			wantIn:    "otel.gone",
		},
		{
			name:      "unknown database",
			err:       describeErr(codeUnknownDatabase, "Database nodb does not exist."),
			wantCheck: lint.CheckRuleColumns,
			wantIn:    "nodb",
		},
		{
			// Says nothing about the SQL. Its own check, because an operator
			// answers it per source: an error on the cluster that evaluates
			// the rule, off on a replica with no grants (spec 7.7, 10.3).
			name:      "access denied",
			err:       describeErr(codeAccessDenied, "ruler_payments: Not enough privileges."),
			wantCheck: lint.CheckRuleTableAccess,
			wantIn:    "privileges",
		},
		{
			name:    "connection failed",
			err:     &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			wantErr: true,
		},
		{
			name:    "some other server error",
			err:     describeErr(241, "Memory limit exceeded"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := classifyDescribe(tt.err)

			switch {
			case tt.wantErr && err == nil:
				t.Fatalf("err = nil, want one: %v is not the rule's fault", tt.err)
			case !tt.wantErr && err != nil:
				t.Fatalf("err = %v, want none", err)
			}
			if tt.wantCheck == "" {
				if got != nil {
					t.Fatalf("finding = %v, want none", got)
				}
				return
			}
			if got == nil {
				t.Fatal("finding = nil, want one")
			}
			if got.Check != tt.wantCheck {
				t.Errorf("check = %q, want %s", got.Check, tt.wantCheck)
			}
			if !contains(got.Detail, tt.wantIn) {
				t.Errorf("detail = %q, want it to carry %q", got.Detail, tt.wantIn)
			}
		})
	}
}
