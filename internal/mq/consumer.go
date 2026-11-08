package mq

import (
	"context"
	"sync"
	"time"
)

// ReceivedMessage is one delivered payload plus the opaque transport handle used
// for ack/nack. Workers only need Body/Topic/Key/DeliveryAttempt.
type ReceivedMessage struct {
	Topic           string
	MessageID       string
	Key             string
	Body            []byte
	DeliveryAttempt int
	handle          any
}

// PullTransport is the SDK seam for consumers (fakes implement it in tests).
type PullTransport interface {
	Start() error
	Stop() error
	Receive(ctx context.Context, maxMessages int32, invisibleDuration time.Duration) ([]ReceivedMessage, error)
	Ack(ctx context.Context, message ReceivedMessage) error
	ChangeInvisibleDuration(ctx context.Context, message ReceivedMessage, invisibleDuration time.Duration) error
}

// Handler consumes one message. Returning true acknowledges it; false is a
// nack (the visibility timeout makes it redeliverable, or OnExhausted decides
// after MaxAttempts deliveries).
type Handler func(ctx context.Context, message ReceivedMessage) bool

// Consumer is the worker-facing lifecycle seam.
type Consumer interface {
	Start(ctx context.Context) error
	Stop() error
}

// ConsumerConfig is the simple-pull consumer configuration.
type ConsumerConfig struct {
	Topic             string
	Group             string
	BatchSize         int32
	VisibilityTimeout time.Duration
	AwaitDuration     time.Duration
	PollInterval      time.Duration
	// MaxAttempts > 0 sends exhausted messages to OnExhausted and acks them.
	MaxAttempts int
}

// DefaultConsumerConfig returns the Python SDK-compatible pull defaults.
func DefaultConsumerConfig(topic, group string) ConsumerConfig {
	return ConsumerConfig{
		Topic:             topic,
		Group:             group,
		BatchSize:         DefaultBatchSize,
		VisibilityTimeout: DefaultVisibilityTimeout,
		AwaitDuration:     DefaultAwaitDuration,
		PollInterval:      DefaultPollInterval,
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
	return nil
}

// PullConsumer is the framework-light simple-pull loop used by the future
// worker binaries. It never interprets payloads: the handler gets raw bytes.
type PullConsumer struct {
	Config    ConsumerConfig
	transport PullTransport
	handler   Handler
	// OnExhausted runs before a message at MaxAttempts is acked (dead letter
	// bookkeeping); an error is recorded but still acks to stop redelivery.
	OnExhausted func(ctx context.Context, message ReceivedMessage) error

	mutex   sync.Mutex
	started bool
	stopped bool
	stopCh  chan struct{}
	doneCh  chan struct{}
	cancel  context.CancelFunc
	lastErr error
}

var _ Consumer = (*PullConsumer)(nil)

// NewPullConsumer builds a consumer over an injected transport.
func NewPullConsumer(config ConsumerConfig, transport PullTransport, handler Handler) *PullConsumer {
	return &PullConsumer{Config: config, transport: transport, handler: handler}
}

// NewRocketMQPullConsumer builds the SDK-backed consumer. A missing endpoint is
// reported as ErrNotConfigured.
func NewRocketMQPullConsumer(settings Settings, config ConsumerConfig, handler Handler) (*PullConsumer, error) {
	if !settings.Configured() {
		return nil, newSafeError(ErrNotConfigured, "consumer", nil)
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	transport, err := newSDKPullTransport(settings, config)
	if err != nil {
		return nil, newSafeError(ErrConsumerFailed, "consumer start", err)
	}
	return NewPullConsumer(config, transport, handler), nil
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

// Stop stops the loop and closes the transport exactly once.
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
	if !started || c.transport == nil {
		return nil
	}
	if err := c.transport.Stop(); err != nil {
		return newSafeError(ErrConsumerFailed, "consumer stop", err)
	}
	return nil
}

// LastError returns the sanitized last receive error (nil after success).
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
			select {
			case <-ctx.Done():
				return
			default:
			}
			acked := c.handler(ctx, message)
			if acked {
				_ = c.transport.Ack(ctx, message)
				continue
			}
			if c.Config.MaxAttempts > 0 && message.DeliveryAttempt >= c.Config.MaxAttempts {
				if c.OnExhausted != nil {
					if exhaustedErr := c.OnExhausted(ctx, message); exhaustedErr != nil {
						c.setLastError(newSafeError(ErrConsumerFailed, "consumer exhausted", exhaustedErr))
					}
				}
				_ = c.transport.Ack(ctx, message)
				continue
			}
			// Nack: extend the invisibility window; the broker redelivers it.
			_ = c.transport.ChangeInvisibleDuration(ctx, message, c.Config.VisibilityTimeout)
		}
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
