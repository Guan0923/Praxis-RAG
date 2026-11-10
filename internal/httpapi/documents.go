package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/config"
)

// Document limits mirror service_knowledge constants.
const (
	MaxFilesPerBatch = 10
	MaxFileBytes     = 10 * 1024 * 1024
)

// SupportedExtensions is the accepted upload suffix set.
var SupportedExtensions = []string{".doc", ".docx", ".md", ".pdf", ".txt"}

// UploadFile is one client-supplied document.
type UploadFile struct {
	Name    string
	Content []byte
}

// CheckFile is one deduplication probe.
type CheckFile struct {
	Name   string
	SHA256 string
}

// DocumentRuntime manages knowledge documents and reads their content.
type DocumentRuntime interface {
	ListDocuments(ctx context.Context, namespace string) ([]map[string]any, error)
	Upload(ctx context.Context, namespace string, files []UploadFile, idempotencyKey string) (map[string]any, error)
	// AdmitDocument durably admits one file for asynchronous indexing and
	// returns {artifact_id, status, reused}. It is the async counterpart of
	// Upload: it commits MySQL state and returns before parsing starts.
	AdmitDocument(ctx context.Context, namespace, name string, content []byte, idempotencyKey string) (map[string]any, error)
	// GetDocumentStatus reads one async admission's {artifact_id, status,
	// stage, error} projection.
	GetDocumentStatus(ctx context.Context, namespace, artifactID string) (map[string]any, error)
	Delete(ctx context.Context, namespace, name string) (map[string]any, error)
	CheckDocuments(ctx context.Context, namespace string, files []CheckFile) (map[string]any, error)
	ReadDocument(ctx context.Context, namespace, name string) (text string, version string, err error)
	ReadDocumentFile(ctx context.Context, namespace, name string) ([]byte, error)
}

func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	if _, ok := s.requireScopeRead(w, r); !ok {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	documents, err := s.runtime.ListDocuments(r.Context(), namespace)
	if err != nil {
		writeRuntimeError(w, err, false)
		return
	}
	// A file-level reader only ever sees the documents explicitly granted to it.
	documents = scopedDocuments(r, documents)
	extensions := append([]string(nil), SupportedExtensions...)
	sort.Strings(extensions)
	writeJSON(w, http.StatusOK, map[string]any{
		"documents":            documents,
		"supported_extensions": extensions,
		"max_files_per_batch":  MaxFilesPerBatch,
		"max_file_bytes":       MaxFileBytes,
	})
}

func (s *Server) handleUploadDocuments(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	idempotencyKey, details := idempotencyHeader(r)
	if details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	files, problem := readMultipartFiles(r)
	if problem != nil {
		if problem.validation != nil {
			writeValidation(w, problem.status, problem.validation)
			return
		}
		writeDetail(w, problem.status, problem.detail)
		return
	}
	// A file-level writer may only replace documents it holds write access to;
	// a whole-knowledge-base writer may upload any file name.
	if !s.requireScopeWriteBatch(w, r, files) {
		return
	}
	if s.uploadMode() == config.UploadModeAsync {
		s.handleUploadDocumentsAsync(w, r, namespace, files, idempotencyKey)
		return
	}
	var before []map[string]any
	if s.docNotifier != nil {
		before, _ = s.runtime.ListDocuments(r.Context(), namespace)
	}
	payload, err := s.runtime.Upload(r.Context(), namespace, files, idempotencyKey)
	if err != nil {
		writeRuntimeError(w, err, false)
		return
	}
	if s.docNotifier != nil {
		s.docNotifier.DocumentsChanged(namespace, before, payloadDocuments(payload))
	}
	payload["mode"] = config.UploadModeSync
	writeJSON(w, http.StatusOK, payload)
}

// uploadMode reports the configured POST /v1/documents admission mode. Async is
// the default: only an explicit "sync" setting keeps the legacy in-request parse
// behaviour, so an empty or unrecognised setting stays asynchronous.
func (s *Server) uploadMode() string {
	if strings.EqualFold(strings.TrimSpace(s.settings.UploadMode), config.UploadModeSync) {
		return config.UploadModeSync
	}
	return config.UploadModeAsync
}

// asyncUploadJob is one admitted file in the async upload response. The Console
// polls GET /v1/document-jobs/{artifact_id} for the job's stage and outcome.
type asyncUploadJob struct {
	Name       string `json:"name"`
	ArtifactID string `json:"artifact_id"`
	Status     string `json:"status"`
	Reused     bool   `json:"reused"`
}

// handleUploadDocumentsAsync admits every file through the durable document job
// queue and answers with artifact ids instead of waiting for parsing/embedding.
// Per-file admission failures are isolated into "failed", exactly like the
// synchronous batch response, so one bad file never rejects the whole batch.
func (s *Server) handleUploadDocumentsAsync(
	w http.ResponseWriter,
	r *http.Request,
	namespace string,
	files []UploadFile,
	idempotencyKey string,
) {
	if problem := validateUploadBatch(files); problem != nil {
		if problem.validation != nil {
			writeValidation(w, problem.status, problem.validation)
			return
		}
		writeDetail(w, problem.status, problem.detail)
		return
	}
	jobs := make([]asyncUploadJob, 0, len(files))
	failed := make([]map[string]any, 0)
	for position, file := range files {
		requestKey := uploadFileIdempotencyKey(idempotencyKey, position)
		payload, err := s.runtime.AdmitDocument(r.Context(), namespace, file.Name, file.Content, requestKey)
		if err != nil {
			failed = append(failed, map[string]any{
				"name": file.Name, "error": uploadAdmissionErrorText(err),
			})
			continue
		}
		job := asyncUploadJob{Name: file.Name}
		if artifactID, ok := payload["artifact_id"].(string); ok {
			job.ArtifactID = artifactID
		}
		if status, ok := payload["status"].(string); ok {
			job.Status = status
		}
		if reused, ok := payload["reused"].(bool); ok {
			job.Reused = reused
		}
		jobs = append(jobs, job)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":   config.UploadModeAsync,
		"jobs":   jobs,
		"failed": failed,
	})
}

// validateUploadBatch mirrors the runtime's synchronous pre-validation so an
// async batch rejects unsupported input up front instead of queueing jobs that
// can only fail later.
func validateUploadBatch(files []UploadFile) *uploadProblem {
	if len(files) > MaxFilesPerBatch {
		return &uploadProblem{
			status: http.StatusUnprocessableEntity,
			validation: []validationDetail{{
				Type: "too_long", Loc: []any{"body", "files"},
				Msg: "List should have at most 10 items after validation",
			}},
		}
	}
	for _, file := range files {
		if !supportedExtension(strings.ToLower(filepath.Ext(file.Name))) {
			return &uploadProblem{status: http.StatusBadRequest, detail: unsupportedFileTypeMessage(file.Name)}
		}
	}
	return nil
}

// uploadFileIdempotencyKey derives the per-file idempotency key of one batch,
// matching the synchronous runtime derivation sha256("<header>:<position>") so
// the same batch retried under either mode addresses the same logical file.
func uploadFileIdempotencyKey(batchKey string, position int) string {
	if batchKey == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", batchKey, position)))
	return hex.EncodeToString(digest[:])
}

// uploadAdmissionErrorText renders one per-file admission failure with the same
// safety rules as the synchronous response: known validation/service messages
// are truncated, everything else is reported by a generic category.
func uploadAdmissionErrorText(err error) string {
	var validation *apierrors.KnowledgeValidationError
	if errors.As(err, &validation) {
		return truncateDetailText(validation.Message, 500)
	}
	var knowledge *apierrors.KnowledgeError
	if errors.As(err, &knowledge) {
		return truncateDetailText(knowledge.Message, 500)
	}
	return "上传失败"
}

func truncateDetailText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func idempotencyHeader(r *http.Request) (string, []validationDetail) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) == 0 {
		return "", nil
	}
	value := values[0]
	if len(value) < 1 || len(value) > 128 {
		return "", []validationDetail{{
			Type: "string_too_long", Loc: []any{"header", "idempotency-key"},
			Msg: "String should have at most 128 characters",
		}}
	}
	return value, nil
}

// uploadProblem describes why a multipart upload was rejected.
type uploadProblem struct {
	status     int
	detail     string
	validation []validationDetail
}

// readMultipartFiles streams the multipart body, rejecting non-file fields and
// oversized files before they reach the runtime.
func readMultipartFiles(r *http.Request) ([]UploadFile, *uploadProblem) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, &uploadProblem{status: http.StatusBadRequest, detail: "There was an error parsing the body"}
	}
	files := make([]UploadFile, 0, 4)
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nil, &uploadProblem{status: http.StatusBadRequest, detail: "There was an error parsing the body"}
		}
		name := part.FormName()
		filename := part.FileName()
		if name != "files" || filename == "" {
			_ = part.Close()
			return nil, &uploadProblem{
				status: http.StatusBadRequest,
				detail: "上传仅接受文件，切分和向量参数由知识库配置决定",
			}
		}
		content, readErr := io.ReadAll(io.LimitReader(part, MaxFileBytes+1))
		_ = part.Close()
		if readErr != nil {
			return nil, &uploadProblem{status: http.StatusBadRequest, detail: "Could not read the uploaded file"}
		}
		if len(content) > MaxFileBytes {
			return nil, &uploadProblem{
				status: http.StatusBadRequest,
				detail: "单个知识文件不能超过 10 MiB：" + filename,
			}
		}
		files = append(files, UploadFile{Name: filename, Content: content})
	}
	if len(files) == 0 {
		return nil, &uploadProblem{
			status:     http.StatusUnprocessableEntity,
			validation: []validationDetail{{Type: "missing", Loc: []any{"body", "files"}, Msg: "Field required"}},
		}
	}
	return files, nil
}

func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	if !s.requireScopeWriteDocument(w, r, pathParam(r, "name")) {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	var before []map[string]any
	if s.docNotifier != nil {
		before, _ = s.runtime.ListDocuments(r.Context(), namespace)
	}
	payload, err := s.runtime.Delete(r.Context(), namespace, pathParam(r, "name"))
	if err != nil {
		var validation *apierrors.KnowledgeValidationError
		if errors.As(err, &validation) {
			writeDetail(w, http.StatusNotFound, validation.Message)
			return
		}
		writeRuntimeError(w, err, false)
		return
	}
	if s.docNotifier != nil {
		s.docNotifier.DocumentsChanged(namespace, before, payloadDocuments(payload))
	}
	writeJSON(w, http.StatusOK, payload)
}

// payloadDocuments extracts the post-write document list from a runtime
// payload (Python returns it for MCP change notifications).
func payloadDocuments(payload map[string]any) []map[string]any {
	raw, ok := payload["documents"]
	if !ok {
		return nil
	}
	switch typed := raw.(type) {
	case []map[string]any:
		return typed
	case []any:
		documents := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if document, ok := item.(map[string]any); ok {
				documents = append(documents, document)
			}
		}
		return documents
	default:
		return nil
	}
}

func (s *Server) handleDocumentContent(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	name := pathParam(r, "name")
	if !s.requireScopeReadDocument(w, r, name) {
		return
	}
	writeNoStore := func() { w.Header().Set("Cache-Control", "no-store") }
	if strings.HasSuffix(strings.ToLower(name), ".pdf") {
		content, err := s.runtime.ReadDocumentFile(r.Context(), namespace, name)
		if err != nil {
			writeDocumentReadError(w, err)
			return
		}
		writeNoStore()
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
		return
	}
	text, _, err := s.runtime.ReadDocument(r.Context(), namespace, name)
	if err != nil {
		writeDocumentReadError(w, err)
		return
	}
	writeNoStore()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, text)
}

func writeDocumentReadError(w http.ResponseWriter, err error) {
	var validation *apierrors.KnowledgeValidationError
	if errors.As(err, &validation) {
		writeDetail(w, http.StatusNotFound, validation.Message)
		return
	}
	writeRuntimeError(w, err, false)
}

// checkDocumentsBody mirrors http_service.CheckDocumentsBody (extra=forbid).
type checkDocumentsBody struct {
	Files []checkFileBody `json:"files"`
}

type checkFileBody struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

func (s *Server) handleCheckDocuments(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	// The check route reveals whether documents already exist, so it follows the
	// same read visibility rule as GET /v1/documents. Management credentials and
	// servers without ownership enforcement keep their historical behaviour.
	if _, ok := s.requireScopeRead(w, r); !ok {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	var body checkDocumentsBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	details := validateCheckFiles(body.Files)
	if details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	for _, item := range body.Files {
		suffix := strings.ToLower(filepath.Ext(item.Name))
		if !supportedExtension(suffix) {
			// Python raises KnowledgeValidationError, which the check route
			// renders as a 400 detail string.
			writeDetail(w, http.StatusBadRequest, unsupportedFileTypeMessage(item.Name))
			return
		}
	}
	files := make([]CheckFile, 0, len(body.Files))
	for _, item := range body.Files {
		files = append(files, CheckFile{Name: item.Name, SHA256: item.SHA256})
	}
	payload, err := s.runtime.CheckDocuments(r.Context(), namespace, files)
	if err != nil {
		writeRuntimeError(w, err, false)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func validateCheckFiles(files []checkFileBody) []validationDetail {
	if len(files) == 0 {
		return []validationDetail{{Type: "missing", Loc: []any{"body", "files"}, Msg: "Field required"}}
	}
	if len(files) > MaxFilesPerBatch {
		return []validationDetail{{
			Type: "too_long", Loc: []any{"body", "files"},
			Msg: "List should have at most 10 items after validation",
		}}
	}
	for index, file := range files {
		if len(file.Name) < 1 || len(file.Name) > 255 {
			return []validationDetail{{
				Type: "string_too_long", Loc: []any{"body", "files", index, "name"},
				Msg: "String should have at most 255 characters",
			}}
		}
		if !isSHA256(file.SHA256) {
			return []validationDetail{{
				Type: "string_pattern_mismatch", Loc: []any{"body", "files", index, "sha256"},
				Msg: "String should match pattern '^[a-f0-9]{64}$'",
			}}
		}
	}
	return nil
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

var _ = json.Marshal

// unsupportedFileTypeMessage mirrors runtime.UnsupportedFileTypeMessage /
// service_knowledge.validate_uploads. The runtime validates the same rule for
// direct callers; this handler avoids duplicating the exact literal in two
// places by building it here.
func unsupportedFileTypeMessage(name string) string {
	extensions := append([]string(nil), SupportedExtensions...)
	sort.Strings(extensions)
	return "暂不支持该文件类型：" + name + "；请选择 " + strings.Join(extensions, "、") + "。"
}

// supportedExtension reports whether suffix is one of the accepted upload types.
func supportedExtension(suffix string) bool {
	for _, extension := range SupportedExtensions {
		if extension == suffix {
			return true
		}
	}
	return false
}
