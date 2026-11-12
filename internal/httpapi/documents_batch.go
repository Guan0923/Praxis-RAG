package httpapi

import (
	"net/http"
	"strconv"
)

// documentBatchListLimit bounds the active-batch restore query.
const documentBatchListLimit = 20

// handleCreateDocumentBatch admits a batch of files in one durable batch
// record. Every file is still admitted into the document job queue exactly as
// POST /v1/documents does; the batch only adds per-file progress and a stable
// batch id the Console can poll, so the endpoint answers 202 as soon as the
// batch row and every admission have committed.
func (s *Server) handleCreateDocumentBatch(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	idempotencyKey, problem := idempotencyHeader(r)
	if problem != nil {
		writeValidation(w, http.StatusUnprocessableEntity, problem)
		return
	}
	files, uploadProblem := readMultipartFiles(r)
	if uploadProblem != nil {
		if uploadProblem.validation != nil {
			writeValidation(w, uploadProblem.status, uploadProblem.validation)
			return
		}
		writeDetail(w, uploadProblem.status, uploadProblem.detail)
		return
	}
	if uploadProblem := validateUploadBatch(files); uploadProblem != nil {
		if uploadProblem.validation != nil {
			writeValidation(w, uploadProblem.status, uploadProblem.validation)
			return
		}
		writeDetail(w, uploadProblem.status, uploadProblem.detail)
		return
	}
	if !s.requireScopeWriteBatch(w, r, files) {
		return
	}
	payload, err := s.runtime.SubmitDocumentBatch(
		r.Context(), namespace, files, idempotencyKey, sessionUserID(r))
	if err != nil {
		writeRuntimeError(w, err, false)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, payload)
}

// handleDocumentBatchStatus returns one batch with every item outcome and the
// accepted items' document job stage, so a single poll drives the Console
// progress bubble.
func (s *Server) handleDocumentBatchStatus(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	if _, ok := s.requireScopeRead(w, r); !ok {
		return
	}
	batchID := pathParam(r, "batch_id")
	if batchID == "" {
		writeDetail(w, http.StatusNotFound, "Document batch not found")
		return
	}
	payload, err := s.runtime.GetDocumentBatch(r.Context(), namespace, batchID, sessionUserID(r))
	if err != nil {
		writeRuntimeError(w, err, false)
		return
	}
	if payload == nil {
		writeDetail(w, http.StatusNotFound, "Document batch not found")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, payload)
}

// handleListDocumentBatches lists the caller's unfinished batches. The Console
// uses it to restore the upload bubble after a reload or a workspace switch.
func (s *Server) handleListDocumentBatches(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	if _, ok := s.requireScopeRead(w, r); !ok {
		return
	}
	limit := documentBatchListLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 100 {
			writeDetail(w, http.StatusUnprocessableEntity, "limit must be an integer between 1 and 100")
			return
		}
		limit = parsed
	}
	payload, err := s.runtime.ListDocumentBatches(r.Context(), namespace, sessionUserID(r), limit)
	if err != nil {
		writeRuntimeError(w, err, false)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, payload)
}

// sessionUserID returns the signed-in Console account id, or "" for the legacy
// API-key surface where every caller shares the namespace.
func sessionUserID(r *http.Request) string {
	session, ok := sessionUserFromContext(r.Context())
	if !ok || session == nil {
		return ""
	}
	return session.User.ID
}
