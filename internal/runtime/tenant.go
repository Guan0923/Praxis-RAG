package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/kbconfig"
	"github.com/Guan0923/xinghe_rag/internal/modelapi"
	"github.com/Guan0923/xinghe_rag/internal/qdrant"
	"github.com/Guan0923/xinghe_rag/internal/retrieval"
)

// runtimeImplementation is the debug implementation string of the service
// runtime orchestrator (Python RagServiceRuntime._tenant).
const runtimeImplementation = "rag-service-qdrant-hybrid-rerank-v1"

// tenantResources caches the per-namespace model/Qdrant stack built from the
// committed knowledge-base configuration. It mirrors the search-relevant part of
// rag_mcp.service_knowledge.TenantResources: the orchestrator, its Qdrant store,
// and the resolved model connections.
type tenantResources struct {
	settings     map[string]any
	collection   string
	store        *qdrant.Store
	orchestrator *retrieval.Orchestrator
	embedding    modelapi.ModelConnection
	reranker     modelapi.ModelConnection
}

// tenant returns the cached tenant resources, rebuilding them from the
// authoritative knowledge-base row on first use. The cache is keyed by
// namespace and invalidated explicitly by configuration mutations
// (SearchEngine.Invalidate), matching the runtime's _tenants dict.
func (e *SearchEngine) tenant(ctx context.Context, namespace string) (*tenantResources, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if tenant, ok := e.tenants[namespace]; ok {
		return tenant, nil
	}
	if e.closed {
		return nil, &apierrors.KnowledgeError{Message: "RAG service runtime is not available"}
	}
	settings, err := e.stores.KnowledgeBases.Get(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if settings == nil {
		return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageKnowledgeBaseMissing}
	}
	tenant, err := e.buildTenant(namespace, settings)
	if err != nil {
		return nil, err
	}
	e.tenants[namespace] = tenant
	return tenant, nil
}

// buildTenant wires one namespace's HTTP model connections, Qdrant store, and
// orchestrator. It performs no network I/O; the first search validates the
// collection lazily through the store.
func (e *SearchEngine) buildTenant(namespace string, settings map[string]any) (*tenantResources, error) {
	collection, err := settingString(settings, "collection")
	if err != nil {
		return nil, err
	}
	vectorSize, err := settingInteger(settings, "vector_size")
	if err != nil {
		return nil, err
	}
	encryptionKey := e.stores.Settings.EncryptionKey
	embeddingConnection, err := kbconfig.ModelConnectionForKey(settings, "embedding", encryptionKey)
	if err != nil {
		return nil, err
	}
	rerankerConnection, err := kbconfig.ModelConnectionForKey(settings, "reranker", encryptionKey)
	if err != nil {
		return nil, err
	}
	store, err := qdrant.NewStore(qdrant.StoreConfig{
		Collection:     collection,
		URL:            e.config.QdrantURL,
		Path:           e.config.QdrantPath,
		VectorSize:     vectorSize,
		Namespace:      namespace,
		ActiveVersions: e.activeVersions(namespace),
		HTTPClient:     e.httpClient,
	})
	if err != nil {
		return nil, err
	}
	embedder := modelapi.NewHTTPEmbedder(embeddingConnection, modelapi.DefaultTimeout)
	encoder := e.encoder
	reranker, err := retrieval.NewHTTPCrossEncoderReranker(rerankerConnection, retrieval.CrossEncoderOptions{
		ModelName:     rerankerConnection.Model,
		Device:        e.config.RerankerDevice,
		BatchSize:     e.config.RerankerBatchSize,
		MaxCandidates: e.config.RerankerMaxCandidates,
		MaxLength:     e.config.RerankerMaxLength,
		MaxTextChars:  e.config.RerankerMaxTextChars,
	}, modelapi.DefaultTimeout)
	if err != nil {
		return nil, err
	}
	orchestrator := retrieval.NewOrchestrator(
		map[contracts.RetrievalMode]retrieval.Retriever{
			contracts.RetrievalBM25:      retrieval.NewBm25Retriever(encoder, store),
			contracts.RetrievalEmbedding: retrieval.NewDenseRetriever(embedder, store),
			contracts.RetrievalHybrid:    retrieval.NewHybridRetriever(embedder, encoder, store),
		},
		map[contracts.RerankMode]retrieval.Reranker{
			contracts.RerankCrossEncoder: reranker,
		},
		runtimeImplementation,
		nil, // no Phase-1 mock limitations
		nil, // warmup is a no-op for HTTP models
		nil, // artifact projection is attached by the document/artifact layer
	)
	return &tenantResources{
		settings:     settings,
		collection:   collection,
		store:        store,
		orchestrator: orchestrator,
		embedding:    embeddingConnection,
		reranker:     rerankerConnection,
	}, nil
}

// activeVersions exposes the document heads of one namespace to Qdrant's
// publication filter. A state read failure is treated as "no active versions"
// so the query fails closed to legacy points instead of surfacing stale heads.
func (e *SearchEngine) activeVersions(namespace string) func() map[string]string {
	return func() map[string]string {
		versions, err := e.stores.Documents.VisibleVersions(context.Background(), namespace)
		if err != nil {
			return nil
		}
		return versions
	}
}

// settingString reads one required string setting from a knowledge-base row.
func settingString(settings map[string]any, key string) (string, error) {
	value, ok := settings[key]
	if !ok {
		return "", fmt.Errorf("knowledge base settings are missing %q", key)
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return "", fmt.Errorf("knowledge base settings %q must be a non-empty string", key)
	}
	return text, nil
}

// settingInteger reads one required integral setting. Rows read back from the
// state database decode numbers as float64, so integral floats are accepted.
func settingInteger(settings map[string]any, key string) (int, error) {
	value, ok := settings[key]
	if !ok {
		return 0, fmt.Errorf("knowledge base settings are missing %q", key)
	}
	switch typed := value.(type) {
	case int:
		return typed, nil
	case int64:
		return int(typed), nil
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) {
			break
		}
		return int(typed), nil
	case json.Number:
		text := string(typed)
		if parsed, err := strconv.Atoi(text); err == nil {
			return parsed, nil
		}
	}
	return 0, fmt.Errorf("knowledge base settings %q must be an integer", key)
}
