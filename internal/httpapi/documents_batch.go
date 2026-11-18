package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"

	"github.com/Guan0923/Praxis-RAG/internal/access"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// documentBatchListLimit bounds the active-batch restore query.
const documentBatchListLimit = 20

// documentBatchEventItem is the subset of one admitted-batch payload item the
// upload SSE stream needs: the per-file outcome the Console renders as a row.
type documentBatchEventItem struct {
	Position int     `json:"position"`
	Status   string  `json:"status"`
	Reason   *string `json:"reason"`
	Error    *string `json:"error"`
}

// handleCreateDocumentBatch admits a batch of files in one durable batch
// record. Each file has its own document job, while the batch provides
// per-file progress and a stable id the Console can poll. It answers 202 once the
// batch row and every admission have committed.
//
// An optional X-Upload-ID header names a stream opened on
// GET /v1/document-batches/upload-events/{upload_id}: the handler then
// publishes per-file transfer progress while reading the body and one save
// result per file before the terminal done frame. Without the header the
// response and side effects are exactly the historical batch contract.
func (s *Server) handleCreateDocumentBatch(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	// The upload id names the progress stream and never reaches the runtime.
	// Rejecting it here refuses a malformed request before the body is read.
	uploadID := r.Header.Get(uploadIDHeader)
	if uploadID != "" && !validUploadID(uploadID) {
		writeDetail(w, http.StatusBadRequest, "Invalid upload id.")
		return
	}
	eventKey := ""
	if uploadID != "" {
		eventKey = uploadEventKey(namespace, uploadID)
	}
	publishDone := func(batchID, detail *string) {
		if eventKey == "" {
			return
		}
		s.uploadEvents.publishDone(eventKey, batchID, detail)
	}

	idempotencyKey, problem := idempotencyHeader(r)
	if problem != nil {
		publishDone(nil, uploadProblemEventDetail(&uploadProblem{validation: problem}))
		writeValidation(w, http.StatusUnprocessableEntity, problem)
		return
	}
	var onProgress func(index int, received int64)
	if eventKey != "" {
		onProgress = func(index int, received int64) {
			s.uploadEvents.publishProgress(eventKey, index, received)
		}
	}
	files, uploadProblem := readMultipartFilesWithProgress(r, onProgress)
	if uploadProblem != nil {
		publishDone(nil, uploadProblemEventDetail(uploadProblem))
		writeUploadProblem(w, uploadProblem)
		return
	}
	if uploadProblem := validateUploadBatch(files); uploadProblem != nil {
		publishDone(nil, uploadProblemEventDetail(uploadProblem))
		writeUploadProblem(w, uploadProblem)
		return
	}
	if !s.requireScopeWriteBatch(w, r, files) {
		detail := access.MessageWriteRequired
		publishDone(nil, &detail)
		return
	}
	payload, err := s.runtime.SubmitDocumentBatch(
		r.Context(), namespace, files, idempotencyKey, sessionUserID(r))
	if err != nil {
		_, detail := runtimeErrorStatus(err)
		publishDone(nil, &detail)
		writeRuntimeError(w, err, false)
		return
	}
	if eventKey != "" {
		s.publishDocumentBatchOutcome(eventKey, payload)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, payload)
}

// writeUploadProblem mirrors the FastAPI-shaped rejection already returned by
// the upload endpoints.
func writeUploadProblem(w http.ResponseWriter, problem *uploadProblem) {
	if problem.validation != nil {
		writeValidation(w, problem.status, problem.validation)
		return
	}
	writeDetail(w, problem.status, problem.detail)
}

// uploadProblemEventDetail derives the safe done-frame text for one rejected
// upload: the same detail the HTTP response carries, never an internal error.
func uploadProblemEventDetail(problem *uploadProblem) *string {
	if problem == nil {
		return nil
	}
	if problem.detail != "" {
		return &problem.detail
	}
	for _, validation := range problem.validation {
		if validation.Msg != "" {
			message := validation.Msg
			return &message
		}
	}
	return nil
}

// publishDocumentBatchOutcome pushes one result frame per admitted file, in
// payload position order, then the terminal done frame carrying the batch id.
func (s *Server) publishDocumentBatchOutcome(eventKey string, payload map[string]any) {
	batchID, items := documentBatchEventItems(payload)
	sort.SliceStable(items, func(left, right int) bool {
		return items[left].Position < items[right].Position
	})
	for _, item := range items {
		status, ok := uploadResultStatus(item.Status)
		if !ok {
			// Unknown admission outcomes stay out of the stream; the batch
			// response is the authoritative record the Console reconciles with.
			continue
		}
		s.uploadEvents.publishResult(eventKey, item.Position, status, item.Reason, item.Error)
	}
	if batchID == "" {
		s.uploadEvents.publishDone(eventKey, nil, nil)
		return
	}
	s.uploadEvents.publishDone(eventKey, &batchID, nil)
}

// documentBatchEventItems decodes only the batch id and per-file outcomes from
// the runtime payload, which is a map[string]any by contract.
func documentBatchEventItems(payload map[string]any) (string, []documentBatchEventItem) {
	if payload == nil {
		return "", nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", nil
	}
	var decoded struct {
		BatchID string                   `json:"batch_id"`
		Items   []documentBatchEventItem `json:"items"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return "", nil
	}
	return decoded.BatchID, decoded.Items
}

// uploadResultStatus maps a durable batch item status onto the SSE result
// status the Console renders.
func uploadResultStatus(status string) (string, bool) {
	switch status {
	case state.DocumentBatchItemAccepted:
		return "saved", true
	case state.DocumentBatchItemSkipped:
		return "skipped", true
	case state.DocumentBatchItemFailed:
		return "failed", true
	default:
		return "", false
	}
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
