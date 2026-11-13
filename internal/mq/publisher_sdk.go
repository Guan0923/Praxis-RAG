package mq

import (
	"context"

	"github.com/segmentio/kafka-go"
)

// sdkRawPublisher publishes raw payloads (dead letters) to one topic.
type sdkRawPublisher struct {
	writer *kafka.Writer
	topic  string
}

func newSDKRawPublisher(settings Settings, topic string) (*sdkRawPublisher, error) {
	writer, err := newKafkaWriter(settings, topic, settings.PublishTimeout)
	if err != nil {
		return nil, err
	}
	return &sdkRawPublisher{writer: writer, topic: topic}, nil
}

// Publish writes one raw payload to the dead-letter topic.
func (p *sdkRawPublisher) Publish(ctx context.Context, topic, key string, body []byte) error {
	if p.topic != "" && topic != p.topic {
		return newSafeError(ErrPublishFailed, "dead-letter publish", nil)
	}
	return p.writer.WriteMessages(ctx, kafka.Message{Key: []byte(key), Value: body})
}

// Close releases the writer.
func (p *sdkRawPublisher) Close() error { return p.writer.Close() }
