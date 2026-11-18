package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Guan0923/Praxis-RAG/internal/access"
	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/runtime"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// SearchRuntime runs one synchronous retrieval action.
type SearchRuntime interface {
	Search(ctx context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error)
}

// ArtifactRuntime reads expiring artifacts.
type ArtifactRuntime interface {
	GetArtifact(ctx context.Context, namespace, artifactID string, offset, length int) (map[string]any, error)
	DownloadArtifact(ctx context.Context, namespace, artifactID string) (content []byte, mimeType string, name string, err error)
}

// KnowledgeBaseRuntime exposes the public knowledge-base settings.
type KnowledgeBaseRuntime interface {
	PublicKnowledgeBase(ctx context.Context, namespace string) (map[string]any, error)
}

// Runtime is the HTTP-facing service runtime. It grows as endpoints are ported.
type Runtime interface {
	SearchRuntime
	ArtifactRuntime
	DocumentRuntime
	KnowledgeBaseRuntime
	UploadStatusRuntime
}

// WithRuntime wires the service runtime.
func WithRuntime(runtime Runtime) Option {
	return func(s *Server) { s.runtime = runtime }
}

// runtimeErrorStatus maps runtime errors to the Python HTTP status codes.
func runtimeErrorStatus(err error) (int, string) {
	var validation *contracts.SearchValidationError
	if errors.As(err, &validation) {
		return http.StatusBadRequest, validation.Message
	}
	var knowledgeValidation *apierrors.KnowledgeValidationError
	if errors.As(err, &knowledgeValidation) {
		return http.StatusBadRequest, knowledgeValidation.Message
	}
	var conflict *apierrors.UploadConflict
	if errors.As(err, &conflict) {
		return http.StatusConflict, conflict.Message
	}
	var knowledgeConflict *apierrors.KnowledgeConflictError
	if errors.As(err, &knowledgeConflict) {
		return http.StatusConflict, knowledgeConflict.Message
	}
	var unavailable *apierrors.SnapshotUnavailable
	if errors.As(err, &unavailable) {
		return http.StatusGone, unavailable.Message
	}
	var forbidden *apierrors.ArtifactForbiddenError
	if errors.As(err, &forbidden) {
		return http.StatusForbidden, forbidden.Message
	}
	var knowledge *apierrors.KnowledgeError
	if errors.As(err, &knowledge) {
		return http.StatusServiceUnavailable, knowledge.Message
	}
	var operation *contracts.RagOperationError
	if errors.As(err, &operation) {
		return http.StatusServiceUnavailable, operation.Message
	}
	// The display-query MySQL fallback budget is exhausted: the request was
	// rejected to protect the database, so REST answers the shared busy detail
	// on the existing error envelope instead of retrying anywhere else.
	var backpressure *state.ViewBackpressureError
	if errors.As(err, &backpressure) {
		return http.StatusServiceUnavailable, backpressure.Message
	}
	return http.StatusInternalServerError, "Internal Server Error"
}

func writeRuntimeError(w http.ResponseWriter, err error, noStore bool) {
	status, detail := runtimeErrorStatus(err)
	if noStore {
		w.Header().Set("Cache-Control", "no-store")
	}
	writeDetail(w, status, detail)
}

// requireRuntime reports a 503 when the runtime is not wired.
func (s *Server) requireRuntime(w http.ResponseWriter, runtime any) bool {
	if runtime != nil {
		return true
	}
	writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
	return false
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	// Resolved only for login sessions with ownership enforcement enabled; a
	// request without a scope keeps the historical unrestricted behaviour.
	scope, hasScope := access.ScopeFromContext(r.Context())
	if hasScope && !scope.AllowsAnyRead() {
		writeDetail(w, http.StatusForbidden, access.MessageKnowledgeAccess)
		return
	}
	var body searchBody
	if decoded, details := decodeSearchBody(r); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	} else {
		body = decoded
	}
	arguments, details := body.toArguments()
	if details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	request, err := contracts.SearchRequestFromArguments(arguments)
	if err != nil {
		writeRuntimeError(w, err, true)
		return
	}
	// A file-level grantee only retrieves the documents they may read: the
	// runtime filters results and snapshots by these ids (nil = unrestricted,
	// empty slice = nothing visible).
	if hasScope && scope.Restricted() {
		request.DocumentIDs = []string{}
		if s.access != nil {
			ids, resolveErr := s.access.ResolveDocumentIDs(r.Context(), namespace, scope.ReadDocumentNames())
			if resolveErr != nil {
				writeAccessError(w, resolveErr)
				return
			}
			if ids != nil {
				request.DocumentIDs = ids
			}
		}
	}
	payload, err := s.runtime.Search(r.Context(), namespace, request)
	if err != nil {
		writeRuntimeError(w, err, true)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// artifactDocumentGate builds the document-level read gate for the two artifact
// entries from the caller's resolved scope. It is nil for management
// credentials and whole-knowledge-base readers (historical unrestricted
// behaviour); a restricted scope may only read artifacts of documents it may
// read.
func artifactDocumentGate(r *http.Request) func(string) bool {
	scope, ok := access.ScopeFromContext(r.Context())
	if !ok || !scope.Restricted() {
		return nil
	}
	return scope.CanReadDocument
}

func (s *Server) handleGetArtifactContent(w http.ResponseWriter, r *http.Request) {
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
	artifactID, err := contracts.ValidateUUID(pathParam(r, "artifact_id"))
	if err != nil {
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	offset, details := intQuery(r, "offset", 0, 0, maxIntValue)
	if details != nil {
		writeValidation(w, http.StatusBadRequest, details)
		return
	}
	length, details := intQuery(r, "length", 65536, 1, contracts.MaxArtifactLength)
	if details != nil {
		writeValidation(w, http.StatusBadRequest, details)
		return
	}
	if err := contracts.ValidateArtifactRange(offset, length); err != nil {
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := runtime.WithDocumentReadGate(r.Context(), artifactDocumentGate(r))
	payload, err := s.runtime.GetArtifact(ctx, namespace, artifactID, offset, length)
	if err != nil {
		writeRuntimeError(w, err, true)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

const maxIntValue = int(^uint(0) >> 1)

// intQuery parses one bounded integer query parameter and returns FastAPI-like
// 400 validation details for the search-result/artifact prefixes.
func intQuery(r *http.Request, name string, fallback, minimum, maximum int) (int, []validationDetail) {
	values, present := r.URL.Query()[name]
	if !present {
		return fallback, nil
	}
	if len(values) != 1 {
		return 0, []validationDetail{{Type: "int_parsing", Loc: []any{"query", name}, Msg: "Input should be a valid integer"}}
	}
	raw := strings.TrimSpace(values[0])
	value, err := parseIntStrict(raw)
	if err != nil {
		return 0, []validationDetail{{Type: "int_parsing", Loc: []any{"query", name}, Msg: "Input should be a valid integer"}}
	}
	if value < minimum || value > maximum {
		return 0, []validationDetail{{
			Type: "greater_than_equal", Loc: []any{"query", name},
			Msg: "Input should be greater than or equal to " + itoa(minimum),
		}}
	}
	return value, nil
}

func (s *Server) handleDownloadArtifact(w http.ResponseWriter, r *http.Request) {
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
	artifactID, err := contracts.ValidateUUID(pathParam(r, "artifact_id"))
	if err != nil {
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := runtime.WithDocumentReadGate(r.Context(), artifactDocumentGate(r))
	content, mimeType, name, err := s.runtime.DownloadArtifact(ctx, namespace, artifactID)
	if err != nil {
		writeRuntimeError(w, err, true)
		return
	}
	disposition, safeType := artifactDisposition(name, mimeType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Type", safeType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}
