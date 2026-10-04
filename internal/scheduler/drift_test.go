package scheduler

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// driftRule is a rule against however many sources a case needs, with the file
// and line a finding has to name.
func driftRule(sources ...source.Source) ruleset.Rule {
	return ruleset.Rule{
		Rule:    rule.Rule{Alert: "SlowCheckout"},
		File:    "rules/payments.yaml",
		Path:    "rules/payments.yaml",
		Labels:  map[string]string{"team": "payments"},
		Sources: sources,
	}
}

func shape(pairs ...string) []query.Column {
	var out []query.Column
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, query.Column{Name: pairs[i], Type: pairs[i+1]})
	}
	return out
}

// costPolicy sets rule/cost's ceilings to what a case wants to cross.
func costPolicy(rows, rate string) *policy.Policy {
	return &policy.Policy{
		File: "rules/policy.yaml",
		Checks: map[string]policy.Setting{
			lint.CheckRuleCost: {
				Severity: lint.SeverityWarning,
				Keys:     []string{lint.LimitRowsRead + ":" + rows, lint.LimitRowsPerSecond + ":" + rate},
				File:     "rules/policy.yaml",
				Line:     4,
			},
		},
	}
}

// Two evaluations establish the baseline, so the first one compares against
// nothing and reports nothing (spec 6.3.2).
func TestDriftFirstEvaluationReportsNothing(t *testing.T) {
	src := source.Source{Name: "payments_prod"}
	d := newDrift(driftRule(src))

	got := d.inspect([]evaluated{{
		source: src,
		shape:  shape("value", "Float64", "ServiceName", "String"),
	}}, nil, time.Now())

	if len(got) != 0 {
		t.Fatalf("first evaluation reported %d problems, want none: %v", len(got), got)
	}
}

func TestDriftShapeComparison(t *testing.T) {
	src := source.Source{Name: "payments_prod"}
	first := shape("value", "Float64", "ServiceName", "String")

	tests := []struct {
		name  string
		next  []query.Column
		want  string
		quiet bool
	}{
		{
			name:  "unchanged",
			next:  shape("ServiceName", "String", "value", "Float64"),
			quiet: true,
		},
		{
			name: "retyped",
			next: shape("value", "Int64", "ServiceName", "String"),
			want: "returns value as Float64",
		},
		{
			name: "dropped",
			next: shape("value", "Float64"),
			want: "returns ServiceName",
		},
		{
			name: "added",
			next: shape("value", "Float64", "ServiceName", "String", "Env", "String"),
			want: "returns Env",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := newDrift(driftRule(src))
			now := time.Now()
			d.inspect([]evaluated{{source: src, shape: first}}, nil, now)

			got := d.inspect([]evaluated{{source: src, shape: tc.next}}, nil, now.Add(time.Minute))
			if tc.quiet {
				if len(got) != 0 {
					t.Fatalf("reported %d problems, want none: %v", len(got), got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("reported %d problems, want 1: %v", len(got), got)
			}
			p := got[0]
			if p.Check != lint.CheckRuleColumns {
				t.Errorf("check is %s, want %s", p.Check, lint.CheckRuleColumns)
			}
			if p.Severity != lint.SeverityError {
				t.Errorf("severity is %s, want error: rule/columns cannot be softened", p.Severity)
			}
			if p.File != "rules/payments.yaml" || p.Subject != "SlowCheckout" {
				t.Errorf("problem names %s %s, want the rule's own file and alert", p.File, p.Subject)
			}
			if !strings.Contains(p.Text, tc.want) {
				t.Errorf("text is %q, want it to contain %q", p.Text, tc.want)
			}
			if !strings.Contains(p.Text, src.Name) {
				t.Errorf("text is %q, want it to name source %s", p.Text, src.Name)
			}
		})
	}
}

// The baseline moves with every evaluation, so a shape that changed once is
// reported once rather than on every tick afterwards.
func TestDriftReportsAChangeOnce(t *testing.T) {
	src := source.Source{Name: "payments_prod"}
	d := newDrift(driftRule(src))
	now := time.Now()

	d.inspect([]evaluated{{source: src, shape: shape("value", "Float64")}}, nil, now)
	if got := d.inspect([]evaluated{{source: src, shape: shape("value", "Int64")}}, nil, now); len(got) != 1 {
		t.Fatalf("the evaluation that changed reported %d problems, want 1", len(got))
	}
	if got := d.inspect([]evaluated{{source: src, shape: shape("value", "Int64")}}, nil, now); len(got) != 0 {
		t.Fatalf("the evaluation after it reported %d problems, want none: %v", len(got), got)
	}
}

// Row counts carry no signal: zero rows is the healthy state of most rules and
// a rule that matched five rows and now matches none is the ordinary resolve
// path, so nothing about what a query read is compared against the last one
// (spec 6.3.2).
func TestDriftIgnoresWhatTheQueryRead(t *testing.T) {
	src := source.Source{Name: "payments_prod"}
	d := newDrift(driftRule(src))
	now := time.Now()

	same := shape("value", "Float64")
	d.inspect([]evaluated{{source: src, shape: same, usage: query.Usage{ReadRows: 5_000_000}}}, nil, now)

	got := d.inspect([]evaluated{{source: src, shape: same, usage: query.Usage{ReadRows: 0}}}, nil, now)
	if len(got) != 0 {
		t.Fatalf("reported %d problems, want none: %v", len(got), got)
	}
}

// Two sources of one rule that stopped agreeing, which the evaluations already
// happening can see on any tick (spec 6.10.1).
func TestDriftReportsSourcesThatStoppedAgreeing(t *testing.T) {
	prod := source.Source{Name: "payments_prod"}
	eu := source.Source{Name: "payments_eu"}
	d := newDrift(driftRule(prod, eu))

	got := d.inspect([]evaluated{
		{source: prod, shape: shape("value", "Float64")},
		{source: eu, shape: shape("value", "Int64")},
	}, nil, time.Now())

	if len(got) != 1 {
		t.Fatalf("reported %d problems, want 1: %v", len(got), got)
	}
	if got[0].Check != lint.CheckRuleSourceSchema {
		t.Fatalf("check is %s, want %s", got[0].Check, lint.CheckRuleSourceSchema)
	}
	for _, want := range []string{"payments_prod", "payments_eu"} {
		if !strings.Contains(got[0].Text, want) {
			t.Errorf("text is %q, want it to name %s", got[0].Text, want)
		}
	}
}

// What an evaluation measured against the ceilings the same rule's cost was
// predicted against at check time (spec 6.7).
func TestDriftReportsCostOverTheCeiling(t *testing.T) {
	src := source.Source{Name: "payments_prod"}
	r := driftRule(src)
	r.Group = rule.Group{Interval: time.Minute}
	r.Policy = costPolicy("1000", "1000000")
	d := newDrift(r)
	now := time.Now()

	same := shape("value", "Float64")
	d.inspect([]evaluated{{source: src, shape: same, usage: query.Usage{ReadRows: 10}}}, nil, now)

	got := d.inspect([]evaluated{{source: src, shape: same, usage: query.Usage{ReadRows: 5000}}}, nil, now)
	if len(got) != 1 {
		t.Fatalf("reported %d problems, want 1: %v", len(got), got)
	}
	p := got[0]
	if p.Check != lint.CheckRuleCost {
		t.Fatalf("check is %s, want %s", p.Check, lint.CheckRuleCost)
	}
	if p.Severity != lint.SeverityWarning {
		t.Errorf("severity is %s, want the warning policy set", p.Severity)
	}
	if p.PolicyFile != "rules/policy.yaml" {
		t.Errorf("policy file is %q, want the file that set the ceiling", p.PolicyFile)
	}
	if !strings.Contains(p.Text, "5000 rows") {
		t.Errorf("text is %q, want it to carry what the evaluation read", p.Text)
	}
}

// A check an operator switched off is not reported, and a source with an
// unexpired exemption for it is not reported either (spec 7.7).
func TestDriftHonoursPolicyAndExemptions(t *testing.T) {
	now := time.Now()
	same := shape("value", "Float64")

	t.Run("off", func(t *testing.T) {
		src := source.Source{Name: "payments_prod"}
		r := driftRule(src)
		r.Group = rule.Group{Interval: time.Minute}
		r.Policy = &policy.Policy{Checks: map[string]policy.Setting{
			lint.CheckRuleCost: {Severity: lint.SeverityOff},
		}}
		d := newDrift(r)

		d.inspect([]evaluated{{source: src, shape: same, usage: query.Usage{ReadRows: 10}}}, nil, now)
		if got := d.inspect([]evaluated{{source: src, shape: same, usage: query.Usage{ReadRows: 1 << 40}}}, nil, now); len(got) != 0 {
			t.Fatalf("reported %d problems, want none: %v", len(got), got)
		}
	})

	t.Run("exempt", func(t *testing.T) {
		src := source.Source{
			Name:       "payments_prod",
			Exemptions: []source.Exemption{{Check: lint.CheckRuleCost, Until: now.Add(24 * time.Hour)}},
		}
		r := driftRule(src)
		r.Group = rule.Group{Interval: time.Minute}
		r.Policy = costPolicy("1000", "1000000")
		d := newDrift(r)

		d.inspect([]evaluated{{source: src, shape: same, usage: query.Usage{ReadRows: 10}}}, nil, now)
		if got := d.inspect([]evaluated{{source: src, shape: same, usage: query.Usage{ReadRows: 5000}}}, nil, now); len(got) != 0 {
			t.Fatalf("reported %d problems, want none: %v", len(got), got)
		}
	})
}

// A query that fails is the fourth thing an evaluation knows for free, and the
// one the counter cannot address: clickhouse_ruler_rule_evaluation_failures_total
// tells an operator that something failed, and this tells the rule's owner
// which rule, in which file, on which cluster (spec 6.3.2).
func TestDriftReportsAQueryThatFailed(t *testing.T) {
	src := source.Source{Name: "payments_prod"}
	d := newDrift(driftRule(src))

	got := d.inspect(nil, []failed{{
		source: src,
		err:    errors.New("Code: 47. Unknown expression identifier 'status_code'"),
	}}, time.Now())

	if len(got) != 1 {
		t.Fatalf("reported %d problems, want 1: %v", len(got), got)
	}
	p := got[0]
	if p.Check != lint.CheckRuleExecution {
		t.Errorf("check is %s, want %s", p.Check, lint.CheckRuleExecution)
	}
	if p.Severity != lint.SeverityError {
		t.Errorf("severity is %s, want error: a rule whose query does not run cannot page", p.Severity)
	}
	if p.File != "rules/payments.yaml" || p.Subject != "SlowCheckout" {
		t.Errorf("problem names %s %s, want the rule's own file and alert", p.File, p.Subject)
	}
	for _, want := range []string{"payments_prod", "status_code"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("text is %q, want it to contain %q", p.Text, want)
		}
	}
}

// Reported on the first evaluation and on every one after it. The gauge is
// rebuilt per pass, so a finding that stays raised is a rule still broken and
// one that disappears is a rule running again; waiting for a transition would
// leave a ruler restarted into a broken cluster reporting nothing at all.
func TestDriftReportsAQueryThatKeepsFailing(t *testing.T) {
	src := source.Source{Name: "payments_prod"}
	d := newDrift(driftRule(src))
	now := time.Now()
	fail := []failed{{source: src, err: errors.New("Code: 60. Table does not exist")}}

	for i, when := range []time.Time{now, now.Add(time.Minute), now.Add(2 * time.Minute)} {
		if got := d.inspect(nil, fail, when); len(got) != 1 {
			t.Fatalf("evaluation %d reported %d problems, want 1: %v", i, len(got), got)
		}
	}
}

// The shape baseline of a source that failed is left standing, so the next
// successful evaluation is compared against the last real result rather than
// against nothing (spec 6.3.2).
func TestDriftKeepsTheBaselineOfASourceThatFailed(t *testing.T) {
	src := source.Source{Name: "payments_prod"}
	d := newDrift(driftRule(src))
	now := time.Now()

	d.inspect([]evaluated{{source: src, shape: shape("value", "Float64")}}, nil, now)
	d.inspect(nil, []failed{{source: src, err: errors.New("Code: 60. Table does not exist")}}, now)

	got := d.inspect([]evaluated{{source: src, shape: shape("value", "Int64")}}, nil, now)
	if len(got) != 1 {
		t.Fatalf("reported %d problems, want the retype the baseline should still catch: %v", len(got), got)
	}
	if got[0].Check != lint.CheckRuleColumns {
		t.Errorf("check is %s, want %s", got[0].Check, lint.CheckRuleColumns)
	}
}

// A cluster an operator knows is broken, turned down per source the way every
// other runtime finding can be (spec 7.7).
func TestDriftHonoursPolicyForAFailedQuery(t *testing.T) {
	now := time.Now()
	fail := errors.New("Code: 60. Table does not exist")

	t.Run("off", func(t *testing.T) {
		src := source.Source{Name: "payments_prod"}
		r := driftRule(src)
		r.Policy = &policy.Policy{Checks: map[string]policy.Setting{
			lint.CheckRuleExecution: {Severity: lint.SeverityOff},
		}}
		d := newDrift(r)

		if got := d.inspect(nil, []failed{{source: src, err: fail}}, now); len(got) != 0 {
			t.Fatalf("reported %d problems, want none: %v", len(got), got)
		}
	})

	t.Run("exempt", func(t *testing.T) {
		src := source.Source{
			Name:       "payments_prod",
			Exemptions: []source.Exemption{{Check: lint.CheckRuleExecution, Until: now.Add(24 * time.Hour)}},
		}
		d := newDrift(driftRule(src))

		if got := d.inspect(nil, []failed{{source: src, err: fail}}, now); len(got) != 0 {
			t.Fatalf("reported %d problems, want none: %v", len(got), got)
		}
	})
}
