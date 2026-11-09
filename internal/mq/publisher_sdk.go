package mq

import (
	"context"
	"sync"

	"github.com/segmentio/kafka-go"
)

// sdkProducerTransport wraps kafka-go writers, one lazily created writer per
// topic. The writer is synchronous with RequiredAcks=RequireAll so a successful
// Send means the brokers acknowledged the record.
type sdkProducerTransport struct {
	settings Settings

	mutex   sync.Mutex
	writers map[string]*kafka.Writer
	closed  bool
}

func newSDKProducerTransport(settings Settings) (*sdkProducerTransport, error) {
	// Resolve TLS/SASL once up front so an invalid configuration fails at
	// construction instead of on the first publish.
	if _, err := newKafkaTransport(settings); err != nil {
		return nil, err
	}
	return &sdkProducerTransport{settings: settings, writers: map[string]*kafka.Writer{}}, nil
}

func (t *sdkProducerTransport) Start() error { return nil }

func (t *sdkProducerTransport) Send(ctx context.Context, topic, key string, body []byte) error {
	writer, err := t.writer(topic)
	if err != nil {
		return err
	}
	message := kafka.Message{Key: []byte(key), Value: body}
	return writer.WriteMessages(ctx, message)
}

func (t *sdkProducerTransport) writer(topic string) (*kafka.Writer, error) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if t.closed {
		return nil, newSafeError(ErrPublisherClosed, "publish", nil)
	}
	if writer := t.writers[topic]; writer != nil {
		return writer, nil
	}
	writer, err := newKafkaWriter(t.settings, topic, t.settings.PublishTimeout)
	if err != nil {
		return nil, err
	}
	t.writers[topic] = writer
	return writer, nil
}

func (t *sdkProducerTransport) Stop() error {
	t.mutex.Lock()
	if t.closed {
		t.mutex.Unlock()
		return nil
	}
	t.closed = true
	writers := make([]*kafka.Writer, 0, len(t.writers))
	for _, writer := range t.writers {
		writers = append(writers, writer)
	}
	t.writers = map[string]*kafka.Writer{}
	t.mutex.Unlock()
	var firstErr error
	for _, writer := range writers {
		if err := writer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

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

func (p *sdkRawPublisher) Publish(ctx context.Context, topic, key string, body []byte) error {
	if p.topic != "" && topic != p.topic {
		return newSafeError(ErrPublishFailed, "dead-letter publish", nil)
	}
	return p.writer.WriteMessages(ctx, kafka.Message{Key: []byte(key), Value: body})
}

func (p *sdkRawPublisher) Close() error { return p.writer.Close() }
