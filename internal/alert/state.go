package alert

import (
	"fmt"
	"maps"
	"sort"
	"text/template"
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

	// Annotations are rendered here, when the instance is evaluated, rather
	// than at send time. They are part of what the alert is: a resolved
	// instance says what it said when it fired, a broken template is an
	// evaluation problem rather than a delivery one, and one unrenderable
	// annotation cannot block the batch it travels in (spec 6.5).
	Annotations map[string]string

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

// LabelAlertname carries the rule's name, and with LabelSource it is the pair
// the ruler sets itself on every alert. Both are named here so the checks can
// say which labels a query may not produce (spec 6.3.1).
const LabelAlertname = "alertname"

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

	// annotations are the rule's annotation templates, compiled once because
	// they are fixed for the rule's lifetime. parseErrs holds the ones that did
	// not compile, so a failure reaches the page rather than vanishing.
	annotations map[string]*template.Template
	parseErrs   map[string]error

	// retention is how long a resolved instance is kept so its notification can
	// be retried. Retention has to outlive delivery, or the guarantee that a
	// failed send is retried covers firing alerts and quietly not resolves.
	retention time.Duration

	// hash identifies an instance by its labels. It is a field so a test can
	// force the collision that cannot be produced by chance; nothing outside
	// this package can change it, and the real fingerprint is what New sets.
	hash func(map[string]string) uint64
}

// New builds the state for one rule against one source. Group labels are the
// weakest, then the rule's own labels; the source's are applied per evaluation
// because they outrank the query's own columns.
//
// resolvedRetention is how long a resolved instance is kept and re-asserted
// after it resolves, so a resolve whose notification failed still has attempts
// left. The caller derives it from how long delivery can take (spec 6.5); a
// zero or negative value keeps the old behaviour of returning a resolve once.
func New(r rule.Rule, groupLabels map[string]string, src source.Source, resolvedRetention time.Duration) *State {
	base := baseLabels(r, groupLabels)

	parsed, parseErrs := parseAnnotations(r.Annotations)

	return &State{
		rule:        r,
		src:         src,
		base:        base,
		annotations: parsed,
		parseErrs:   parseErrs,
		active:      map[uint64][]*instance{},
		hash:        fingerprint,
		retention:   resolvedRetention,
	}
}

// baseLabels is the rule's file-level label set: group labels overlaid with
// the rule's own, which is levels 1 and 2 of spec 6.3.1.
func baseLabels(r rule.Rule, groupLabels map[string]string) map[string]string {
	base := make(map[string]string, len(groupLabels)+len(r.Labels)+1)
	for k, v := range groupLabels {
		base[k] = v
	}
	for k, v := range r.Labels {
		base[k] = v
	}
	return base
}

// Adopt re-points this state at a reloaded definition of the same rule,
// keeping every instance it tracks, and reports whether it could. A false
// return means the definition is a different alert and the caller has to build
// a fresh state for it.
//
// What "the same rule" means here is exactly what an alert's identity is made
// of (spec 6.3): the label set. That is the rule's effective labels, its name,
// and the source it evaluated against, because labelsFor assembles a
// fingerprint from all three. Change any one of them and every instance held
// here has a fingerprint no future evaluation will produce: it would never be
// seen again, so it would resolve on the next evaluation and then be re-created
// under its new identity with its `for` timer starting from zero. Refusing is
// the same outcome arrived at in one step instead of two, and without a resolve
// notification for an alert that did not recover.
//
// Everything else about a rule is free to change. A new threshold, a new
// window, a different `for`, an edited annotation: none of them alter which
// alert this is, and all of them are edits an author makes to a rule that is
// already pending. Keeping the instance is the entire point of reloading rather
// than restarting, and the edited definition takes effect from this evaluation
// on: a shortened `for` is measured against the ActiveAt the instance already
// had, and an annotation is re-rendered on every evaluation anyway.
func (s *State) Adopt(r rule.Rule, groupLabels map[string]string, src source.Source, resolvedRetention time.Duration) bool {
	base := baseLabels(r, groupLabels)
	if r.Alert != s.rule.Alert || !maps.Equal(base, s.base) {
		return false
	}
	if src.Name != s.src.Name || !maps.Equal(src.Labels, s.src.Labels) {
		return false
	}

	parsed, parseErrs := parseAnnotations(r.Annotations)

	s.rule = r
	s.base = base
	// The source carries more than its labels: an address, a timestamp column,
	// the caps a query is sent with. Those belong to the reloaded file too,
	// even though they are not part of what the alert is.
	s.src = src
	s.annotations = parsed
	s.parseErrs = parseErrs
	s.retention = resolvedRetention
	return true
}

// Eval advances every instance by one evaluation and returns every instance it
// still tracks: pending, firing, and resolved ones inside their retention
// window. A resolve is returned on every evaluation of that window, so a
// notification that failed has further attempts (spec 6.5).
//
// An error means the evaluation did not happen: nothing is tracked, no timer
// moves, and the caller has the same state it had before. That is deliberately
// the same outcome as a failed query, so a rule that cannot be evaluated does
// not also lose the alerts it already had (spec 6.3).
//
// The AnnotationErrors are not that. They report templates that would not
// render, which leaves the alert intact and still worth sending, and they are
// deduplicated to one entry per annotation however many instances the rule
// produced.
func (s *State) Eval(now time.Time, samples []Sample) ([]Alert, []AnnotationError, error) {
	labelled, err := s.resolveLabels(samples)
	if err != nil {
		return nil, nil, err
	}

	present := make(map[*instance]bool, len(labelled))
	broken := map[string]AnnotationError{}

	for _, l := range labelled {
		inst := s.track(l, now)
		present[inst] = true

		inst.Value = l.value
		inst.lastSeen = now

		if inst.Phase == PhasePending && now.Sub(inst.ActiveAt) >= s.rule.For {
			inst.Phase = PhaseFiring
			inst.FiredAt = now
		}

		// Rendered after the value is set, and on every evaluation, so a firing
		// alert's summary carries the value the responder is being paged about.
		// An instance that stops being returned keeps what it last rendered,
		// which is what it said when it fired.
		annotations, failures := annotate(s.annotations, s.parseErrs, inst.Alert)
		inst.Annotations = annotations
		for _, f := range failures {
			broken[f.Annotation] = f
		}
	}

	return s.expire(now, present), sortedFailures(broken), nil
}

// sortedFailures orders the deduplicated annotation failures by name, so a rule
// with two broken templates reports them the same way on every evaluation.
func sortedFailures(broken map[string]AnnotationError) []AnnotationError {
	if len(broken) == 0 {
		return nil
	}

	names := make([]string, 0, len(broken))
	for name := range broken {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]AnnotationError, 0, len(names))
	for _, name := range names {
		out = append(out, broken[name])
	}
	return out
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
//
// A label set that matches a resolved instance is a condition that came back,
// not the old alert continuing. It gets a new instance, so its `for` timer runs
// again from now and the recovery that was already reported stays reported.
func (s *State) track(l labelled, now time.Time) *instance {
	insts := s.active[l.fp]
	for i, inst := range insts {
		if !sameLabels(inst.Labels, l.labels) {
			continue
		}
		if inst.Phase != PhaseResolved {
			return inst
		}
		fresh := newInstance(l, now)
		insts[i] = fresh
		return fresh
	}

	inst := newInstance(l, now)
	s.active[l.fp] = append(s.active[l.fp], inst)
	return inst
}

func newInstance(l labelled, now time.Time) *instance {
	return &instance{Alert: Alert{
		Fingerprint: l.fp,
		Labels:      l.labels,
		Phase:       PhasePending,
		ActiveAt:    now,
	}}
}

// expire handles instances the latest evaluation did not return.
//
// A resolved instance is kept for the retention window rather than deleted on
// the way out, so it is returned on every evaluation in that window and the
// caller gets to try delivering the resolve again. Deleting it in the same step
// that first reported it meant a single failed notification lost the resolve
// outright: no retry, and Alertmanager holding a firing alert for something
// that had recovered.
func (s *State) expire(now time.Time, present map[*instance]bool) []Alert {
	for fp, insts := range s.active {
		kept := insts[:0]
		for _, inst := range insts {
			if present[inst] {
				kept = append(kept, inst)
				continue
			}
			// A pending instance was never notified, so it leaves without a
			// resolve for something nobody was told about, and with nothing to
			// retain because there is nothing to retry.
			if inst.Phase == PhasePending {
				continue
			}
			if inst.Phase == PhaseResolved {
				if now.Sub(inst.ResolvedAt) > s.retention {
					continue
				}
				kept = append(kept, inst)
				continue
			}
			if now.Sub(inst.lastSeen) < s.rule.KeepFiringFor {
				kept = append(kept, inst)
				continue
			}

			inst.Phase = PhaseResolved
			inst.ResolvedAt = now
			kept = append(kept, inst)
		}

		if len(kept) == 0 {
			delete(s.active, fp)
			continue
		}
		s.active[fp] = kept
	}

	return s.snapshot()
}

// tracked is how many instances the state holds, resolved ones included. Used
// by tests to prove the retention window actually frees them.
func (s *State) tracked() int {
	n := 0
	for _, insts := range s.active {
		n += len(insts)
	}
	return n
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
	labels[LabelAlertname] = s.rule.Alert
	labels[LabelSource] = s.src.Name
	return labels
}

// snapshot returns every tracked instance, pending, firing, and resolved ones
// still inside their retention window, in fingerprint order so a caller sees
// the same sequence for the same state.
//
// Two instances sharing a fingerprint make that order incomplete, so the label
// set breaks the tie. Without it the order of a colliding pair would follow
// Go's randomized map iteration, and a batch whose order changes from tick to
// tick is needlessly hard to read in a log or a diff.
func (s *State) snapshot() []Alert {
	if len(s.active) == 0 {
		return nil
	}

	out := make([]Alert, 0, len(s.active))
	for _, insts := range s.active {
		for _, inst := range insts {
			out = append(out, inst.Alert)
		}
	}

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
