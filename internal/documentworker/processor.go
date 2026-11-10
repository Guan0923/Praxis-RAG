// Package documentworker implements the durable document-ingest consumer: it
// leases one committed document job, keeps the SQL lease alive while the
// runtime parses/indexes it, reconciles committed journal publications after a
// crash, and records retryable or terminal failures.
//
// It is a port of rag_mcp/document_worker.py plus the dead-letter half of
// rag_mcp/dead_letter_worker.py. The package stays runtime independent through a
// narrow Ingester seam; the runtime owns parsing, chunking and Qdrant writes.
package documentworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// Payload is the validated, identifier-complete input of one document job. It
// carries everything the Python worker read back from the durable row so a
// redelivered notification can never disagree with the committed request.
type Payload struct {
	ArtifactID           string
	Namespace            string
	Name                 string
	Content              []byte
	Size                 int64
	SHA256               string
	ConfigurationVersion int64
	IdempotencyKey       *string
	CreatedAt            float64
	Attempts             int
}

// PayloadFromTask projects the leased job row into the ingester payload.
func PayloadFromTask(task *state.DocumentJobTask) Payload {
	if task == nil {
		return Payload{}
	}
	return Payload{
		ArtifactID:           task.ArtifactID,
		Namespace:            task.Namespace,
		Name:                 task.Name,
		Content:              task.Content,
		Size:                 task.Size,
		SHA256:               task.SHA256,
		ConfigurationVersion: task.ConfigurationVersion,
		IdempotencyKey:       task.IdempotencyKey,
		CreatedAt:            task.CreatedAt,
		Attempts:             task.Attempts,
	}
}

// Ingester performs the actual document publication. The runtime implements it
// (parse/chunk/embed/upsert + journal commit); returning nil means the journal
// publication is committed and Reconcile can make the job terminal.
type Ingester interface {
	ProcessDocumentJob(ctx context.Context, job Payload) (map[string]any, error)
}

// Receiver is the optional local notification source used by Run/RunOnce.
// Production workers consume broker messages through MessageHandler instead.
type Receiver interface {
	Receive(ctx context.Context) (state.DocumentNotification, bool, error)
}

// JournalRecoverer is the optional runtime hook for upload-journal recovery
// (UploadCoordinator.recover in Python). It is best effort: maintenance
// failures never block consumption.
type JournalRecoverer interface {
	RecoverDocumentUploads(ctx context.Context) error
}

// Options configures a Processor. Jobs is required; Ingester may be nil only
// for a dead-letter-only worker, whose Handle calls are then nacked.
type Options struct {
	Jobs     *state.DocumentJobs
	Ingester Ingester
	Logger   *slog.Logger

	// PollInterval is the Run loop idle interval (default one second).
	PollInterval time.Duration
	// BatchSize bounds one RunOnce pass (default one notification).
	BatchSize int
	// Receiver supplies notifications for Run/RunOnce; nil leaves them no-ops.
	Receiver Receiver
	// HeartbeatInterval overrides the job setting (tests use a short value).
	HeartbeatInterval time.Duration
	// TerminalError overrides the failure classification. The default mirrors
	// Python: knowledge/document validation errors are terminal, everything
	// else is retried.
	TerminalError func(error) bool
	// JournalRecovery optionally recovers interrupted upload journals.
	JournalRecovery JournalRecoverer
	// SweepLimit bounds one maintenance sweep (default 50).
	SweepLimit int
}

// Processor consumes durable document jobs. Handles may run concurrently; the
// durable lease/execution id fencing decides which one may finish.
type Processor struct {
	jobs              *state.DocumentJobs
	ingester          Ingester
	logger            *slog.Logger
	pollInterval      time.Duration
	batchSize         int
	receiver          Receiver
	heartbeatInterval time.Duration
	terminalError     func(error) bool
	journalRecovery   JournalRecoverer
	sweepLimit        int

	stopOnce sync.Once
	stopCh   chan struct{}

	mutex   sync.Mutex
	running bool
	closed  bool
}

// New validates options and builds the processor.
func New(options Options) (*Processor, error) {
	if options.Jobs == nil {
		return nil, fmt.Errorf("documentworker: Jobs is required")
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
	heartbeat := options.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = time.Duration(options.Jobs.Settings.HeartbeatSeconds * float64(time.Second))
	}
	if heartbeat <= 0 {
		heartbeat = time.Second
	}
	terminal := options.TerminalError
	if terminal == nil {
		terminal = defaultTerminalError
	}
	sweepLimit := options.SweepLimit
	if sweepLimit <= 0 {
		sweepLimit = 50
	}
	return &Processor{
		jobs:              options.Jobs,
		ingester:          options.Ingester,
		logger:            logger,
		pollInterval:      pollInterval,
		batchSize:         batchSize,
		receiver:          options.Receiver,
		heartbeatInterval: heartbeat,
		terminalError:     terminal,
		journalRecovery:   options.JournalRecovery,
		sweepLimit:        sweepLimit,
		stopCh:            make(chan struct{}),
	}, nil
}

// WithOutboxReceiver configures broker-less local delivery: it sets
// options.Receiver to an OutboxReceiver so Run/RunOnce drain committed PENDING
// document_job_outbox rows directly instead of consuming broker messages. It
// returns the updated options so callers can build the processor in one
// expression, and is a no-op for the broker mode where Receiver stays nil.
func WithOutboxReceiver(options Options) Options {
	options.Receiver = NewOutboxReceiver(options.Jobs)
	return options
}

// defaultTerminalError ports `retry = not isinstance(exc, KnowledgeValidationError)`.
func defaultTerminalError(err error) bool {
	var validation *state.DocumentValidationError
	if errors.As(err, &validation) {
		return true
	}
	var knowledgeValidation *apierrors.KnowledgeValidationError
	if errors.As(err, &knowledgeValidation) {
		return true
	}
	return false
}

// Handle processes one identifier-only notification and reports whether the
// broker delivery may be acknowledged (true) or must be retried (false).
func (p *Processor) Handle(ctx context.Context, notification state.DocumentNotification) bool {
	if p.ingester == nil {
		// Dead-letter-only worker: ingestion must never run here.
		p.logWarn("document worker has no ingester; leaving delivery retryable",
			"artifact_id", notification.ArtifactID)
		return false
	}
	validated, err := state.ValidateDocumentNotification(map[string]any{
		"schema":      float64(notification.Schema),
		"artifact_id": notification.ArtifactID,
		"namespace":   notification.Namespace,
		"name":        notification.Name,
		"created_at":  notification.CreatedAt,
	})
	if err != nil {
		p.logWarn("document notification invalid", "error_type", typeName(err))
		return false
	}
	notification = validated
	job, err := p.jobs.Get(ctx, notification.ArtifactID, &notification.Namespace)
	if err != nil {
		p.logWarn("document job unavailable", "artifact_id", notification.ArtifactID, "error_type", typeName(err))
		return false
	}
	if job == nil {
		// An unknown or purged job cannot become processable by retrying.
		p.logInfo("document job unknown; acknowledging", "artifact_id", notification.ArtifactID)
		return true
	}
	if isTerminalStatus(job.Status) {
		return true
	}
	if reconciled, err := p.jobs.Reconcile(ctx, &state.DocumentJobTask{DocumentJob: *job}); err != nil {
		p.logWarn("document reconcile unavailable", "artifact_id", notification.ArtifactID, "error_type", typeName(err))
		return false
	} else if reconciled {
		return true
	}

	task, err := p.jobs.Claim(ctx, notification)
	if err != nil {
		p.logWarn("document claim unavailable", "artifact_id", notification.ArtifactID, "error_type", typeName(err))
		return false
	}
	if task == nil {
		// Another live worker owns the lease (or the job turned terminal).
		return p.terminalOrNack(ctx, notification.ArtifactID)
	}

	stopHeartbeat := p.startHeartbeat(task)
	defer stopHeartbeat()

	if reconciled, err := p.jobs.Reconcile(ctx, task); err != nil {
		p.logWarn("document reconcile unavailable", "artifact_id", notification.ArtifactID, "error_type", typeName(err))
		return false
	} else if reconciled {
		return true
	}
	busy, err := p.jobs.ExecutionBusy(ctx, task)
	if err != nil {
		p.logWarn("document execution state unavailable", "artifact_id", notification.ArtifactID, "error_type", typeName(err))
		return false
	}
	if busy {
		return false
	}

	_, ingestErr := p.ingester.ProcessDocumentJob(ctx, PayloadFromTask(task))
	// Ingestion is the only slow phase; stop renewing before the terminal
	// bookkeeping so a heartbeat write cannot race the reconciliation
	// transaction (notably on SQLite in tests). The deferred stop is a backstop.
	stopHeartbeat()
	retryCause := "Document reconciliation unavailable"
	if ingestErr == nil {
		reconciled, err := p.jobs.Reconcile(ctx, task)
		if err != nil {
			// Best-effort requeue so the next delivery can reconcile immediately
			// instead of waiting for the lease to expire.
			_ = p.jobs.Fail(ctx, task, true, &retryCause)
			p.logWarn("document reconcile unavailable", "artifact_id", notification.ArtifactID, "error_type", typeName(err))
			return false
		}
		if !reconciled {
			// The publication is not committed yet; leave the delivery retryable.
			return false
		}
	} else {
		if reconciled, err := p.jobs.Reconcile(ctx, task); err == nil && reconciled {
			return true
		}
		retry := !p.terminalError(ingestErr)
		message := "Document processing failed (" + typeName(ingestErr) + ")"
		if !retry {
			message = "Invalid document or changed knowledge-base configuration"
		}
		if failErr := p.jobs.Fail(ctx, task, retry, &message); failErr != nil {
			// UploadConflict is expected when another worker won the lease.
			var conflict *state.UploadConflict
			if !errors.As(failErr, &conflict) {
				p.logWarn("document failure could not be recorded", "artifact_id", notification.ArtifactID, "error_type", typeName(failErr))
				return false
			}
		}
		p.logWarn("document processing deferred", "artifact_id", notification.ArtifactID,
			"retry", retry, "error_type", typeName(ingestErr))
	}
	return p.terminalOrNack(ctx, notification.ArtifactID)
}

// startHeartbeat renews the lease while ingestion runs. A failed heartbeat ends
// the goroutine and lets lease expiry recover the job, like the Python worker.
func (p *Processor) startHeartbeat(task *state.DocumentJobTask) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(p.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := p.jobs.Heartbeat(context.Background(), task); err != nil {
					p.logWarn("document lease heartbeat failed", "artifact_id", task.ArtifactID, "error_type", typeName(err))
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
		})
	}
}

func (p *Processor) terminalOrNack(ctx context.Context, artifactID string) bool {
	terminal, err := p.jobs.Terminal(ctx, artifactID)
	if err != nil {
		p.logWarn("document terminal state unavailable", "artifact_id", artifactID, "error_type", typeName(err))
		return false
	}
	return terminal
}

// MessageHandler adapts the processor to mq.PullConsumer. Malformed schema-3
// payloads are nacked so the broker retry/DLQ policy applies, exactly like the
// Python queue's validate-before-callback behaviour.
func (p *Processor) MessageHandler() mq.Handler {
	return func(ctx context.Context, message mq.ReceivedMessage) bool {
		notification, err := mq.DecodeDocumentNotification(message.Body)
		if err != nil {
			p.logWarn("malformed document message ignored", "error_type", typeName(err))
			return false
		}
		return p.Handle(ctx, notification)
	}
}

// DeadLetter records broker retry exhaustion as a durable terminal state.
func (p *Processor) DeadLetter(ctx context.Context, notification state.DocumentNotification) (bool, error) {
	complete, err := p.jobs.DeadLetter(ctx, notification)
	if err != nil {
		p.logWarn("document dead-letter failed", "artifact_id", notification.ArtifactID, "error_type", typeName(err))
		return false, err
	}
	if complete {
		p.logInfo("document dead-letter outcome saved or job absent/terminal",
			"artifact_id", notification.ArtifactID)
	}
	return complete, nil
}

// DeadLetterMessage decodes one dead-letter delivery and records it.
func (p *Processor) DeadLetterMessage(ctx context.Context, message mq.ReceivedMessage) (bool, error) {
	notification, err := mq.DecodeDocumentNotification(message.Body)
	if err != nil {
		p.logWarn("malformed document dead-letter message ignored", "error_type", typeName(err))
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

// Recover runs one maintenance pass: requeue stranded/expired jobs, garbage
// collect retained terminal rows and the outbox, then recover interrupted
// upload journals when a runtime hook is configured. Failures are logged, never
// returned as fatal (the Python recover() never blocks consumption).
func (p *Processor) Recover(ctx context.Context) error {
	var firstErr error
	if swept, err := p.jobs.Sweep(ctx, p.sweepLimit); err != nil {
		p.logWarn("document job sweep failed", "error_type", typeName(err))
		if firstErr == nil {
			firstErr = err
		}
	} else if swept > 0 {
		p.logInfo("document jobs re-emitted", "count", swept)
	}
	if _, err := p.jobs.GCJobs(ctx, nil); err != nil {
		p.logWarn("document terminal GC failed", "error_type", typeName(err))
		if firstErr == nil {
			firstErr = err
		}
	}
	if _, err := p.jobs.GCOutbox(ctx, nil); err != nil {
		p.logWarn("document outbox GC failed", "error_type", typeName(err))
		if firstErr == nil {
			firstErr = err
		}
	}
	if p.journalRecovery != nil {
		if err := p.journalRecovery.RecoverDocumentUploads(ctx); err != nil {
			p.logWarn("document upload journal recovery failed", "error_type", typeName(err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
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

// Run performs startup recovery once, then polls RunOnce every PollInterval
// until ctx is cancelled or Stop/Close is called. It is idempotent on shutdown
// and returns nil for a graceful stop.
func (p *Processor) Run(ctx context.Context) error {
	p.mutex.Lock()
	if p.closed {
		p.mutex.Unlock()
		return fmt.Errorf("documentworker: processor is closed")
	}
	if p.running {
		p.mutex.Unlock()
		return fmt.Errorf("documentworker: processor is already running")
	}
	p.running = true
	p.mutex.Unlock()
	defer func() {
		p.mutex.Lock()
		p.running = false
		p.mutex.Unlock()
	}()

	// Python's main loop calls worker.recover() before each consumption pass;
	// the Go loop performs it once at start and exposes Recover for periodic use.
	_ = p.Recover(ctx)

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
			p.logWarn("document worker pass failed", "error_type", typeName(err))
		}
		timer.Reset(p.pollInterval)
	}
}

// RunConsumer starts an mq consumer whose handler is MessageHandler and blocks
// until ctx is done, then stops the consumer. It is the production lifecycle.
func (p *Processor) RunConsumer(ctx context.Context, consumer mq.Consumer) error {
	if consumer == nil {
		return fmt.Errorf("documentworker: consumer is required")
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

// OutboxReceiver is the broker-less local delivery mode: it drains committed
// PENDING outbox rows, marks them published, and yields their schema-3
// notifications. Job-level retries are re-emitted by Sweep, so a failed
// delivery is redelivered after the sweep delay without a broker.
type OutboxReceiver struct {
	Jobs  *state.DocumentJobs
	Limit int
}

// NewOutboxReceiver builds the local outbox-backed receiver.
func NewOutboxReceiver(jobs *state.DocumentJobs) *OutboxReceiver {
	return &OutboxReceiver{Jobs: jobs, Limit: 1}
}

// Receive implements Receiver.
func (r *OutboxReceiver) Receive(ctx context.Context) (state.DocumentNotification, bool, error) {
	if r.Jobs == nil {
		return state.DocumentNotification{}, false, fmt.Errorf("documentworker: jobs are required")
	}
	limit := r.Limit
	if limit <= 0 {
		limit = 1
	}
	events, err := r.Jobs.PendingOutbox(ctx, limit)
	if err != nil {
		return state.DocumentNotification{}, false, err
	}
	if len(events) == 0 {
		return state.DocumentNotification{}, false, nil
	}
	event := events[0]
	notification, err := state.ValidateDocumentNotification(map[string]any{
		"schema":      float64(state.DocumentEnvelopeSchema),
		"artifact_id": event.ArtifactID,
		"namespace":   event.Namespace,
		"name":        event.Name,
		"created_at":  event.CreatedAt,
	})
	if err != nil {
		// Poison outbox row: record a bounded error so it backs off, then skip.
		_ = r.Jobs.MarkPublished(ctx, event.ID, err)
		return state.DocumentNotification{}, false, nil
	}
	if err := r.Jobs.MarkPublished(ctx, event.ID, nil); err != nil {
		return state.DocumentNotification{}, false, err
	}
	return notification, true, nil
}

func isTerminalStatus(status string) bool {
	return status == state.DocumentJobSucceeded || status == state.DocumentJobFailed
}

func (p *Processor) logWarn(message string, args ...any) {
	p.logger.Warn(message, args...)
}

func (p *Processor) logInfo(message string, args ...any) {
	p.logger.Info(message, args...)
}

func typeName(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}
