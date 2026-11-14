package retrieval

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/modelapi"
	"github.com/Guan0923/xinghe_rag/internal/qdrant"
)

type recordingRetriever struct {
	candidates []contracts.Candidate
	err        error
	query      string
	limit      int
	filters    map[string]any
	calls      int
}

func (r *recordingRetriever) Search(
	_ context.Context,
	query string,
	limit int,
	filters map[string]any,
) ([]contracts.Candidate, error) {
	r.calls++
	r.query = query
	r.limit = limit
	r.filters = filters
	return r.candidates, r.err
}

type recordingReranker struct {
	candidates []contracts.Candidate
	calls      int
}

func (r *recordingReranker) Rerank(
	_ context.Context,
	_ string,
	candidates []contracts.Candidate,
) ([]contracts.Candidate, error) {
	r.calls++
	return candidates, nil
}

func newTestOrchestrator(t *testing.T, retriever Retriever, reranker Reranker, implementation string) *Orchestrator {
	t.Helper()
	return NewOrchestrator(
		map[contracts.RetrievalMode]Retriever{
			contracts.RetrievalBM25:      retriever,
			contracts.RetrievalEmbedding: retriever,
			contracts.RetrievalHybrid:    retriever,
		},
		map[contracts.RerankMode]Reranker{contracts.RerankCrossEncoder: reranker},
		implementation, nil, nil, nil,
	)
}

func TestDefaultSearchReturnsCitableRRFEvidence(t *testing.T) {
	orchestrator, err := BuildDefaultOrchestrator(config.RagConfig{Backend: config.BackendMock})
	if err != nil {
		t.Fatalf("BuildDefaultOrchestrator() error: %v", err)
	}
	request, err := contracts.SearchRequestFromArguments(map[string]any{
		"query": "RRF 怎么融合 BM25 和 Dense？",
	})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error: %v", err)
	}
	if request.CandidateK != 10 {
		t.Fatalf("candidate_k = %d, want 10", request.CandidateK)
	}
	result, err := orchestrator.Search(context.Background(), request)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if result["retrieval"] != "hybrid" || result["rerank"] != "cross_encoder" {
		t.Errorf("mode echo = %v / %v", result["retrieval"], result["rerank"])
	}
	debug := result["debug"].(map[string]any)
	if debug["implementation"] != "mock-v1" {
		t.Errorf("implementation = %v", debug["implementation"])
	}
	limitations := debug["limitations"].([]string)
	if len(limitations) != 1 || !strings.Contains(limitations[0], "Phase-1 mocks") {
		t.Errorf("limitations = %v", limitations)
	}
	results := result["results"].([]map[string]any)
	if len(results) == 0 {
		t.Fatalf("results = %v", results)
	}
	first := results[0]
	if first["document_id"] != "doc-rrf" || first["chunk_id"] != "doc-rrf:1" ||
		first["source"] != "knowledge/rrf.md" {
		t.Errorf("first result = %v", first)
	}
	if first["rank"] != 1 {
		t.Errorf("rank = %v", first["rank"])
	}
	if result["evidence_status"] != "found" {
		t.Errorf("evidence_status = %v", result["evidence_status"])
	}
	if result["answer_guidance"] != "Answer from the returned evidence and cite source plus chunk_id." {
		t.Errorf("answer_guidance = %v", result["answer_guidance"])
	}
}

func TestSearchFilterIsAppliedBeforeRanking(t *testing.T) {
	orchestrator, err := BuildDefaultOrchestrator(config.RagConfig{Backend: config.BackendMock})
	if err != nil {
		t.Fatalf("BuildDefaultOrchestrator() error: %v", err)
	}
	request, err := contracts.SearchRequestFromArguments(map[string]any{
		"query":   "Agent Qdrant RRF",
		"filters": map[string]any{"document_id": "doc-qdrant"},
	})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error: %v", err)
	}
	result, err := orchestrator.Search(context.Background(), request)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	results := result["results"].([]map[string]any)
	documentIDs := make([]string, len(results))
	for index, item := range results {
		documentIDs[index] = item["document_id"].(string)
	}
	if !reflect.DeepEqual(documentIDs, []string{"doc-qdrant"}) {
		t.Errorf("document ids = %v", documentIDs)
	}
}

func TestEmptyRetrievalRequiresInsufficientEvidenceResponse(t *testing.T) {
	empty := &recordingRetriever{candidates: []contracts.Candidate{}}
	orchestrator := newTestOrchestrator(t, empty, NewMockReranker("cross_encoder"), "empty-test")
	request, err := contracts.SearchRequestFromArguments(map[string]any{
		"query": "不存在的内部知识", "rerank": "none",
	})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error: %v", err)
	}
	result, err := orchestrator.Search(context.Background(), request)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if result["evidence_status"] != "no_evidence" {
		t.Errorf("evidence_status = %v", result["evidence_status"])
	}
	results := result["results"].([]map[string]any)
	if len(results) != 0 {
		t.Errorf("results = %v", results)
	}
	if !strings.Contains(result["answer_guidance"].(string), "do not guess") {
		t.Errorf("answer_guidance = %v", result["answer_guidance"])
	}
}

func TestCandidateKAndTopKBounds(t *testing.T) {
	candidates := make([]contracts.Candidate, 10)
	for index := range candidates {
		candidates[index] = contracts.Candidate{
			DocumentID: fmt.Sprintf("doc-%02d", index),
			ChunkID:    fmt.Sprintf("chunk-%02d", index),
			Score:      float64(10 - index),
		}
	}
	retriever := &recordingRetriever{candidates: candidates}
	reranker := &recordingReranker{}
	orchestrator := newTestOrchestrator(t, retriever, reranker, "bounds-test")
	request, err := contracts.SearchRequestFromArguments(map[string]any{
		"query": "bounded", "candidate_k": 8, "top_k": 3, "rerank": "none",
	})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error: %v", err)
	}
	result, err := orchestrator.Search(context.Background(), request)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if retriever.limit != 8 {
		t.Errorf("retriever limit = %d, want candidate_k 8", retriever.limit)
	}
	results := result["results"].([]map[string]any)
	if len(results) != 3 {
		t.Errorf("returned %d results, want top_k 3", len(results))
	}
	debug := result["debug"].(map[string]any)
	if debug["candidate_count"] != 10 || debug["returned_count"] != 3 {
		t.Errorf("debug counts = %v", debug)
	}
	if debug["rerank_ms"] != float64(0) {
		t.Errorf("rerank_ms = %v, want 0", debug["rerank_ms"])
	}
	if reranker.calls != 0 {
		t.Errorf("rerank=none must not call the reranker")
	}
}

func TestUniqueAssetsDeduplicatesByAssetID(t *testing.T) {
	retriever := &recordingRetriever{candidates: []contracts.Candidate{
		{DocumentID: "doc-1", ChunkID: "chunk-1", Metadata: map[string]any{"asset_id": "asset-1"}, Score: 1},
		{DocumentID: "doc-2", ChunkID: "chunk-2", Metadata: map[string]any{"asset_id": "asset-1"}, Score: 0.9},
		{DocumentID: "doc-3", ChunkID: "chunk-3", Metadata: map[string]any{"asset_id": "asset-2"}, Score: 0.8},
		{DocumentID: "doc-4", ChunkID: "chunk-4", Metadata: map[string]any{}, Score: 0.7},
		{DocumentID: "doc-5", ChunkID: "chunk-5", Metadata: map[string]any{"asset_id": ""}, Score: 0.6},
	}}
	orchestrator := newTestOrchestrator(t, retriever, NewMockReranker("cross_encoder"), "assets-test")
	request, err := contracts.SearchRequestFromArguments(map[string]any{
		"query": "assets", "top_k": 10, "candidate_k": 10, "rerank": "none",
	})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error: %v", err)
	}
	result, err := orchestrator.Search(context.Background(), request)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	results := result["results"].([]map[string]any)
	documentIDs := make([]string, len(results))
	for index, item := range results {
		documentIDs[index] = item["document_id"].(string)
	}
	// asset-1 appears once, asset-2 once, then both chunk keys (chunk-4 and
	// chunk-5 with an empty asset id) stay distinct.
	want := []string{"doc-1", "doc-3", "doc-4", "doc-5"}
	if !reflect.DeepEqual(documentIDs, want) {
		t.Errorf("document ids = %v, want %v", documentIDs, want)
	}
}

func TestUnknownModesReturnErrors(t *testing.T) {
	orchestrator := NewOrchestrator(nil, nil, "test", nil, nil, nil)
	request := contracts.SearchRequest{Query: "q", Retrieval: contracts.RetrievalHybrid, Rerank: contracts.RerankNone}
	if _, err := orchestrator.Search(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "Unsupported retrieval mode") {
		t.Errorf("missing retriever error = %v", err)
	}
	retriever := &recordingRetriever{candidates: []contracts.Candidate{}}
	orchestrator = NewOrchestrator(
		map[contracts.RetrievalMode]Retriever{contracts.RetrievalHybrid: retriever},
		nil, "test", nil, nil, nil,
	)
	request.Rerank = contracts.RerankCrossEncoder
	if _, err := orchestrator.Search(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "Unsupported rerank mode") {
		t.Errorf("missing reranker error = %v", err)
	}
}

func TestWarmupAndProjectorHooks(t *testing.T) {
	retriever := &recordingRetriever{candidates: []contracts.Candidate{
		{DocumentID: "doc", ChunkID: "chunk", Title: "T", Source: "s", Text: "text", Score: 0.5,
			Metadata: map[string]any{}},
	}}
	warmed := false
	projected := 0
	orchestrator := NewOrchestrator(
		map[contracts.RetrievalMode]Retriever{contracts.RetrievalHybrid: retriever},
		map[contracts.RerankMode]Reranker{contracts.RerankCrossEncoder: NewMockReranker("cross_encoder")},
		"hooks",
		nil,
		func(context.Context) error { warmed = true; return nil },
		func(candidates []contracts.Candidate) []map[string]any {
			projected = len(candidates)
			return []map[string]any{{"custom": true}}
		},
	)
	if err := orchestrator.Warmup(context.Background()); err != nil {
		t.Fatalf("Warmup() error: %v", err)
	}
	if !warmed {
		t.Errorf("warmup hook not called")
	}
	request, err := contracts.SearchRequestFromArguments(map[string]any{"query": "q", "rerank": "none"})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error: %v", err)
	}
	result, err := orchestrator.Search(context.Background(), request)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if projected != 1 {
		t.Errorf("projector received %d candidates", projected)
	}
	results := result["results"].([]map[string]any)
	if len(results) != 1 || results[0]["custom"] != true {
		t.Errorf("projected results = %v", results)
	}
	debug := result["debug"].(map[string]any)
	if debug["returned_count"] != 1 || debug["candidate_count"] != 1 {
		t.Errorf("debug = %v", debug)
	}
}

func TestBuildDefaultOrchestratorQdrantRequiresConnections(t *testing.T) {
	_, err := BuildDefaultOrchestrator(config.RagConfig{Backend: config.BackendQdrant})
	if err == nil || !strings.Contains(err.Error(), "BuildQdrantOrchestrator") {
		t.Fatalf("error = %v", err)
	}
}

func TestMockBackendKeepsSAG2ExplicitlyUnavailable(t *testing.T) {
	orchestrator, err := BuildDefaultOrchestrator(config.RagConfig{Backend: config.BackendMock})
	if err != nil {
		t.Fatalf("BuildDefaultOrchestrator() error = %v", err)
	}
	request := contracts.SearchRequest{
		Query: "多跳", Retrieval: contracts.RetrievalSAG2, Rerank: contracts.RerankNone,
	}
	_, err = orchestrator.Search(context.Background(), request)
	if err == nil {
		t.Fatal("mock backend accepted sag2 retrieval")
	}
	var operationError *contracts.RagOperationError
	if !errors.As(err, &operationError) ||
		!strings.Contains(operationError.Message, "JEV") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildQdrantOrchestratorRejectsLocalPathMode(t *testing.T) {
	cfg := config.RagConfig{
		Backend:    config.BackendQdrant,
		Collection: "knowledge",
		QdrantPath: "local-qdrant",
		VectorSize: 1024,
	}
	_, err := BuildQdrantOrchestrator(cfg, modelapi.ModelConnection{}, modelapi.ModelConnection{})
	if !errors.Is(err, qdrant.ErrLocalPathUnsupported) {
		t.Fatalf("error = %v, want ErrLocalPathUnsupported", err)
	}
}

func TestBuildQdrantDependenciesWiresStore(t *testing.T) {
	cfg := config.RagConfig{
		Backend:    config.BackendQdrant,
		Collection: "knowledge",
		QdrantURL:  "http://localhost:6333",
		VectorSize: 1024,
	}
	dependencies, err := BuildQdrantDependencies(cfg, modelapi.ModelConnection{})
	if err != nil {
		t.Fatalf("BuildQdrantDependencies() error: %v", err)
	}
	if dependencies.Store == nil || dependencies.Embedder == nil || dependencies.Encoder == nil {
		t.Fatalf("dependencies = %+v", dependencies)
	}
	if dependencies.Store.VectorSize() != 1024 || dependencies.Store.Collection() != "knowledge" {
		t.Errorf("store = %d %q", dependencies.Store.VectorSize(), dependencies.Store.Collection())
	}
}

func TestBuildQdrantOrchestratorEndToEnd(t *testing.T) {
	qdrantPoints := `[{"id":"p1","score":0.9,"payload":{
		"record_type":"chunk","document_id":"doc-e03","chunk_id":"chunk-e03",
		"text":"错误代码 E03 表示水路供水不足","title":"e03","source":"e03.md",
		"metadata":{"kind":"test"},"upload_version":"v1"}}]`
	handler := func(request testRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/points/query") {
			return http.StatusOK, `{"result":{"points":` + qdrantPoints + `}}`
		}
		return 0, ""
	}
	_, qdrantServer := newTestQdrant(t, handler)
	embeddingJSON := make([]any, 1024)
	for index := range embeddingJSON {
		embeddingJSON[index] = 0
	}
	embeddingJSON[0] = 1
	modelBaseURL, modelRequests := newFakeModelServer(t, func(body map[string]any) (int, string) {
		if documents, ok := body["documents"].([]any); ok {
			results := make([]map[string]any, len(documents))
			for index := range documents {
				results[index] = map[string]any{"index": index, "relevance_score": 0.88 - float64(index)*0.1}
			}
			encoded := mustJSON(map[string]any{"results": results})
			return http.StatusOK, encoded
		}
		encoded := mustJSON(map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": embeddingJSON},
		}})
		return http.StatusOK, encoded
	})
	cfg := config.RagConfig{
		Backend:               config.BackendQdrant,
		Collection:            "knowledge",
		QdrantURL:             qdrantServer.URL,
		VectorSize:            1024,
		RerankerModel:         "BAAI/bge-reranker-base",
		RerankerBatchSize:     4,
		RerankerMaxCandidates: 30,
		RerankerMaxLength:     512,
		RerankerMaxTextChars:  4000,
	}
	connection := modelapi.ModelConnection{BaseURL: modelBaseURL, Model: "BAAI/bge-m3"}
	rerankerConnection := modelapi.ModelConnection{BaseURL: modelBaseURL, Model: "BAAI/bge-reranker-base"}
	orchestrator, err := BuildQdrantOrchestrator(cfg, connection, rerankerConnection)
	if err != nil {
		t.Fatalf("BuildQdrantOrchestrator() error: %v", err)
	}
	request, err := contracts.SearchRequestFromArguments(map[string]any{
		"query": "E03 怎么处理", "retrieval": "hybrid", "rerank": "cross_encoder",
		"candidate_k": 5, "top_k": 2,
	})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error: %v", err)
	}
	result, err := orchestrator.Search(context.Background(), request)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	debug := result["debug"].(map[string]any)
	if debug["implementation"] != "qdrant-hybrid-rerank-v1" {
		t.Errorf("implementation = %v", debug["implementation"])
	}
	results := result["results"].([]map[string]any)
	if len(results) != 1 || results[0]["document_id"] != "doc-e03" {
		t.Fatalf("results = %v", results)
	}
	if results[0]["score"] != 0.88 {
		t.Errorf("score = %v, want 0.88", results[0]["score"])
	}
	metadata := results[0]["metadata"].(map[string]any)
	if metadata["retrieval_score"] != 0.9 {
		t.Errorf("retrieval_score = %v, want 0.9", metadata["retrieval_score"])
	}
	if len(modelRequests.all()) != 2 {
		t.Errorf("model requests = %v", modelRequests.all())
	}
}
