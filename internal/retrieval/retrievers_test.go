package retrieval

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/modelapi"
	"github.com/Guan0923/xinghe_rag/internal/sparse"
)

const evidencePoints = `[
	{"id":"p1","score":0.9,"payload":{
		"record_type":"chunk","document_id":"doc-e03","chunk_id":"chunk-e03",
		"text":"错误代码 E03 表示水路供水不足","title":"e03","source":"e03.md",
		"metadata":{"kind":"test"},"upload_version":"v1"}},
	{"id":"p2","score":0.4,"payload":{
		"record_type":"chunk","document_id":"doc-temp","chunk_id":"chunk-temp",
		"text":"咖啡温度偏低时先预热杯子","title":"temperature","source":"temperature.md",
		"metadata":{"kind":"test"},"upload_version":"v1"}}
]`

func TestDenseRetrieverProjectsCitableEvidence(t *testing.T) {
	fake, server := newTestQdrant(t, queryResult(evidencePoints))
	store := newTestStore(t, server)
	embedder := &fakeQueryEmbedder{vector: []float64{0.25, 0.5, 0.75, 1}}
	results, err := NewDenseRetriever(embedder, store).Search(context.Background(), "证据", 5, nil)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %v", results)
	}
	first := results[0]
	if first.DocumentID != "doc-e03" || first.ChunkID != "chunk-e03" || first.Source != "e03.md" ||
		first.Text != "错误代码 E03 表示水路供水不足" || first.Score != 0.9 {
		t.Errorf("candidate = %+v", first)
	}
	if embedder.calls != 1 {
		t.Errorf("embed calls = %d", embedder.calls)
	}
	queries := fake.find(http.MethodPost, "/points/query")
	if len(queries) != 1 {
		t.Fatalf("expected one query request, got %d", len(queries))
	}
	body := queries[0].Body
	if body["using"] != "dense" {
		t.Errorf("using = %v", body["using"])
	}
	wantVector := []any{0.25, 0.5, 0.75, float64(1)}
	if !reflect.DeepEqual(body["query"], wantVector) {
		t.Errorf("query vector = %v", body["query"])
	}
	if body["limit"] != float64(5) || body["with_payload"] != true || body["with_vector"] != false {
		t.Errorf("query options = %v", body)
	}
}

func TestDenseRetrieverWrapsEmbedderErrors(t *testing.T) {
	_, server := newTestQdrant(t, nil)
	store := newTestStore(t, server)
	embedder := &fakeQueryEmbedder{err: fmt.Errorf("model down")}
	_, err := NewDenseRetriever(embedder, store).Search(context.Background(), "q", 5, nil)
	const want = "Dense retrieval failed: model down"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	assertRagOperationError(t, err)
}

func TestDenseRetrieverWrapsStoreErrors(t *testing.T) {
	handler := func(request testRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/points/query") {
			return http.StatusInternalServerError, `{"status":{"error":"boom"}}`
		}
		return 0, ""
	}
	_, server := newTestQdrant(t, handler)
	store := newTestStore(t, server)
	embedder := &fakeQueryEmbedder{vector: []float64{1, 0, 0, 0}}
	_, err := NewDenseRetriever(embedder, store).Search(context.Background(), "q", 5, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "Dense retrieval failed: ") || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("error = %v", err)
	}
	assertRagOperationError(t, err)
}

func TestBm25RetrieverPrioritizesAnExactErrorCode(t *testing.T) {
	fake, server := newTestQdrant(t, queryResult(evidencePoints))
	store := newTestStore(t, server)
	encoder := sparse.NewDefaultEncoder()
	results, err := NewBm25Retriever(encoder, store).Search(context.Background(), "E03 怎么处理", 5, nil)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 2 || results[0].DocumentID != "doc-e03" || results[0].Source != "e03.md" {
		t.Fatalf("results = %+v", results)
	}
	queries := fake.find(http.MethodPost, "/points/query")
	if len(queries) != 1 {
		t.Fatalf("expected one sparse query, got %d", len(queries))
	}
	body := queries[0].Body
	if body["using"] != "sparse" {
		t.Errorf("using = %v", body["using"])
	}
	queryVector, ok := body["query"].(map[string]any)
	if !ok || len(queryVector["indices"].([]any)) == 0 {
		t.Errorf("sparse query = %v", body["query"])
	}
}

func TestBm25RetrieverEmptyQueryIsNoop(t *testing.T) {
	_, server := newTestQdrant(t, nil)
	store := newTestStore(t, server)
	results, err := NewBm25Retriever(sparse.NewDefaultEncoder(), store).Search(context.Background(), "！！！", 5, nil)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %v", results)
	}
}

func TestBm25RetrieverWrapsErrors(t *testing.T) {
	handler := func(request testRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/points/query") {
			return http.StatusInternalServerError, `{"status":{"error":"boom"}}`
		}
		return 0, ""
	}
	_, server := newTestQdrant(t, handler)
	store := newTestStore(t, server)
	_, err := NewBm25Retriever(sparse.NewDefaultEncoder(), store).Search(context.Background(), "E03", 5, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "BM25 retrieval failed: ") {
		t.Fatalf("error = %v", err)
	}
	assertRagOperationError(t, err)
}

func TestHybridRetrieverUsesServerSideRRF(t *testing.T) {
	fake, server := newTestQdrant(t, queryResult(evidencePoints))
	store := newTestStore(t, server)
	embedder := &fakeQueryEmbedder{vector: []float64{1, 0, 0, 0}}
	results, err := NewHybridRetriever(embedder, sparse.NewDefaultEncoder(), store).
		Search(context.Background(), "E03 供水", 2, map[string]any{"document_id": "doc-e03"})
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 2 || results[0].DocumentID != "doc-e03" {
		t.Fatalf("results = %+v", results)
	}
	queries := fake.find(http.MethodPost, "/points/query")
	if len(queries) != 1 {
		t.Fatalf("expected one hybrid query, got %d", len(queries))
	}
	body := queries[0].Body
	prefetch, ok := body["prefetch"].([]any)
	if !ok || len(prefetch) != 2 {
		t.Fatalf("prefetch = %v", body["prefetch"])
	}
	fusion, ok := body["query"].(map[string]any)
	if !ok || fusion["fusion"] != "rrf" {
		t.Errorf("fusion query = %v", body["query"])
	}
	for _, entry := range prefetch {
		entryMap := entry.(map[string]any)
		filter := entryMap["filter"].(map[string]any)
		must := filter["must"].([]any)
		foundDocumentFilter := false
		for _, condition := range must {
			conditionMap := condition.(map[string]any)
			if conditionMap["key"] == "document_id" {
				foundDocumentFilter = true
			}
		}
		if !foundDocumentFilter {
			t.Errorf("prefetch filter missing document_id: %v", filter)
		}
	}
	if body["limit"] != float64(2) {
		t.Errorf("limit = %v", body["limit"])
	}
}

func TestHybridRetrieverFallsBackToDenseWithoutSparseTerms(t *testing.T) {
	fake, server := newTestQdrant(t, queryResult(evidencePoints))
	store := newTestStore(t, server)
	embedder := &fakeQueryEmbedder{vector: []float64{1, 0, 0, 0}}
	results, err := NewHybridRetriever(embedder, sparse.NewDefaultEncoder(), store).
		Search(context.Background(), "！！！", 2, nil)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %v", results)
	}
	queries := fake.find(http.MethodPost, "/points/query")
	if len(queries) != 1 {
		t.Fatalf("expected one dense fallback query, got %d", len(queries))
	}
	body := queries[0].Body
	if body["using"] != "dense" {
		t.Errorf("using = %v", body["using"])
	}
	if _, hasPrefetch := body["prefetch"]; hasPrefetch {
		t.Errorf("dense fallback must not send prefetch: %v", body["prefetch"])
	}
}

func TestHybridRetrieverWrapsEmbedderErrors(t *testing.T) {
	_, server := newTestQdrant(t, nil)
	store := newTestStore(t, server)
	embedder := &fakeQueryEmbedder{err: fmt.Errorf("model down")}
	_, err := NewHybridRetriever(embedder, sparse.NewDefaultEncoder(), store).
		Search(context.Background(), "E03", 5, nil)
	const want = "Hybrid retrieval failed: model down"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	assertRagOperationError(t, err)
}

func TestUnavailableRetrieverFailsSafely(t *testing.T) {
	_, err := NewUnavailableRetriever("retrieval not implemented yet").Search(context.Background(), "q", 5, nil)
	if err == nil || err.Error() != "retrieval not implemented yet" {
		t.Fatalf("error = %v", err)
	}
	assertRagOperationError(t, err)
}

func TestHybridRetrieverReRaisesModelAPIErrorsUnchanged(t *testing.T) {
	// Python's ModelAPIError subclasses RagOperationError, so the hybrid
	// retriever's `except RagOperationError: raise` must not add a prefix.
	_, server := newTestQdrant(t, nil)
	store := newTestStore(t, server)
	embedder := &fakeQueryEmbedder{err: &modelapi.ModelAPIError{Message: "模型服务请求超时"}}
	_, err := NewHybridRetriever(embedder, sparse.NewDefaultEncoder(), store).
		Search(context.Background(), "E03", 5, nil)
	if err == nil || err.Error() != "模型服务请求超时" {
		t.Fatalf("error = %v", err)
	}
	var modelError *modelapi.ModelAPIError
	if !errors.As(err, &modelError) {
		t.Fatalf("error type = %T", err)
	}
}

func TestDenseRetrieverWrapsModelAPIErrors(t *testing.T) {
	_, server := newTestQdrant(t, nil)
	store := newTestStore(t, server)
	embedder := &fakeQueryEmbedder{err: &modelapi.ModelAPIError{Message: "模型服务请求超时"}}
	_, err := NewDenseRetriever(embedder, store).Search(context.Background(), "q", 5, nil)
	const want = "Dense retrieval failed: 模型服务请求超时"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func assertRagOperationError(t *testing.T, err error) {
	t.Helper()
	var operationError *contracts.RagOperationError
	if !asRagOperationError(err, &operationError) {
		t.Fatalf("error %v is not a RagOperationError", err)
	}
}
