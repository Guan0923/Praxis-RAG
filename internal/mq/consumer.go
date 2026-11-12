package mq

import (
	"context"
	"sync"
	"time"
)

// defaultCommitTimeout bounds one commit call so a dead broker cannot hang
// shutdown while completed work waits for its offset to advance.
const defaultCommitTimeout = 5 * time.Second

// ReceivedMessage is one delivered payload plus the opaque transport handle used
// for ack/commit. Workers only need Body/Topic/Key/DeliveryAttempt.
//
// Partition and Offset locate a record inside its topic partition. A Kafka
// consumer group stores one offset per partition, so committing offset N also
// commits every earlier record of that partition; the consumer therefore commits
// only the contiguous completed prefix of each partition it consumes.
type ReceivedMessage struct {
	Topic           string
	MessageID       string
	Key             string
	Body            []byte
	DeliveryAttempt int
	Partition       int32
	Offset          int64
	handle          any
}

// PullTransport is the SDK seam for consumers (fakes implement it in tests).
//
// Receive returns up to maxMessages records in delivery order and fills
// Partition/Offset so the consumer can advance each partition's committed offset
// over completed records only. Ack commits the record; ChangeInvisibleDuration is
// retained for seam compatibility but Kafka consumers retry in-process without
// committing.
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
//
// Handlers may run concurrently when ConsumerConfig.Concurrency > 1, so they must
// be safe for concurrent calls.
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

// ConsumerConfig is the Kafka consumer configuration. BatchSize is both the
// transport receive hint and the local prefetch queue depth; Concurrency is the
// number of handler goroutines. VisibilityTimeout and AwaitDuration are kept for
// the shared transport/worker contract; a Kafka reader fetches records in the
// background and retries in-process.
type ConsumerConfig struct {
	Topic             string
	Group             string
	BatchSize         int32
	Concurrency       int
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

// DefaultConsumerConfig returns the shared worker defaults.
func DefaultConsumerConfig(topic, group string) ConsumerConfig {
	return ConsumerConfig{
		Topic:             topic,
		Group:             group,
		BatchSize:         DefaultBatchSize,
		Concurrency:       DefaultConsumerConcurrency,
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
	if c.BatchSize <= 0 || c.BatchSize > MaxConsumerBatchSize {
		return newConfigError("Consumer batch size must be between 1 and 32")
	}
	if c.Concurrency < 0 || c.Concurrency > MaxConsumerConcurrency {
		return newConfigError("Consumer concurrency must be between 1 and 64")
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
// The pull loop keeps a local prefetch queue filled from the transport and
// Concurrency handler goroutines drain it, so one worker process can process
// several documents at a time. Per-partition offset order is preserved: a record
// is committed only once every earlier record of its partition has completed.
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

	// deliver is the local prefetch queue between the pull loop and the handler
	// goroutines.
	deliver chan delivery
	// workers tracks the handler goroutines for a bounded shutdown.
	workers sync.WaitGroup
	// tracker orders dispatched records per partition for in-order commits.
	tracker *commitTracker
	// commitMu serializes commits so one partition's offset only moves forward.
	commitMu sync.Mutex

	mutex   sync.Mutex
	started bool
	stopped bool
	stopCh  chan struct{}
	doneCh  chan struct{}
	cancel  context.CancelFunc
	lastErr error

	dlqOnce sync.Once
}

// delivery pairs one dispatched record with its commit-tracking entry.
type delivery struct {
	message ReceivedMessage
	tracked *trackedMessage
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

// Start validates, starts the transport and launches the pull loop plus the
// handler goroutines.
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
	concurrency := c.Config.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultConsumerConcurrency
	}
	c.tracker = newCommitTracker()
	c.deliver = make(chan delivery, c.prefetchDepth())
	c.workers.Add(concurrency)
	for worker := 0; worker < concurrency; worker++ {
		go c.worker(loopContext)
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

// prefetchDepth bounds the local queue between the pull loop and the handlers.
func (c *PullConsumer) prefetchDepth() int {
	depth := int(c.Config.BatchSize)
	if depth <= 0 {
		depth = int(DefaultBatchSize)
	}
	if depth > int(MaxConsumerBatchSize) {
		depth = int(MaxConsumerBatchSize)
	}
	return depth
}

// loop pulls batches from the transport and feeds the local queue. It blocks on
// a full queue, which stops fetching until the handlers catch up.
func (c *PullConsumer) loop(ctx context.Context) {
	defer func() {
		close(c.deliver)
		c.workers.Wait()
		close(c.doneCh)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		messages, err := c.transport.Receive(ctx, int32(c.prefetchDepth()), c.Config.VisibilityTimeout)
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
			tracked := c.tracker.track(message)
			if tracked == nil {
				// A duplicate delivery of a record already in flight (or a
				// partition whose commits are blocked) is dropped: the record
				// stays uncommitted and is redelivered later.
				continue
			}
			select {
			case c.deliver <- delivery{message: message, tracked: tracked}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// worker drains the local queue. It returns when the queue is closed or the
// context is cancelled; unprocessed records stay uncommitted and are redelivered.
func (c *PullConsumer) worker(ctx context.Context) {
	defer c.workers.Done()
	for item := range c.deliver {
		if ctx.Err() != nil {
			return
		}
		c.processMessage(ctx, item)
	}
}

// processMessage runs one delivery through handler until it is committed,
// exhausted, or the context is cancelled. The commit-tracking entry travels with
// the message so an out-of-order completion cannot commit a later offset.
func (c *PullConsumer) processMessage(ctx context.Context, item delivery) {
	for attempt := 1; ; attempt++ {
		item.message.DeliveryAttempt = attempt
		if c.handler(ctx, item.message) {
			c.commit(ctx, item.tracked)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if c.Config.MaxAttempts > 0 && attempt >= c.Config.MaxAttempts {
			c.deadLetter(ctx, item.message)
			c.commit(ctx, item.tracked)
			return
		}
		if !c.retryDelay(ctx, attempt) {
			return
		}
	}
}

// commit advances the partition's committed offset over the contiguous completed
// prefix. Commits are serialized because committing a later offset before an
// earlier one would silently skip the earlier record.
func (c *PullConsumer) commit(parent context.Context, entry *trackedMessage) {
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	for _, message := range c.tracker.complete(entry) {
		commitContext, cancel := context.WithTimeout(context.WithoutCancel(parent), defaultCommitTimeout)
		err := c.transport.Ack(commitContext, message)
		cancel()
		if err != nil {
			// Never advance past a failed commit: block the partition so its
			// records are redelivered instead of being skipped by a later offset.
			c.tracker.block(partitionKeyOf(message))
			c.setLastError(newSafeError(ErrConsumerFailed, "consumer commit", err))
			return
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
