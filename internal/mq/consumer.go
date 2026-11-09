package mq

import (
	"context"
	"sync"
	"time"
)

// ReceivedMessage is one delivered payload plus the opaque transport handle used
// for ack/commit. Workers only need Body/Topic/Key/DeliveryAttempt.
type ReceivedMessage struct {
	Topic           string
	MessageID       string
	Key             string
	Body            []byte
	DeliveryAttempt int
	handle          any
}

// PullTransport is the SDK seam for consumers (fakes implement it in tests).
//
// Ack commits the record; ChangeInvisibleDuration is retained for seam
// compatibility but Kafka consumers retry in-process without committing.
type PullTransport interface {
	Start() error
	Stop() error
	Receive(ctx context.Context, maxMessages int32, invisibleDuration time.Duration) ([]ReceivedMessage, error)
	Ack(ctx context.Context, message ReceivedMessage) error
	ChangeInvisibleDuration(ctx context.Context, message ReceivedMessage, invisibleDuration time.Duration) error
}

// Handler consumes one message. Returning true acknowledges it (Kafka commit);
// false triggers an in-process retry of the same record with exponential
// backoff, and after MaxAttempts deliveries the consumer dead-letters it.
type Handler func(ctx context.Context, message ReceivedMessage) bool

// Consumer is the worker-facing lifecycle seam.
type Consumer interface {
	Start(ctx context.Context) error
	Stop() error
}

// RawPublisher publishes raw payloads to a Kafka topic (dead letters).
type RawPublisher interface {
	Publish(ctx context.Context, topic, key string, body []byte) error
	Close() error
}

// ConsumerConfig is the Kafka consumer configuration. BatchSize, VisibilityTimeout
// and AwaitDuration are kept for the shared transport/worker contract; a Kafka
// reader fetches one record at a time and retries in-process.
type ConsumerConfig struct {
	Topic             string
	Group             string
	BatchSize         int32
	VisibilityTimeout time.Duration
	AwaitDuration     time.Duration
	PollInterval      time.Duration
	// MaxAttempts > 0 bounds in-process redeliveries; the record is published to
	// DLQTopic (when configured), OnExhausted runs, and only then is it committed.
	MaxAttempts int
	// DLQTopic receives the raw payload on exhaustion; empty disables publishing.
	DLQTopic string
	// RetryBaseDelay/RetryMaxDelay shape the exponential in-process backoff.
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
}

// DefaultConsumerConfig returns the Python SDK-compatible defaults.
func DefaultConsumerConfig(topic, group string) ConsumerConfig {
	return ConsumerConfig{
		Topic:             topic,
		Group:             group,
		BatchSize:         DefaultBatchSize,
		VisibilityTimeout: DefaultVisibilityTimeout,
		AwaitDuration:     DefaultAwaitDuration,
		PollInterval:      DefaultPollInterval,
		RetryBaseDelay:    DefaultRetryBaseDelay,
		RetryMaxDelay:     DefaultRetryMaxDelay,
	}
}

// Validate checks the fields a hand-built ConsumerConfig can get wrong.
func (c ConsumerConfig) Validate() error {
	if c.Topic == "" || c.Group == "" {
		return newConfigError("Consumer topic and group must not be empty")
	}
	if c.BatchSize <= 0 || c.BatchSize > 32 {
		return newConfigError("Consumer batch size must be between 1 and 32")
	}
	if c.VisibilityTimeout <= 0 {
		return newConfigError("Consumer visibility timeout must be positive")
	}
	if c.MaxAttempts < 0 {
		return newConfigError("Consumer max attempts must not be negative")
	}
	if c.RetryBaseDelay < 0 || c.RetryMaxDelay < 0 {
		return newConfigError("Consumer retry delays must not be negative")
	}
	return nil
}

// PullConsumer is the framework-light consumer loop used by the worker binaries.
// It never interprets payloads: the handler gets raw bytes.
//
// Semantics (Kafka): handler true => commit; handler false => retry the same
// record in-process with exponential backoff without committing; after
// MaxAttempts the raw body is published to DLQTopic (when set), OnExhausted runs,
// and the record is committed so the group advances.
type PullConsumer struct {
	Config    ConsumerConfig
	transport PullTransport
	handler   Handler
	// OnExhausted runs after the DLQ publish and before the commit.
	OnExhausted func(ctx context.Context, message ReceivedMessage) error
	// DLQ publishes exhausted raw payloads; nil (or empty DLQTopic) skips it.
	DLQ RawPublisher

	mutex   sync.Mutex
	started bool
	stopped bool
	stopCh  chan struct{}
	doneCh  chan struct{}
	cancel  context.CancelFunc
	lastErr error

	dlqOnce sync.Once
}

var _ Consumer = (*PullConsumer)(nil)

// NewPullConsumer builds a consumer over an injected transport.
func NewPullConsumer(config ConsumerConfig, transport PullTransport, handler Handler) *PullConsumer {
	return &PullConsumer{Config: config, transport: transport, handler: handler}
}

// NewKafkaPullConsumer builds the kafka-go-backed consumer. A missing bootstrap
// list is reported as ErrNotConfigured.
func NewKafkaPullConsumer(settings Settings, config ConsumerConfig, handler Handler) (*PullConsumer, error) {
	if !settings.Configured() {
		return nil, newSafeError(ErrNotConfigured, "consumer", nil)
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if config.DLQTopic == "" {
		config.DLQTopic = settings.DLQTopicFor(config.Topic)
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	transport, err := newSDKPullTransport(settings, config)
	if err != nil {
		return nil, newSafeError(ErrConsumerFailed, "consumer start", err)
	}
	consumer := NewPullConsumer(config, transport, handler)
	if config.DLQTopic != "" {
		raw, err := newSDKRawPublisher(settings, config.DLQTopic)
		if err != nil {
			_ = transport.Stop()
			return nil, newSafeError(ErrConsumerFailed, "consumer start", err)
		}
		consumer.DLQ = raw
	}
	return consumer, nil
}

// Start validates, starts the transport and launches the pull loop.
func (c *PullConsumer) Start(ctx context.Context) error {
	if c.handler == nil {
		return newConfigError("Consumer handler must not be nil")
	}
	if err := c.Config.Validate(); err != nil {
		return err
	}
	c.mutex.Lock()
	if c.stopped {
		c.mutex.Unlock()
		return newSafeError(ErrConsumerFailed, "consumer start", nil)
	}
	if c.started {
		c.mutex.Unlock()
		return nil
	}
	c.started = true
	c.stopCh = make(chan struct{})
	c.doneCh = make(chan struct{})
	loopContext, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.mutex.Unlock()
	if err := c.transport.Start(); err != nil {
		cancel()
		close(c.doneCh)
		c.mutex.Lock()
		c.started = false
		c.mutex.Unlock()
		return newSafeError(ErrConsumerFailed, "consumer start", err)
	}
	go c.loop(loopContext)
	return nil
}

// Stop stops the loop and closes the transport/DLQ publisher exactly once.
func (c *PullConsumer) Stop() error {
	c.mutex.Lock()
	if c.stopped {
		c.mutex.Unlock()
		return nil
	}
	c.stopped = true
	started := c.started
	stopCh, doneCh, cancel := c.stopCh, c.doneCh, c.cancel
	c.mutex.Unlock()
	if cancel != nil {
		cancel()
	}
	if started && stopCh != nil {
		close(stopCh)
	}
	if doneCh != nil {
		<-doneCh
	}
	var firstErr error
	if c.transport != nil {
		if err := c.transport.Stop(); err != nil {
			firstErr = newSafeError(ErrConsumerFailed, "consumer stop", err)
		}
	}
	c.dlqOnce.Do(func() {
		if c.DLQ != nil {
			_ = c.DLQ.Close()
		}
	})
	return firstErr
}

// LastError returns the sanitized last error (nil after success).
func (c *PullConsumer) LastError() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.lastErr
}

func (c *PullConsumer) setLastError(err error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.lastErr = err
}

func (c *PullConsumer) loop(ctx context.Context) {
	defer close(c.doneCh)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		messages, err := c.transport.Receive(ctx, c.Config.BatchSize, c.Config.VisibilityTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.setLastError(newSafeError(ErrReceiveFailed, "consumer receive", err))
			if !c.sleep(ctx) {
				return
			}
			continue
		}
		for _, message := range messages {
			if !c.process(ctx, message) {
				return
			}
		}
	}
}

// process runs one delivery through handler until it ackes, exhausts, or the
// context is cancelled. It returns false when the loop must stop.
func (c *PullConsumer) process(ctx context.Context, message ReceivedMessage) bool {
	for attempt := 1; ; attempt++ {
		message.DeliveryAttempt = attempt
		if c.handler(ctx, message) {
			if err := c.transport.Ack(ctx, message); err != nil {
				c.setLastError(newSafeError(ErrConsumerFailed, "consumer commit", err))
			}
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		if c.Config.MaxAttempts > 0 && attempt >= c.Config.MaxAttempts {
			c.deadLetter(ctx, message)
			if err := c.transport.Ack(ctx, message); err != nil {
				c.setLastError(newSafeError(ErrConsumerFailed, "consumer commit", err))
			}
			return true
		}
		if !c.retryDelay(ctx, attempt) {
			return false
		}
	}
}

// deadLetter publishes the raw payload (when a DLQ topic is configured) and runs
// the OnExhausted bookkeeping before the offset is committed.
func (c *PullConsumer) deadLetter(ctx context.Context, message ReceivedMessage) {
	if c.Config.DLQTopic != "" && c.DLQ != nil {
		publishContext, cancel := context.WithTimeout(ctx, DefaultPublishTimeout)
		err := c.DLQ.Publish(publishContext, c.Config.DLQTopic, message.Key, message.Body)
		cancel()
		if err != nil {
			c.setLastError(newSafeError(ErrPublishFailed, "consumer dead-letter", err))
		}
	}
	if c.OnExhausted != nil {
		if err := c.OnExhausted(ctx, message); err != nil {
			c.setLastError(newSafeError(ErrConsumerFailed, "consumer exhausted", err))
		}
	}
}

// retryDelay waits the exponential backoff before redelivering the same record
// in-process. It returns false when the context is cancelled.
func (c *PullConsumer) retryDelay(ctx context.Context, attempt int) bool {
	base := c.Config.RetryBaseDelay
	if base <= 0 {
		base = DefaultRetryBaseDelay
	}
	maxDelay := c.Config.RetryMaxDelay
	if maxDelay <= 0 {
		maxDelay = DefaultRetryMaxDelay
	}
	delay := base
	for step := 1; step < attempt; step++ {
		if delay >= maxDelay {
			delay = maxDelay
			break
		}
		delay *= 2
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (c *PullConsumer) sleep(ctx context.Context) bool {
	interval := c.Config.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
