package alert

import (
	"fmt"
	"sort"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// Sample is one row returned by a rule query. Every result column except
// value becomes a label, so one row is one alert instance.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Phase is where an alert instance sits in its lifecycle.
type Phase int

const (
	PhasePending Phase = iota
	PhaseFiring
	PhaseResolved
)

func (p Phase) String() string {
	switch p {
	case PhasePending:
		return "pending"
	case PhaseFiring:
		return "firing"
	case PhaseResolved:
		return "resolved"
	default:
		return fmt.Sprintf("phase(%d)", int(p))
	}
}

// Alert is one instance of a rule, identified by its label set.
type Alert struct {
	Fingerprint uint64
	Labels      map[string]string
	Value       float64
	Phase       Phase
	ActiveAt    time.Time
	FiredAt     time.Time
	ResolvedAt  time.Time

	// ValidUntil is how long Alertmanager should hold this alert without
	// hearing about it again. It is stamped by notify.Cadence at send time
	// rather than here, because its length is a property of how often the
	// alert is re-sent and the state machine knows nothing about that.
	ValidUntil time.Time
}

// instance is a tracked alert plus the bookkeeping a caller never sees.
type instance struct {
	Alert

	// lastSeen is the most recent evaluation whose samples included this
	// instance. keep_firing_for is measured from it.
	lastSeen time.Time
}

// LabelSource names the source an alert came from. It is what keeps two
// clusters' alerts apart when one rule evaluates against both.
const LabelSource = "source"

// State tracks the alert instances of a single rule against a single source.
//
// A rule matching several sources gets a State per source. They share nothing:
// a rule that recovers on one cluster and not another must resolve one alert
// and leave the other firing, which only works if each source counts its
// instances separately (spec 6.10.1).
type State struct {
	rule rule.Rule
	src  source.Source
	base map[string]string

	// active holds every tracked instance, grouped by fingerprint. A slice
	// rather than one instance per key because the fingerprint is 64 bits and
	// two different label sets can hash alike; merging them would give two
	// unrelated alerts one value and one `for` timer. In every real case the
	// slice holds exactly one instance.
	active map[uint64][]*instance

	// hash identifies an instance by its labels. It is a field so a test can
	// force the collision that cannot be produced by chance; nothing outside
	// this package can change it, and the real fingerprint is what New sets.
	hash func(map[string]string) uint64
}

// New builds the state for one rule against one source. Group labels are the
// weakest, then the rule's own labels; the source's are applied per evaluation
// because they outrank the query's own columns.
func New(r rule.Rule, groupLabels map[string]string, src source.Source) *State {
	base := make(map[string]string, len(groupLabels)+len(r.Labels)+1)
	for k, v := range groupLabels {
		base[k] = v
	}
	for k, v := range r.Labels {
		base[k] = v
	}

	return &State{
		rule:   r,
		src:    src,
		base:   base,
		active: map[uint64][]*instance{},
		hash:   fingerprint,
	}
}

// Eval advances every instance by one evaluation and returns the pending and
// firing instances along with any that resolved on this evaluation. A
// resolved instance is returned once and then forgotten.
//
// An error means the evaluation did not happen: nothing is tracked, no timer
// moves, and the caller has the same state it had before. That is deliberately
// the same outcome as a failed query, so a rule that cannot be evaluated does
// not also lose the alerts it already had (spec 6.3).
func (s *State) Eval(now time.Time, samples []Sample) ([]Alert, error) {
	labelled, err := s.resolveLabels(samples)
	if err != nil {
		return nil, err
	}

	present := make(map[*instance]bool, len(labelled))

	for _, l := range labelled {
		inst := s.track(l, now)
		present[inst] = true

		inst.Value = l.value
		inst.lastSeen = now

		if inst.Phase == PhasePending && now.Sub(inst.ActiveAt) >= s.rule.For {
			inst.Phase = PhaseFiring
			inst.FiredAt = now
		}
	}

	return s.expire(now, present), nil
}

// labelled is one sample with its final label set and fingerprint worked out.
type labelled struct {
	labels map[string]string
	fp     uint64
	value  float64
}

// resolveLabels applies the label rules to every sample and refuses a result
// where two rows ended up as the same alert.
//
// It runs before anything is tracked, because failing half way through an
// evaluation would leave some instances advanced and others not, which is a
// worse state to recover from than not having evaluated at all.
func (s *State) resolveLabels(samples []Sample) ([]labelled, error) {
	out := make([]labelled, 0, len(samples))

	for _, smpl := range samples {
		labels := s.labelsFor(smpl)
		fp := s.hash(labels)

		for _, seen := range out {
			// Same fingerprint is not enough: two different label sets can
			// hash alike, and that is a collision to keep apart rather than a
			// rule to reject.
			if seen.fp == fp && sameLabels(seen.labels, labels) {
				return nil, &DuplicateLabelSetError{Alert: s.rule.Alert, Labels: labels}
			}
		}
		out = append(out, labelled{labels: labels, fp: fp, value: smpl.Value})
	}
	return out, nil
}

// track finds the instance this row belongs to, creating it if the row is new.
// Labels are compared rather than trusted to the fingerprint, so two label
// sets that hash alike get an instance each.
func (s *State) track(l labelled, now time.Time) *instance {
	for _, inst := range s.active[l.fp] {
		if sameLabels(inst.Labels, l.labels) {
			return inst
		}
	}

	inst := &instance{Alert: Alert{
		Fingerprint: l.fp,
		Labels:      l.labels,
		Phase:       PhasePending,
		ActiveAt:    now,
	}}
	s.active[l.fp] = append(s.active[l.fp], inst)
	return inst
}

// expire handles instances the latest evaluation did not return.
func (s *State) expire(now time.Time, present map[*instance]bool) []Alert {
	var resolved []Alert

	for fp, insts := range s.active {
		kept := insts[:0]
		for _, inst := range insts {
			if present[inst] {
				kept = append(kept, inst)
				continue
			}
			// A pending instance was never notified, so it leaves without a
			// resolve for something nobody was told about.
			if inst.Phase == PhasePending {
				continue
			}
			if now.Sub(inst.lastSeen) < s.rule.KeepFiringFor {
				kept = append(kept, inst)
				continue
			}

			inst.Phase = PhaseResolved
			inst.ResolvedAt = now
			resolved = append(resolved, inst.Alert)
		}

		if len(kept) == 0 {
			delete(s.active, fp)
			continue
		}
		s.active[fp] = kept
	}

	return s.snapshot(resolved)
}

// labelsFor builds an instance's final label set. A result column overrides a
// rule label of the same name, but alertname is set last because an alert
// whose name could be changed by query data would be a routing hazard.
// labelsFor composes an instance's labels in the precedence order of spec
// 6.3.1: group, rule, result columns, then the source.
//
// The source wins over the query on purpose. Its labels state where the
// evaluation actually happened, and a result column claiming otherwise is
// reporting something untrue. alertname and source are written last because
// they are the alert's identity, and an identity query data can set is a
// routing hazard.
func (s *State) labelsFor(smpl Sample) map[string]string {
	labels := make(map[string]string, len(s.base)+len(smpl.Labels)+len(s.src.Labels)+2)
	for k, v := range s.base {
		labels[k] = v
	}
	for k, v := range smpl.Labels {
		labels[k] = v
	}
	for k, v := range s.src.Labels {
		labels[k] = v
	}
	labels["alertname"] = s.rule.Alert
	labels[LabelSource] = s.src.Name
	return labels
}

// snapshot returns the still-tracked instances together with the ones that
// resolved, in fingerprint order so a caller sees the same sequence for the
// same state.
//
// Two instances sharing a fingerprint make that order incomplete, so the label
// set breaks the tie. Without it the order of a colliding pair would follow
// Go's randomized map iteration, and a batch whose order changes from tick to
// tick is needlessly hard to read in a log or a diff.
func (s *State) snapshot(resolved []Alert) []Alert {
	if len(s.active) == 0 && len(resolved) == 0 {
		return nil
	}

	out := make([]Alert, 0, len(s.active)+len(resolved))
	for _, insts := range s.active {
		for _, inst := range insts {
			out = append(out, inst.Alert)
		}
	}
	out = append(out, resolved...)

	sort.Slice(out, func(i, j int) bool {
		if out[i].Fingerprint != out[j].Fingerprint {
			return out[i].Fingerprint < out[j].Fingerprint
		}
		return labelKey(out[i].Labels) < labelKey(out[j].Labels)
	})
	return out
}

// DuplicateLabelSetError reports two result rows that became the same alert.
//
// The rule is asking for something it cannot express, so the author is the one
// who has to act, and the message carries the label set they collapsed onto.
// Prometheus reports the same condition as ErrDuplicateAlertLabelSet.
type DuplicateLabelSetError struct {
	Alert  string
	Labels map[string]string
}

func (e *DuplicateLabelSetError) Error() string {
	return fmt.Sprintf("rule %q: two result rows produce the same alert {%s}; "+
		"a source label is overwriting the column meant to tell them apart",
		e.Alert, labelKey(e.Labels))
}
