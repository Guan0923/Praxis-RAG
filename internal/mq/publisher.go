package mq

import (
	"context"
	"errors"
	"sync"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// Safe error kinds. Error() strings never include broker details, payloads, or
// credentials; the wrapped cause is available through Cause() for diagnostics
// that must not be logged verbatim.
var (
	ErrNotConfigured   = errors.New("kafka bootstrap servers are not configured")
	ErrPublishFailed   = errors.New("kafka publish failed")
	ErrPublishTimeout  = errors.New("kafka publish timed out")
	ErrPayloadInvalid  = errors.New("kafka payload is invalid")
	ErrPublisherClosed = errors.New("kafka publisher is closed")
	ErrConsumerFailed  = errors.New("kafka consumer failed")
	ErrReceiveFailed   = errors.New("kafka receive failed")
)

// SafeError maps a transport failure to a sanitized category while keeping the
// underlying error reachable only through Cause (never printed by Error).
type SafeError struct {
	Kind  error
	Op    string
	cause error
}

// Error renders only the sanitized operation/category.
func (e *SafeError) Error() string {
	if e.Op == "" {
		return e.Kind.Error()
	}
	return e.Op + ": " + e.Kind.Error()
}

// Unwrap exposes only the safe sentinel so errors.Is works without leaking.
func (e *SafeError) Unwrap() error { return e.Kind }

// Cause returns the underlying transport error. It may contain broker text and
// must not be logged or returned to clients.
func (e *SafeError) Cause() error { return e.cause }

func newSafeError(kind error, op string, cause error) *SafeError {
	return &SafeError{Kind: kind, Op: op, cause: cause}
}

// ProducerTransport is the SDK seam: the official client adapter implements it,
// and fakes implement it in tests.
type ProducerTransport interface {
	Start() error
	Send(ctx context.Context, topic, key string, body []byte) error
	Stop() error
}

// Publisher implements both state notification publisher seams with one
// Kafka writer set. It lazily starts the transport on the first send.
type Publisher struct {
	Settings  Settings
	transport ProducerTransport

	mutex     sync.Mutex
	started   bool
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

var (
	_ state.DocumentNotificationPublisher = (*Publisher)(nil)
	_ state.SearchNotificationPublisher   = (*Publisher)(nil)
)

// NewPublisher builds the SDK-backed publisher. A missing endpoint returns
// ErrNotConfigured so services can run without a broker.
func NewPublisher(settings Settings) (*Publisher, error) {
	if !settings.Configured() {
		return nil, newSafeError(ErrNotConfigured, "publish", nil)
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	transport, err := newSDKProducerTransport(settings)
	if err != nil {
		return nil, newSafeError(ErrPublishFailed, "publish", err)
	}
	return NewPublisherWithTransport(settings, transport), nil
}

// NewPublisherWithTransport is the injection seam for tests and worker binaries.
func NewPublisherWithTransport(settings Settings, transport ProducerTransport) *Publisher {
	return &Publisher{Settings: settings, transport: transport}
}

func (p *Publisher) ensureStarted() error {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if p.closed {
		return newSafeError(ErrPublisherClosed, "publish", nil)
	}
	if p.started {
		return nil
	}
	if p.transport == nil {
		return newSafeError(ErrNotConfigured, "publish", nil)
	}
	if err := p.transport.Start(); err != nil {
		return newSafeError(ErrPublishFailed, "publish start", err)
	}
	p.started = true
	return nil
}

func (p *Publisher) publish(ctx context.Context, op, topic, key string, body []byte) error {
	if err := p.ensureStarted(); err != nil {
		return err
	}
	timeout := p.Settings.PublishTimeout
	if timeout <= 0 {
		timeout = DefaultPublishTimeout
	}
	sendContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := p.transport.Send(sendContext, topic, key, body); err != nil {
		if errors.Is(sendContext.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return newSafeError(ErrPublishTimeout, op, err)
		}
		return newSafeError(ErrPublishFailed, op, err)
	}
	return nil
}

// SendDocumentNotification publishes one schema-3 identifiers-only notification.
func (p *Publisher) SendDocumentNotification(ctx context.Context, notification state.DocumentNotification) error {
	body, key, err := EncodeDocumentNotification(notification)
	if err != nil {
		return err
	}
	topic := p.Settings.DocumentTopic
	if topic == "" {
		return newSafeError(ErrNotConfigured, "publish document notification", nil)
	}
	return p.publish(ctx, "publish document notification", topic, key, body)
}

// SendSearchEnvelope publishes one schema-2 full-request envelope.
func (p *Publisher) SendSearchEnvelope(ctx context.Context, envelope state.SearchEnvelope) error {
	body, key, err := EncodeSearchEnvelope(envelope)
	if err != nil {
		return err
	}
	topic := p.Settings.SearchTopic
	if topic == "" {
		return newSafeError(ErrNotConfigured, "publish search envelope", nil)
	}
	return p.publish(ctx, "publish search envelope", topic, key, body)
}

// Close stops the transport once. Later calls are idempotent.
func (p *Publisher) Close() error {
	p.closeOnce.Do(func() {
		p.mutex.Lock()
		p.closed = true
		started := p.started
		p.started = false
		p.mutex.Unlock()
		if started && p.transport != nil {
			if err := p.transport.Stop(); err != nil {
				p.closeErr = newSafeError(ErrPublishFailed, "publisher close", err)
			}
		}
	})
	return p.closeErr
}
