package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
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
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	documents, err := s.runtime.ListDocuments(r.Context(), namespace)
	if err != nil {
		writeRuntimeError(w, err, false)
		return
	}
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
	payload, err := s.runtime.Upload(r.Context(), namespace, files, idempotencyKey)
	if err != nil {
		writeRuntimeError(w, err, false)
		return
	}
	writeJSON(w, http.StatusOK, payload)
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
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
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
	writeJSON(w, http.StatusOK, payload)
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
