package httpapi

import (
	"errors"
	"net/http"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
)

// handleDocumentJobStatus serves one asynchronous admission status. The
// artifact id is returned on accepted items from POST /v1/document-batches
// and by MCP add_document. It stays readable until the job's terminal retention
// window expires.
//
// Status values: queued, processing (with a stage such as parsing, chunking,
// embedding, indexing or publishing), succeeded and failed. Failed jobs carry
// the durable terminal error text.
func (s *Server) handleDocumentJobStatus(w http.ResponseWriter, r *http.Request) {
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
	payload, err := s.runtime.GetDocumentStatus(r.Context(), namespace, pathParam(r, "artifact_id"))
	if err != nil {
		// Invalid, unknown and other-tenant artifact ids are indistinguishable to
		// the caller: a 404 keeps them from probing other namespaces.
		var validation *apierrors.KnowledgeValidationError
		if errors.As(err, &validation) {
			writeDetail(w, http.StatusNotFound, validation.Message)
			return
		}
		writeRuntimeError(w, err, true)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.record(r, "read_job", namespace, pathParam(r, "artifact_id"), "success")
	writeJSON(w, http.StatusOK, payload)
}
