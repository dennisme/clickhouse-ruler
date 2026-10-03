package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

// countingSender records how many batches it was handed, safely, because a
// fan-out hands them over from one goroutine per endpoint.
type countingSender struct {
	mu     sync.Mutex
	alerts int
	err    error
}

func (s *countingSender) Send(_ context.Context, alerts []alert.Alert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alerts += len(alerts)
	return s.err
}

func (s *countingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alerts
}

// The whole point of a list: a member partitioned from its peers still has the
// alert, because it was posted to rather than chosen by a balancer (spec 6.5).
func TestFanoutReachesEveryEndpoint(t *testing.T) {
	a, b, c := &countingSender{}, &countingSender{}, &countingSender{}
	f := NewFanout(a, b, c)

	if err := f.Send(context.Background(), []alert.Alert{firing(1), firing(2)}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for i, s := range []*countingSender{a, b, c} {
		if got := s.count(); got != 2 {
			t.Errorf("endpoint %d got %d alerts, want 2", i, got)
		}
	}
}

// Posted in sequence, three endpoints against a cluster that is down is three
// retry ladders of forty two seconds on the one delivery worker. This blocks
// every endpoint until all of them have been entered, so a sequential fan-out
// cannot finish it (spec 6.5).
func TestFanoutPostsConcurrently(t *testing.T) {
	const endpoints = 3

	entered := make(chan struct{}, endpoints)
	release := make(chan struct{})

	senders := make([]Sender, 0, endpoints)
	for range endpoints {
		senders = append(senders, senderFunc(func(context.Context, []alert.Alert) error {
			entered <- struct{}{}
			<-release
			return nil
		}))
	}

	done := make(chan error, 1)
	go func() {
		done <- NewFanout(senders...).Send(context.Background(), []alert.Alert{firing(1)})
	}()

	for i := range endpoints {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d endpoints were posted to at once", i, endpoints)
		}
	}
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// One endpoint accepting is a delivered send, because gossip carries the alert
// to the rest, and Cadence records a delivered send as sent (spec 6.5).
func TestFanoutIsDeliveredWhenOneEndpointAccepts(t *testing.T) {
	down := &countingSender{err: errors.New("connection refused")}
	up := &countingSender{}

	if err := NewFanout(down, up).Send(context.Background(), []alert.Alert{firing(1)}); err != nil {
		t.Errorf("Send = %v, want it delivered: one endpoint accepted it", err)
	}
}

// Every endpoint failing is one error, so the delivery worker writes one line
// for one undelivered batch rather than one per endpoint, and every endpoint's
// reason is in it.
func TestFanoutFailsWhenEveryEndpointFails(t *testing.T) {
	first := &countingSender{err: errors.New("connection refused")}
	second := &countingSender{err: errors.New("alertmanager returned 503 Service Unavailable")}

	err := NewFanout(first, second).Send(context.Background(), []alert.Alert{firing(1)})
	if err == nil {
		t.Fatal("Send = nil, want a failure: no endpoint accepted the alerts")
	}
	for _, want := range []string{"connection refused", "503"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
}

// A partial failure is recorded as sent, which is what keeps a cluster with one
// member down from re-posting every firing alert on every evaluation (spec 6.5).
func TestCadenceRecordsAPartiallyDeliveredSend(t *testing.T) {
	down := &countingSender{err: errors.New("connection refused")}
	up := &countingSender{}
	c := NewCadence(NewFanout(down, up), time.Minute, DefaultResendTolerance)

	now := time.Now()
	for range 2 {
		if err := c.Send(context.Background(), now, testEvalInterval, []alert.Alert{firing(1)}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	if got := up.count(); got != 1 {
		t.Errorf("the endpoint that accepted got %d alerts, want 1: the first send was recorded", got)
	}
}
