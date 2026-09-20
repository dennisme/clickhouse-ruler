package notify

import (
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

// Alert is one entry in an Alertmanager POST /api/v2/alerts body.
//
// Times are strings rather than time.Time so that an unset endsAt marshals as
// an absent field. Alertmanager treats a zero endsAt as "resolve now", so
// emitting one on a firing alert would resolve it the instant it fired.
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
