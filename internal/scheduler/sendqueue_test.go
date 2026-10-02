package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// blockingSender holds every send until it is released, which is what an
// Alertmanager working through the retry ladder looks like from here. It
// returns on cancellation as well, so a send a shutdown cut off does not hold
// the test's goroutine.
type blockingSender struct {
	entered chan struct{}
	release chan struct{}

	mu        sync.Mutex
	delivered [][]alert.Alert
}

func newBlockingSender() *blockingSender {
	return &blockingSender{entered: make(chan struct{}, 64), release: make(chan struct{})}
}

func (s *blockingSender) Send(ctx context.Context, alerts []alert.Alert) error {
	s.entered <- struct{}{}
	select {
	case <-s.release:
		s.mu.Lock()
		defer s.mu.Unlock()
		s.delivered = append(s.delivered, alerts)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *blockingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.delivered)
}

// slowSender takes a fixed time per send and respects cancellation, so a
// shutdown that does not wait for the queue delivers measurably less than one
// that does.
type slowSender struct {
	per time.Duration

	mu        sync.Mutex
	delivered [][]alert.Alert
}

func (s *slowSender) Send(ctx context.Context, alerts []alert.Alert) error {
	select {
	case <-time.After(s.per):
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = append(s.delivered, alerts)
	return nil
}

func (s *slowSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.delivered)
}

func testQueue(sender notify.Sender, capacity int) *SendQueue {
	m := NewMetrics(prometheus.NewRegistry())
	cadence := NewCadence(sender, "am", testResend, m, NewRealClock())
	return NewSendQueue(cadence, capacity, m, NewRealClock(), slog.New(slog.DiscardHandler))
}

// queueFor is the delivery path a test drives by hand. No worker is started,
// so drain() performs the real send on the test's own goroutine: a test that
// asserts what Alertmanager received stays free of sleeps and of ordering
// between two goroutines.
// loggingQueue is the queue a test reads log lines out of, sharing one logger
// and one registry with the scheduler in front of it.
func loggingQueue(sender notify.Sender, log *slog.Logger) (*SendQueue, *Metrics) {
	m := NewMetrics(prometheus.NewRegistry())
	cadence := NewCadence(sender, "am", testResend, m, NewRealClock())
	return NewSendQueue(cadence, DefaultNotificationQueueCapacity, m, NewRealClock(), log), m
}

func queueFor(sender notify.Sender, resendInterval time.Duration) *SendQueue {
	m := NewMetrics(prometheus.NewRegistry())
	cadence := notify.NewCadence(sender, resendInterval, notify.DefaultResendTolerance)
	return NewSendQueue(cadence, DefaultNotificationQueueCapacity, m, NewRealClock(), slog.New(slog.DiscardHandler))
}

// evaluateAndDeliver runs one evaluation and then performs the send it
// enqueued, which a running ruler's worker does a moment later on a goroutine
// of its own. A test that reads what Alertmanager received needs both halves,
// and needs them in this order (spec 6.5).
func evaluateAndDeliver(ctx context.Context, eval *RuleEval, now time.Time) Result {
	res := eval.Evaluate(ctx, now)
	eval.queue.drain()
	return res
}

func firing(name string) []alert.Alert {
	return []alert.Alert{{
		Phase:       alert.PhaseFiring,
		Fingerprint: fingerprintOf(name),
		Labels:      map[string]string{"alertname": name},
		FiredAt:     time.Now(),
	}}
}

// Distinct fingerprints, because Cadence throttles by fingerprint and two
// alerts sharing one would make the second look already sent.
func fingerprintOf(name string) uint64 {
	var h uint64 = 1469598103934665603
	for _, b := range []byte(name) {
		h = (h ^ uint64(b)) * 1099511628211
	}
	return h
}

func intervalRuleSet(alertName string, interval time.Duration) *ruleset.Set {
	return &ruleset.Set{Rules: []ruleset.Rule{{
		Rule:    rule.Rule{Alert: alertName},
		File:    "f.yaml",
		Path:    "f.yaml",
		Group:   testGroup("g1", interval),
		Labels:  map[string]string{},
		Sources: []source.Source{{Name: "src1"}},
	}}}
}

// The whole point of the queue. An evaluation hands its alerts over and
// returns; the retry ladder is the worker's problem, not the group's
// (spec 6.5).
func TestEvaluationReturnsWhileTheSendIsBlocked(t *testing.T) {
	sender := newBlockingSender()
	queue := testQueue(sender, DefaultNotificationQueueCapacity)
	queue.Start()
	defer queue.Drain(0)

	q := &fakeQuerier{samples: oneSample()}
	eval := NewRuleEval(testRule(0), map[string]Querier{"src1": q}, queue, newQueryLimits(0, nil, nil), testRetention)

	// The first evaluation's alerts reach the worker, which is now stuck
	// inside Send for as long as this test wants it to be.
	eval.Evaluate(context.Background(), time.Now())
	select {
	case <-sender.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never attempted the send")
	}

	done := make(chan struct{})
	go func() {
		eval.Evaluate(context.Background(), time.Now().Add(time.Second))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an evaluation blocked behind a send that has not returned")
	}
}

// What the missed iterations counter is supposed to mean: the evaluation
// overran the interval. An Alertmanager that stopped answering is not that,
// and it used to read as that because the retry ladder ran on the group's
// goroutine (spec 8.2).
func TestAGroupMissesNoIterationWhileSendsAreBlocked(t *testing.T) {
	const interval = 20 * time.Millisecond

	sender := newBlockingSender()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	cadence := NewCadence(sender, "am", testResend, m, NewRealClock())
	queue := NewSendQueue(cadence, DefaultNotificationQueueCapacity, m, NewRealClock(), slog.New(slog.DiscardHandler))

	sched := New(intervalRuleSet("Blocked", interval),
		map[string]Querier{"src1": &fakeQuerier{samples: oneSample()}},
		queue, m, NewRealClock(), 0, nil, testResend, 0)

	ctx, cancel := context.WithCancel(context.Background())
	sched.Start(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for testutil.ToFloat64(m.IterationsTotal.WithLabelValues("f.yaml:g1")) < 5 {
		if time.Now().After(deadline) {
			t.Fatal("the group stopped ticking while the send was blocked")
		}
		time.Sleep(time.Millisecond)
	}

	// Still blocked, so the group kept its interval with delivery stopped
	// outright rather than merely slow.
	if got := sender.count(); got != 0 {
		t.Fatalf("%d sends completed, want none: the test never released them", got)
	}

	cancel()
	sched.Shutdown(0)

	if got := testutil.ToFloat64(m.IterationsMissedTotal.WithLabelValues("f.yaml:g1")); got != 0 {
		t.Errorf("missed iterations = %v, want 0: delivery is not evaluation", got)
	}
}

// Blocking is what the queue removes, so a full queue cannot block. It drops,
// and it drops the oldest, because the oldest batch carries the `now` furthest
// in the past and its endsAt is the one Alertmanager can expire on arrival
// (spec 6.5).
func TestAFullQueueDropsTheOldestAlerts(t *testing.T) {
	sender := &recordingSender{}
	queue := testQueue(sender, 2)

	queue.Enqueue("f.yaml:g1", "First", time.Now(), time.Minute, firing("First"))
	queue.Enqueue("f.yaml:g1", "Second", time.Now(), time.Minute, firing("Second"))
	queue.Enqueue("f.yaml:g1", "Third", time.Now(), time.Minute, firing("Third"))

	queue.Drain(time.Second)

	var sent []string
	for _, call := range sender.calls {
		for _, a := range call {
			sent = append(sent, a.Labels["alertname"])
		}
	}
	if len(sent) != 2 || sent[0] != "Second" || sent[1] != "Third" {
		t.Fatalf("delivered %v, want the two newest in order", sent)
	}
	if got := testutil.ToFloat64(queue.metrics.NotificationsDropped); got != 1 {
		t.Errorf("dropped = %v, want 1 alert", got)
	}
}

// The bound is counted in alerts and a batch is one rule's evaluation of one
// tick, so half of one is a page nobody can reason about. One oversized batch
// is the one case the bound is exceeded (spec 6.5).
func TestABatchLargerThanTheQueueIsKeptWhole(t *testing.T) {
	sender := &recordingSender{}
	queue := testQueue(sender, 2)

	batch := append(firing("A"), append(firing("B"), firing("C")...)...)
	queue.Enqueue("f.yaml:g1", "Wide", time.Now(), time.Minute, batch)
	queue.Drain(time.Second)

	if len(sender.calls) != 1 || len(sender.calls[0]) != 3 {
		t.Fatalf("delivered %v, want one batch of three", sender.calls)
	}
	if got := testutil.ToFloat64(queue.metrics.NotificationsDropped); got != 0 {
		t.Errorf("dropped = %v, want 0: nothing was dropped, the bound was exceeded", got)
	}
}

// Shutdown waits for evaluations in flight so a send is not cut off part way
// through. With the send behind a queue, the queue is where that promise now
// lives: without the drain a restart loses every page that was waiting.
func TestShutdownDeliversWhatIsStillQueued(t *testing.T) {
	sender := &slowSender{per: 20 * time.Millisecond}
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	cadence := NewCadence(sender, "am", testResend, m, NewRealClock())
	queue := NewSendQueue(cadence, DefaultNotificationQueueCapacity, m, NewRealClock(), slog.New(slog.DiscardHandler))
	queue.Start()

	for i := 0; i < 5; i++ {
		name := "Queued" + string(rune('A'+i))
		queue.Enqueue("f.yaml:g1", name, time.Now(), time.Minute, firing(name))
	}

	sched := New(&ruleset.Set{}, map[string]Querier{}, queue, m, NewRealClock(), 0, nil, testResend, 0)
	sched.Start(context.Background())
	sched.Shutdown(5 * time.Second)

	if got := sender.count(); got != 5 {
		t.Errorf("delivered %d of 5 queued sends, want all of them", got)
	}
	if got := testutil.ToFloat64(m.NotificationQueueLength); got != 0 {
		t.Errorf("queue length = %v after shutdown, want 0", got)
	}
}

// A send that fails is the worker's to report, because the evaluation it came
// from returned long before. Nothing else names the rule that could not be
// delivered (spec 8.4).
func TestTheWorkerLogsWhichRuleCouldNotBeDelivered(t *testing.T) {
	log, buf := logBuffer()
	sender := &recordingSender{err: errors.New("alertmanager unreachable")}
	queue, _ := loggingQueue(sender, log)

	queue.Enqueue("f.yaml:g1", "Undeliverable", time.Now(), time.Minute, firing("Undeliverable"))
	queue.Drain(time.Second)

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %v", len(lines), lines)
	}
	wantFields(t, lines[0], map[string]string{
		"level":      "ERROR",
		"rule_group": "f.yaml:g1",
		"rule":       "Undeliverable",
	})
}
