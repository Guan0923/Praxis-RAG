// Package runtime: the knowledge-graph mirror.
//
// mirrorDocumentGraph copies one published document's chunks into the LightRAG
// sidecar. The index cleanup worker retires its graph copy. The graph is
// a derived index, not a source of truth: a mirror failure never fails the
// upload, it only leaves a failed ledger row an operator can retry from
// lightrag_documents.
//
// The mirror runs after the MySQL publication commits, on a detached context,
// because one extraction call per chunk easily outlives an HTTP request. Chunk
// text, chunk identifiers and model credentials are never logged.

package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/chunking"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
	"github.com/Guan0923/Praxis-RAG/internal/lightrag"
	"github.com/Guan0923/Praxis-RAG/internal/logging"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// graphMirrorBudget bounds one detached mirror run. Entity extraction runs one
// model call per chunk, so the budget is generous compared to a search call;
// the HTTP timeout of one sidecar request still applies inside it.
const graphMirrorBudget = 30 * time.Minute

// graphTimestampFormat matches the timestamp shape the state layer stores in
// indexed_at, so a ledger row written here is indistinguishable from one the
// repository writes itself.
const graphTimestampFormat = "2006-01-02T15:04:05.000000+00:00"

// graphMirror is the resolved knowledge-graph configuration of one knowledge
// base. A nil value means the capability is off for that namespace.
type graphMirror struct {
	client      *lightrag.Client
	embedding   modelapi.ModelConnection
	llm         modelapi.ModelConnection
	vectorSize  int
	fingerprint string
}

// graphClientCache reuses one HTTP client per sidecar base URL. The client is
// stateless, so a process-wide cache avoids rebuilding the transport on every
// upload while keeping the runtime Service unchanged.
var (
	graphClientsMu sync.Mutex
	graphClients   = map[string]*lightrag.Client{}
)

// graphClient returns the sidecar client of this deployment, or nil when the
// knowledge-graph capability is not configured.
func (s *Service) graphClient() (*lightrag.Client, error) {
	baseURL := strings.TrimSpace(s.config.LightRAGServiceURL)
	if baseURL == "" {
		return nil, nil
	}
	graphClientsMu.Lock()
	defer graphClientsMu.Unlock()
	if client, ok := graphClients[baseURL]; ok {
		return client, nil
	}
	client, err := lightrag.NewClient(lightrag.Options{
		BaseURL: baseURL,
		Token:   s.config.LightRAGServiceToken,
		Timeout: s.config.LightRAGServiceTimeout,
	})
	if err != nil {
		return nil, err
	}
	graphClients[baseURL] = client
	return client, nil
}

// resolveGraphMirror reads the knowledge-base graph settings and validates them
// before any work starts. It returns (nil, nil) when the knowledge base has the
// capability switched off, so every caller can treat nil as "do nothing".
//
// A knowledge base that switched graph indexing on without a usable model or
// without a configured sidecar gets an actionable error instead of a silent
// skip: the upload is rejected at admission, the same way a PDF upload under
// the vision engine is rejected when no VLM is configured.
func (s *Service) resolveGraphMirror(settings map[string]any) (*graphMirror, error) {
	if settings == nil {
		return nil, nil
	}
	enabled, _ := settings["graph_enabled"].(bool)
	if !enabled {
		return nil, nil
	}
	client, err := s.graphClient()
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "知识图谱服务地址无效，请检查 RAG_LIGHTRAG_SERVICE_URL"}
	}
	if client == nil {
		return nil, &apierrors.KnowledgeError{
			Message: "知识库已启用知识图谱，但部署未配置 RAG_LIGHTRAG_SERVICE_URL",
		}
	}
	encryptionKey := ""
	if s.stores != nil {
		encryptionKey = s.stores.Settings.EncryptionKey
	}
	llm, err := kbconfig.ModelConnectionForKey(settings, "graph", encryptionKey)
	if err != nil {
		return nil, &apierrors.KnowledgeError{
			Message: "知识图谱模型配置无效，请到「模型配置」重新保存知识图谱模型",
		}
	}
	if strings.TrimSpace(llm.BaseURL) == "" || strings.TrimSpace(llm.Model) == "" {
		return nil, &apierrors.KnowledgeError{
			Message: "知识库已启用知识图谱，但未选择知识图谱模型",
		}
	}
	embedding, err := kbconfig.ModelConnectionForKey(settings, "embedding", encryptionKey)
	if err != nil {
		return nil, adminError(err)
	}
	vectorSize := 0
	if embedding.VectorSize != nil {
		vectorSize = *embedding.VectorSize
	}
	return &graphMirror{
		client:      client,
		embedding:   embedding,
		llm:         llm,
		vectorSize:  vectorSize,
		fingerprint: graphFingerprint(s.config.LightRAGServiceURL, embedding, llm),
	}, nil
}

// graphFingerprint digests the values that make a graph copy meaningless when
// they change. The sidecar rebuilds a workspace when the fingerprint differs,
// so an embedding model swap or an extraction model swap cannot leave a graph
// that silently mixes two vector spaces or two entity vocabularies.
func graphFingerprint(sidecar string, embedding, llm modelapi.ModelConnection) string {
	parts := []string{
		strings.TrimRight(strings.TrimSpace(sidecar), "/"),
		strings.TrimSpace(embedding.BaseURL),
		strings.TrimSpace(embedding.Model),
		strings.TrimSpace(llm.BaseURL),
		strings.TrimSpace(llm.Model),
	}
	return sha256Hex(strings.Join(parts, "\x00"))
}

// workspaceConfig builds the sidecar workspace body for one knowledge base.
// The extraction and keyword roles share one model connection on purpose: the
// project configures a single graph model per knowledge base.
func (m *graphMirror) workspaceConfig(namespace string) lightrag.WorkspaceConfig {
	embedding := lightrag.ModelEndpoint{
		BaseURL: m.embedding.BaseURL,
		Model:   m.embedding.Model,
		APIKey:  m.embedding.APIKey,
	}
	if m.vectorSize > 0 {
		dimension := m.vectorSize
		embedding.Dimension = &dimension
	}
	extract := lightrag.LLMEndpoint{
		BaseURL: m.llm.BaseURL,
		Model:   m.llm.Model,
		APIKey:  m.llm.APIKey,
	}
	return lightrag.WorkspaceConfig{
		Namespace:         namespace,
		ConfigFingerprint: m.fingerprint,
		Language:          "Chinese",
		Embedding:         embedding,
		ExtractLLM:        extract,
		KeywordLLM:        extract,
	}
}

// scheduleGraphMirror starts the mirror of one published document without
// blocking the caller. The ledger row is written inside the detached run, so a
// process crash leaves an "indexed" row only when the sidecar actually holds
// the document.
func (s *Service) scheduleGraphMirror(
	ctx context.Context,
	namespace, name, documentID, version, sourceHash string,
	mirror *graphMirror,
	chunks []chunking.Chunk,
) {
	if mirror == nil || len(chunks) == 0 {
		return
	}
	payload := graphChunkPayload(chunks)
	if len(payload) == 0 {
		return
	}
	detached := context.WithoutCancel(ctx)
	go func() {
		runContext, cancel := context.WithTimeout(detached, graphMirrorBudget)
		defer cancel()
		s.mirrorDocumentGraph(runContext, namespace, name, documentID, version, sourceHash, mirror, payload)
	}()
}

// mirrorDocumentGraph ensures the workspace, replaces the document chunks and
// records the outcome. Failures are recorded in the ledger and logged; they are
// never returned to the upload path.
func (s *Service) mirrorDocumentGraph(
	ctx context.Context,
	namespace, name, documentID, version, sourceHash string,
	mirror *graphMirror,
	chunks []lightrag.Chunk,
) {
	if _, err := mirror.client.EnsureWorkspace(ctx, mirror.workspaceConfig(namespace)); err != nil {
		s.recordGraphFailure(ctx, namespace, documentID, err)
		return
	}
	// Read the sidecar generation first, then the publication head. If
	// another mirror or prune wins the race, repeat both reads.
	var result *lightrag.IngestResult
	var err error
	for ctx.Err() == nil {
		var revision string
		revision, err = mirror.client.DocumentRevision(ctx, namespace, documentID)
		if err != nil {
			break
		}
		var versions map[string]string
		versions, err = s.stores.Documents.VisibleVersions(ctx, namespace)
		if err != nil {
			break
		}
		if versions[documentID] != version {
			return
		}
		result, err = mirror.client.IngestChunks(ctx, namespace, lightrag.IngestRequest{
			DocumentID: documentID, DocumentName: name, Chunks: chunks, PublicationVersion: version, SourceHash: sourceHash, ExpectedRevision: &revision,
		})
		var changed *lightrag.Error
		if !errors.As(err, &changed) || changed.Status != 409 {
			break
		}
	}
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		s.recordGraphFailure(ctx, namespace, documentID, err)
		return
	}
	count := len(chunks)
	if result != nil && result.Accepted > 0 {
		count = result.Accepted
	}
	record := &state.LightRAGDocument{
		Namespace:         namespace,
		DocumentID:        documentID,
		DocumentName:      name,
		ConfigFingerprint: mirror.fingerprint,
		ChunkCount:        count,
		Status:            state.LightRAGStatusIndexed,
		IndexedAt:         time.Now().UTC().Format(graphTimestampFormat),
	}
	if err := s.stores.LightRAGDocuments.Upsert(ctx, record); err != nil {
		logging.FromContext(ctx).Warn("Knowledge-graph ledger write failed",
			append([]any{"namespace", namespace}, logging.ErrorAttrs(err)...)...)
	}
}

// recordGraphFailure writes the failed ledger row and a redacted warning. The
// document stays fully usable through the vector index; only the graph copy is
// missing.
func (s *Service) recordGraphFailure(ctx context.Context, namespace, documentID string, err error) {
	logging.FromContext(ctx).Warn("Knowledge-graph mirror failed",
		append([]any{"namespace", namespace, "document_id", documentID},
			logging.ErrorAttrs(err)...)...)
	if s.stores == nil || s.stores.LightRAGDocuments == nil {
		return
	}
	message := graphErrorMessage(err)
	if writeErr := s.stores.LightRAGDocuments.MarkFailed(ctx, namespace, documentID, message); writeErr != nil {
		logging.FromContext(ctx).Warn("Knowledge-graph failure ledger write failed",
			append([]any{"namespace", namespace}, logging.ErrorAttrs(writeErr)...)...)
	}
}

// graphChunkPayload converts indexed chunks into sidecar ingest rows, dropping
// chunks without text. Page and heading are read from the chunk metadata the
// parser wrote; a format without them yields zero/empty values.
func graphChunkPayload(chunks []chunking.Chunk) []lightrag.Chunk {
	payload := make([]lightrag.Chunk, 0, len(chunks))
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk.Text) == "" {
			continue
		}
		payload = append(payload, lightrag.Chunk{
			ChunkID: chunk.ChunkID,
			Text:    chunk.Text,
			Page:    chunkMetadataInt(chunk.Metadata, "page"),
			Heading: chunkMetadataString(chunk.Metadata, "heading", "section"),
		})
	}
	return payload
}

// chunkMetadataInt reads one integer metadata value, accepting the JSON number
// forms a parser may produce.
func chunkMetadataInt(metadata map[string]any, key string) int {
	if metadata == nil {
		return 0
	}
	switch value := metadata[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case float32:
		return int(value)
	}
	return 0
}

// chunkMetadataString returns the first non-empty string among the candidate
// metadata keys.
func chunkMetadataString(metadata map[string]any, keys ...string) string {
	if metadata == nil {
		return ""
	}
	for _, key := range keys {
		if text, ok := metadata[key].(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

// graphErrorMessage reduces a sidecar failure to a short safe text. The raw
// message may quote a URL with credentials, so only the classified surface is
// kept.
func graphErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 240 {
		message = message[:240]
	}
	return message
}
