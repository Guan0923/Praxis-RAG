package mq

import (
	"context"
	"testing"
	"time"
)

func TestWorkerDrainCommitsOnlyFinishedDeliveries(t *testing.T) {
	for _, finish := range []bool{true, false} {
		name := "interrupted"
		if finish {
			name = "completed"
		}
		t.Run(name, func(t *testing.T) {
			transport := &fakePullTransport{batches: [][]ReceivedMessage{{{Topic: "docs", Offset: 1}, {Topic: "docs", Offset: 2}}}}
			config := testConsumerConfig()
			config.Concurrency = 1
			started := make(chan int64, 2)
			release := make(chan struct{})
			c := NewPullConsumer(config, transport, func(ctx context.Context, m ReceivedMessage) bool {
				started <- m.Offset
				select {
				case <-release:
					return true
				case <-ctx.Done():
					return false
				}
			})
			c.drainTimeout = 100 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := c.Start(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("first handler did not run")
			}
			cancel()
			if finish {
				close(release)
			}
			if err := c.Stop(); err != nil {
				t.Fatal(err)
			}
			acked, _, _ := transport.snapshot()
			if finish && (len(acked) != 1 || acked[0].Offset != 1) {
				t.Fatalf("completed prefix: %+v", acked)
			}
			if !finish && len(acked) != 0 {
				t.Fatalf("interrupted record committed: %+v", acked)
			}
			select {
			case offset := <-started:
				t.Fatalf("started queued offset %d after shutdown", offset)
			default:
			}
		})
	}
}
