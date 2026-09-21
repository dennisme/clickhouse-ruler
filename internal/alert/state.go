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
	rule   rule.Rule
	src    source.Source
	base   map[string]string
	active map[uint64]*instance
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
		active: map[uint64]*instance{},
	}
}

// Eval advances every instance by one evaluation and returns the pending and
// firing instances along with any that resolved on this evaluation. A
// resolved instance is returned once and then forgotten.
func (s *State) Eval(now time.Time, samples []Sample) []Alert {
	present := make(map[uint64]bool, len(samples))

	for _, smpl := range samples {
		labels := s.labelsFor(smpl)
		fp := fingerprint(labels)
		present[fp] = true

		inst, tracked := s.active[fp]
		if !tracked {
			inst = &instance{Alert: Alert{
				Fingerprint: fp,
				Labels:      labels,
				Phase:       PhasePending,
				ActiveAt:    now,
			}}
			s.active[fp] = inst
		}
		inst.Value = smpl.Value
		inst.lastSeen = now

		if inst.Phase == PhasePending && now.Sub(inst.ActiveAt) >= s.rule.For {
			inst.Phase = PhaseFiring
			inst.FiredAt = now
		}
	}

	return s.expire(now, present)
}

// expire handles instances the latest evaluation did not return.
func (s *State) expire(now time.Time, present map[uint64]bool) []Alert {
	var resolved []Alert

	for fp, inst := range s.active {
		if present[fp] {
			continue
		}
		// A pending instance was never notified, so it leaves without a
		// resolve for something nobody was told about.
		if inst.Phase == PhasePending {
			delete(s.active, fp)
			continue
		}
		if now.Sub(inst.lastSeen) < s.rule.KeepFiringFor {
			continue
		}

		inst.Phase = PhaseResolved
		inst.ResolvedAt = now
		resolved = append(resolved, inst.Alert)
		delete(s.active, fp)
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
func (s *State) snapshot(resolved []Alert) []Alert {
	if len(s.active) == 0 && len(resolved) == 0 {
		return nil
	}

	out := make([]Alert, 0, len(s.active)+len(resolved))
	for _, inst := range s.active {
		out = append(out, inst.Alert)
	}
	out = append(out, resolved...)

	sort.Slice(out, func(i, j int) bool {
		return out[i].Fingerprint < out[j].Fingerprint
	})
	return out
}
