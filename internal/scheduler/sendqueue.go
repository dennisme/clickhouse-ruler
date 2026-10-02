package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
)

// DefaultNotificationQueueCapacity is how many alerts may wait for delivery
// unless an operator says otherwise, counted in alerts rather than in batches
// because memory is what a bound is for: one batch holds one rule's rendered
// alerts, and a rule that returns ten thousand rows produces ten thousand of
// them. Prometheus' notifier bounds itself at the same number in the same unit
// (spec 6.5).
const DefaultNotificationQueueCapacity = 10000

// sendItem is one evaluation's alerts waiting for delivery.
//
// now and interval travel with the alerts because they are what notify.Cadence
// answers its two questions from: what is due, and how long Alertmanager should
// hold a firing alert. A worker that re-derived either would be answering a
// different question from the one the evaluation asked, so `endsAt` is stamped
// from the evaluation's clock reading even where the item has aged in the queue
// (spec 6.5).
//
// group and rule are what a failed send is reported against. The evaluation
// that produced the alerts returned long before the send was attempted, so
// nothing else is left holding the name of the rule that could not be
// delivered (spec 8.4).
type sendItem struct {
	group    string
	rule     string
	now      time.Time
	interval time.Duration
	alerts   []alert.Alert
	queuedAt time.Time
}

// SendQueue takes an evaluation's alerts off the evaluation goroutine and
// delivers them from one worker of its own.
//
// Four attempts at a ten second timeout with doubling backoff is roughly forty
// two seconds, which is longer than a thirty second group's whole interval, so
// a send on the evaluation's goroutine made an Alertmanager outage read as
// missed iterations: the signal spec 8.2 calls the most important one here,
// raised for something that is not evaluation (spec 6.5).
//
// One queue per ruler, not per group: scheduling is per group and delivery is
// one Alertmanager, so a depth per group is a number the ruler does not have.
//
// It sits in front of notify.Cadence rather than between the Cadence and the
// client. The Cadence deliberately does not record a failed send, so an alert
// whose notification failed is due again on the very next evaluation instead of
// waiting out a resend interval; a queue behind that record would write
// "delivered" before anything had been (spec 6.5).
type SendQueue struct {
	cadence  *notify.Cadence
	capacity int
	metrics  *Metrics
	clock    Clock
	log      *slog.Logger

	// ctx is what every send runs under, and it is deliberately not any
	// evaluation's context. Shutdown cancels the context the groups run under
	// and then waits, so a send on that context was cut off at the cancel
	// rather than finished by the wait.
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	items  []sendItem
	queued int
	closed bool

	work     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	started  bool
	stopOnce sync.Once
}

// NewSendQueue builds the queue a Scheduler enqueues through. A nil log
// discards every line.
func NewSendQueue(cadence *notify.Cadence, capacity int, metrics *Metrics, clock Clock, log *slog.Logger) *SendQueue {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if capacity <= 0 {
		capacity = DefaultNotificationQueueCapacity
	}

	ctx, cancel := context.WithCancel(context.Background())
	q := &SendQueue{
		cadence:  cadence,
		capacity: capacity,
		metrics:  metrics,
		clock:    clock,
		log:      log,
		ctx:      ctx,
		cancel:   cancel,
		work:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}

	// A depth says nothing without the number it is a depth out of, and an
	// operator who hardcodes the flag reads every expression against a value
	// somebody else can change (spec 8.8).
	metrics.NotificationQueueCapacity.Set(float64(capacity))
	return q
}

// Enqueue hands one evaluation's alerts over for delivery and returns. It
// never blocks: a full queue drops the oldest alerts in it, because blocking
// is what the queue exists to remove and dropping the newest would leave only
// the stalest batches to deliver, whose `endsAt` Alertmanager can expire on
// arrival (spec 6.5).
//
// A drop is not a lost page. Nothing was recorded as sent, so the rule's next
// evaluation enqueues the same instances, and a drop therefore spends the
// resend tolerance that a failed send spends.
func (q *SendQueue) Enqueue(group, ruleName string, now time.Time, interval time.Duration, alerts []alert.Alert) {
	if len(alerts) == 0 {
		return
	}

	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		q.metrics.NotificationsDropped.Add(float64(len(alerts)))
		q.log.Warn("alerts dropped: the send queue is closed",
			"rule_group", group, "rule", ruleName, "alerts", len(alerts))
		return
	}

	// Whole batches, where Prometheus truncates a flat queue of alerts: a
	// batch here is one rule's evaluation of one tick, and half of one is a
	// page nobody can reason about. So a single batch larger than the entire
	// capacity is enqueued whole, which is the one case the bound is exceeded.
	dropped := 0
	for len(q.items) > 0 && q.queued+len(alerts) > q.capacity {
		oldest := q.items[0]
		q.items = q.items[1:]
		q.queued -= len(oldest.alerts)
		dropped += len(oldest.alerts)
	}

	q.items = append(q.items, sendItem{
		group:    group,
		rule:     ruleName,
		now:      now,
		interval: interval,
		alerts:   alerts,
		queuedAt: q.clock.Now(),
	})
	q.queued += len(alerts)
	// Set under the lock, as the worker's own update is. Written outside it,
	// this and a delivery that finished in between can land in the wrong
	// order, which leaves the gauge holding a depth the queue does not have.
	q.metrics.NotificationQueueLength.Set(float64(q.queued))
	q.mu.Unlock()

	if dropped > 0 {
		q.metrics.NotificationsDropped.Add(float64(dropped))
		q.log.Warn("the send queue is full, dropping the oldest alerts",
			"dropped", dropped, "capacity", q.capacity)
	}
	q.signal()
}

// signal wakes the worker. A signal already waiting to be consumed is the same
// instruction, so there is nothing to add to it.
func (q *SendQueue) signal() {
	select {
	case q.work <- struct{}{}:
	default:
	}
}

// Start launches the one goroutine that delivers. Calling it twice launches
// nothing the second time, because the queue outlives every reload.
func (q *SendQueue) Start() {
	q.mu.Lock()
	if q.started {
		q.mu.Unlock()
		return
	}
	q.started = true
	q.mu.Unlock()

	go q.loop()
}

func (q *SendQueue) loop() {
	defer close(q.done)

	for {
		select {
		case <-q.stop:
			// The backlog is delivered before the worker goes away, so a
			// restart does not lose the pages that were waiting.
			q.drain()
			return
		case <-q.work:
			q.drain()
		}
	}
}

// Drain stops the queue accepting and gives what is already in it up to
// timeout to be delivered. Whatever is left when the timeout expires is
// counted as dropped and logged with its depth, so a restart that lost pages
// says so.
func (q *SendQueue) Drain(timeout time.Duration) {
	q.mu.Lock()
	q.closed = true
	started := q.started
	q.mu.Unlock()

	q.stopOnce.Do(func() { close(q.stop) })

	if !started {
		// Nothing is delivering, so the caller does: a queue with no worker
		// still has to be drainable, which is also how a test runs the real
		// send path without a goroutine.
		q.drain()
		q.cancel()
		return
	}

	select {
	case <-q.done:
	case <-time.After(timeout):
		// Cuts the send in flight and abandons the rest, which is what the
		// timeout is for: the only alternative is a shutdown that does not
		// finish.
		q.cancel()
		<-q.done
	}
	q.cancel()
}

// drain delivers every item in the queue on the calling goroutine, and stops
// early once the queue's context is cancelled.
func (q *SendQueue) drain() {
	for {
		if q.ctx.Err() != nil {
			q.abandon()
			return
		}
		item, ok := q.next()
		if !ok {
			return
		}
		q.send(item)
	}
}

func (q *SendQueue) next() (sendItem, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) == 0 {
		return sendItem{}, false
	}
	item := q.items[0]
	q.items = q.items[1:]
	q.queued -= len(item.alerts)
	q.metrics.NotificationQueueLength.Set(float64(q.queued))
	return item, true
}

// abandon drops what is left once delivery has been cancelled. Attempting the
// rest against a cancelled context would report them as failed sends, which
// says Alertmanager refused alerts it was never offered.
func (q *SendQueue) abandon() {
	q.mu.Lock()
	left := q.queued
	q.items = nil
	q.queued = 0
	q.mu.Unlock()

	if left == 0 {
		return
	}
	q.metrics.NotificationsDropped.Add(float64(left))
	q.metrics.NotificationQueueLength.Set(0)
	q.log.Warn("the send queue was not drained, dropping what was left",
		"dropped", left)
}

// send delivers one item and reports what the evaluation it came from can no
// longer report.
//
// The wait is observed here rather than folded into
// clickhouse_ruler_notification_latency_seconds, which holds sends Alertmanager
// accepted and is summed as exactly that by the lag budget in spec 8.8.
func (q *SendQueue) send(item sendItem) {
	q.metrics.NotificationQueueWait.Observe(q.clock.Now().Sub(item.queuedAt).Seconds())

	if err := q.cadence.Send(q.ctx, item.now, item.interval, item.alerts); err != nil {
		q.log.Error("sending alerts to alertmanager failed",
			"rule_group", item.group, "rule", item.rule, "error", err.Error())
	}
}
