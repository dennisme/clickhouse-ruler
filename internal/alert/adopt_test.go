package alert

import (
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// A reload that leaves a rule's identity alone has to keep the instances the
// running state already tracks, because the alternative is that every pending
// alert restarts its `for` whenever anybody edits any file.
func TestAdoptKeepsTrackedInstancesWhenIdentityIsUnchanged(t *testing.T) {
	r := testRule(10*time.Minute, 0)
	r.Annotations = map[string]string{"summary": "{{ .ServiceName }} is slow"}
	src := source.Source{Name: "otel_traces", Labels: map[string]string{"region": "eu"}}

	s := New(r, nil, src, testRetention)
	got := evalOK(t, s, t0, []Sample{sample("checkout", 900)})
	assertAlerts(t, got, []want{{service: "checkout", phase: PhasePending, value: 900, activeAt: t0}})

	// A new annotation and a shorter `for`, the same labels: the author edited
	// what the alert says and how long it waits, not which alert it is.
	edited := testRule(2*time.Minute, 0)
	edited.Annotations = map[string]string{"summary": "{{ .ServiceName }} is very slow"}

	if !s.Adopt(edited, nil, src, testRetention) {
		t.Fatal("Adopt refused a rule whose identity did not change")
	}

	// Five minutes on from the first evaluation, which is past the edited
	// `for` and short of the original one. The instance fires only if its
	// ActiveAt survived the reload.
	at := t0.Add(5 * time.Minute)
	got = evalOK(t, s, at, []Sample{sample("checkout", 950)})
	assertAlerts(t, got, []want{{service: "checkout", phase: PhaseFiring, value: 950, activeAt: t0, firedAt: at}})

	if summary := got[0].Annotations["summary"]; summary != "checkout is very slow" {
		t.Errorf("summary = %q, want the reloaded template's text", summary)
	}
}

// Everything an alert's identity is made of (spec 6.3): the rule's effective
// labels, its name, and the source it evaluated against. A reload that
// changes any of them produces different fingerprints, so the instances being
// tracked belong to alerts that no longer exist and carrying them over would
// leave instances nothing ever resolves.
func TestAdoptRefusesAChangedIdentity(t *testing.T) {
	src := source.Source{Name: "otel_traces", Labels: map[string]string{"region": "eu"}}

	renamed := testRule(time.Minute, 0)
	renamed.Alert = "HighLatency"

	relabelled := testRule(time.Minute, 0)
	relabelled.Labels = map[string]string{"team": "checkout", "severity": "warning"}

	extraLabel := testRule(time.Minute, 0)
	extraLabel.Labels = map[string]string{"team": "payments", "severity": "warning", "tier": "1"}

	tests := []struct {
		name        string
		rule        rule.Rule
		groupLabels map[string]string
		src         source.Source
	}{
		{name: "alert renamed", rule: renamed, src: src},
		{name: "rule label changed", rule: relabelled, src: src},
		{name: "rule label added", rule: extraLabel, src: src},
		{
			name:        "group label added",
			rule:        testRule(time.Minute, 0),
			groupLabels: map[string]string{"cluster": "eu1"},
			src:         src,
		},
		{
			name: "source renamed",
			rule: testRule(time.Minute, 0),
			src:  source.Source{Name: "otel_traces_eu", Labels: map[string]string{"region": "eu"}},
		},
		{
			name: "source label changed",
			rule: testRule(time.Minute, 0),
			src:  source.Source{Name: "otel_traces", Labels: map[string]string{"region": "us"}},
		},
	}

	for _, tc := range tests {
		s := New(testRule(time.Minute, 0), nil, src, testRetention)
		evalOK(t, s, t0, []Sample{sample("checkout", 900)})

		if s.Adopt(tc.rule, tc.groupLabels, tc.src, testRetention) {
			t.Errorf("%s: Adopt accepted a changed identity", tc.name)
		}
		if s.tracked() != 1 {
			t.Errorf("%s: refused Adopt left %d instances tracked, want the state untouched", tc.name, s.tracked())
		}
	}
}
