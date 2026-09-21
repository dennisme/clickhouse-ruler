package notify

import (
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

// Alert is one entry in an Alertmanager POST /api/v2/alerts body.
//
// Times are strings rather than time.Time so that an unset endsAt marshals as
// an absent field. Alertmanager reads an absent endsAt as "hold this for my
// own resolve_timeout", and an endsAt in the past as resolved, so the field
// decides when an alert expires and a wrong one resolves it early.
type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations,omitempty"`
	StartsAt    string            `json:"startsAt,omitempty"`
	EndsAt      string            `json:"endsAt,omitempty"`
}

// Payload renders annotations and maps evaluated alerts to the v2 wire format.
//
// Pending alerts are dropped. A `for` duration exists precisely so that a
// condition which has not held long enough does not page, and Prometheus
// notifies on firing and resolved only.
//
// A firing alert's endsAt is its validity, so the ruler decides when
// Alertmanager may expire it. A resolved alert's is when it resolved, which
// outranks whatever validity it was carrying while it fired.
func Payload(alerts []alert.Alert, annotations map[string]string) ([]Alert, error) {
	out := make([]Alert, 0, len(alerts))

	for _, a := range alerts {
		if a.Phase == alert.PhasePending {
			continue
		}

		rendered, err := Render(annotations, a)
		if err != nil {
			return nil, err
		}

		entry := Alert{
			Labels:      a.Labels,
			Annotations: rendered,
			StartsAt:    format(a.FiredAt),
			EndsAt:      format(a.ValidUntil),
		}
		if a.Phase == alert.PhaseResolved {
			entry.EndsAt = format(a.ResolvedAt)
		}
		out = append(out, entry)
	}
	return out, nil
}

func format(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
