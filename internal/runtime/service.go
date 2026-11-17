// Package runtime owns the long-lived service runtime: configuration, the
// shared state database, the search engine (Qdrant/model orchestration), and the
// document/artifact read paths. Search is synchronous: every call runs retrieval
// and returns evidence directly. The HTTP and MCP layers see the same Service.
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/document"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/version"
)

// windowsReserved mirrors service_knowledge._WINDOWS_RESERVED.
var windowsReserved = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

// Service is the process-wide RAG service runtime.
type Service struct {
	config        config.RagConfig
	stores        *state.Stores
	dataRoot      string
	search        *SearchEngine
	searchTimeout time.Duration
	// clock is the test seam for membership/quota timestamps. Production
	// leaves it nil and uses time.Now.
	clock func() time.Time
	// activityRecorder is the optional monitoring counter recorded once per
	// Search call. It is set once at startup, before the service serves traffic.
	activityRecorder ActivityRecorder
	// views is the cached read-view source for display lists. It is nil when
	// the store exposes no views, in which case the authoritative repository is
	// used directly.
	views documentReadViews
}

// ActivityRecorder records coarse-grained activity counters for the Console
// monitoring page (for example state.ActivityCounters). It is a narrow seam so
// the runtime does not depend on the monitoring read path.
type ActivityRecorder interface {
	Record(ctx context.Context, metric string, now time.Time) error
}

// SetActivityRecorder injects the optional activity counter. It must be called
// during startup, before the service starts handling requests; a nil recorder
// disables counting.
func (s *Service) SetActivityRecorder(recorder ActivityRecorder) {
	s.activityRecorder = recorder
}

// documentReadViews is the read-through cached document list used by
// /v1/documents (Python state.views.documents).
type documentReadViews interface {
	Documents(ctx context.Context, namespace string) ([]map[string]any, error)
}

// New validates the runtime configuration and prepares the data root. A backend
// other than qdrant is rejected exactly like RagServiceRuntime.
func New(cfg config.RagConfig, stores *state.Stores, dataRoot string) (*Service, error) {
	if cfg.Backend != config.BackendQdrant {
		return nil, fmt.Errorf("Standalone RAG service requires RAG_BACKEND=qdrant.")
	}
	if stores == nil {
		return nil, fmt.Errorf("Standalone RAG service requires the shared state store.")
	}
	root := config.ExpandPath(dataRoot)
	if root == "" {
		return nil, fmt.Errorf("RAG service data root cannot be empty.")
	}
	if info, err := os.Lstat(root); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("RAG service data root cannot be a symbolic link.")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("Cannot create the RAG service data root: %s", root)
	}
	search, err := NewSearchEngine(cfg, stores)
	if err != nil {
		return nil, err
	}
	searchTimeout, err := searchTimeoutFromEnv()
	if err != nil {
		return nil, err
	}
	service := &Service{config: cfg, stores: stores, dataRoot: root, search: search, searchTimeout: searchTimeout}
	if stores.Views != nil {
		service.views = stores.Views
	}
	return service, nil
}

// searchTimeoutEnvName is the synchronous search budget. Before the queued
// search flow was removed the same variable bounded one queued task's deadline;
// it now bounds a single synchronous retrieval call.
const searchTimeoutEnvName = "RAG_SEARCH_TIMEOUT_SECONDS"

// defaultSearchTimeoutSeconds keeps the historical 120-second default.
const defaultSearchTimeoutSeconds = 120

// searchTimeoutFromEnv resolves the per-call search budget. An invalid value
// fails startup instead of silently running unbounded, matching the previous
// queued-search task setting.
func searchTimeoutFromEnv() (time.Duration, error) {
	raw, ok := os.LookupEnv(searchTimeoutEnvName)
	if !ok {
		return defaultSearchTimeoutSeconds * time.Second, nil
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", searchTimeoutEnvName)
	}
	return time.Duration(seconds) * time.Second, nil
}

// DataRoot returns the resolved data root.
func (s *Service) DataRoot() string { return s.dataRoot }

// Config returns the resolved retrieval configuration.
func (s *Service) Config() config.RagConfig { return s.config }

// Stores returns the shared state stores.
func (s *Service) Stores() *state.Stores { return s.stores }

// SearchEngine exposes the retrieval half of the runtime.
func (s *Service) SearchEngine() *SearchEngine { return s.search }

// Status renders the secret-free health payload returned by /health.
func (s *Service) Status(context.Context) map[string]any {
	return map[string]any{
		"status":     "ok",
		"service":    version.ServiceName,
		"backend":    string(s.config.Backend),
		"collection": s.config.Collection,
		"state":      s.stores.Describe(),
	}
}

// Close releases the runtime resources (search engine, then the shared store).
// Close stops background workers, then releases the search engine and store.
func (s *Service) Close(ctx context.Context) error {
	s.StopBackground()
	if s.search != nil {
		_ = s.search.Close(ctx)
	}
	return s.stores.Close()
}

// Invalidate drops the cached tenant after a knowledge-base configuration
// change, so the next search rebuilds connections and collection references.
func (s *Service) Invalidate(namespace string) { s.search.Invalidate(namespace) }

// Search runs one synchronous retrieval action. The call is bounded by the
// server-side search budget (RAG_SEARCH_TIMEOUT_SECONDS) so a slow Qdrant or
// model service cannot hold the request open indefinitely; client cancellation
// still propagates through the request context.
func (s *Service) Search(ctx context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error) {
	if s.searchTimeout > 0 {
		timeoutCtx, cancel := context.WithTimeout(ctx, s.searchTimeout)
		defer cancel()
		ctx = timeoutCtx
	}
	result, err := s.search.Search(ctx, namespace, request)
	if errors.Is(err, context.DeadlineExceeded) {
		// The search budget (or an earlier parent deadline) expired. Report the
		// same operational category as a model/Qdrant outage so HTTP answers 503
		// and MCP answers a tool error instead of a generic internal error.
		err = contracts.NewRagOperationError("Search timed out")
	}
	// Every retrieval execution counts, including a failed one: the Console
	// activity card reports usage, not success. Counting failures are silently
	// ignored so a monitoring write can never change the retrieval outcome, and
	// the cancellation signal is dropped so a client disconnect still counts.
	if s.activityRecorder != nil {
		_ = s.activityRecorder.Record(context.WithoutCancel(ctx), state.ActivitySearch, time.Now())
	}
	return result, err
}

// PublicKnowledgeBase returns the secret-free knowledge-base settings.
func (s *Service) PublicKnowledgeBase(ctx context.Context, namespace string) (map[string]any, error) {
	return s.search.PublicKnowledgeBase(ctx, namespace)
}

// ListDocuments returns the namespace's document records. The shape matches
// Python's KnowledgeDocumentStore.list_documents (dicts with JSON field names).
// ListDocuments returns the display document list through the cached read view
// (Python state.views.documents), so repeated reads share the same snapshot and
// event ordering as the other view endpoints. The view already overlays the
// knowledge-base collection/embedding_model; when views are unavailable or fail
// the authoritative repository is used as a fallback.
func (s *Service) ListDocuments(ctx context.Context, namespace string) ([]map[string]any, error) {
	if s.views != nil {
		documents, err := s.views.Documents(ctx, namespace)
		if err == nil {
			return documents, nil
		}
	}
	return s.authoritativeDocuments(ctx, namespace)
}

// authoritativeDocuments reads current records directly from the repository.
func (s *Service) authoritativeDocuments(ctx context.Context, namespace string) ([]map[string]any, error) {
	records, err := s.stores.Documents.List(ctx, namespace)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "Document records cannot be read."}
	}
	settings, _ := s.stores.KnowledgeBases.Get(ctx, namespace)
	result := make([]map[string]any, 0, len(records))
	for _, record := range records {
		item := recordMap(record)
		if settings != nil {
			if collection, ok := settings["collection"].(string); ok && collection != "" {
				item["collection"] = collection
			}
			if embeddingModel, ok := settings["embedding_model"].(string); ok && embeddingModel != "" {
				if chunking, ok := item["chunking"].(map[string]any); ok {
					chunking["embedding_model"] = embeddingModel
				}
			}
		}
		result = append(result, item)
	}
	return result, nil
}

// ReadDocument returns the saved rendered text and version of one document.
//
// Versioned PDF/Word records serve the staged document.md from the upload
// journal object store (the immutable publication artifact). Unversioned parser
// records fall back to the per-namespace asset bucket, and plain md/txt reads
// the source with Python's utf-8-sig + MAX_EXTRACTED_CHARACTERS rules
// (service_knowledge.KnowledgeDocumentStore.read_text).
func (s *Service) ReadDocument(ctx context.Context, namespace, name string) (string, string, error) {
	safeName, err := SanitizeName(name)
	if err != nil || safeName != name {
		return "", "", &apierrors.KnowledgeValidationError{Message: "Invalid document name"}
	}
	record, err := s.stores.Documents.Get(ctx, namespace, name)
	if err != nil {
		return "", "", &apierrors.KnowledgeError{Message: "Document records cannot be read."}
	}
	if record == nil {
		return "", "", &apierrors.KnowledgeValidationError{Message: "Document does not exist"}
	}
	suffix := strings.ToLower(filepath.Ext(name))
	version := record.IndexedAt
	if record.OperationID != "" {
		if bucket, ok := artifactBucketForSuffix(suffix); ok {
			content, found, readErr := s.readVersionedArtifact(ctx, record.OperationID, record.DocumentID, bucket)
			if readErr != nil || !found || !utf8.Valid(content) {
				return "", "", savedContentUnavailableError()
			}
			return string(content), version, nil
		}
	}
	if suffix == document.SuffixPDF {
		// Stored PDFs record the engine that produced their document.md
		// (go-native-v1 or docling-service-v1), so validate the recorded value
		// against the supported set instead of one fixed constant.
		recorded := parserVersionValue(record.Chunking, "pdf_parser_version")
		if !document.IsSupportedPDFParserVersion(recorded) {
			return "", "", &apierrors.KnowledgeValidationError{Message: pdfParserChangedMessage}
		}
		text, readErr := s.readSavedArtifactText(ctx, namespace, name, record,
			state.PDFAssetBucket, "pdf_parser_version", recorded,
			pdfParserChangedMessage, savedPDFContentUnavailableMessage)
		if readErr != nil {
			return "", "", readErr
		}
		return text, version, nil
	}
	if suffix == document.SuffixDOC || suffix == document.SuffixDOCX {
		if parserVersionValue(record.Chunking, "word_parser_version") != "" {
			text, readErr := s.readSavedArtifactText(ctx, namespace, name, record,
				state.WordAssetBucket, "word_parser_version", document.WordParserVersion,
				wordParserChangedMessage, savedWordContentUnavailableMessage)
			if readErr != nil {
				return "", "", readErr
			}
			return text, version, nil
		}
	}
	content, err := s.readSource(ctx, namespace, name)
	if err != nil {
		return "", "", err
	}
	text, err := decodeSourceText(content, name)
	if err != nil {
		return "", "", err
	}
	return text, version, nil
}

// artifactBucketForSuffix reports the parser-artifact bucket for suffixes whose
// rendered document.md is stored with a versioned upload.
func artifactBucketForSuffix(suffix string) (string, bool) {
	switch suffix {
	case document.SuffixPDF:
		return state.PDFAssetBucket, true
	case document.SuffixDOC, document.SuffixDOCX:
		return state.WordAssetBucket, true
	default:
		return "", false
	}
}

// readVersionedArtifact reads one immutable publication artifact from the
// upload journal object store (Python state.uploads.get_object).
func (s *Service) readVersionedArtifact(ctx context.Context, version, documentID, bucket string) ([]byte, bool, error) {
	if s.stores.Uploads == nil || version == "" || documentID == "" {
		return nil, false, nil
	}
	return s.stores.Uploads.GetObject(ctx, version, bucket, documentID+"/document.md")
}

// readSavedArtifactText reads an unversioned parser artifact from the namespace
// asset bucket after validating the parser version, mirroring
// pdf_assets.read_pdf_content / word_assets.read_word_content.
func (s *Service) readSavedArtifactText(
	ctx context.Context,
	namespace, name string,
	record *state.DocumentRecord,
	bucket, parserKey, expectedParser, changedMessage, unavailableMessage string,
) (string, error) {
	if parserVersionValue(record.Chunking, parserKey) != expectedParser {
		return "", &apierrors.KnowledgeValidationError{Message: changedMessage}
	}
	documentID := chunking.DocumentID(name)
	content, found, err := s.stores.Blobs.Get(ctx, namespace, bucket, documentID+"/document.md")
	if err != nil {
		return "", &apierrors.KnowledgeError{Message: "Document content is unavailable"}
	}
	if !found || !utf8.Valid(content) {
		return "", &apierrors.KnowledgeValidationError{Message: unavailableMessage}
	}
	return string(content), nil
}

// parserVersionValue reads a parser version from the decoded chunking metadata.
func parserVersionValue(value any, key string) string {
	metadata, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	version, _ := metadata[key].(string)
	return version
}

// decodeSourceText ports content.decode("utf-8-sig") plus the extracted-size
// guard of read_text.
func decodeSourceText(content []byte, name string) (string, error) {
	text := string(bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF}))
	if !utf8.ValidString(text) {
		return "", &apierrors.KnowledgeValidationError{Message: textEncodingErrorPrefix + name}
	}
	if utf8.RuneCountInString(text) > document.MaxExtractedCharacters {
		return "", &apierrors.KnowledgeValidationError{Message: textTooLargePrefix + name}
	}
	return text, nil
}

// Saved-content messages mirror the Python runtime strings exactly (the HTTP
// handlers map KnowledgeValidationError to 404).
const (
	savedContentUnavailableMessage     = "Saved content unavailable; reimport this document"
	savedPDFContentUnavailableMessage  = "Saved PDF content is unavailable; explicitly reimport this PDF"
	savedWordContentUnavailableMessage = "Saved Word content unavailable; reimport this document"
	pdfParserChangedMessage            = "PDF parser version changed; explicitly reimport this PDF"
	wordParserChangedMessage           = "Word parser changed; reimport this document"
	textEncodingErrorPrefix            = "文本文件必须使用 UTF-8 编码："
	textTooLargePrefix                 = "文档提取文本过大："
)

func savedContentUnavailableError() error {
	return &apierrors.KnowledgeValidationError{Message: savedContentUnavailableMessage}
}

// ReadDocumentFile returns the stored source bytes of one document.
func (s *Service) ReadDocumentFile(ctx context.Context, namespace, name string) ([]byte, error) {
	safeName, err := SanitizeName(name)
	if err != nil || safeName != name {
		return nil, &apierrors.KnowledgeValidationError{Message: "Invalid document name"}
	}
	record, err := s.stores.Documents.Get(ctx, namespace, name)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "Document records cannot be read."}
	}
	if record == nil {
		return nil, &apierrors.KnowledgeValidationError{Message: "Document does not exist"}
	}
	return s.readSource(ctx, namespace, name)
}

func (s *Service) readSource(ctx context.Context, namespace, name string) ([]byte, error) {
	content, found, err := s.stores.Blobs.Get(ctx, namespace, state.SourceBucket, name)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "Document content is unavailable"}
	}
	if !found {
		return nil, &apierrors.KnowledgeValidationError{Message: "Document content is unavailable"}
	}
	return content, nil
}

// CheckDocuments reports existing documents that already match the supplied
// content hashes. Duplicate detection compares the stored chunking metadata
// with the namespace's committed configuration like Python's find_duplicate.
// SupportedUploadExtensions is the accepted upload suffix set in Python's sorted
// order (used verbatim in the unsupported-type message).
var SupportedUploadExtensions = []string{".doc", ".docx", ".md", ".pdf", ".txt"}

// UnsupportedFileTypeMessage mirrors service_knowledge.validate_uploads:
// "暂不支持该文件类型：<name>；请选择 .doc、.docx、.md、.pdf、.txt。".
func UnsupportedFileTypeMessage(name string) string {
	return "暂不支持该文件类型：" + name + "；请选择 " + strings.Join(SupportedUploadExtensions, "、") + "。"
}

// SupportedUploadExtension reports whether name has an accepted suffix
// (case-insensitive, mirroring Path(...).suffix.casefold()).
func SupportedUploadExtension(name string) bool {
	extension := strings.ToLower(filepath.Ext(name))
	for _, supported := range SupportedUploadExtensions {
		if extension == supported {
			return true
		}
	}
	return false
}

func (s *Service) CheckDocuments(ctx context.Context, namespace string, files []CheckFile) (map[string]any, error) {
	settings, err := s.stores.KnowledgeBases.Get(ctx, namespace)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "Knowledge-base settings cannot be read."}
	}
	if settings == nil {
		return nil, &apierrors.KnowledgeValidationError{Message: "该知识库没有配置记录，请先创建知识库"}
	}
	configuration := chunkingMetadata(settings)
	result := make([]map[string]any, 0, len(files))
	for _, file := range files {
		safeName, nameErr := SanitizeName(file.Name)
		if nameErr != nil || safeName != file.Name {
			return nil, &apierrors.KnowledgeValidationError{Message: "Invalid document name: " + file.Name}
		}
		// Python validate_uploads checks the casefolded suffix after sanitizing
		// and raises the shared unsupported-type message (HTTP 400).
		if !SupportedUploadExtension(safeName) {
			return nil, &apierrors.KnowledgeValidationError{Message: UnsupportedFileTypeMessage(safeName)}
		}
		candidates, listErr := s.stores.Documents.Candidates(ctx, namespace, file.SHA256, &file.Name)
		if listErr != nil {
			return nil, &apierrors.KnowledgeError{Message: "Document records cannot be read."}
		}
		var documentPayload any
		for _, candidate := range candidates {
			exists, blobErr := s.stores.Blobs.Exists(ctx, namespace, state.SourceBucket, candidate.Name)
			if blobErr != nil || !exists {
				continue
			}
			if sameChunking(candidate.Chunking, configuration) {
				documentPayload = recordMap(candidate)
				break
			}
		}
		result = append(result, map[string]any{
			"name": file.Name, "sha256": file.SHA256, "document": documentPayload,
		})
	}
	return map[string]any{"files": result}, nil
}

// GetArtifact reads one bounded byte chunk of an expiring artifact.
func (s *Service) GetArtifact(ctx context.Context, namespace, artifactID string, offset, length int) (map[string]any, error) {
	chunk, err := s.stores.Artifacts.Chunk(ctx, namespace, artifactID, offset, length)
	if err != nil {
		return nil, artifactError(err)
	}
	return marshalToMap(chunk)
}

// DownloadArtifact returns the artifact bytes plus its MIME type and name.
func (s *Service) DownloadArtifact(ctx context.Context, namespace, artifactID string) ([]byte, string, string, error) {
	artifact, content, err := s.stores.Artifacts.Read(ctx, namespace, artifactID)
	if err != nil {
		return nil, "", "", artifactError(err)
	}
	return content, artifact.MimeType, artifact.Name, nil
}

func artifactError(err error) error {
	var unavailable *state.ArtifactUnavailable
	if errors.As(err, &unavailable) {
		return &apierrors.SnapshotUnavailable{Message: unavailable.Message}
	}
	return &apierrors.SnapshotUnavailable{Message: "Artifact is unavailable"}
}

// Upload writes and indexes one or more documents through the durable upload
// journal. See documents_upload.go for the transaction flow.
func (s *Service) Upload(ctx context.Context, namespace string, files []UploadFile, idempotencyKey string) (map[string]any, error) {
	return s.uploadDocuments(ctx, namespace, files, idempotencyKey)
}

// Delete logically deletes one document through the upload coordinator.
func (s *Service) Delete(ctx context.Context, namespace, name string) (map[string]any, error) {
	return s.deleteDocumentByName(ctx, namespace, name)
}

// AddDocument (async MCP admission) needs the document jobs queue from G7.
// AddDocument durably admits one upload for asynchronous indexing.
func (s *Service) AddDocument(ctx context.Context, namespace, name string, content []byte, idempotencyKey string) (map[string]any, error) {
	return s.addDocumentAsync(ctx, namespace, name, content, idempotencyKey)
}

// GetDocumentStatus reports the indexing status of one artifact.
func (s *Service) GetDocumentStatus(ctx context.Context, namespace, artifactID string) (map[string]any, error) {
	return s.documentStatus(ctx, namespace, artifactID)
}

// WriteDocument creates or updates one document synchronously (MCP writes).
func (s *Service) WriteDocument(ctx context.Context, namespace, name string, content []byte, overwrite bool, idempotencyKey string) (map[string]any, error) {
	return s.writeDocumentContent(ctx, namespace, name, content, overwrite, idempotencyKey)
}

// DeleteDocument deletes one document synchronously (MCP delete).
func (s *Service) DeleteDocument(ctx context.Context, namespace, name string) (map[string]any, error) {
	return s.deleteDocumentByName(ctx, namespace, name)
}

// UploadFile is one client-supplied document for the REST upload endpoint.
type UploadFile struct {
	Name    string
	Content []byte
}

// CheckFile is one deduplication probe.
type CheckFile struct {
	Name   string
	SHA256 string
}

// SanitizeName ports KnowledgeDocumentStore.sanitize_name.
func SanitizeName(rawName string) (string, error) {
	name := strings.Trim(strings.TrimSpace(windowsReserved.ReplaceAllString(rawName, "_")), " .")
	runes := []rune(name)
	if len(runes) > 200 {
		name = string(runes[:200])
	}
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("知识文件名不能为空。")
	}
	return name, nil
}

func recordMap(record state.DocumentRecord) map[string]any {
	item, err := marshalToMap(record)
	if err != nil {
		return map[string]any{"name": record.Name}
	}
	return item
}

func marshalToMap(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	result := map[string]any{}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func chunkingMetadata(settings map[string]any) map[string]any {
	metadata := map[string]any{
		"strategy":      settings["strategy"],
		"chunk_size":    settings["chunk_size"],
		"chunk_overlap": settings["chunk_overlap"],
		"tokenizer":     settings["tokenizer"],
	}
	if embeddingModel, ok := settings["embedding_model"].(string); ok {
		metadata["embedding_model"] = embeddingModel
	}
	return metadata
}

func sameChunking(stored any, configuration map[string]any) bool {
	encodedStored, err := json.Marshal(stored)
	if err != nil {
		return false
	}
	encodedConfiguration, err := json.Marshal(configuration)
	if err != nil {
		return false
	}
	var left, right map[string]any
	if json.Unmarshal(encodedStored, &left) != nil || json.Unmarshal(encodedConfiguration, &right) != nil {
		return false
	}
	if len(left) > len(right) {
		return false
	}
	for key, expected := range right {
		actual, ok := left[key]
		if !ok {
			return false
		}
		actualJSON, _ := json.Marshal(actual)
		expectedJSON, _ := json.Marshal(expected)
		if string(actualJSON) != string(expectedJSON) {
			return false
		}
	}
	return true
}

// SearchRaw runs retrieval without projecting artifact references. It is an
// internal seam for callers that only need the bare evidence payload.
func (s *Service) SearchRaw(ctx context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error) {
	return s.search.SearchRaw(ctx, namespace, request)
}
