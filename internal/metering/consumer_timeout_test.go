package metering

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// stallLedger models a database that accepts a connection and then never
// answers: every write waits until its context ends.
type stallLedger struct{}

func (stallLedger) Add(ctx context.Context, _ Event) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

func (stallLedger) Count(context.Context) (int, error) { return 0, nil }

func (stallLedger) Balance(context.Context, string) (int64, error) { return 0, nil }

func (stallLedger) Snapshot(context.Context) (map[string]Event, error) { return nil, nil }

// A stalled ledger must not hold the consumer loop: each write is bounded by
// LedgerTimeout, so the message is left pending for the reclaim loop and the
// consumer moves on.
func TestConsumerBoundsEachLedgerWrite(t *testing.T) {
	consumer := NewConsumerWithOptions(nil, stallLedger{}, ConsumerOptions{LedgerTimeout: 50 * time.Millisecond})
	payload, err := json.Marshal(Event{EventID: "stalled-write", TenantID: "tenant-a", CostCents: 1, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	message := redis.XMessage{ID: "1-0", Values: map[string]interface{}{"data": string(payload)}}

	done := make(chan struct{})
	go func() {
		consumer.handle(context.Background(), message)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled ledger write held the consumer past its timeout")
	}
}
