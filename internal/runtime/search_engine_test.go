package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
)

func mustSearchRequest(t *testing.T, arguments map[string]any) contracts.SearchRequest {
	t.Helper()
	request, err := contracts.SearchRequestFromArguments(arguments)
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error = %v", err)
	}
	return request
}

// payloadResults reads the JSON-shaped results list ([]any after the runtime
// boundary normalization) and converts it for the existing assertions; the
// pre-normalization []map[string]any form stays accepted for fixture payloads.
func payloadResults(t *testing.T, payload map[string]any) []map[string]any {
	t.Helper()
	switch results := payload["results"].(type) {
	case []map[string]any:
		return results
	case []any:
		converted := make([]map[string]any, 0, len(results))
		for _, item := range results {
			hit, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("results item = %T %v", item, item)
			}
			converted = append(converted, hit)
		}
		return converted
	default:
		t.Fatalf("results = %T %v", payload["results"], payload["results"])
		return nil
	}
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
	if first["document_id"] != "doc-index" || first["chunk_id"] != "chunk-index" || first["source"] != "index.md" {
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
		"query": "如何索引文档", "retrieval": "bm25", "rerank": "none", "candidate_k": 5, "top_k": 1,
	})
	payload, err := engine.Search(context.Background(), "alpha", request)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	results := payloadResults(t, payload)
	if len(results) != 1 || results[0]["document_id"] != "doc-index" {
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
		"query": "文档索引", "retrieval": "hybrid", "rerank": "cross_encoder",
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
	if results[0]["document_id"] != "doc-index" || results[0]["score"] != 1.0 {
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
		"query": "索引", "retrieval": "hybrid", "rerank": "none",
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

func TestSearchSnapshotContext(t *testing.T) {
	qdrantURL, _ := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, _ := newFakeModelAPI(t, nil)
	engine, _ := newSearchEngineForTest(t, qdrantURL, modelURL)
	request := mustSearchRequest(t, map[string]any{
		"query": "证据", "retrieval": "embedding", "rerank": "none", "candidate_k": 5, "top_k": 2,
	})

	assertSnapshot := func(label string, payload map[string]any) {
		t.Helper()
		context, ok := payload["snapshot_context"].(map[string]any)
		if !ok {
			t.Fatalf("%s snapshot_context = %T %v", label, payload["snapshot_context"], payload["snapshot_context"])
		}
		want := map[string]any{
			"collection":            "knowledge_test",
			"embedding_model":       "embed-a",
			"reranker_model":        "rank-a",
			"vector_size":           int64(3),
			"chunk_tokenizer_model": "BAAI/bge-m3",
		}
		for key, value := range want {
			if context[key] != value {
				t.Errorf("%s snapshot_context[%q] = %v, want %v", label, key, context[key], value)
			}
		}
		if _, present := context["configuration_version"]; !present {
			t.Errorf("%s snapshot_context missing configuration_version: %v", label, context)
		}
		for _, leaked := range []string{"embedding", "reranker", "namespace", "strategy"} {
			if _, present := context[leaked]; present {
				t.Errorf("%s snapshot_context leaked %q: %v", label, leaked, context)
			}
		}
	}

	payload, err := engine.Search(context.Background(), "alpha", request)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	assertSnapshot("Search", payload)

	raw, err := engine.SearchRaw(context.Background(), "alpha", request)
	if err != nil {
		t.Fatalf("SearchRaw() error = %v", err)
	}
	assertSnapshot("SearchRaw", raw)
}

// TestNormalizeSearchPayloadShapes pins the JSON-shaped contract at the runtime
// boundary: orchestrator results are []map[string]any, while searchrefs requires
// []any. Nil payloads and result-less payloads must stay nil/absent, and
// already-normalized payloads must pass through untouched.
func TestNormalizeSearchPayloadShapes(t *testing.T) {
	if normalized := normalizeSearchPayload(nil); normalized != nil {
		t.Fatalf("normalizeSearchPayload(nil) = %v, want nil", normalized)
	}

	absent := map[string]any{"query": "索引"}
	normalizedAbsent := normalizeSearchPayload(absent)
	if _, present := normalizedAbsent["results"]; present {
		t.Fatalf("result-less payload gained a results key: %v", normalizedAbsent)
	}
	if normalizedAbsent["query"] != "索引" {
		t.Fatalf("result-less payload lost its other fields: %v", normalizedAbsent)
	}

	emptyTyped := map[string]any{"query": "索引", "results": []map[string]any{}}
	normalizedEmpty := normalizeSearchPayload(emptyTyped)
	emptyResults, ok := normalizedEmpty["results"].([]any)
	if !ok || emptyResults == nil || len(emptyResults) != 0 {
		t.Fatalf("empty typed results = %#v, want non-nil []any{}", normalizedEmpty["results"])
	}
	if _, stillTyped := emptyTyped["results"].([]map[string]any); !stillTyped {
		t.Fatalf("normalizeSearchPayload mutated its input: %#v", emptyTyped["results"])
	}

	typed := map[string]any{
		"query":            "索引",
		"snapshot_context": map[string]any{"collection": "knowledge_test"},
		"results": []map[string]any{
			{"rank": 1, "document_id": "doc-index"},
			{"rank": 2, "document_id": "doc-access"},
		},
	}
	normalized := normalizeSearchPayload(typed)
	results, ok := normalized["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("normalized results = %#v", normalized["results"])
	}
	first, ok := results[0].(map[string]any)
	if !ok || first["document_id"] != "doc-index" {
		t.Fatalf("normalized first result = %#v", results[0])
	}
	if normalized["query"] != "索引" {
		t.Fatalf("normalized payload lost query: %v", normalized)
	}
	if contextMap, ok := normalized["snapshot_context"].(map[string]any); !ok || contextMap["collection"] != "knowledge_test" {
		t.Fatalf("normalized payload lost snapshot_context: %v", normalized["snapshot_context"])
	}

	alreadyJSON := map[string]any{"results": []any{map[string]any{"rank": 1}}}
	normalizedAlready := normalizeSearchPayload(alreadyJSON)
	if results, ok := normalizedAlready["results"].([]any); !ok || len(results) != 1 {
		t.Fatalf("already-normalized results changed: %#v", normalizedAlready["results"])
	}
}

// TestSearchResultsAreJSONShaped pins the runtime-boundary contract: both the
// synchronous (captured) payload and the raw worker payload expose results as
// []any, matching Python's list of dicts and the snapshot/searchrefs contract.
func TestSearchResultsAreJSONShaped(t *testing.T) {
	qdrantURL, _ := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, _ := newFakeModelAPI(t, nil)
	engine, _ := newSearchEngineForTest(t, qdrantURL, modelURL)

	request := mustSearchRequest(t, map[string]any{
		"query": "证据", "retrieval": "embedding", "rerank": "none", "candidate_k": 5, "top_k": 2,
	})
	payload, err := engine.Search(context.Background(), "alpha", request)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	results, ok := payload["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("Search() results = %T %v, want []any of 2", payload["results"], payload["results"])
	}
	raw, err := engine.SearchRaw(context.Background(), "alpha", request)
	if err != nil {
		t.Fatalf("SearchRaw() error = %v", err)
	}
	rawResults, ok := raw["results"].([]any)
	if !ok || len(rawResults) != 2 {
		t.Fatalf("SearchRaw() results = %T %v, want []any of 2", raw["results"], raw["results"])
	}
}
