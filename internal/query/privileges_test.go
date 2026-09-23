package query

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/dennisme/clickhouse-ruler/internal/policy"
)

// denied is the exception ClickHouse returns when a privilege is missing.
func denied() error {
	return &clickhouse.Exception{
		Code:    codeAccessDenied,
		Message: "ruler_payments: Not enough privileges. To execute this query, it's necessary to have the grant READ ON URL.",
	}
}

func serverErr(code int32, message string) error {
	return &clickhouse.Exception{Code: code, Message: message}
}

// dialErr is the shape of a failure that never reached the server, so nothing
// was learned about any privilege.
func dialErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}

func TestClassifyDenialProbe(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status Status
	}{
		// A privilege check happens before ClickHouse executes anything, so a
		// denial is the only outcome that proves the grant is absent.
		{"denied is the pass", denied(), StatusPass},

		// The query reached execution, which it could only do with the
		// privilege in hand. The endpoint being unreachable is why it then
		// failed, and says nothing about access.
		{"succeeded", nil, StatusFail},
		{"failed after the privilege check", serverErr(1000, "Connection refused"), StatusFail},

		// Nothing was learned: the server never answered.
		{"connection to clickhouse failed", dialErr(), StatusInconclusive},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, detail := classifyDenial(tt.err)
			if got != tt.status {
				t.Errorf("classifyDenial(%v) = %v, want %v", tt.err, got, tt.status)
			}
			if got != StatusPass && detail == "" {
				t.Error("a non-passing assertion must say why")
			}
		})
	}
}

func TestClassifyReadable(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status Status
	}{
		{"readable", nil, StatusPass},
		{"denied", denied(), StatusFail},
		{"missing table", serverErr(60, "Table otel.nope does not exist"), StatusInconclusive},
		{"connection to clickhouse failed", dialErr(), StatusInconclusive},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, detail := classifyReadable(tt.err)
			if got != tt.status {
				t.Errorf("classifyReadable(%v) = %v, want %v", tt.err, got, tt.status)
			}
			if got != StatusPass && detail == "" {
				t.Error("a non-passing assertion must say why")
			}
		})
	}
}

// A driver error that wraps an exception still has to be recognised, because
// the driver does not always hand one back bare.
func TestClassifyWrappedException(t *testing.T) {
	wrapped := fmt.Errorf("running probe: %w", denied())
	if got, _ := classifyDenial(wrapped); got != StatusPass {
		t.Errorf("classifyDenial(wrapped) = %v, want %v", got, StatusPass)
	}
}

func str(s string) *string { return &s }

func TestEvalReadonly(t *testing.T) {
	tests := []struct {
		name   string
		rows   []settingRow
		status Status
		detail string
	}{
		{
			name:   "readonly 2 and const",
			rows:   []settingRow{{Name: "readonly", Value: "2", Readonly: 1}},
			status: StatusPass,
		},
		{
			// A user who can write is not a user whose rules are only ever
			// reading (spec 6.7.2).
			name:   "writable",
			rows:   []settingRow{{Name: "readonly", Value: "0", Readonly: 1}},
			status: StatusFail,
			detail: "readonly is 0",
		},
		{
			// readonly = 1 forbids SET outright, so the ruler cannot send the
			// per-query limits it is supposed to send.
			name:   "readonly 1 refuses the settings the ruler sends",
			rows:   []settingRow{{Name: "readonly", Value: "1", Readonly: 1}},
			status: StatusFail,
			detail: "readonly is 1",
		},
		{
			name:   "not const",
			rows:   []settingRow{{Name: "readonly", Value: "2", Readonly: 0}},
			status: StatusFail,
			detail: "a query can lower it",
		},
		{
			name:   "no row",
			rows:   nil,
			status: StatusInconclusive,
			detail: "system.settings",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalReadonly(tt.rows)
			if got.Name != AssertionReadonly {
				t.Errorf("Name = %q, want %q", got.Name, AssertionReadonly)
			}
			if got.Status != tt.status {
				t.Errorf("Status = %v (%s), want %v", got.Status, got.Detail, tt.status)
			}
			if tt.detail != "" && !contains(got.Detail, tt.detail) {
				t.Errorf("Detail = %q, want it to mention %q", got.Detail, tt.detail)
			}
		})
	}
}

func TestEvalConstraints(t *testing.T) {
	// A profile meeting the contract: every bounded setting carries a max,
	// and every setting that must not change is const.
	compliant := []settingRow{
		{Name: "max_execution_time", Max: str("10")},
		{Name: "max_memory_usage", Max: str("1073741824")},
		{Name: "max_result_rows", Max: str("1000")},
		{Name: "result_overflow_mode", Readonly: 1},
		{Name: "timeout_overflow_mode", Readonly: 1},
		{Name: "skip_unavailable_shards", Readonly: 1},
	}

	if got := evalConstraints(compliant); got.Status != StatusPass {
		t.Errorf("compliant profile: Status = %v (%s), want pass", got.Status, got.Detail)
	}

	t.Run("a bounded setting with no constraint", func(t *testing.T) {
		rows := append([]settingRow(nil), compliant...)
		rows[0] = settingRow{Name: "max_execution_time"}

		got := evalConstraints(rows)
		if got.Status != StatusFail {
			t.Fatalf("Status = %v, want fail", got.Status)
		}
		if !contains(got.Detail, "max_execution_time") {
			t.Errorf("Detail = %q, want it to name the setting", got.Detail)
		}
	})

	t.Run("a min is a constraint too", func(t *testing.T) {
		rows := append([]settingRow(nil), compliant...)
		rows[0] = settingRow{Name: "max_execution_time", Min: str("1")}

		if got := evalConstraints(rows); got.Status != StatusPass {
			t.Errorf("Status = %v (%s), want pass", got.Status, got.Detail)
		}
	})

	t.Run("an overflow mode that is not const", func(t *testing.T) {
		rows := append([]settingRow(nil), compliant...)
		rows[3] = settingRow{Name: "result_overflow_mode", Max: str("throw")}

		got := evalConstraints(rows)
		if got.Status != StatusFail {
			t.Fatalf("Status = %v, want fail", got.Status)
		}
		if !contains(got.Detail, "result_overflow_mode") {
			t.Errorf("Detail = %q, want it to name the setting", got.Detail)
		}
	})

	t.Run("a setting missing from the answer", func(t *testing.T) {
		got := evalConstraints(compliant[1:])
		if got.Status != StatusFail {
			t.Fatalf("Status = %v, want fail", got.Status)
		}
		if !contains(got.Detail, "max_execution_time") {
			t.Errorf("Detail = %q, want it to name the setting", got.Detail)
		}
	})
}

func TestSelected(t *testing.T) {
	tests := []struct {
		name    string
		require []string
		want    []string
	}{
		{
			// An empty list is the operator having turned every assertion
			// off, which is not the same as the default. Defaults are
			// applied by policy, not here.
			name:    "none",
			require: nil,
			want:    nil,
		},
		{
			name:    "all, in report order rather than configured order",
			require: []string{AssertionTableReadable, AssertionReadonly, AssertionConstraints, AssertionSourcesRevoked},
			want:    Assertions(),
		},
		{
			name:    "a subset, for a cluster that cannot meet the whole contract",
			require: []string{AssertionSourcesRevoked, AssertionTableReadable},
			want:    []string{AssertionSourcesRevoked, AssertionTableReadable},
		},
		{
			// Policy rejects unknown names, so reaching here means something
			// bypassed it. Running nothing is safer than guessing.
			name:    "an unknown name is dropped",
			require: []string{AssertionReadonly, "sources-revokd"},
			want:    []string{AssertionReadonly},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selected(tt.require)
			if len(got) != len(tt.want) {
				t.Fatalf("selected(%v) = %v, want %v", tt.require, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("selected(%v) = %v, want %v", tt.require, got, tt.want)
				}
			}
		})
	}
}

// The probes and the list an operator may require are the same set, or
// policy can require an assertion that never runs. They live in two packages
// because policy cannot import this one without a cycle, so nothing but this
// test keeps them in step.
func TestAssertionsMatchPolicyDefaults(t *testing.T) {
	want := append([]string(nil), Assertions()...)
	sort.Strings(want)

	got := policy.Defaults().For(policy.CheckSourcePrivileges).Keys
	if len(got) != len(want) {
		t.Fatalf("policy requires %v, this package probes %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("policy requires %v, this package probes %v", got, want)
		}
	}
}

// The check names this package reports and the names policy knows are the
// same set, or a finding arrives under a name no policy can resolve. They are
// spelled twice because policy cannot import this package without a cycle.
func TestCheckNamesAreKnownToPolicy(t *testing.T) {
	configurable := []string{
		CheckSelectStar,
		CheckTableFunction,
		CheckNondeterministic,
		CheckForeignTable,
		CheckComplexity,
	}
	for _, name := range configurable {
		if !policy.Configurable(name) {
			t.Errorf("%s is not configurable in policy, so its severity cannot be resolved", name)
		}
	}

	for _, name := range []string{CheckSyntax, CheckInspect, CheckSettings} {
		if !policy.Fixed(name) {
			t.Errorf("%s must be fixed: there is no severity to resolve for it", name)
		}
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// A probe that is still running when its deadline passes got past the
// privilege check, which is the same finding as any other non-denial. The
// alternative is reporting nothing at all for a granted url(), which retries
// inside ClickHouse for over two minutes against a refused port.
func TestClassifyProbeTimeout(t *testing.T) {
	got, detail := classifyProbe(context.DeadlineExceeded, true)
	if got != StatusFail {
		t.Errorf("classifyProbe(deadline, alive) = %v, want %v", got, StatusFail)
	}
	if !contains(detail, "granted") {
		t.Errorf("detail = %q, want it to say the privilege is granted", detail)
	}

	// Once the whole check is out of time, a probe timing out says nothing
	// about any privilege.
	if got, _ := classifyProbe(context.DeadlineExceeded, false); got != StatusInconclusive {
		t.Errorf("classifyProbe(deadline, expired) = %v, want %v", got, StatusInconclusive)
	}

	// Anything else still goes through the ordinary reading.
	if got, _ := classifyProbe(denied(), true); got != StatusPass {
		t.Errorf("classifyProbe(denied) = %v, want %v", got, StatusPass)
	}
}
