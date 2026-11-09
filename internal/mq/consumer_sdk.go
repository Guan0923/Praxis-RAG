package mq

import (
	"context"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
)

// sdkPullTransport wraps a kafka-go consumer-group Reader. Offsets are committed
// manually (CommitInterval=0): Ack commits the record, so a crash or nack leaves
// the offset uncommitted and the record is redelivered after rebalance/restart.
type sdkPullTransport struct {
	reader *kafka.Reader
}

func newSDKPullTransport(settings Settings, config ConsumerConfig) (*sdkPullTransport, error) {
	dialer, err := newKafkaDialer(settings)
	if err != nil {
		return nil, err
	}
	maxWait := config.AwaitDuration
	if maxWait <= 0 {
		maxWait = DefaultAwaitDuration
	}
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        settings.BootstrapServers,
		GroupID:        config.Group,
		Topic:          config.Topic,
		Dialer:         dialer,
		CommitInterval: 0,
		MinBytes:       1,
		MaxBytes:       10 << 20,
		MaxWait:        maxWait,
	})
	return &sdkPullTransport{reader: reader}, nil
}

// Start is a no-op: kafka-go starts connections lazily on the first fetch.
func (t *sdkPullTransport) Start() error { return nil }

// Stop closes the reader and leaves the consumer group cleanly.
func (t *sdkPullTransport) Stop() error { return t.reader.Close() }

// Receive fetches exactly one record (Kafka has no visibility timeout; the
// consumer implements retries in-process without committing).
func (t *sdkPullTransport) Receive(ctx context.Context, _ int32, _ time.Duration) ([]ReceivedMessage, error) {
	message, err := t.reader.FetchMessage(ctx)
	if err != nil {
		return nil, err
	}
	return []ReceivedMessage{{
		Topic:           message.Topic,
		MessageID:       fmt.Sprintf("%s/%d/%d", message.Topic, message.Partition, message.Offset),
		Key:             string(message.Key),
		Body:            message.Value,
		DeliveryAttempt: 0,
		handle:          message,
	}}, nil
}

// Ack commits the record offset (ack => commit).
func (t *sdkPullTransport) Ack(ctx context.Context, message ReceivedMessage) error {
	record, ok := message.handle.(kafka.Message)
	if !ok {
		return newSafeError(ErrConsumerFailed, "consumer commit", nil)
	}
	return t.reader.CommitMessages(ctx, record)
}

// ChangeInvisibleDuration is retained for the PullTransport seam. Kafka has no
// visibility timeout; nacks are retried in-process without committing.
func (t *sdkPullTransport) ChangeInvisibleDuration(context.Context, ReceivedMessage, time.Duration) error {
	return nil
}
