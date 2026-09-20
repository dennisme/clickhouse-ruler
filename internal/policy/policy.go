// Package policy decides which checks are mandatory.
//
// Whether a rule must carry a team label is an organisation's choice, not this
// tool's. Hardcoding it means a rule that would run perfectly refuses to load,
// which narrows who can use the ruler to people who already agree with every
// convention it happened to pick (spec 7.6).
//
// Correctness checks are not configurable. A rule that fails one cannot do its
// job, and turning it off produces something that looks fine and never fires.
package policy

import (
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// Setting is the resolved configuration for one check, and where it came from.
type Setting struct {
	Severity lint.Severity

	// Keys is the list a check requires, for the checks that take one.
	Keys []string

	// File and Line are the policy that set this severity, empty when it came
	// from the shipped default. `ruler check --explain` reads them so an
	// author can see which file raised a check rather than having to guess
	// (spec 7.8).
	File string
	Line int
}

// Policy is the configuration for every configurable check.
type Policy struct {
	File   string
	Checks map[string]Setting
}

// For returns the setting for a check, falling back to its default.
func (p *Policy) For(check string) Setting {
	if p != nil {
		if s, ok := p.Checks[check]; ok {
			return s
		}
	}
	if s, ok := defaults[check]; ok {
		return s
	}
	// Anything not configurable always runs at error. Correctness checks land
	// here, which is the point: there is no setting that can soften them.
	return Setting{Severity: lint.SeverityError}
}

// Parse reads a policy file, collecting every problem rather than stopping at
// the first.
func Parse(file string, data []byte) (*Policy, []lint.Problem) {
	p := &Policy{File: file, Checks: map[string]Setting{}}
	r := lint.NewReader(file)

	doc, ok := r.Document(data)
	if !ok {
		return p, r.Problems()
	}
	if !r.Mapping(doc, "file") {
		return p, r.Problems()
	}

	for _, e := range lint.Entries(doc) {
		switch e.Key.Value {
		case "checks":
			parseChecks(r, p, e.Value)
		default:
			r.UnknownField(e.Key, "file")
		}
	}
	return p, r.Problems()
}

func parseChecks(r *lint.Reader, p *Policy, n *yaml.Node) {
	if !r.Mapping(n, "checks") {
		return
	}

	for _, e := range lint.Entries(n) {
		name := e.Key.Value

		switch {
		case Fixed(name):
			r.Add(e.Key.Line, checkPolicyFixed, lint.SeverityError,
				"%q is a correctness check and cannot be configured: a rule that fails it cannot run", name)
			continue
		case !Configurable(name):
			r.Add(e.Key.Line, checkPolicyUnknown, lint.SeverityError,
				"unknown check %q", name)
			continue
		}

		setting := p.For(name)
		setting.File = r.File()
		setting.Line = e.Key.Line
		parseSetting(r, &setting, e.Value, name)
		p.Checks[name] = setting
	}
}

func parseSetting(r *lint.Reader, s *Setting, n *yaml.Node, name string) {
	if !r.Mapping(n, "check "+name) {
		return
	}

	for _, e := range lint.Entries(n) {
		switch e.Key.Value {
		case "severity":
			raw, ok := r.Scalar(e.Value, "severity")
			if !ok {
				continue
			}
			sev, ok := ParseSeverity(raw)
			if !ok {
				r.Add(e.Value.Line, checkPolicySeverity, lint.SeverityError,
					"severity must be off, warn or error, got %q", raw)
				continue
			}
			s.Severity = sev
			s.Line = e.Key.Line
		case "keys":
			s.Keys = parseKeys(r, e.Value)
		default:
			r.UnknownField(e.Key, "check "+name)
		}
	}
}

func parseKeys(r *lint.Reader, n *yaml.Node) []string {
	if !r.Sequence(n, "keys") {
		return nil
	}
	out := make([]string, 0, len(n.Content))
	for _, item := range n.Content {
		if v, ok := r.Scalar(item, "key"); ok {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// ParseSeverity converts a configured name to a severity.
func ParseSeverity(s string) (lint.Severity, bool) {
	switch s {
	case "off":
		return lint.SeverityOff, true
	case "warn", "warning":
		return lint.SeverityWarning, true
	case "error":
		return lint.SeverityError, true
	}
	return lint.SeverityOff, false
}

// ParseNode reads a checks block embedded in another file, so a source can
// carry policy without that file having to know how checks are shaped.
func ParseNode(r *lint.Reader, n *yaml.Node) *Policy {
	p := &Policy{File: r.File(), Checks: map[string]Setting{}}
	parseChecks(r, p, n)
	return p
}
