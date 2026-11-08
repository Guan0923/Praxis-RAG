package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/kbconfig"
)

func mustSearchRequest(t *testing.T, arguments map[string]any) contracts.SearchRequest {
	t.Helper()
	request, err := contracts.SearchRequestFromArguments(arguments)
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error = %v", err)
	}
	return request
}

func payloadResults(t *testing.T, payload map[string]any) []map[string]any {
	t.Helper()
	results, ok := payload["results"].([]map[string]any)
	if !ok {
		t.Fatalf("results = %T %v", payload["results"], payload["results"])
	}
	return results
}

func payloadDebug(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	debug, ok := payload["debug"].(map[string]any)
	if !ok {
		t.Fatalf("debug = %T %v", payload["debug"], payload["debug"])
	}
	return debug
}

func lastQueryBody(t *testing.T, recorder *httpRecorder) map[string]any {
	t.Helper()
	queries := recorder.find(http.MethodPost, "/points/query")
	if len(queries) == 0 {
		t.Fatalf("no Qdrant points/query request recorded")
	}
	return queries[len(queries)-1].Body
}

func TestSearchEmbeddingPayload(t *testing.T) {
	qdrantURL, qdrantCalls := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, modelCalls := newFakeModelAPI(t, nil)
	engine, _ := newSearchEngineForTest(t, qdrantURL, modelURL)

	request := mustSearchRequest(t, map[string]any{
		"query": "证据", "retrieval": "embedding", "rerank": "none", "candidate_k": 5, "top_k": 2,
	})
	payload, err := engine.Search(context.Background(), "alpha", request)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if payload["query"] != "证据" || payload["retrieval"] != "embedding" || payload["rerank"] != "none" {
		t.Errorf("mode echo = %v", payload)
	}
	if payload["evidence_status"] != "found" {
		t.Errorf("evidence_status = %v", payload["evidence_status"])
	}
	if payload["answer_guidance"] != "Answer from the returned evidence and cite source plus chunk_id." {
		t.Errorf("answer_guidance = %v", payload["answer_guidance"])
	}
	results := payloadResults(t, payload)
	if len(results) != 2 {
		t.Fatalf("results = %v", results)
	}
	first := results[0]
	if first["document_id"] != "doc-e03" || first["chunk_id"] != "chunk-e03" || first["source"] != "e03.md" {
		t.Errorf("first result = %v", first)
	}
	if first["rank"] != 1 || first["score"] != 0.9 {
		t.Errorf("rank/score = %v/%v", first["rank"], first["score"])
	}
	debug := payloadDebug(t, payload)
	if debug["implementation"] != runtimeImplementation {
		t.Errorf("implementation = %v", debug["implementation"])
	}
	if debug["candidate_count"] != 2 || debug["returned_count"] != 2 {
		t.Errorf("debug counts = %v", debug)
	}

	body := lastQueryBody(t, qdrantCalls)
	if body["using"] != "dense" {
		t.Errorf("query using = %v", body["using"])
	}
	if vector, ok := body["query"].([]any); !ok || len(vector) != 3 {
		t.Errorf("dense query vector = %v", body["query"])
	}
	if body["with_payload"] != true {
		t.Errorf("with_payload = %v", body["with_payload"])
	}
	embeddings := modelCalls.find(http.MethodPost, "/embeddings")
	if len(embeddings) != 1 {
		t.Fatalf("embedding calls = %d", len(embeddings))
	}
	if embeddings[0].Header.Get("Authorization") != "Bearer embedding-key" {
		t.Errorf("embedding Authorization = %q", embeddings[0].Header.Get("Authorization"))
	}
	if embeddings[0].Body["model"] != "embed-a" {
		t.Errorf("embedding model = %v", embeddings[0].Body["model"])
	}
}

func TestSearchBM25Payload(t *testing.T) {
	qdrantURL, qdrantCalls := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, modelCalls := newFakeModelAPI(t, nil)
	engine, _ := newSearchEngineForTest(t, qdrantURL, modelURL)

	request := mustSearchRequest(t, map[string]any{
		"query": "E03 怎么处理", "retrieval": "bm25", "rerank": "none", "candidate_k": 5, "top_k": 1,
	})
	payload, err := engine.Search(context.Background(), "alpha", request)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	results := payloadResults(t, payload)
	if len(results) != 1 || results[0]["document_id"] != "doc-e03" {
		t.Fatalf("results = %v", results)
	}
	body := lastQueryBody(t, qdrantCalls)
	if body["using"] != "sparse" {
		t.Errorf("query using = %v", body["using"])
	}
	if _, ok := body["query"].(map[string]any); !ok {
		t.Errorf("sparse query = %v", body["query"])
	}
	if modelCalls.count(http.MethodPost, "/embeddings") != 0 {
		t.Errorf("bm25 must not call the embedding model")
	}
}

func TestSearchHybridWithCrossEncoder(t *testing.T) {
	qdrantURL, qdrantCalls := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, modelCalls := newFakeModelAPI(t, nil)
	engine, _ := newSearchEngineForTest(t, qdrantURL, modelURL)

	request := mustSearchRequest(t, map[string]any{
		"query": "E03 供水", "retrieval": "hybrid", "rerank": "cross_encoder",
		"candidate_k": 5, "top_k": 2,
	})
	payload, err := engine.Search(context.Background(), "alpha", request)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if payload["retrieval"] != "hybrid" || payload["rerank"] != "cross_encoder" {
		t.Errorf("mode echo = %v/%v", payload["retrieval"], payload["rerank"])
	}
	results := payloadResults(t, payload)
	if len(results) != 2 {
		t.Fatalf("results = %v", results)
	}
	if results[0]["document_id"] != "doc-e03" || results[0]["score"] != 1.0 {
		t.Errorf("reranked first result = %v", results[0])
	}
	metadata, _ := results[0]["metadata"].(map[string]any)
	if metadata["retrieval_score"] != 0.9 {
		t.Errorf("retrieval_score = %v, want 0.9", metadata["retrieval_score"])
	}

	body := lastQueryBody(t, qdrantCalls)
	prefetch, ok := body["prefetch"].([]any)
	if !ok || len(prefetch) != 2 {
		t.Fatalf("prefetch = %v", body["prefetch"])
	}
	fusion, ok := body["query"].(map[string]any)
	if !ok || fusion["fusion"] != "rrf" {
		t.Errorf("fusion query = %v", body["query"])
	}
	if modelCalls.count(http.MethodPost, "/embeddings") != 1 {
		t.Errorf("hybrid must embed the query once")
	}
	rerankCalls := modelCalls.find(http.MethodPost, "/rerank")
	if len(rerankCalls) != 1 {
		t.Fatalf("rerank calls = %d", len(rerankCalls))
	}
	documents, _ := rerankCalls[0].Body["documents"].([]any)
	if len(documents) != 2 {
		t.Errorf("rerank documents = %v", documents)
	}
	if rerankCalls[0].Body["model"] != "rank-a" {
		t.Errorf("rerank model = %v", rerankCalls[0].Body["model"])
	}
}

func TestSearchMapsQdrantFailuresToRagOperationError(t *testing.T) {
	qdrantURL := newFailingQdrantAPI(t)
	modelURL, _ := newFakeModelAPI(t, nil)
	engine, _ := newSearchEngineForTest(t, qdrantURL, modelURL)

	request := mustSearchRequest(t, map[string]any{
		"query": "证据", "retrieval": "embedding", "rerank": "none",
	})
	_, err := engine.Search(context.Background(), "alpha", request)
	var operation *contracts.RagOperationError
	if !errors.As(err, &operation) {
		t.Fatalf("Search() error = %v, want RagOperationError", err)
	}
	if !strings.Contains(operation.Message, "Dense retrieval failed") ||
		!strings.Contains(operation.Message, "HTTP 500") {
		t.Errorf("message = %q", operation.Message)
	}
}

func TestSearchMapsModelAPIErrorsToRagOperationError(t *testing.T) {
	qdrantURL, _ := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, _ := newFakeModelAPI(t, unauthorizedModelHandler)
	engine, _ := newSearchEngineForTest(t, qdrantURL, modelURL)

	// Hybrid re-raises ModelAPIError unchanged (Python's ModelAPIError
	// subclasses RagOperationError); the engine folds it into the HTTP 503
	// category with its message intact.
	request := mustSearchRequest(t, map[string]any{
		"query": "E03", "retrieval": "hybrid", "rerank": "none",
	})
	_, err := engine.Search(context.Background(), "alpha", request)
	var operation *contracts.RagOperationError
	if !errors.As(err, &operation) {
		t.Fatalf("Search() error = %v, want RagOperationError", err)
	}
	if operation.Message != "HTTP 401：认证失败，请检查 apikey" {
		t.Errorf("message = %q", operation.Message)
	}
}

func TestSearchMissingKnowledgeBase(t *testing.T) {
	qdrantURL, _ := newFakeQdrantAPI(t, testQueryPoints)
	stores := newSearchTestStores(t)
	engine, err := NewSearchEngine(config.RagConfig{
		Backend: config.BackendQdrant, QdrantURL: qdrantURL, Collection: "unused",
	}, stores)
	if err != nil {
		t.Fatalf("NewSearchEngine() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	request := mustSearchRequest(t, map[string]any{"query": "x", "rerank": "none"})
	_, err = engine.Search(context.Background(), "missing", request)
	var validation *apierrors.KnowledgeValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Search() error = %v, want KnowledgeValidationError", err)
	}
	if validation.Message != kbconfig.MessageKnowledgeBaseMissing {
		t.Errorf("message = %q", validation.Message)
	}
}

func TestPublicKnowledgeBase(t *testing.T) {
	qdrantURL, _ := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, _ := newFakeModelAPI(t, nil)
	engine, _ := newSearchEngineForTest(t, qdrantURL, modelURL)
	ctx := context.Background()

	public, err := engine.PublicKnowledgeBase(ctx, "alpha")
	if err != nil {
		t.Fatalf("PublicKnowledgeBase() error = %v", err)
	}
	if public["namespace"] != "alpha" || public["collection"] != "knowledge_test" {
		t.Errorf("public settings = %v", public)
	}
	if public["vector_size"] != float64(3) {
		t.Errorf("vector_size = %v (%T)", public["vector_size"], public["vector_size"])
	}
	embedding, ok := public["embedding"].(map[string]any)
	if !ok {
		t.Fatalf("embedding = %v", public["embedding"])
	}
	if embedding["base_url"] != modelURL || embedding["api_key_set"] != true {
		t.Errorf("public embedding = %v", embedding)
	}
	reranker, _ := public["reranker"].(map[string]any)
	if reranker["api_key_set"] != false {
		t.Errorf("public reranker = %v", reranker)
	}
	for key, value := range public {
		if key == "embedding" || key == "reranker" {
			continue
		}
		if text, ok := value.(string); ok && strings.Contains(text, "enc:v1:") {
			t.Errorf("public settings leaked a protected value: %v=%q", key, text)
		}
	}
	if _, present := public["protected_key"]; present {
		t.Errorf("public settings leaked protected_key")
	}

	if _, err := engine.PublicKnowledgeBase(ctx, "missing"); err == nil {
		t.Fatalf("PublicKnowledgeBase() accepted an unknown namespace")
	} else {
		var validation *apierrors.KnowledgeValidationError
		if !errors.As(err, &validation) || validation.Message != kbconfig.MessageKnowledgeBaseMissing {
			t.Errorf("PublicKnowledgeBase() error = %v", err)
		}
	}
}

func TestGetSearchResultUnavailableWithoutTransientStore(t *testing.T) {
	stores := newSearchTestStores(t)
	engine, err := NewSearchEngine(config.RagConfig{
		Backend: config.BackendQdrant, QdrantURL: "http://127.0.0.1:1", Collection: "unused",
	}, stores)
	if err != nil {
		t.Fatalf("NewSearchEngine() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	const resultID = "123e4567-e89b-12d3-a456-426614174000"
	_, err = engine.GetSearchResult(context.Background(), "alpha", resultID, 1, 10)
	var unavailable *apierrors.SnapshotUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("GetSearchResult() error = %v, want SnapshotUnavailable", err)
	}
	if unavailable.Message != "Search result expired or unavailable" {
		t.Errorf("message = %q", unavailable.Message)
	}

	if _, err := engine.GetSearchResult(context.Background(), "alpha", "not-a-uuid", 1, 10); err == nil {
		t.Fatalf("GetSearchResult() accepted an invalid result id")
	} else if !errors.As(err, &unavailable) {
		t.Errorf("invalid id error = %v, want SnapshotUnavailable", err)
	}
}

func TestGetSearchResultAvailable(t *testing.T) {
	stores := newSearchTestStores(t)
	transient := &fakeTransient{values: map[string]any{}}
	stores.Results.Transient = transient
	const resultID = "123e4567-e89b-12d3-a456-426614174000"
	stored := map[string]any{
		"query": "E03",
		"results": []any{
			map[string]any{"rank": 1, "document_id": "doc-1"},
			map[string]any{"rank": 2, "document_id": "doc-2"},
			map[string]any{"rank": 3, "document_id": "doc-3"},
		},
	}
	if _, err := stores.Results.Save("alpha", stored, strPtr(resultID), nil); err != nil {
		t.Fatalf("Results.Save() error = %v", err)
	}
	engine, err := NewSearchEngine(config.RagConfig{
		Backend: config.BackendQdrant, QdrantURL: "http://127.0.0.1:1", Collection: "unused",
	}, stores)
	if err != nil {
		t.Fatalf("NewSearchEngine() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	payload, err := engine.GetSearchResult(context.Background(), "alpha", resultID, 2, 2)
	if err != nil {
		t.Fatalf("GetSearchResult() error = %v", err)
	}
	if payload["page"] != 2 || payload["page_size"] != 2 || payload["total_results"] != 3 {
		t.Errorf("page payload = %v", payload)
	}
	results, _ := payload["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("page results = %v", results)
	}
	if next, present := payload["next_page"]; !present || next != nil {
		t.Errorf("next_page = %v", payload["next_page"])
	}

	firstPage, err := engine.GetSearchResult(context.Background(), "alpha", resultID, 1, 2)
	if err != nil {
		t.Fatalf("GetSearchResult(first page) error = %v", err)
	}
	if firstPage["next_page"] != 2 {
		t.Errorf("first page next_page = %v", firstPage["next_page"])
	}

	// A snapshot belonging to another tenant is unavailable, not readable.
	if _, err := engine.GetSearchResult(context.Background(), "beta", resultID, 1, 2); err == nil {
		t.Fatalf("GetSearchResult() read another tenant's snapshot")
	} else {
		var unavailable *apierrors.SnapshotUnavailable
		if !errors.As(err, &unavailable) {
			t.Errorf("cross-tenant error = %v, want SnapshotUnavailable", err)
		}
	}
}

func TestGetSearchResultPageDoesNotExist(t *testing.T) {
	stores := newSearchTestStores(t)
	stores.Results.Transient = &fakeTransient{values: map[string]any{}}
	const resultID = "123e4567-e89b-12d3-a456-426614174000"
	stored := map[string]any{"results": []any{map[string]any{"rank": 1}}}
	if _, err := stores.Results.Save("alpha", stored, strPtr(resultID), nil); err != nil {
		t.Fatalf("Results.Save() error = %v", err)
	}
	engine, err := NewSearchEngine(config.RagConfig{
		Backend: config.BackendQdrant, QdrantURL: "http://127.0.0.1:1", Collection: "unused",
	}, stores)
	if err != nil {
		t.Fatalf("NewSearchEngine() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	_, err = engine.GetSearchResult(context.Background(), "alpha", resultID, 9, 10)
	var validation *contracts.SearchValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("GetSearchResult() error = %v, want SearchValidationError", err)
	}
	if validation.Message != "Result page does not exist" {
		t.Errorf("message = %q", validation.Message)
	}
}

// newFailingQdrantAPI answers collection metadata but fails points/query with
// HTTP 500 so retrieval error wrapping can be asserted.
func newFailingQdrantAPI(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/exists"):
			_, _ = writer.Write([]byte(`{"result":{"exists":true}}`))
		case strings.HasSuffix(request.URL.Path, "/points/query"):
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"status":{"error":"qdrant unavailable"}}`))
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/collections/"):
			_, _ = writer.Write([]byte(`{"result":{"config":{"params":{"sparse_vectors":{"sparse":{}}}}}}`))
		default:
			_, _ = writer.Write([]byte(`{"result":true}`))
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func strPtr(value string) *string { return &value }

// fakeTransient is an in-memory state.TransientStore used to exercise the
// result-snapshot read path without Redis.
type fakeTransient struct {
	mu     sync.Mutex
	values map[string]any
}

func (f *fakeTransient) key(kind, identity string) string { return kind + ":" + identity }

func (f *fakeTransient) Get(kind, identity string) (any, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, found := f.values[f.key(kind, identity)]
	return value, found, nil
}

func (f *fakeTransient) Put(kind, identity string, value any, _ float64, nx bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := f.key(kind, identity)
	if _, exists := f.values[key]; exists && nx {
		return false, nil
	}
	f.values[key] = value
	return true, nil
}

func (f *fakeTransient) Delete(kind, identity string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := f.key(kind, identity)
	if _, exists := f.values[key]; !exists {
		return 0, nil
	}
	delete(f.values, key)
	return 1, nil
}
