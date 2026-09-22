package notify

import (
	"context"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

// testEvalInterval is shorter than every cadence these tests use, so the
// cadence interval is what paces a resend and the group interval never is.
const testEvalInterval = time.Second

// recordingSender stands in for a Client, so cadence logic can be tested
// without an HTTP server or the real retry path.
type recordingSender struct {
	calls [][]alert.Alert
	err   error
}

func (s *recordingSender) Send(_ context.Context, alerts []alert.Alert) error {
	s.calls = append(s.calls, alerts)
	return s.err
}

func firing(fp uint64) alert.Alert {
	return alert.Alert{Fingerprint: fp, Phase: alert.PhaseFiring, FiredAt: payloadAnchor}
}

func resolved(fp uint64) alert.Alert {
	return alert.Alert{Fingerprint: fp, Phase: alert.PhaseResolved, FiredAt: payloadAnchor, ResolvedAt: payloadAnchor}
}

// A firing alert must reach Alertmanager the first time Cadence sees it,
// otherwise nothing would ever fire.
func TestCadenceSendsAFiringAlertTheFirstTime(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute, DefaultResendTolerance)

	now := time.Now()
	if err := c.Send(context.Background(), now, testEvalInterval, []alert.Alert{firing(1)}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(s.calls) != 1 || len(s.calls[0]) != 1 {
		t.Fatalf("got %v, want one call with one alert", s.calls)
	}
}

// Re-sending on every evaluation is the traffic problem 6.5 exists to avoid.
// A firing alert seen again before its cadence interval elapses must not be
// re-posted.
func TestCadenceDropsAFiringAlertBeforeItsCadenceElapses(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute, DefaultResendTolerance)

	now := time.Now()
	if err := c.Send(context.Background(), now, testEvalInterval, []alert.Alert{firing(1)}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Well inside the one minute cadence.
	if err := c.Send(context.Background(), now.Add(10*time.Second), testEvalInterval, []alert.Alert{firing(1)}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(s.calls) != 1 {
		t.Fatalf("got %d calls, want 1: the second evaluation should not have posted", len(s.calls))
	}
}

// Alertmanager expires a firing alert after resolve_timeout, so once the
// cadence interval elapses the alert has to go out again to keep it alive.
func TestCadenceResendsAFiringAlertOnceItsCadenceElapses(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute, DefaultResendTolerance)

	now := time.Now()
	if err := c.Send(context.Background(), now, testEvalInterval, []alert.Alert{firing(1)}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := c.Send(context.Background(), now.Add(time.Minute), testEvalInterval, []alert.Alert{firing(1)}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(s.calls) != 2 {
		t.Fatalf("got %d calls, want 2: the alert should have been resent once its cadence elapsed", len(s.calls))
	}
}

// A resolve is not held behind the cadence of the firing alert it replaces. The
// instance has recovered, and every evaluation Alertmanager spends not knowing
// that is one where it shows an alert that is no longer true.
func TestCadenceSendsAResolveWithoutWaitingForTheInterval(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute, DefaultResendTolerance)

	now := time.Now()
	if err := c.Send(context.Background(), now, testEvalInterval, []alert.Alert{firing(1)}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Resolved arrives on the very next evaluation, well inside the cadence.
	res := resolved(1)
	res.ResolvedAt = now.Add(time.Second)
	if err := c.Send(context.Background(), now.Add(time.Second), testEvalInterval, []alert.Alert{res}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(s.calls) != 2 {
		t.Fatalf("got %d calls, want 2: the resolve must not be held back by cadence", len(s.calls))
	}
	if len(s.calls[1]) != 1 || s.calls[1][0].Phase != alert.PhaseResolved {
		t.Fatalf("second call = %v, want the resolved alert", s.calls[1])
	}
}

// A send failure must not be recorded as sent, so the outage is retried on
// the next evaluation rather than waiting out a full cadence interval. This
// is what keeps a notification failure from losing the alert (spec 6.5).
func TestCadenceRetriesAfterASendFailureWithoutWaitingForCadence(t *testing.T) {
	s := &recordingSender{err: context.DeadlineExceeded}
	c := NewCadence(s, time.Minute, DefaultResendTolerance)

	now := time.Now()
	if err := c.Send(context.Background(), now, testEvalInterval, []alert.Alert{firing(1)}); err == nil {
		t.Fatal("want error from a failing sender")
	}

	s.err = nil
	if err := c.Send(context.Background(), now.Add(time.Second), testEvalInterval, []alert.Alert{firing(1)}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(s.calls) != 2 || len(s.calls[1]) != 1 {
		t.Fatalf("got %v, want the retry to include the alert", s.calls)
	}
}

// A firing alert has to tell Alertmanager how long to hold it, or expiry
// falls back to a resolve_timeout the ruler cannot read. Four resend
// intervals absorbs three consecutive failed sends before Alertmanager
// wrongly resolves it.
func TestCadenceStampsValidityFromTheCadenceInterval(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute, DefaultResendTolerance)

	now := time.Now()
	if err := c.Send(context.Background(), now, testEvalInterval, []alert.Alert{firing(1)}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(s.calls) != 1 || len(s.calls[0]) != 1 {
		t.Fatalf("got %v, want one call with one alert", s.calls)
	}
	want := now.Add(4 * time.Minute)
	if got := s.calls[0][0].ValidUntil; !got.Equal(want) {
		t.Errorf("ValidUntil = %v, want %v", got, want)
	}
}

// A group ticking slower than the cadence is what actually paces the resend,
// because Cadence cannot send between evaluations it is never called on. A
// validity sized off the cadence alone would expire in that gap.
func TestCadenceStampsValidityFromTheGroupIntervalWhenItIsLonger(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute, DefaultResendTolerance)

	now := time.Now()
	if err := c.Send(context.Background(), now, 10*time.Minute, []alert.Alert{firing(1)}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(s.calls) != 1 || len(s.calls[0]) != 1 {
		t.Fatalf("got %v, want one call with one alert", s.calls)
	}
	want := now.Add(40 * time.Minute)
	if got := s.calls[0][0].ValidUntil; !got.Equal(want) {
		t.Errorf("ValidUntil = %v, want %v", got, want)
	}
}

// A resolved alert ends when it resolved. Stamping a future validity on it
// would tell Alertmanager to keep holding an alert that is no longer true.
func TestCadenceLeavesAResolvedAlertWithoutValidity(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute, DefaultResendTolerance)

	now := time.Now()
	if err := c.Send(context.Background(), now, testEvalInterval, []alert.Alert{resolved(1)}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(s.calls) != 1 || len(s.calls[0]) != 1 {
		t.Fatalf("got %v, want one call with one alert", s.calls)
	}
	if got := s.calls[0][0].ValidUntil; !got.IsZero() {
		t.Errorf("ValidUntil = %v, want zero on a resolved alert", got)
	}
}

// Four resend periods is what Prometheus gives itself, and it is sized for a
// local evaluation failing. A ClickHouse outage is a network call to a
// separate database, so an operator has to be able to buy more room than
// Prometheus needed.
func TestCadenceStampsValidityFromTheConfiguredTolerance(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute, 10)

	now := time.Now()
	if err := c.Send(context.Background(), now, testEvalInterval, []alert.Alert{firing(1)}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(s.calls) != 1 || len(s.calls[0]) != 1 {
		t.Fatalf("got %v, want one call with one alert", s.calls)
	}
	want := now.Add(10 * time.Minute)
	if got := s.calls[0][0].ValidUntil; !got.Equal(want) {
		t.Errorf("ValidUntil = %v, want %v", got, want)
	}
}

// The default is Prometheus' own number, so an operator who changes nothing
// gets the behaviour a Prometheus ruler would have given them.
func TestDefaultResendToleranceMatchesPrometheus(t *testing.T) {
	if DefaultResendTolerance != 4 {
		t.Errorf("DefaultResendTolerance = %d, want 4", DefaultResendTolerance)
	}
}

// A retained resolve is returned on every evaluation for its whole window, so
// without a throttle it would post on every tick. It is re-asserted on the
// resend interval, like a firing alert.
func TestCadenceThrottlesARetainedResolve(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute, DefaultResendTolerance)

	now := time.Now()
	a := resolved(1)
	a.ResolvedAt = now

	for _, at := range []time.Time{now, now.Add(10 * time.Second), now.Add(30 * time.Second)} {
		if err := c.Send(context.Background(), at, testEvalInterval, []alert.Alert{a}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if len(s.calls) != 1 {
		t.Fatalf("got %d calls inside one interval, want 1", len(s.calls))
	}

	if err := c.Send(context.Background(), now.Add(time.Minute), testEvalInterval, []alert.Alert{a}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(s.calls) != 2 {
		t.Fatalf("got %d calls, want 2: the resolve is due again after the interval", len(s.calls))
	}
}

// The same fingerprint comes back when a condition recurs, and the new instance
// is a new alert. It must not be throttled behind the resolve that preceded it,
// or a real recovery and re-fire would page late by up to a resend interval.
func TestCadenceSendsARecurrenceImmediatelyAfterAResolve(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, 5*time.Minute, DefaultResendTolerance)

	now := time.Now()
	res := resolved(1)
	res.ResolvedAt = now
	if err := c.Send(context.Background(), now, testEvalInterval, []alert.Alert{res}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Fires again a second later, which is well inside the resend interval.
	again := firing(1)
	again.FiredAt = now.Add(time.Second)
	if err := c.Send(context.Background(), now.Add(time.Second), testEvalInterval, []alert.Alert{again}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(s.calls) != 2 {
		t.Fatalf("got %d calls, want 2: the re-fire must not wait for the interval", len(s.calls))
	}
	if s.calls[1][0].Phase != alert.PhaseFiring {
		t.Errorf("second call phase = %v, want firing", s.calls[1][0].Phase)
	}
}

// lastSent used to be deleted the moment an alert resolved, on the reasoning
// that a resolve was returned once. Retention makes that false, so the entry
// now outlives the resolve and something has to remove it, or the map grows for
// the life of the process.
func TestCadenceDoesNotKeepEntriesForever(t *testing.T) {
	s := &recordingSender{}
	c := NewCadence(s, time.Minute, DefaultResendTolerance)

	now := time.Now()
	for fp := uint64(1); fp <= 100; fp++ {
		a := resolved(fp)
		a.ResolvedAt = now
		if err := c.Send(context.Background(), now, testEvalInterval, []alert.Alert{a}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if got := c.entries(); got != 100 {
		t.Fatalf("got %d tracked fingerprints, want 100", got)
	}

	// Far beyond any window in which those fingerprints could still be
	// re-asserted, and one unrelated alert keeps arriving.
	later := now.Add(time.Hour)
	keep := resolved(999)
	keep.ResolvedAt = later
	if err := c.Send(context.Background(), later, testEvalInterval, []alert.Alert{keep}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := c.entries(); got != 1 {
		t.Errorf("got %d tracked fingerprints, want 1: the dead ones must be pruned", got)
	}
}
