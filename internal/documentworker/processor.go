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
	"net/http"
	"sync"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/logging"
	"github.com/Guan0923/Praxis-RAG/internal/mq"
	"github.com/Guan0923/Praxis-RAG/internal/state"
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
	heartbeatInterval time.Duration
	terminalError     func(error) bool
	journalRecovery   JournalRecoverer
	sweepLimit        int
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
		heartbeatInterval: heartbeat,
		terminalError:     terminal,
		journalRecovery:   options.JournalRecovery,
		sweepLimit:        sweepLimit,
	}, nil
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
//
// After the durable job is loaded, the context carries the job's correlation
// fields (artifact_id, namespace, operation_id, attempt) so every failure log
// line can be joined with the job row and the Console status view.
func (p *Processor) Handle(ctx context.Context, notification state.DocumentNotification) bool {
	if p.ingester == nil {
		// Dead-letter-only worker: ingestion must never run here.
		p.logWarn(ctx, "document worker has no ingester; leaving delivery retryable",
			"artifact_id", notification.ArtifactID)
		return false
	}
	// The broker decoder already validated the Debezium/app envelope;
	// revalidate here so an in-process caller cannot bypass the identifier and
	// scheduling-version contract.
	validated, err := state.ValidateDocumentNotification(map[string]any{
		"schema":           float64(state.DocumentEnvelopeSchema),
		"artifact_id":      notification.ArtifactID,
		"namespace":        notification.Namespace,
		"schedule_version": notification.ScheduleVersion,
	})
	if err != nil {
		p.logWarn(ctx, "document notification invalid", logging.ErrorAttrs(err)...)
		return false
	}
	notification = validated
	ctx = logging.With(ctx,
		logging.FieldArtifactID, notification.ArtifactID,
		logging.FieldNamespace, notification.Namespace)
	job, err := p.jobs.Get(ctx, notification.ArtifactID, &notification.Namespace)
	if err != nil {
		p.logWarn(ctx, "document job unavailable", logging.ErrorAttrs(err)...)
		return false
	}
	if job == nil {
		// An unknown or purged job cannot become processable by retrying.
		p.logInfo(ctx, "document job unknown; acknowledging")
		return true
	}
	if job.OperationID != nil && *job.OperationID != "" {
		ctx = logging.WithOperationID(ctx, *job.OperationID)
	}
	ctx = logging.With(ctx, "attempt", job.Attempts)
	if isTerminalStatus(job.Status) {
		return true
	}
	if reconciled, err := p.jobs.Reconcile(ctx, &state.DocumentJobTask{DocumentJob: *job}); err != nil {
		p.logWarn(ctx, "document reconcile unavailable", logging.ErrorAttrs(err)...)
		return false
	} else if reconciled {
		return true
	}

	task, err := p.jobs.Claim(ctx, notification)
	if err != nil {
		p.logWarn(ctx, "document claim unavailable", logging.ErrorAttrs(err)...)
		return false
	}
	if task == nil {
		// A stale schedule version, a terminal job, a live lease held by another
		// worker, an early retry or a purged row: all are safe to acknowledge.
		// The sweep emits due retries and expired-lease replacements; a merely
		// queued job stays on its Kafka notification instead of being replayed.
		p.logInfo(ctx, "document notification skipped",
			"artifact_id", notification.ArtifactID,
			"schedule_version", notification.ScheduleVersion,
			"job_status", job.Status)
		return true
	}

	stopHeartbeat := p.startHeartbeat(ctx, task)
	defer stopHeartbeat()

	if reconciled, err := p.jobs.Reconcile(ctx, task); err != nil {
		p.logWarn(ctx, "document reconcile unavailable", logging.ErrorAttrs(err)...)
		return false
	} else if reconciled {
		return true
	}
	busy, err := p.jobs.ExecutionBusy(ctx, task)
	if err != nil {
		p.logWarn(ctx, "document execution state unavailable", logging.ErrorAttrs(err)...)
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
			p.logWarn(ctx, "document reconcile unavailable", logging.ErrorAttrs(err)...)
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
		// The persisted message stays recognizable for the Console and for
		// users, names the execution count and keeps the safe
		// category/stage/status/detail that makes the failure diagnosable
		// without secrets. The third failure terminalizes inside Fail.
		message := documentFailureMessage(task.Attempts, retry, ingestErr)
		if failErr := p.jobs.Fail(ctx, task, retry, &message); failErr != nil {
			// UploadConflict is expected when another worker won the lease.
			var conflict *state.UploadConflict
			if !errors.As(failErr, &conflict) {
				p.logWarn(ctx, "document failure could not be recorded", logging.ErrorAttrs(failErr)...)
				return false
			}
		}
		p.logWarn(ctx, "document processing failed",
			append([]any{"retry", retry,
				"terminal", !retry || task.Attempts >= state.MaxDocumentJobAttempts},
				logging.ErrorAttrs(ingestErr)...)...)
	}
	// The failure path always records a durable outcome (a terminal row or a
	// retry_wait row whose replacement notification the sweep emits), so the
	// delivery can be acknowledged.
	return true
}

// startHeartbeat renews the lease while ingestion runs. A failed heartbeat ends
// the goroutine and lets lease expiry recover the job, like the Python worker.
func (p *Processor) startHeartbeat(ctx context.Context, task *state.DocumentJobTask) func() {
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
					p.logWarn(ctx, "document lease heartbeat failed", logging.ErrorAttrs(err)...)
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

// documentFailureMessage renders the durable job error with the execution count
// so the Console can distinguish attempt 2 of 3 from the final failure.
func documentFailureMessage(attempt int, retry bool, err error) string {
	prefix := fmt.Sprintf("Document processing failed: attempt %d/%d",
		attempt, state.MaxDocumentJobAttempts)
	if !retry {
		prefix = fmt.Sprintf("Invalid document or changed knowledge-base configuration: attempt %d/%d",
			attempt, state.MaxDocumentJobAttempts)
	}
	if isDocumentParseTimeout(err) {
		return prefix + ": parse timeout"
	}
	if summary := logging.ErrorSummary(err); summary != "" {
		return prefix + ": " + summary
	}
	return prefix
}

// isDocumentParseTimeout reports a timeout of the parse stage only. A deadline
// while embedding or indexing is a different failure and keeps its own
// category. Both the remote parse service's 504 and a caller-side deadline
// during parsing mean the per-attempt parse window expired.
func isDocumentParseTimeout(err error) bool {
	if err == nil || logging.ErrorStage(err) != "parsing" {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var status interface{ HTTPStatus() int }
	return errors.As(err, &status) && status.HTTPStatus() == http.StatusGatewayTimeout
}

// MessageHandler adapts the processor to mq.PullConsumer. The decoder accepts
// both the native schema-4 envelope and raw Debezium document_jobs records.
// Records that are not business changes (tombstones, deletes filtered by the
// connector) are acked without retries; malformed payloads are nacked so the
// broker retry/DLQ policy applies.
func (p *Processor) MessageHandler() mq.Handler {
	return func(ctx context.Context, message mq.ReceivedMessage) bool {
		notification, err := mq.DecodeDocumentNotification(message.Body)
		if err != nil {
			if state.IgnorableCDCChange(err) {
				return true
			}
			p.logWarn(ctx, "malformed document message ignored", logging.ErrorAttrs(err)...)
			return false
		}
		return p.Handle(ctx, notification)
	}
}

// DeadLetter records broker retry exhaustion as a durable terminal state.
func (p *Processor) DeadLetter(ctx context.Context, notification state.DocumentNotification) (bool, error) {
	complete, err := p.jobs.DeadLetter(ctx, notification)
	if err != nil {
		p.logWarn(ctx, "document dead-letter failed",
			append([]any{"artifact_id", notification.ArtifactID}, logging.ErrorAttrs(err)...)...)
		return false, err
	}
	if complete {
		p.logInfo(ctx, "document dead-letter outcome saved or job absent/terminal",
			"artifact_id", notification.ArtifactID)
	}
	return complete, nil
}

// DeadLetterMessage decodes one dead-letter delivery and records it.
//
// Records that are not business changes are already complete: replaying an old
// tombstone or connector-filtered delete event through the DLQ must ack it
// without touching any document job.
func (p *Processor) DeadLetterMessage(ctx context.Context, message mq.ReceivedMessage) (bool, error) {
	notification, err := mq.DecodeDocumentNotification(message.Body)
	if err != nil {
		if state.IgnorableCDCChange(err) {
			return true, nil
		}
		p.logWarn(ctx, "malformed document dead-letter message ignored", logging.ErrorAttrs(err)...)
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
// collect retained terminal rows, then recover interrupted upload journals when
// a runtime hook is configured. Requeueing increments schedule_version, so
// Debezium emits the replacement notification; no outbox row is involved.
// Failures are logged, never returned as fatal (the Python recover() never
// blocks consumption).
func (p *Processor) Recover(ctx context.Context) error {
	var firstErr error
	if swept, err := p.jobs.Sweep(ctx, p.sweepLimit); err != nil {
		p.logWarn(ctx, "document job sweep failed", logging.ErrorAttrs(err)...)
		if firstErr == nil {
			firstErr = err
		}
	} else if swept > 0 {
		p.logInfo(ctx, "document jobs re-emitted", "count", swept)
	}
	if _, err := p.jobs.GCJobs(ctx, nil); err != nil {
		p.logWarn(ctx, "document terminal GC failed", logging.ErrorAttrs(err)...)
		if firstErr == nil {
			firstErr = err
		}
	}
	if p.journalRecovery != nil {
		if err := p.journalRecovery.RecoverDocumentUploads(ctx); err != nil {
			p.logWarn(ctx, "document upload journal recovery failed", logging.ErrorAttrs(err)...)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
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

func isTerminalStatus(status string) bool {
	return status == state.DocumentJobSucceeded || status == state.DocumentJobFailed
}

// logWarn writes a warning enriched with the context correlation fields.
func (p *Processor) logWarn(ctx context.Context, message string, args ...any) {
	p.logger.Warn(message, append(logging.Attrs(ctx), args...)...)
}

// logInfo writes an informational line enriched with the context correlation
// fields.
func (p *Processor) logInfo(ctx context.Context, message string, args ...any) {
	p.logger.Info(message, append(logging.Attrs(ctx), args...)...)
}
