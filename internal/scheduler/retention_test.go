package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
)

// Retention has to outlive delivery, and delivery is already written down: the
// tolerance times the period an alert is actually re-sent on. Deriving it means
// an operator who widens either flag cannot silently re-open the bug where a
// failed resolve is lost (spec 6.5).
func TestRetentionDerivesFromTheResendSettings(t *testing.T) {
	tests := []struct {
		name          string
		resend        Resend
		groupInterval time.Duration
		want          time.Duration
	}{
		{
			name:          "shipped defaults",
			resend:        Resend{Interval: notify.DefaultResendInterval, Tolerance: notify.DefaultResendTolerance},
			groupInterval: time.Minute,
			want:          4 * 100 * time.Second,
		},
		{
			// A group ticking slower than the cadence is what actually paces a
			// resend, because nothing is sent between two evaluations.
			name:          "group interval is longer",
			resend:        Resend{Interval: time.Minute, Tolerance: 4},
			groupInterval: 10 * time.Minute,
			want:          40 * time.Minute,
		},
		{
			name:          "wider tolerance",
			resend:        Resend{Interval: time.Minute, Tolerance: 10},
			groupInterval: time.Minute,
			want:          10 * time.Minute,
		},
		{
			name:          "wider interval",
			resend:        Resend{Interval: 5 * time.Minute, Tolerance: 4},
			groupInterval: time.Minute,
			want:          20 * time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.resend.retention(tc.groupInterval); got != tc.want {
				t.Errorf("retention = %s, want %s", got, tc.want)
			}
		})
	}
}

// Widening either setting must widen the window, in both directions, so nobody
// has to reason about which one dominates.
func TestWideningEitherResendSettingWidensRetention(t *testing.T) {
	base := Resend{Interval: time.Minute, Tolerance: 4}
	const group = time.Minute

	if wider := (Resend{Interval: base.Interval, Tolerance: base.Tolerance + 1}); wider.retention(group) <= base.retention(group) {
		t.Errorf("tolerance %d -> %d did not widen retention: %s -> %s",
			base.Tolerance, wider.Tolerance, base.retention(group), wider.retention(group))
	}
	if wider := (Resend{Interval: base.Interval * 2, Tolerance: base.Tolerance}); wider.retention(group) <= base.retention(group) {
		t.Errorf("interval %s -> %s did not widen retention: %s -> %s",
			base.Interval, wider.Interval, base.retention(group), wider.retention(group))
	}
}

// The window an alert.State keeps a resolve for is the same span Cadence stamps
// as a validity. They are the same question asked twice: how long can delivery
// still be in progress. Letting them drift would mean either retaining a
// resolve nobody will re-send, or dropping one that is still being retried.
func TestRetentionMatchesTheValidityCadenceStamps(t *testing.T) {
	resend := Resend{Interval: 100 * time.Second, Tolerance: 4}
	const group = 10 * time.Minute

	sender := &recordingSender{}
	cadence := notify.NewCadence(sender, resend.Interval, resend.Tolerance)

	// Cadence has no accessor for validity, so it is read off the wire: the
	// endsAt it stamps on a firing alert is now plus that span.
	now := time.Now()
	fired := alert.Alert{Fingerprint: 1, Phase: alert.PhaseFiring, FiredAt: now}
	if err := cadence.Send(context.Background(), now, group, []alert.Alert{fired}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(sender.calls) != 1 || len(sender.calls[0]) != 1 {
		t.Fatalf("got %v, want one call with one alert", sender.calls)
	}

	stamped := sender.calls[0][0].ValidUntil.Sub(now)
	if got := resend.retention(group); got != stamped {
		t.Errorf("retention = %s, but Cadence stamps a validity of %s", got, stamped)
	}
}
