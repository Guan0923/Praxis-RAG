package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// documentBatchAdmissionConcurrency bounds the per-batch admission fan-out.
// Admissions for different names run truly in parallel (each one owns its own
// per-name lease row and transaction); same-name files still serialize on the
// shared lease row, which is exactly the correctness requirement.
const documentBatchAdmissionConcurrency = 6

// documentBatchAdmissionBudget bounds one batch admission fan-out even if the
// client disconnects: the bytes were already accepted over HTTP, so the batch
// keeps settling instead of being abandoned halfway.
const documentBatchAdmissionBudget = 5 * time.Minute

// SubmitDocumentBatch admits every file of one batch upload.
//
// The batch row is committed first (so the Console can poll it while the
// admissions run), then each file is admitted in its own transaction with a
// bounded goroutine pool, and finally every item result and the batch counters
// are committed. Every file is still a durable document_jobs row: the batch is
// a grouping and progress record, never a second copy of the file bytes.
func (s *Service) SubmitDocumentBatch(
	ctx context.Context,
	namespace string,
	files []UploadFile,
	idempotencyKey string,
	owner string,
) (map[string]any, error) {
	if s.stores == nil || s.stores.DocumentBatches == nil {
		return nil, &apierrors.KnowledgeError{Message: asyncSubmissionFailedMessage}
	}
	if s.stores.DocumentJobs == nil {
		return nil, &apierrors.KnowledgeError{Message: asyncSubmissionFailedMessage}
	}
	if err := s.validateUploads(files); err != nil {
		return nil, err
	}
	names := make([]string, len(files))
	for index, file := range files {
		names[index] = file.Name
	}
	var ownerPointer *string
	if owner != "" {
		ownerPointer = &owner
	}
	batch, reused, err := s.stores.DocumentBatches.Create(ctx, namespace, ownerPointer, idempotencyKey, names)
	if err != nil {
		return nil, asyncDocumentError(err)
	}
	if batch == nil {
		return nil, &apierrors.KnowledgeError{Message: asyncSubmissionFailedMessage}
	}
	if reused {
		// A retried request must show the original outcomes instead of
		// admitting the same files a second time.
		return documentBatchPayload(batch), nil
	}
	admissionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), documentBatchAdmissionBudget)
	defer cancel()
	results := make([]state.DocumentBatchItemResult, len(files))
	limit := documentBatchAdmissionConcurrency
	if limit > len(files) {
		limit = len(files)
	}
	var waitGroup sync.WaitGroup
	slots := make(chan struct{}, limit)
	for index := range files {
		waitGroup.Add(1)
		go func(position int) {
			defer waitGroup.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[position] = s.admitDocumentBatchFile(
				admissionCtx, namespace, position, files[position],
				documentBatchFileKey(idempotencyKey, position))
		}(index)
	}
	waitGroup.Wait()
	if err := s.stores.DocumentBatches.Finish(admissionCtx, namespace, batch.ID, results); err != nil {
		// The admissions are already durable; report the batch so the Console
		// can keep polling instead of losing every accepted artifact id.
		log.Printf("Document batch settle failed batch_id=%s error_type=%s",
			batch.ID, runtimeErrorTypeName(err))
		return documentBatchPayload(batch), nil
	}
	finished, err := s.stores.DocumentBatches.Get(admissionCtx, namespace, batch.ID)
	if err != nil || finished == nil {
		return documentBatchPayload(batch), nil
	}
	return documentBatchPayload(finished), nil
}

// GetDocumentBatch reads one batch as the Console progress projection. A batch
// owned by another account is reported as absent instead of leaking file names.
func (s *Service) GetDocumentBatch(ctx context.Context, namespace, batchID, viewer string) (map[string]any, error) {
	if s.stores == nil || s.stores.DocumentBatches == nil {
		return nil, &apierrors.KnowledgeError{Message: documentOperationUnavailableMessage}
	}
	batch, err := s.stores.DocumentBatches.Get(ctx, namespace, batchID)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: documentOperationUnavailableMessage}
	}
	if batch == nil {
		return nil, nil
	}
	if batch.Owner != nil && *batch.Owner != "" && viewer != "" && *batch.Owner != viewer {
		return nil, nil
	}
	return documentBatchPayload(batch), nil
}

// ListDocumentBatches returns the unfinished batches of one namespace; the
// Console restores its upload bubble from this after a reload.
func (s *Service) ListDocumentBatches(ctx context.Context, namespace, viewer string, limit int) (map[string]any, error) {
	if s.stores == nil || s.stores.DocumentBatches == nil {
		return nil, &apierrors.KnowledgeError{Message: documentOperationUnavailableMessage}
	}
	var owner *string
	if viewer != "" {
		owner = &viewer
	}
	batches, err := s.stores.DocumentBatches.ListActive(ctx, namespace, owner, limit)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: documentOperationUnavailableMessage}
	}
	payload := make([]map[string]any, 0, len(batches))
	for _, batch := range batches {
		// Only unfinished batches are restored: a batch whose jobs are all
		// terminal must not resurrect a "completed" chip after a reload.
		switch batch.Status {
		case state.DocumentBatchSaving, state.DocumentBatchProcessing, state.DocumentBatchInterrupted:
			payload = append(payload, documentBatchPayload(batch))
		}
	}
	return map[string]any{"batches": payload}, nil
}

// admitDocumentBatchFile admits one file and classifies the outcome into the
// three Console states: accepted, skipped (an existing/queued name) or failed
// (safe category + message).
func (s *Service) admitDocumentBatchFile(
	ctx context.Context,
	namespace string,
	position int,
	file UploadFile,
	idempotencyKey *string,
) state.DocumentBatchItemResult {
	result := state.DocumentBatchItemResult{Position: position}
	if err := s.validateRemotePDFAdmission(ctx, namespace, file.Name); err != nil {
		result.Status = state.DocumentBatchItemFailed
		result.Reason = state.DocumentBatchReasonConfig
		result.Error = batchFailureText(err)
		return result
	}
	admission, err := s.stores.DocumentJobs.Submit(ctx, namespace, file.Name, file.Content, idempotencyKey)
	if err != nil {
		return classifyBatchAdmissionFailure(position, err)
	}
	result.Status = state.DocumentBatchItemAccepted
	result.ArtifactID = admission.ArtifactID
	return result
}

// classifyBatchAdmissionFailure maps one admission error to the batch item
// categories without leaking raw internal errors to the Console.
func classifyBatchAdmissionFailure(position int, err error) state.DocumentBatchItemResult {
	result := state.DocumentBatchItemResult{Position: position}
	var exists *state.DocumentExistsConflict
	if errors.As(err, &exists) {
		result.Status = state.DocumentBatchItemSkipped
		result.Reason = state.DocumentBatchReasonExists
		return result
	}
	var queued *state.DocumentQueuedConflict
	if errors.As(err, &queued) {
		result.Status = state.DocumentBatchItemSkipped
		result.Reason = state.DocumentBatchReasonQueued
		return result
	}
	result.Status = state.DocumentBatchItemFailed
	var validation *state.DocumentValidationError
	if errors.As(err, &validation) {
		result.Reason = state.DocumentBatchReasonValidation
		result.Error = batchFailureText(validation)
		return result
	}
	var valueError *state.ValueError
	if errors.As(err, &valueError) {
		result.Reason = state.DocumentBatchReasonValidation
		result.Error = batchFailureText(valueError)
		return result
	}
	var knowledgeValidation *apierrors.KnowledgeValidationError
	if errors.As(err, &knowledgeValidation) {
		result.Reason = state.DocumentBatchReasonValidation
		result.Error = batchFailureText(knowledgeValidation)
		return result
	}
	var conflict *state.UploadConflict
	if errors.As(err, &conflict) {
		result.Reason = state.DocumentBatchReasonConflict
		result.Error = asyncUploadConflictMessage
		return result
	}
	result.Reason = state.DocumentBatchReasonInternal
	result.Error = asyncSubmissionFailedMessage
	return result
}

// documentBatchFileKey derives the per-file idempotency key of one batch,
// so a retried batch addresses the same logical files.
func documentBatchFileKey(batchKey string, position int) *string {
	if batchKey == "" {
		return nil
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", batchKey, position)))
	value := hex.EncodeToString(digest[:])
	return &value
}

// runtimeErrorTypeName names an error type without leaking its message.
func runtimeErrorTypeName(err error) string {
	if err == nil {
		return "none"
	}
	return fmt.Sprintf("%T", err)
}

// batchFailureText truncates a safe error message for one batch item.
func batchFailureText(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	runes := []rune(text)
	if len(runes) <= 500 {
		return text
	}
	return string(runes[:500])
}

// documentBatchPayload projects one batch for the Console.
func documentBatchPayload(batch *state.DocumentBatch) map[string]any {
	items := make([]map[string]any, 0, len(batch.Items))
	for _, item := range batch.Items {
		entry := map[string]any{
			"position":    item.Position,
			"name":        item.Name,
			"status":      item.Status,
			"reason":      nil,
			"artifact_id": nil,
			"error":       nil,
		}
		if item.Reason != nil {
			entry["reason"] = *item.Reason
		}
		if item.ArtifactID != nil {
			entry["artifact_id"] = *item.ArtifactID
		}
		if item.Error != nil {
			entry["error"] = *item.Error
		}
		if item.Job != nil {
			job := map[string]any{
				"status": item.Job.Status,
				"stage":  item.Job.Stage,
				"error":  nil,
			}
			if item.Job.Error != nil {
				job["error"] = *item.Job.Error
			}
			entry["job"] = job
		}
		items = append(items, entry)
	}
	return map[string]any{
		"batch_id":   batch.ID,
		"namespace":  batch.Namespace,
		"status":     batch.Status,
		"total":      batch.Total,
		"accepted":   batch.Accepted,
		"skipped":    batch.Skipped,
		"failed":     batch.Failed,
		"created_at": batch.CreatedAt,
		"items":      items,
	}
}
