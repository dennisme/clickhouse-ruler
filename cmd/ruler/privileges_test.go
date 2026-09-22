package main

import (
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

func errorSetting() policy.Setting {
	return policy.Setting{
		Severity: lint.SeverityError,
		Keys:     query.Assertions(),
		File:     "ruler.yaml",
		Line:     7,
	}
}

func TestPrivilegeProblemsReportsEachFailedAssertion(t *testing.T) {
	results := []query.Assertion{
		{Name: query.AssertionSourcesRevoked, Status: query.StatusFail, Detail: "URL (the query ran)"},
		{Name: query.AssertionReadonly, Status: query.StatusPass},
		{Name: query.AssertionConstraints, Status: query.StatusFail, Detail: "max_execution_time (no constraint)"},
		{Name: query.AssertionTableReadable, Status: query.StatusPass},
	}

	problems := privilegeProblems("sources.yaml", "payments_prod", 12, errorSetting(), results)
	if len(problems) != 2 {
		t.Fatalf("got %d problems, want one per failed assertion: %v", len(problems), problems)
	}

	for _, p := range problems {
		if p.Check != policy.CheckSourcePrivileges {
			t.Errorf("check = %q, want %q", p.Check, policy.CheckSourcePrivileges)
		}
		if p.Severity != lint.SeverityError {
			t.Errorf("severity = %v, want the configured error", p.Severity)
		}
		if p.Subject != "payments_prod" {
			t.Errorf("subject = %q, want the source name", p.Subject)
		}
		if p.File != "sources.yaml" || p.Line != 12 {
			t.Errorf("location = %s:%d, want the source's line in the sources file", p.File, p.Line)
		}
		// --explain has to be able to name the file that raised this.
		if p.PolicyFile != "ruler.yaml" || p.PolicyLine != 7 {
			t.Errorf("policy origin = %s:%d, want ruler.yaml:7", p.PolicyFile, p.PolicyLine)
		}
	}

	// A finding has to say which half of the contract is missing, or an
	// operator knows only that something about the user is wrong.
	if !strings.Contains(problems[0].Text, query.AssertionSourcesRevoked) {
		t.Errorf("text = %q, want it to name the assertion", problems[0].Text)
	}
	if !strings.Contains(problems[0].Text, "URL") {
		t.Errorf("text = %q, want it to carry the detail", problems[0].Text)
	}
}

// An inconclusive result is about what the ruler could see, not about how the
// cluster is configured. Blocking a deploy on one would let a network blip
// refuse a source that meets the contract perfectly well.
func TestPrivilegeProblemsNeverBlockOnInconclusive(t *testing.T) {
	results := []query.Assertion{
		{Name: query.AssertionReadonly, Status: query.StatusInconclusive, Detail: "clickhouse did not answer"},
	}

	problems := privilegeProblems("sources.yaml", "payments_prod", 12, errorSetting(), results)
	if len(problems) != 1 {
		t.Fatalf("got %d problems, want 1: %v", len(problems), problems)
	}
	if problems[0].Severity != lint.SeverityWarning {
		t.Errorf("severity = %v, want warning even where policy says error", problems[0].Severity)
	}
	if !strings.Contains(problems[0].Text, "inconclusive") {
		t.Errorf("text = %q, want it to say the assertion could not be decided", problems[0].Text)
	}
}

func TestPrivilegeProblemsSaysNothingWhenTheContractHolds(t *testing.T) {
	results := []query.Assertion{
		{Name: query.AssertionSourcesRevoked, Status: query.StatusPass},
		{Name: query.AssertionReadonly, Status: query.StatusPass},
	}

	if problems := privilegeProblems("sources.yaml", "payments_prod", 12, errorSetting(), results); len(problems) != 0 {
		t.Fatalf("got %v, want nothing", problems)
	}
}

// At error severity the source is refused, so its rules do not evaluate. A
// rule matching a healthy source as well keeps that one (spec 7.6).
func TestRefuseSourcesDropsOnlyTheRefused(t *testing.T) {
	set := &ruleset.Set{Rules: []ruleset.Rule{
		{Sources: []source.Source{{Name: "payments_prod"}, {Name: "payments_dc2"}}},
		{Sources: []source.Source{{Name: "payments_prod"}}},
	}}

	refuseSources(set, map[string]bool{"payments_prod": true})

	if got := len(set.Rules[0].Sources); got != 1 {
		t.Fatalf("first rule has %d sources, want 1", got)
	}
	if name := set.Rules[0].Sources[0].Name; name != "payments_dc2" {
		t.Errorf("remaining source = %q, want payments_dc2", name)
	}
	if got := len(set.Rules[1].Sources); got != 0 {
		t.Errorf("second rule has %d sources, want none: its only source was refused", got)
	}
}

// Severity off means the probes are never sent, not that they are sent and
// ignored. A source on a cluster an operator has deliberately excluded should
// see no traffic from this check at all.
func TestPrivilegesSkippedWhenOff(t *testing.T) {
	setting := policy.Setting{Severity: lint.SeverityOff, Keys: query.Assertions()}
	if privilegesEnabled(setting) {
		t.Error("privilegesEnabled = true at severity off, want false")
	}

	setting.Severity = lint.SeverityWarning
	if !privilegesEnabled(setting) {
		t.Error("privilegesEnabled = false at severity warn, want true")
	}

	// Every assertion dropped is the same thing said a different way.
	setting.Keys = nil
	if privilegesEnabled(setting) {
		t.Error("privilegesEnabled = true with no assertions required, want false")
	}
}
