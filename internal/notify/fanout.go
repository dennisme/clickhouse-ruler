package notify

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

// Fanout posts one batch to every member of an Alertmanager cluster.
//
// Members gossip and deduplicate identical alerts, so a sender posting to every
// member gets resilience out of deduplication the cluster already has. A
// balancer in front of them collapses that: it picks one member, and a member
// partitioned from its peers accepts a page no other member ever learns about,
// which nothing on either side reports because the POST succeeded (spec 6.5).
//
// It sits below Cadence rather than beside it. One Cadence holds what was last
// sent per fingerprint, so a Cadence per endpoint is the first one recording
// the send and the next finding nothing due.
type Fanout struct {
	senders []Sender
}

// NewFanout builds a Fanout over the senders given, one per endpoint.
func NewFanout(senders ...Sender) *Fanout {
	return &Fanout{senders: senders}
}

// Send posts to every endpoint at once and reports whether any of them took the
// batch.
//
// Concurrently, because Client.Send holds the retry ladder per client: posted in
// sequence, three endpoints against a cluster that is down is three ladders of
// forty two seconds spent on the one delivery worker, where posted at once it is
// the single ladder one endpoint already costs (spec 6.5).
//
// One endpoint accepting is a delivered send, because gossip carries the alert
// to the rest and Cadence records a delivered send as sent. Requiring all of
// them would mean one member down stops every firing alert from ever being
// recorded, so every alert is re-posted on every evaluation at the traffic the
// resend interval exists to bound.
//
// Every endpoint failing is one error carrying every endpoint's reason, so the
// one undelivered batch is one log line.
func (f *Fanout) Send(ctx context.Context, alerts []alert.Alert) error {
	if len(f.senders) == 0 {
		return errors.New("no alertmanager to send to")
	}

	errs := make([]error, len(f.senders))
	var wg sync.WaitGroup
	for i, s := range f.senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Send(ctx, alerts)
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("no alertmanager accepted the alerts: %w", errors.Join(errs...))
}
