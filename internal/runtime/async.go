package runtime

import (
	"context"
	"errors"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// asyncUploadConflictMessage mirrors rag_mcp.mcp_http.mutate_document's safe
// UploadConflict rendering for add_document.
const asyncUploadConflictMessage = "Document conflicts with an existing filename, queued upload or idempotency key."

// asyncSubmissionFailedMessage mirrors service_knowledge.add_document's generic
// safe failure message.
const asyncSubmissionFailedMessage = "Document submission failed; retry with the same idempotency_key"

// documentOperationUnavailableMessage mirrors rag_mcp.mcp_http.mutate_document's
// generic safe failure message for status lookups.
const documentOperationUnavailableMessage = "Document operation unavailable"

// admitDocument durably submits one upload and maps admission failures to the
// runtime's safe public categories. Callers decide how much of the admission
// projection to expose: MCP returns artifact_id only, REST also reports the
// queued status and the idempotent-reuse flag.
func (s *Service) admitDocument(
	ctx context.Context,
	namespace, name string,
	content []byte,
	idempotencyKey string,
) (state.DocumentAdmission, error) {
	dispatcher := s.stores.DocumentDispatcher
	if dispatcher == nil {
		return state.DocumentAdmission{}, &apierrors.KnowledgeError{Message: asyncSubmissionFailedMessage}
	}
	var key *string
	if idempotencyKey != "" {
		key = &idempotencyKey
	}
	admission, err := dispatcher.Submit(ctx, namespace, name, content, key)
	if err != nil {
		return state.DocumentAdmission{}, asyncDocumentError(err)
	}
	return admission, nil
}

// addDocumentAsync is the real implementation behind Service.AddDocument:
// durable asynchronous admission (job + outbox in one transaction) committed
// before any broker delivery is attempted.
func (s *Service) addDocumentAsync(
	ctx context.Context,
	namespace, name string,
	content []byte,
	idempotencyKey string,
) (map[string]any, error) {
	admission, err := s.admitDocument(ctx, namespace, name, content, idempotencyKey)
	if err != nil {
		return nil, err
	}
	// Python's DocumentJobDispatcher.submit returns exactly {"artifact_id": ...}.
	return map[string]any{"artifact_id": admission.ArtifactID}, nil
}

// AdmitDocument is the REST projection of the same durable admission. The
// Console renders the admitted jobs immediately, so it also gets the queued
// status and the "same idempotency key reused" flag: a resent batch must show
// the original artifact ids instead of looking like a fresh submission.
func (s *Service) AdmitDocument(
	ctx context.Context,
	namespace, name string,
	content []byte,
	idempotencyKey string,
) (map[string]any, error) {
	admission, err := s.admitDocument(ctx, namespace, name, content, idempotencyKey)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"artifact_id": admission.ArtifactID,
		"status":      admission.Status,
		"reused":      admission.Reused,
	}, nil
}

// asyncDocumentError maps state admission errors to the Python runtime
// categories: validation/configuration problems are 400-style validation
// errors, name/idempotency collisions keep the MCP-visible conflict text, and
// everything else is a safe 503.
func asyncDocumentError(err error) error {
	if err == nil {
		return nil
	}
	var validation *state.DocumentValidationError
	if errors.As(err, &validation) {
		return &apierrors.KnowledgeValidationError{Message: validation.Message}
	}
	var valueError *state.ValueError
	if errors.As(err, &valueError) {
		return &apierrors.KnowledgeValidationError{Message: valueError.Message}
	}
	var conflict *state.UploadConflict
	if errors.As(err, &conflict) {
		return &apierrors.KnowledgeValidationError{Message: asyncUploadConflictMessage}
	}
	var lookup *state.LookupError
	if errors.As(err, &lookup) {
		return &apierrors.KnowledgeValidationError{Message: lookup.Message}
	}
	var knowledgeValidation *apierrors.KnowledgeValidationError
	if errors.As(err, &knowledgeValidation) {
		return knowledgeValidation
	}
	var knowledge *apierrors.KnowledgeError
	if errors.As(err, &knowledge) {
		return knowledge
	}
	return &apierrors.KnowledgeError{Message: asyncSubmissionFailedMessage}
}

// documentStatus is the real implementation behind Service.GetDocumentStatus:
// the display-only projection {artifact_id, status, stage, error} backed by
// state.DocumentJobs.Status.
func (s *Service) documentStatus(ctx context.Context, namespace, artifactID string) (map[string]any, error) {
	if s.stores.DocumentJobs == nil {
		return nil, &apierrors.KnowledgeError{Message: documentOperationUnavailableMessage}
	}
	status, err := s.stores.DocumentJobs.Status(ctx, namespace, artifactID)
	if err != nil {
		if mapped := documentStatusError(err); mapped != nil {
			return nil, mapped
		}
		return nil, &apierrors.KnowledgeError{Message: documentOperationUnavailableMessage}
	}
	payload, marshalErr := marshalToMap(status)
	if marshalErr != nil {
		return nil, &apierrors.KnowledgeError{Message: documentOperationUnavailableMessage}
	}
	return payload, nil
}

// documentStatusError maps LookupError/ValueError to validation errors exactly
// like service_knowledge.get_document_status; other failures return nil so the
// caller uses its safe generic error.
func documentStatusError(err error) error {
	var validation *state.DocumentValidationError
	if errors.As(err, &validation) {
		return &apierrors.KnowledgeValidationError{Message: validation.Message}
	}
	var valueError *state.ValueError
	if errors.As(err, &valueError) {
		return &apierrors.KnowledgeValidationError{Message: valueError.Message}
	}
	var lookup *state.LookupError
	if errors.As(err, &lookup) {
		return &apierrors.KnowledgeValidationError{Message: lookup.Message}
	}
	return nil
}
