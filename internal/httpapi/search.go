package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
)

// SearchRuntime runs one synchronous retrieval action.
type SearchRuntime interface {
	Search(ctx context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error)
}

// ResultRuntime reads stored search-result snapshots.
type ResultRuntime interface {
	GetSearchResult(ctx context.Context, namespace, resultID string, page, pageSize int) (map[string]any, error)
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
	ResultRuntime
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
	var unavailable *apierrors.SnapshotUnavailable
	if errors.As(err, &unavailable) {
		return http.StatusGone, unavailable.Message
	}
	var knowledge *apierrors.KnowledgeError
	if errors.As(err, &knowledge) {
		return http.StatusServiceUnavailable, knowledge.Message
	}
	var operation *contracts.RagOperationError
	if errors.As(err, &operation) {
		return http.StatusServiceUnavailable, operation.Message
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
	payload, err := s.runtime.Search(r.Context(), namespace, request)
	if err != nil {
		writeRuntimeError(w, err, true)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleGetSearchResult(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	resultID, err := contracts.ValidateUUID(pathParam(r, "result_id"))
	if err != nil {
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	page, pageSize, details := resultPageQuery(r)
	if details != nil {
		writeValidation(w, http.StatusBadRequest, details)
		return
	}
	payload, err := s.runtime.GetSearchResult(r.Context(), namespace, resultID, page, pageSize)
	if err != nil {
		writeRuntimeError(w, err, true)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func resultPageQuery(r *http.Request) (int, int, []validationDetail) {
	page, details := intQuery(r, "page", 1, 1, contracts.MaxResultPage)
	if details != nil {
		return 0, 0, details
	}
	pageSize, details := intQuery(r, "page_size", 10, 1, 100)
	if details != nil {
		return 0, 0, details
	}
	return page, pageSize, nil
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
	payload, err := s.runtime.GetArtifact(r.Context(), namespace, artifactID, offset, length)
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
	artifactID, err := contracts.ValidateUUID(pathParam(r, "artifact_id"))
	if err != nil {
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	content, mimeType, name, err := s.runtime.DownloadArtifact(r.Context(), namespace, artifactID)
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
