// Package searchworker implements the queued-search consumer side of the
// service: it leases one durable search task, runs retrieval outside the
// request path, captures expiring artifact references without publishing a
// snapshot, and stores the final immutable result.
//
// It is a port of rag_mcp/search_worker.py for the broker-driven part; the
// Python implementation executes retrieval in a spawned child process for
// isolation, while the Go port exposes a narrow Searcher seam so the runtime
// keeps ownership of retrieval and the worker only owns durable task state.
//
// The package is transport-neutral: production binaries wire MessageHandler
// into internal/mq.PullConsumer, while tests and broker-less deployments can
// drive Handle/RunOnce/Run directly.
package searchworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// Searcher executes one validated retrieval action. The runtime implements it;
// the worker never imports internal/runtime.
type Searcher interface {
	Search(ctx context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error)
}

// Capturer projects expiring artifact references onto the retrieval payload.
// internal/searchrefs.Capture satisfies it. The worker always calls Capture with
// saveSnapshot=false, because the durable task completion owns the snapshot.
type Capturer interface {
	Capture(ctx context.Context, namespace string, payload map[string]any, saveSnapshot bool) (map[string]any, error)
}

// Receiver is the optional local envelope source used by Run/RunOnce. Production
// workers consume broker messages through MessageHandler instead.
type Receiver interface {
	// Receive returns the next envelope, or ok=false when nothing is available.
	Receive(ctx context.Context) (state.SearchEnvelope, bool, error)
}

// Options configures a Processor. Tasks is required; Searcher may be nil only
// for a dead-letter-only worker, whose Handle calls are then nacked.
type Options struct {
	Tasks    *state.SearchTasks
	Searcher Searcher
	Capturer Capturer
	Logger   *slog.Logger

	// PollInterval is the Run loop idle interval (default one second).
	PollInterval time.Duration
	// BatchSize bounds one RunOnce pass (default one envelope).
	BatchSize int
	// Receiver supplies envelopes for Run/RunOnce; nil leaves them no-ops.
	Receiver Receiver
	// Retryable overrides the failure classification. The default mirrors
	// Python: ValueError-shaped errors are terminal, everything else retries.
	Retryable func(error) bool
}

// Processor consumes queued search tasks. It is safe for concurrent Handle
// calls; Run/RunOnce serialize on one loop at a time.
type Processor struct {
	tasks        *state.SearchTasks
	searcher     Searcher
	capturer     Capturer
	logger       *slog.Logger
	pollInterval time.Duration
	batchSize    int
	receiver     Receiver
	retryable    func(error) bool

	stopOnce sync.Once
	stopCh   chan struct{}

	mutex   sync.Mutex
	running bool
	closed  bool
}

// New validates options and builds the processor.
func New(options Options) (*Processor, error) {
	if options.Tasks == nil {
		return nil, fmt.Errorf("searchworker: Tasks is required")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	pollInterval := options.PollInterval
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	batchSize := options.BatchSize
	if batchSize <= 0 {
		batchSize = 1
	}
	retryable := options.Retryable
	if retryable == nil {
		retryable = defaultRetryable
	}
	return &Processor{
		tasks:        options.Tasks,
		searcher:     options.Searcher,
		capturer:     options.Capturer,
		logger:       logger,
		pollInterval: pollInterval,
		batchSize:    batchSize,
		receiver:     options.Receiver,
		retryable:    retryable,
		stopCh:       make(chan struct{}),
	}, nil
}

// defaultRetryable ports SearchWorker's `retry=not isinstance(exc, ValueError)`:
// request-validation failures are terminal; infrastructure failures retry.
func defaultRetryable(err error) bool {
	var validation *contracts.SearchValidationError
	if errors.As(err, &validation) {
		return false
	}
	var valueError *state.ValueError
	if errors.As(err, &valueError) {
		return false
	}
	return true
}

// Handle processes one full-request envelope and reports whether the broker
// delivery may be acknowledged (true) or must be retried (false).
func (p *Processor) Handle(ctx context.Context, envelope state.SearchEnvelope) bool {
	if p.searcher == nil {
		// Dead-letter-only worker: execution must never run here.
		p.logWarn("search worker has no searcher; leaving delivery retryable", "result_id", envelope.ResultID)
		return false
	}
	task, err := p.tasks.Claim(ctx, envelope)
	if err != nil {
		// A malformed envelope or unavailable task state must not be acked.
		p.logWarn("search claim unavailable", "result_id", envelope.ResultID, "error_type", typeName(err))
		return false
	}
	if task == nil {
		return p.terminalOrNack(ctx, envelope)
	}

	deadline := time.Now().Add(durationSeconds(task.RemainingSec))
	executionContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var failure error
	payload, searchErr := p.search(executionContext, envelope, task)
	if searchErr != nil {
		failure = searchErr
	} else {
		payload = p.capture(ctx, envelope, payload)
		if _, completeErr := p.tasks.Complete(ctx, task, payload); completeErr != nil {
			failure = completeErr
		}
	}

	if failure != nil {
		// Completion may have committed before an acknowledgement was lost.
		if terminal, terminalErr := p.tasks.Terminal(ctx, envelopeValues(envelope)); terminalErr == nil && terminal {
			return true
		}
		retry := p.retryable(failure)
		cause := "Search execution failed"
		if _, failErr := p.tasks.Fail(ctx, task, retry, &cause); failErr != nil {
			p.logWarn("search failure could not be recorded", "result_id", envelope.ResultID, "error_type", typeName(failErr))
		}
		p.logWarn("search execution deferred", "result_id", envelope.ResultID, "retry", retry,
			"error_type", typeName(failure))
	}
	return p.terminalOrNack(ctx, envelope)
}

// search converts the durable envelope arguments into the validated contract
// and runs the injected retrieval.
func (p *Processor) search(ctx context.Context, envelope state.SearchEnvelope, task *state.SearchTask) (map[string]any, error) {
	request, err := contracts.SearchRequestFromArguments(task.Arguments)
	if err != nil {
		return nil, err
	}
	return p.searcher.Search(ctx, envelope.Namespace, request)
}

// capture projects artifacts onto the payload with saveSnapshot=false. Capture
// failures never discard retrieval results (the searchrefs seam is already
// failure tolerant; this is a second safety net).
func (p *Processor) capture(ctx context.Context, envelope state.SearchEnvelope, payload map[string]any) map[string]any {
	if p.capturer == nil {
		return payload
	}
	enriched, err := p.capturer.Capture(ctx, envelope.Namespace, payload, false)
	if err != nil {
		p.logWarn("search reference capture unavailable", "result_id", envelope.ResultID, "error_type", typeName(err))
		return payload
	}
	return enriched
}

func (p *Processor) terminalOrNack(ctx context.Context, envelope state.SearchEnvelope) bool {
	terminal, err := p.tasks.Terminal(ctx, envelopeValues(envelope))
	if err != nil {
		p.logWarn("search terminal state unavailable", "result_id", envelope.ResultID, "error_type", typeName(err))
		return false
	}
	return terminal
}

// envelopeValues renders the durable envelope as the validated map shape the
// state machine Terminal/DeadLetter helpers accept (they compare namespace).
func envelopeValues(envelope state.SearchEnvelope) map[string]any {
	return map[string]any{
		"schema":     float64(envelope.Schema),
		"result_id":  envelope.ResultID,
		"namespace":  envelope.Namespace,
		"arguments":  envelope.Arguments,
		"created_at": envelope.CreatedAt,
		"expires_at": envelope.ExpiresAt,
		"deadline":   envelope.Deadline,
	}
}

// MessageHandler adapts the processor to mq.PullConsumer: malformed payloads are
// nacked so the broker retry/DLQ policy applies, exactly like the Python
// queue's validate-before-callback behaviour.
func (p *Processor) MessageHandler() mq.Handler {
	return func(ctx context.Context, message mq.ReceivedMessage) bool {
		envelope, err := mq.DecodeSearchEnvelope(message.Body)
		if err != nil {
			p.logWarn("malformed search message ignored", "error_type", typeName(err))
			return false
		}
		return p.Handle(ctx, envelope)
	}
}

// DeadLetter records broker retry exhaustion as a durable terminal state.
func (p *Processor) DeadLetter(ctx context.Context, envelope state.SearchEnvelope) (bool, error) {
	complete, err := p.tasks.DeadLetter(ctx, envelopeValues(envelope))
	if err != nil {
		p.logWarn("search dead-letter failed", "result_id", envelope.ResultID, "error_type", typeName(err))
		return false, err
	}
	if complete {
		p.logInfo("search dead-letter outcome saved or task absent", "result_id", envelope.ResultID)
	}
	return complete, nil
}

// DeadLetterMessage decodes one dead-letter delivery and records it.
func (p *Processor) DeadLetterMessage(ctx context.Context, message mq.ReceivedMessage) (bool, error) {
	envelope, err := mq.DecodeSearchEnvelope(message.Body)
	if err != nil {
		p.logWarn("malformed search dead-letter message ignored", "error_type", typeName(err))
		return false, nil
	}
	return p.DeadLetter(ctx, envelope)
}

// DeadLetterHandler adapts DeadLetterMessage to mq.PullConsumer.
func (p *Processor) DeadLetterHandler() mq.Handler {
	return func(ctx context.Context, message mq.ReceivedMessage) bool {
		complete, _ := p.DeadLetterMessage(ctx, message)
		return complete
	}
}

// ExhaustedHandler is the mq.PullConsumer.OnExhausted hook: it records the
// terminal outcome of a message that hit MaxAttempts.
func (p *Processor) ExhaustedHandler() func(ctx context.Context, message mq.ReceivedMessage) error {
	return func(ctx context.Context, message mq.ReceivedMessage) error {
		_, err := p.DeadLetterMessage(ctx, message)
		return err
	}
}

// RunOnce drains up to BatchSize envelopes from the configured Receiver and
// returns how many were handled. Without a Receiver it is a no-op.
func (p *Processor) RunOnce(ctx context.Context) (int, error) {
	if p.receiver == nil {
		return 0, nil
	}
	processed := 0
	for processed < p.batchSize {
		if err := ctx.Err(); err != nil {
			return processed, nil
		}
		envelope, ok, err := p.receiver.Receive(ctx)
		if err != nil {
			return processed, err
		}
		if !ok {
			break
		}
		p.Handle(ctx, envelope)
		processed++
	}
	return processed, nil
}

// Run polls RunOnce every PollInterval until ctx is cancelled or Stop/Close is
// called. It is idempotent on shutdown and returns nil for a graceful stop.
func (p *Processor) Run(ctx context.Context) error {
	p.mutex.Lock()
	if p.closed {
		p.mutex.Unlock()
		return fmt.Errorf("searchworker: processor is closed")
	}
	if p.running {
		p.mutex.Unlock()
		return fmt.Errorf("searchworker: processor is already running")
	}
	p.running = true
	p.mutex.Unlock()
	defer func() {
		p.mutex.Lock()
		p.running = false
		p.mutex.Unlock()
	}()

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-p.stopCh:
			return nil
		case <-timer.C:
		}
		if _, err := p.RunOnce(ctx); err != nil {
			p.logWarn("search worker pass failed", "error_type", typeName(err))
		}
		timer.Reset(p.pollInterval)
	}
}

// RunConsumer starts an mq consumer whose handler is MessageHandler and blocks
// until ctx is done, then stops the consumer. It is the production lifecycle.
func (p *Processor) RunConsumer(ctx context.Context, consumer mq.Consumer) error {
	if consumer == nil {
		return fmt.Errorf("searchworker: consumer is required")
	}
	if err := consumer.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = consumer.Stop() }()
	<-ctx.Done()
	return nil
}

// Stop asks the Run loop to exit. It is idempotent and safe to call before Run.
func (p *Processor) Stop() {
	p.stopOnce.Do(func() { close(p.stopCh) })
}

// Close stops the processor loop and marks it closed. It is idempotent.
func (p *Processor) Close() error {
	p.Stop()
	p.mutex.Lock()
	p.closed = true
	p.mutex.Unlock()
	return nil
}

func (p *Processor) logWarn(message string, args ...any) {
	p.logger.Warn(message, args...)
}

func (p *Processor) logInfo(message string, args ...any) {
	p.logger.Info(message, args...)
}

func durationSeconds(seconds float64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func typeName(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}
