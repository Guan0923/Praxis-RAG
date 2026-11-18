package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/document"
	"github.com/Guan0923/Praxis-RAG/internal/documentworker"
	"github.com/Guan0923/Praxis-RAG/internal/ingest"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

var (
	// The runtime is the production Ingester/JournalRecoverer of the document worker.
	_ documentworker.Ingester         = (*Service)(nil)
	_ documentworker.JournalRecoverer = (*Service)(nil)
)

// documentTaskIdempotencyPrefix mirrors process_document_task's
// f"document-task:{artifact_id}:{execution_id}" journal key.
const documentTaskIdempotencyPrefix = "document-task:"

// kbConfigurationChangedMessage mirrors
// service_knowledge.process_document_task's KnowledgeValidationError text. The
// worker replaces it with its own terminal display message.
const kbConfigurationChangedMessage = "Knowledge-base configuration changed; upload again"

// documentJobTerminalMessage is document_worker.py's terminal failure text.
const documentJobTerminalMessage = "Invalid document or changed knowledge-base configuration"

// ProcessDocumentJob is the Ingester seam of internal/documentworker: it indexes
// one leased async-admission job with the same storeAndIndex pipeline as a
// synchronous upload, but bound to the journal task (progress stages,
// ValidatePublication inside the publication transaction, then Succeed).
//
// The worker already claimed the lease, so job.Namespace/ArtifactID identify a
// row whose execution_id/owner fence this call. Returning nil without a
// committed publication would make the worker nack; every success path here has
// committed the journal (or replayed an existing commit).
func (s *Service) ProcessDocumentJob(ctx context.Context, job documentworker.Payload) (map[string]any, error) {
	if s.stores.DocumentJobs == nil {
		return nil, &apierrors.KnowledgeError{Message: documentOperationUnavailableMessage}
	}
	if strings.TrimSpace(job.ArtifactID) == "" || strings.TrimSpace(job.Namespace) == "" ||
		strings.TrimSpace(job.Name) == "" || len(job.Content) == 0 {
		return nil, &state.DocumentValidationError{Message: "Invalid document message"}
	}
	row, err := s.stores.DocumentJobs.Get(ctx, job.ArtifactID, &job.Namespace)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, &state.DocumentValidationError{Message: "Document upload not found"}
	}
	switch row.Status {
	case state.DocumentJobSucceeded:
		// Redelivery after a committed publication: report the terminal outcome
		// without touching Qdrant or the upload journal again.
		return documentJobPublicationResult(row), nil
	case state.DocumentJobFailed:
		message := documentJobTerminalMessage
		if row.Error != nil && *row.Error != "" {
			message = *row.Error
		}
		return nil, &state.DocumentValidationError{Message: message}
	}
	if row.Name != job.Name || row.SHA256 != job.SHA256 || row.Size != job.Size {
		// The identifier-only notification cannot disagree with the committed
		// request; a mismatch means the job row was corrupted or replaced.
		return nil, &state.DocumentValidationError{Message: documentJobTerminalMessage}
	}
	version, found, err := s.stores.KnowledgeBases.Version(ctx, job.Namespace)
	if err != nil {
		return nil, err
	}
	if !found || int64(version) != row.ConfigurationVersion {
		// Python compares the stored KB version before parsing; publication also
		// rechecks it inside the journal transaction (ValidatePublication).
		return nil, &apierrors.KnowledgeValidationError{Message: kbConfigurationChangedMessage}
	}
	if row.ExecutionID == nil || *row.ExecutionID == "" {
		return nil, &state.UploadConflict{Message: "Document execution is no longer current"}
	}
	task := &state.DocumentJobTask{DocumentJob: *row, Content: job.Content}

	if err := s.documentJobProgress(ctx, task, "parsing"); err != nil {
		return nil, documentJobIngestError(err)
	}
	resources, err := s.resolveUploadResources(ctx, job.Namespace)
	if err != nil {
		return nil, documentJobIngestError(err)
	}
	overwrite := false
	result, err := s.storeAndIndex(ctx, job.Namespace, resources, storeIndexOptions{
		Name:                   job.Name,
		Content:                job.Content,
		IdempotencyKey:         documentTaskIdempotencyPrefix + job.ArtifactID + ":" + *row.ExecutionID,
		DeduplicateAcrossNames: false,
		Overwrite:              &overwrite,
		EmbeddingModel:         resources.embeddingModel,
		Task:                   task,
	})
	if err != nil {
		return nil, documentJobIngestError(err)
	}
	// Reconcile the committed publication into the job row. Succeed is
	// first-terminal-wins/idempotent, so a redelivery after a lost ack is safe.
	if err := s.stores.DocumentJobs.Succeed(ctx, task); err != nil {
		return result, err
	}
	return result, nil
}

// RecoverDocumentUploads is the documentworker.JournalRecoverer seam: it runs
// UploadCoordinator recovery/cleanup across every namespace. Recovery failures
// are logged inside the coordinator and never block worker consumption; only
// setup failures (missing journal/coordinator) are returned.
func (s *Service) RecoverDocumentUploads(ctx context.Context) error {
	if s.stores == nil || s.stores.Uploads == nil {
		return fmt.Errorf("document upload journal recovery requires the shared state store")
	}
	coordinator, err := s.uploadCoordinator()
	if err != nil {
		return err
	}
	coordinator.Recover(ctx, s.indexStoreFactory, nil)
	return nil
}

// documentJobProgress records one ingest stage for a queued job. Nil tasks (the
// synchronous upload path) are a no-op.
func (s *Service) documentJobProgress(ctx context.Context, task *state.DocumentJobTask, stage string) error {
	if task == nil || s.stores.DocumentJobs == nil {
		return nil
	}
	return s.stores.DocumentJobs.Progress(ctx, task, stage)
}

// documentJobPublicationResult is the idempotent success payload of an already
// terminal job.
func documentJobPublicationResult(row *state.DocumentJob) map[string]any {
	result := map[string]any{
		"artifact_id": row.ArtifactID,
		"status":      row.Status,
	}
	if row.OperationID != nil {
		result["operation_id"] = *row.OperationID
	}
	if row.Error == nil {
		result["error"] = nil
	}
	return result
}

// documentJobIngestError classifies one ingest failure for the worker:
// knowledge/config-change problems become validation errors (terminal), while
// everything else keeps its original type (retryable). The worker renders the
// fixed messages ("Invalid document or changed knowledge-base configuration",
// "Document processing failed (<Type>)"), so this function only decides
// retryability and preserves the Python categories.
func documentJobIngestError(err error) error {
	if err == nil {
		return nil
	}
	inner := err
	var operationError *documentOperationError
	if errors.As(err, &operationError) {
		inner = operationError.Err
	}
	// Word and HTML are only importable through the Docling service (the
	// in-process OOXML parser was removed). If the URL was removed after the
	// job was admitted, retrying the same request cannot fix it: fail the job
	// as a terminal configuration/validation error instead of burning the
	// retry budget.
	if errors.Is(inner, ingest.ErrDoclingNotConfigured) {
		return &apierrors.KnowledgeValidationError{Message: doclingServiceRequiredMessage}
	}
	var conflict *state.UploadConflict
	if errors.As(inner, &conflict) {
		if conflict.Message == kbConfigurationChangedMessage {
			return &apierrors.KnowledgeValidationError{Message: conflict.Message}
		}
		// Leases/execution fences stay retryable.
		return inner
	}
	var knowledgeValidation *apierrors.KnowledgeValidationError
	if errors.As(inner, &knowledgeValidation) {
		return knowledgeValidation
	}
	// A malformed/encrypted/unsupported document cannot be fixed by parsing it
	// again; fail it now instead of burning retries. Transient read failures
	// (cancellation, temporary I/O) stay retryable.
	if errors.Is(inner, document.ErrInvalidDocument) {
		var readError *document.ReadError
		if errors.As(inner, &readError) {
			return &state.DocumentValidationError{Message: readError.Message}
		}
		return &state.DocumentValidationError{Message: inner.Error()}
	}
	// The parse service rejects malformed uploads (400), over-limit files (413)
	// and invalid options (422); retrying the same request cannot fix them.
	var parseError *document.PDFParseError
	if errors.As(inner, &parseError) {
		switch parseError.StatusCode {
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
			return &state.DocumentValidationError{Message: parseError.Message}
		}
	}
	var documentValidation *state.DocumentValidationError
	if errors.As(inner, &documentValidation) {
		return documentValidation
	}
	var quota *state.QuotaExceededError
	if errors.As(inner, &quota) {
		message := quota.Message
		if message == "" {
			message = "所有者的文档向量化额度不足，请等待额度刷新或升级套餐"
		}
		return &state.DocumentValidationError{Message: message}
	}
	var fileQuota *state.DocumentQuotaExceededError
	if errors.As(inner, &fileQuota) {
		return &state.DocumentValidationError{Message: fileQuota.Message}
	}
	return inner
}
