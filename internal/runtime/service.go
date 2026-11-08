// Package runtime owns the long-lived service runtime: configuration, the
// shared state database, the search engine (Qdrant/model orchestration), and the
// document/artifact read paths. Upload transactions and queued search are wired
// in later phases; the HTTP and MCP layers already see a complete Service.
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
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
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
	config   config.RagConfig
	stores   *state.Stores
	dataRoot string
	search   *SearchEngine
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
	return &Service{config: cfg, stores: stores, dataRoot: root, search: search}, nil
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
func (s *Service) Close(ctx context.Context) error {
	if s.search != nil {
		_ = s.search.Close(ctx)
	}
	return s.stores.Close()
}

// Invalidate drops the cached tenant after a knowledge-base configuration
// change, so the next search rebuilds connections and collection references.
func (s *Service) Invalidate(namespace string) { s.search.Invalidate(namespace) }

// Search runs one synchronous retrieval action.
func (s *Service) Search(ctx context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error) {
	return s.search.Search(ctx, namespace, request)
}

// PublicKnowledgeBase returns the secret-free knowledge-base settings.
func (s *Service) PublicKnowledgeBase(ctx context.Context, namespace string) (map[string]any, error) {
	return s.search.PublicKnowledgeBase(ctx, namespace)
}

// GetSearchResult reads one stored search-result page.
func (s *Service) GetSearchResult(ctx context.Context, namespace, resultID string, page, pageSize int) (map[string]any, error) {
	return s.search.GetSearchResult(ctx, namespace, resultID, page, pageSize)
}

// SubmitSearch enqueues a retrieval action. Queued search (Redis + RocketMQ)
// arrives in G7; until then the service reports the limitation safely instead
// of silently running retrieval.
func (s *Service) SubmitSearch(context.Context, string, contracts.SearchRequest) (string, error) {
	return "", &apierrors.KnowledgeError{Message: "Queued search is not available in this build."}
}

// ListDocuments returns the namespace's document records. The shape matches
// Python's KnowledgeDocumentStore.list_documents (dicts with JSON field names).
func (s *Service) ListDocuments(ctx context.Context, namespace string) ([]map[string]any, error) {
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

// ReadDocument returns the rendered text and version of one document. Word
// documents are re-extracted from the stored source; PDF text requires the
// external parser manifest and reports an actionable error until G3 wiring.
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
	content, err := s.readSource(ctx, namespace, name)
	if err != nil {
		return "", "", err
	}
	suffix := strings.ToLower(filepath.Ext(name))
	version := record.IndexedAt
	switch suffix {
	case ".md", ".txt":
		text := stripBOM(content)
		return text, version, nil
	case ".doc", ".docx":
		text, readErr := readWordText(content, name)
		if readErr != nil {
			return "", "", &apierrors.KnowledgeValidationError{Message: readErr.Error()}
		}
		return text, version, nil
	default:
		return "", "", &apierrors.KnowledgeError{
			Message: "PDF text preview requires the external parser; read the original file instead.",
		}
	}
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

func readWordText(content []byte, name string) (string, error) {
	directory, err := os.MkdirTemp("", "praxis-word-preview-")
	if err != nil {
		return "", fmt.Errorf("Word preview is unavailable")
	}
	defer func() { _ = os.RemoveAll(directory) }()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return "", fmt.Errorf("Word preview is unavailable")
	}
	return document.ReadText(path)
}

func stripBOM(content []byte) string {
	return string(bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF}))
}

// CheckDocuments reports existing documents that already match the supplied
// content hashes. Duplicate detection compares the stored chunking metadata
// with the namespace's committed configuration like Python's find_duplicate.
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

// Upload is wired to the durable upload journal in G2. Until then the endpoint
// reports the limitation instead of silently dropping a write.
func (s *Service) Upload(context.Context, string, []UploadFile, string) (map[string]any, error) {
	return nil, &apierrors.KnowledgeError{Message: "Document upload is not available in this build."}
}

// Delete is wired to the upload coordinator in G2.
func (s *Service) Delete(context.Context, string, string) (map[string]any, error) {
	return nil, &apierrors.KnowledgeError{Message: "Document deletion is not available in this build."}
}

// AddDocument (async MCP admission) needs the document jobs queue from G7.
func (s *Service) AddDocument(context.Context, string, string, []byte, string) (map[string]any, error) {
	return nil, &apierrors.KnowledgeError{Message: "Asynchronous document admission is not available in this build."}
}

// GetDocumentStatus needs the document jobs table from G7.
func (s *Service) GetDocumentStatus(context.Context, string, string) (map[string]any, error) {
	return nil, &apierrors.KnowledgeError{Message: "Document status is not available in this build."}
}

// WriteDocument (synchronous MCP create/update) shares the upload transaction.
func (s *Service) WriteDocument(context.Context, string, string, []byte, bool, string) (map[string]any, error) {
	return nil, &apierrors.KnowledgeError{Message: "Synchronous document writes are not available in this build."}
}

// DeleteDocument (synchronous MCP delete) shares the upload transaction.
func (s *Service) DeleteDocument(context.Context, string, string) (map[string]any, error) {
	return nil, &apierrors.KnowledgeError{Message: "Document deletion is not available in this build."}
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
