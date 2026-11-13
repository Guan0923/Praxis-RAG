// Package searchworker implements the queued-search consumer side of the
// service: it leases one durable search task, runs retrieval outside the
// request path, captures expiring artifact references without publishing a
// snapshot, and stores the final immutable result.
//
// MySQL is the source of truth. The broker carries an identifier-only
// notification (result_id, namespace, schedule_version); the worker reads the
// committed arguments and deadline back from search_tasks, so a redelivery can
// never disagree with the accepted request. Submission itself returns as soon
// as the MySQL transaction commits and does not depend on this consumer.
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
	"math"
	"sync"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/logging"
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

// Receiver is the optional in-process notification source used by Run/RunOnce.
// Production workers consume broker messages through MessageHandler instead.
type Receiver interface {
	// Receive returns the next notification, or ok=false when nothing is
	// available.
	Receive(ctx context.Context) (state.SearchNotification, bool, error)
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
	// BatchSize bounds one RunOnce pass (default one notification).
	BatchSize int
	// Receiver supplies notifications for Run/RunOnce; nil leaves them no-ops.
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

// Handle processes one identifier-only notification and reports whether the
// broker delivery may be acknowledged (true) or must be retried (false).
//
// The lookup order is: validate identifiers -> claim by schedule version ->
// execute against the committed arguments -> complete in one MySQL transaction.
// A stale version, a terminal task, a live lease held by another worker or a
// purged row is safe to acknowledge; the maintenance loop re-emits expired
// deadlines and the sweep/retry path bumps the version when a task needs
// another attempt.
func (p *Processor) Handle(ctx context.Context, notification state.SearchNotification) bool {
	if notification.ResultID != "" {
		ctx = logging.With(ctx, "result_id", notification.ResultID)
	}
	if notification.Namespace != "" {
		ctx = logging.WithNamespace(ctx, notification.Namespace)
	}
	if p.searcher == nil {
		// Dead-letter-only worker: execution must never run here.
		p.logWarn(ctx, "search worker has no searcher; leaving delivery retryable")
		return false
	}
	validated, err := state.ValidateSearchNotification(map[string]any{
		"schema":           float64(state.SearchEnvelopeSchema),
		"result_id":        notification.ResultID,
		"namespace":        notification.Namespace,
		"schedule_version": notification.ScheduleVersion,
	})
	if err != nil {
		p.logWarn(ctx, "search notification invalid", logging.ErrorAttrs(err)...)
		return false
	}
	notification = validated
	task, err := p.tasks.Claim(ctx, notification)
	if err != nil {
		// A malformed notification or unavailable task state must not be acked.
		p.logWarn(ctx, "search claim unavailable", logging.ErrorAttrs(err)...)
		return false
	}
	if task == nil {
		p.logInfo(ctx, "search notification skipped",
			"result_id", notification.ResultID,
			"schedule_version", notification.ScheduleVersion)
		return true
	}

	deadline := time.Now().Add(durationSeconds(task.RemainingSec))
	executionContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var failure error
	payload, searchErr := p.search(executionContext, task)
	if searchErr != nil {
		failure = searchErr
	} else {
		payload = normalizePayload(payload)
		payload = p.capture(ctx, task.Namespace, payload)
		if _, completeErr := p.tasks.Complete(ctx, task, payload); completeErr != nil {
			failure = completeErr
		}
	}

	if failure != nil {
		// Completion may have committed before an acknowledgement was lost.
		if terminal, terminalErr := p.tasks.Terminal(ctx, notification); terminalErr == nil && terminal {
			return true
		}
		retry := p.retryable(failure)
		cause := "Search execution failed"
		applied, failErr := p.tasks.Fail(ctx, task, retry, &cause)
		if failErr != nil {
			// The durable retry/terminal transition is not recorded; the
			// delivery must stay retryable.
			p.logWarn(ctx, "search failure could not be recorded", logging.ErrorAttrs(failErr)...)
			return false
		}
		if !applied {
			// Another worker won the lease or the task already advanced.
			return true
		}
		p.logWarn(ctx, "search execution deferred",
			append([]any{"retry", retry}, logging.ErrorAttrs(failure)...)...)
		return true
	}
	return true
}

// search converts the committed task arguments into the validated contract and
// runs the injected retrieval.
func (p *Processor) search(ctx context.Context, task *state.SearchTask) (map[string]any, error) {
	request, err := contracts.SearchRequestFromArguments(normalizeArguments(task.Arguments))
	if err != nil {
		return nil, err
	}
	return p.searcher.Search(ctx, task.Namespace, request)
}

// normalizeArguments restores Python's integer typing for JSON numbers.
//
// The state (search_tasks.go scanSearchTask) decoder keeps integer tokens as
// json.Number, which the contract validator accepts; this is defence in depth
// for arguments built in process or decoded by an older producer that still
// yield float64 for candidate_k/top_k. Python's json module preserves ints, so
// an integral float64 carries the same wire value.
func normalizeArguments(arguments map[string]any) map[string]any {
	if len(arguments) == 0 {
		return arguments
	}
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		if number, ok := value.(float64); ok && !math.IsNaN(number) && !math.IsInf(number, 0) &&
			number == math.Trunc(number) && math.Abs(number) <= 1e15 {
			normalized[key] = int(number)
			continue
		}
		normalized[key] = value
	}
	return normalized
}

// normalizePayload renders programmatic slice values as the JSON-shaped []any the
// state snapshot and searchrefs layers contract on (Python's json round-trip does
// this implicitly). SearchEngine.SearchRaw returns results as []map[string]any;
// state.ResultSnapshots.Prepare requires []any and searchrefs.project iterates
// []any, so without this normalization the snapshot degrades and artifact
// references are silently skipped.
func normalizePayload(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	results, ok := payload["results"].([]map[string]any)
	if !ok {
		return payload
	}
	normalized := make([]any, len(results))
	for index, item := range results {
		normalized[index] = map[string]any(item)
	}
	updated := make(map[string]any, len(payload))
	for key, value := range payload {
		updated[key] = value
	}
	updated["results"] = normalized
	return updated
}

// capture projects artifacts onto the payload with saveSnapshot=false. Capture
// failures never discard retrieval results (the searchrefs seam is already
// failure tolerant; this is a second safety net).
func (p *Processor) capture(ctx context.Context, namespace string, payload map[string]any) map[string]any {
	if p.capturer == nil {
		return payload
	}
	enriched, err := p.capturer.Capture(ctx, namespace, payload, false)
	if err != nil {
		p.logWarn(ctx, "search reference capture unavailable", logging.ErrorAttrs(err)...)
		return payload
	}
	return enriched
}

// MessageHandler adapts the processor to mq.PullConsumer. The decoder accepts
// both the native schema-3 envelope and raw Debezium search_tasks records.
// Records that are not business changes are acked without retries; malformed
// payloads are nacked so the broker retry/DLQ policy applies.
func (p *Processor) MessageHandler() mq.Handler {
	return func(ctx context.Context, message mq.ReceivedMessage) bool {
		notification, err := mq.DecodeSearchNotification(message.Body)
		if err != nil {
			if state.IgnorableCDCChange(err) {
				return true
			}
			p.logWarn(ctx, "malformed search message ignored", logging.ErrorAttrs(err)...)
			return false
		}
		return p.Handle(ctx, notification)
	}
}

// DeadLetter records broker retry exhaustion as a durable terminal state.
func (p *Processor) DeadLetter(ctx context.Context, notification state.SearchNotification) (bool, error) {
	if notification.ResultID != "" {
		ctx = logging.With(ctx, "result_id", notification.ResultID)
	}
	if notification.Namespace != "" {
		ctx = logging.WithNamespace(ctx, notification.Namespace)
	}
	complete, err := p.tasks.DeadLetter(ctx, notification)
	if err != nil {
		p.logWarn(ctx, "search dead-letter failed", logging.ErrorAttrs(err)...)
		return false, err
	}
	if complete {
		p.logInfo(ctx, "search dead-letter outcome saved or task absent")
	}
	return complete, nil
}

// DeadLetterMessage decodes one dead-letter delivery and records it.
func (p *Processor) DeadLetterMessage(ctx context.Context, message mq.ReceivedMessage) (bool, error) {
	notification, err := mq.DecodeSearchNotification(message.Body)
	if err != nil {
		if state.IgnorableCDCChange(err) {
			return true, nil
		}
		p.logWarn(ctx, "malformed search dead-letter message ignored", logging.ErrorAttrs(err)...)
		return false, nil
	}
	return p.DeadLetter(ctx, notification)
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

// RunOnce drains up to BatchSize notifications from the configured Receiver and
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
		notification, ok, err := p.receiver.Receive(ctx)
		if err != nil {
			return processed, err
		}
		if !ok {
			break
		}
		p.Handle(ctx, notification)
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
			p.logWarn(ctx, "search worker pass failed", logging.ErrorAttrs(err)...)
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

// logWarn writes a warning enriched with the context correlation fields
// (result_id, namespace) and redacted structured error attributes.
func (p *Processor) logWarn(ctx context.Context, message string, args ...any) {
	p.logger.Warn(message, append(logging.Attrs(ctx), args...)...)
}

// logInfo writes an informational line enriched with the context correlation
// fields.
func (p *Processor) logInfo(ctx context.Context, message string, args ...any) {
	p.logger.Info(message, append(logging.Attrs(ctx), args...)...)
}

func durationSeconds(seconds float64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}
