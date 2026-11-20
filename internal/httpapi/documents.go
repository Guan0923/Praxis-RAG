package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
)

// Document limits mirror service_knowledge constants.
const (
	MaxFilesPerBatch = 10
	MaxFileBytes     = 10 * 1024 * 1024
)

// SupportedExtensions is the accepted upload suffix set.
var SupportedExtensions = []string{".doc", ".docx", ".htm", ".html", ".md", ".pdf", ".txt"}

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
	// GetDocumentStatus reads one async admission's {artifact_id, status,
	// stage, error} projection.
	GetDocumentStatus(ctx context.Context, namespace, artifactID string) (map[string]any, error)
	// SubmitDocumentBatch admits every file of one batch upload and returns the
	// durable batch record with per-file outcomes.
	SubmitDocumentBatch(ctx context.Context, namespace string, files []UploadFile, idempotencyKey, owner string) (map[string]any, error)
	// GetDocumentBatch reads one batch record with its items and their document
	// job stages; viewer is the signed-in account id or "".
	GetDocumentBatch(ctx context.Context, namespace, batchID, viewer string) (map[string]any, error)
	// ListDocumentBatches lists the caller's unfinished batches.
	ListDocumentBatches(ctx context.Context, namespace, viewer string, limit int) (map[string]any, error)
	Delete(ctx context.Context, namespace, name string) (map[string]any, error)
	// WriteDocument synchronously creates or replaces one document and rebuilds
	// its index. The Console replace endpoint passes overwrite=true so the
	// existing document keeps its name while its content changes.
	WriteDocument(ctx context.Context, namespace, name string, content []byte, overwrite bool, idempotencyKey string) (map[string]any, error)
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
	s.record(r, "list_documents", namespace, "", "success")
	extensions := append([]string(nil), SupportedExtensions...)
	sort.Strings(extensions)
	writeJSON(w, http.StatusOK, map[string]any{
		"documents":            documents,
		"supported_extensions": extensions,
		"max_files_per_batch":  MaxFilesPerBatch,
		"max_file_bytes":       MaxFileBytes,
	})
}

// validateUploadBatch rejects unsupported or oversized batches before admission.
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

// uploadProgressStep is the minimum number of newly received content bytes
// between two transfer-progress callbacks for the same file part.
const uploadProgressStep = 32 * 1024

// uploadProgressReader counts the content bytes read from one multipart file
// part (multipart headers excluded) and reports throttle-limited progress.
type uploadProgressReader struct {
	reader     io.Reader
	index      int
	received   int64
	lastReport int64
	reported   bool
	onProgress func(index int, received int64)
}

func (p *uploadProgressReader) Read(buffer []byte) (int, error) {
	read, err := p.reader.Read(buffer)
	if read > 0 {
		p.received += int64(read)
		if !p.reported || p.received-p.lastReport >= uploadProgressStep {
			p.reported = true
			p.lastReport = p.received
			p.onProgress(p.index, p.received)
		}
	}
	return read, err
}

// finish emits the exact final byte count of the part when the throttled stream
// did not already end on it.
func (p *uploadProgressReader) finish() {
	if p == nil || p.onProgress == nil {
		return
	}
	if p.reported && p.received == p.lastReport {
		return
	}
	p.reported = true
	p.lastReport = p.received
	p.onProgress(p.index, p.received)
}

// readMultipartFiles streams the multipart body, rejecting non-file fields and
// oversized files before they reach the runtime.
func readMultipartFiles(r *http.Request) ([]UploadFile, *uploadProblem) {
	return readMultipartFilesWithProgress(r, nil)
}

// readMultipartFilesWithProgress is readMultipartFiles plus per-file transfer
// progress. onProgress reports the cumulative content bytes of each file part
// keyed by its 0-based appearance order: the first bytes, then at most one
// callback per additional 32 KiB, and one exact final callback per part. A nil
// callback keeps the original behaviour and cost.
func readMultipartFilesWithProgress(r *http.Request, onProgress func(index int, received int64)) ([]UploadFile, *uploadProblem) {
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
		var contentReader io.Reader = io.LimitReader(part, MaxFileBytes+1)
		var progress *uploadProgressReader
		if onProgress != nil {
			progress = &uploadProgressReader{
				reader:     contentReader,
				index:      len(files),
				onProgress: onProgress,
			}
			contentReader = progress
		}
		content, readErr := io.ReadAll(contentReader)
		progress.finish()
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
	if !s.requireScopeDeleteDocument(w, r, pathParam(r, "name")) {
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
	s.record(r, "delete_document", namespace, pathParam(r, "name"), "success")
	if s.docNotifier != nil {
		s.docNotifier.DocumentsChanged(namespace, before, payloadDocuments(payload))
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleReplaceDocument replaces one existing document's file content while
// keeping its document name. The uploaded file may carry a different filename
// as long as its type (suffix) matches the target document. The write path is
// the same synchronous one MCP update_document uses, so the caller sees the
// rebuilt index in the response and the document keeps one record and one name.
func (s *Server) handleReplaceDocument(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	// Only the target document's updater may replace it; an update grant for
	// this exact name is enough, and a grant for another file is refused.
	name := pathParam(r, "name")
	if !s.requireScopeUpdateDocument(w, r, name) {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	targetSuffix := strings.ToLower(filepath.Ext(name))
	if !supportedExtension(targetSuffix) {
		writeDetail(w, http.StatusBadRequest, unsupportedFileTypeMessage(name))
		return
	}
	file, problem := readReplacementFile(r)
	if problem != nil {
		writeUploadProblem(w, problem)
		return
	}
	uploadSuffix := strings.ToLower(filepath.Ext(file.Name))
	if !supportedExtension(uploadSuffix) {
		writeDetail(w, http.StatusBadRequest, unsupportedFileTypeMessage(file.Name))
		return
	}
	if uploadSuffix != targetSuffix && documentFormatFamily(uploadSuffix) != documentFormatFamily(targetSuffix) {
		writeDetail(w, http.StatusBadRequest,
			"替换文件的类型必须与原文档一致：原文档为 "+targetSuffix+"，新文件为 "+uploadSuffix+"。")
		return
	}
	var before []map[string]any
	if s.docNotifier != nil {
		before, _ = s.runtime.ListDocuments(r.Context(), namespace)
	}
	payload, err := s.runtime.WriteDocument(r.Context(), namespace, name, file.Content, true, "")
	if err != nil {
		var validation *apierrors.KnowledgeValidationError
		if errors.As(err, &validation) {
			const missingPrefix = "Document does not exist: "
			if strings.HasPrefix(validation.Message, missingPrefix) {
				writeDetail(w, http.StatusNotFound,
					"知识文件不存在："+strings.TrimPrefix(validation.Message, missingPrefix))
				return
			}
			writeDetail(w, http.StatusBadRequest, validation.Message)
			return
		}
		writeRuntimeError(w, err, false)
		return
	}
	s.record(r, "update_document", namespace, name, "success")
	if s.docNotifier != nil {
		s.docNotifier.DocumentsChanged(namespace, before, payloadDocuments(payload))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, payload)
}

// readReplacementFile reads exactly one multipart file part named "file" and
// bounds it like a regular upload. Progress events do not apply: the Console
// only needs one explicit replacement submission.
func readReplacementFile(r *http.Request) (UploadFile, *uploadProblem) {
	reader, err := r.MultipartReader()
	if err != nil {
		return UploadFile{}, &uploadProblem{status: http.StatusBadRequest, detail: "There was an error parsing the body"}
	}
	var file UploadFile
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return UploadFile{}, &uploadProblem{status: http.StatusBadRequest, detail: "There was an error parsing the body"}
		}
		field := part.FormName()
		filename := part.FileName()
		if field != "file" || filename == "" {
			_ = part.Close()
			return UploadFile{}, &uploadProblem{
				status: http.StatusBadRequest, detail: "替换仅接受一个文件字段 file",
			}
		}
		if file.Name != "" {
			_ = part.Close()
			return UploadFile{}, &uploadProblem{
				status: http.StatusBadRequest, detail: "替换一次只能上传一个文件",
			}
		}
		content, readErr := io.ReadAll(io.LimitReader(part, MaxFileBytes+1))
		_ = part.Close()
		if readErr != nil {
			return UploadFile{}, &uploadProblem{status: http.StatusBadRequest, detail: "Could not read the uploaded file"}
		}
		if len(content) > MaxFileBytes {
			return UploadFile{}, &uploadProblem{
				status: http.StatusBadRequest, detail: "单个知识文件不能超过 10 MiB：" + filename,
			}
		}
		if len(content) == 0 {
			return UploadFile{}, &uploadProblem{status: http.StatusBadRequest, detail: "替换文件不能为空"}
		}
		file = UploadFile{Name: filename, Content: content}
	}
	if file.Name == "" {
		return UploadFile{}, &uploadProblem{status: http.StatusBadRequest, detail: "请选择要替换的文件"}
	}
	return file, nil
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
	prepareContent := func() {
		s.record(r, "read_document", namespace, name, "success")
		w.Header().Set("Cache-Control", "no-store")
	}
	if strings.HasSuffix(strings.ToLower(name), ".pdf") {
		content, err := s.runtime.ReadDocumentFile(r.Context(), namespace, name)
		if err != nil {
			writeDocumentReadError(w, err)
			return
		}
		prepareContent()
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
	prepareContent()
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

// documentFormatFamily groups suffixes that are interchangeable when replacing
// a document. .html and .htm are the same family because both are parsed by the
// Docling service and produce identical evidence; every other suffix is its own
// family and must match the original name exactly.
func documentFormatFamily(suffix string) string {
	switch suffix {
	case ".html", ".htm":
		return ".html"
	default:
		return suffix
	}
}
