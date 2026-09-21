package notify

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

// senderFunc adapts a plain function to Sender.
type senderFunc func(context.Context, []alert.Alert, map[string]string) error

func (f senderFunc) Send(ctx context.Context, a []alert.Alert, an map[string]string) error {
	return f(ctx, a, an)
}

// One Cadence is shared by every rule in every group, and the scheduler gives
// each group its own goroutine, so two groups whose ticks overlap call Send
// at the same time. That makes concurrent access the normal case rather than
// an exotic one, and an unsynchronised map here is a fatal "concurrent map
// writes" crash of the whole ruler rather than a recoverable error.
func TestCadenceSendIsSafeFromConcurrentGroups(t *testing.T) {
	// A sender that records nothing, so the race detector reports on
	// Cadence's own state rather than on a test helper's slice.
	c := NewCadence(senderFunc(func(context.Context, []alert.Alert, map[string]string) error {
		return nil
	}), time.Minute)

	now := time.Now()
	const groups = 8
	const ticks = 50

	var wg sync.WaitGroup
	for g := 0; g < groups; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < ticks; i++ {
				// Distinct fingerprints per goroutine, the way two groups
				// evaluating different rules produce different instances.
				a := firing(uint64(g*ticks + i))
				_ = c.Send(context.Background(), now.Add(time.Duration(i)*time.Second),
					testEvalInterval, []alert.Alert{a}, nil)
			}
		}(g)
	}
	wg.Wait()
}
