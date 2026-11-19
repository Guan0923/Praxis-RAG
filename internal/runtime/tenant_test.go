package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/qdrant"
	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/statelocktest"
	"github.com/Guan0923/Praxis-RAG/internal/statetest"
)

// testEncryptionKey is a deterministic base64-encoded 32-byte AES key used to
// protect the knowledge-base provider credentials stored in SQLite.
var testEncryptionKey = statetest.TestEncryptionKey

// testQueryPoints is the Qdrant query response used by the search tests.
const testQueryPoints = `[
	{"id":"p1","version":1,"score":0.9,"payload":{
		"record_type":"chunk","document_id":"doc-index","chunk_id":"chunk-index",
		"text":"文档索引包含分块和向量化","title":"index","source":"index.md",
		"metadata":{"kind":"test"},"upload_version":"v1"}},
	{"id":"p2","version":1,"score":0.4,"payload":{
		"record_type":"chunk","document_id":"doc-access","chunk_id":"chunk-access",
		"text":"知识库可以按文档分配读取权限","title":"permissions","source":"permissions.md",
		"metadata":{"kind":"test"},"upload_version":"v1"}}
]`

type recordedHTTP struct {
	Method string
	Path   string
	Header http.Header
	Body   map[string]any
}

type httpRecorder struct {
	mu       sync.Mutex
	requests []recordedHTTP
}

func (r *httpRecorder) all() []recordedHTTP {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := make([]recordedHTTP, len(r.requests))
	copy(copied, r.requests)
	return copied
}

func (r *httpRecorder) find(method, suffix string) []recordedHTTP {
	matches := make([]recordedHTTP, 0, 2)
	for _, request := range r.all() {
		if request.Method == method && strings.HasSuffix(request.Path, suffix) {
			matches = append(matches, request)
		}
	}
	return matches
}

func (r *httpRecorder) count(method, suffix string) int { return len(r.find(method, suffix)) }

// newFakeQdrantAPI serves the small Qdrant REST surface a search touches:
// collection exists/get and points/query.
func newFakeQdrantAPI(t *testing.T, pointsJSON string) (string, *httpRecorder) {
	t.Helper()
	if pointsJSON == "" {
		pointsJSON = "[]"
	}
	recorder := &httpRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		recorded := recordedHTTP{Method: request.Method, Path: request.URL.Path, Header: request.Header.Clone()}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &recorded.Body); err != nil {
				t.Errorf("invalid Qdrant request body: %v (%s)", err, raw)
			}
		}
		recorder.mu.Lock()
		recorder.requests = append(recorder.requests, recorded)
		recorder.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/exists"):
			_, _ = writer.Write([]byte(`{"result":{"exists":true}}`))
		case strings.HasSuffix(request.URL.Path, "/points/query"):
			_, _ = writer.Write([]byte(`{"result":{"points":` + pointsJSON + `}}`))
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/collections/"):
			_, _ = writer.Write([]byte(`{"result":{"config":{"params":{"sparse_vectors":{"sparse":{"modifier":"idf"}}}}}}`))
		default:
			_, _ = writer.Write([]byte(`{"result":true}`))
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, recorder
}

// newFakeModelAPI serves embeddings and rerank for the HTTP model clients.
func newFakeModelAPI(t *testing.T, handler func(path string, body map[string]any) (int, string)) (string, *httpRecorder) {
	t.Helper()
	if handler == nil {
		handler = defaultModelHandler
	}
	recorder := &httpRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		recorded := recordedHTTP{Method: request.Method, Path: request.URL.Path, Header: request.Header.Clone()}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &recorded.Body); err != nil {
				t.Errorf("invalid model request body: %v (%s)", err, raw)
			}
		}
		recorder.mu.Lock()
		recorder.requests = append(recorder.requests, recorded)
		recorder.mu.Unlock()
		status, response := handler(request.URL.Path, recorded.Body)
		if status == 0 {
			status = http.StatusOK
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return server.URL, recorder
}

func defaultModelHandler(path string, body map[string]any) (int, string) {
	switch {
	case strings.HasSuffix(path, "/embeddings"):
		inputs, _ := body["input"].([]any)
		data := make([]map[string]any, len(inputs))
		for index := range inputs {
			data[index] = map[string]any{"index": index, "embedding": []float64{1, 0, 0}}
		}
		encoded, _ := json.Marshal(map[string]any{"data": data})
		return http.StatusOK, string(encoded)
	case strings.HasSuffix(path, "/rerank"):
		documents, _ := body["documents"].([]any)
		results := make([]map[string]any, len(documents))
		for index := range documents {
			results[index] = map[string]any{"index": index, "relevance_score": 1.0 - 0.1*float64(index)}
		}
		encoded, _ := json.Marshal(map[string]any{"results": results})
		return http.StatusOK, string(encoded)
	case strings.HasSuffix(path, "/models"):
		return http.StatusOK, `{"data":[{"id":"embed-a"},{"id":"rank-a"}]}`
	default:
		return http.StatusNotFound, `{}`
	}
}

func unauthorizedModelHandler(string, map[string]any) (int, string) {
	return http.StatusUnauthorized, `{"detail":"private upstream error"}`
}

// newSearchTestStores opens the shared MySQL test store with a fixed
// encryption key and the in-memory document lock backend.
func newSearchTestStores(t *testing.T) *state.Stores {
	t.Helper()
	url, key := statetest.Connection(t)
	stores, err := state.New(state.StateSettings{
		URL:           url,
		EncryptionKey: key,
		AutoSchema:    true,
		PoolSize:      10,
		MaxOverflow:   20,
		PoolRecycle:   1800,
		PoolTimeout:   30,
	}, true)
	if err != nil {
		t.Fatalf("state.New(MySQL) error = %v", err)
	}
	// Document writes need the Redis lease backend; tests use the
	// deterministic in-memory fake.
	if err := stores.AttachDocumentLocker(statelocktest.NewFakeDocumentLocker()); err != nil {
		_ = stores.Close()
		t.Fatalf("AttachDocumentLocker() error = %v", err)
	}
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	return stores
}

// testKnowledgeBaseSettings builds a stored knowledge-base row pointing at the
// fake model provider, with protected (enc:v1) credentials.
func testKnowledgeBaseSettings(t *testing.T, namespace, modelURL, collection string) map[string]any {
	t.Helper()
	embeddingStored, err := kbconfig.StoredConnectionWithKey(modelapi.ModelConnection{
		BaseURL: modelURL, Model: "embed-a", APIKey: "embedding-key",
	}, testEncryptionKey)
	if err != nil {
		t.Fatalf("StoredConnectionWithKey(embedding) error = %v", err)
	}
	rerankerStored, err := kbconfig.StoredConnectionWithKey(modelapi.ModelConnection{
		BaseURL: modelURL, Model: "rank-a",
	}, testEncryptionKey)
	if err != nil {
		t.Fatalf("StoredConnectionWithKey(reranker) error = %v", err)
	}
	return map[string]any{
		"namespace":       namespace,
		"vector_size":     3,
		"embedding_model": "embed-a",
		"reranker_model":  "rank-a",
		"embedding":       embeddingStored,
		"reranker":        rerankerStored,
		"collection":      collection,
		"tokenizer":       "BAAI/bge-m3",
		"strategy":        "character",
		"chunk_size":      120,
		"chunk_overlap":   20,
		"created_at":      "2026-11-08T10:00:00.000000+00:00",
	}
}

// newSearchEngineForTest inserts one knowledge-base row and builds the engine.
func newSearchEngineForTest(t *testing.T, qdrantURL, modelURL string) (*SearchEngine, *state.Stores) {
	t.Helper()
	stores := newSearchTestStores(t)
	settings := testKnowledgeBaseSettings(t, "alpha", modelURL, "knowledge_test")
	if err := stores.KnowledgeBases.Create(context.Background(), settings); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	engine, err := NewSearchEngine(config.RagConfig{
		Backend:    config.BackendQdrant,
		QdrantURL:  qdrantURL,
		Collection: "mini_agent_knowledge_pdf_assets_v1",
		VectorSize: 1024,
	}, stores)
	if err != nil {
		t.Fatalf("NewSearchEngine() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	return engine, stores
}

func TestNewSearchEngineRejectsLocalPathMode(t *testing.T) {
	stores := newSearchTestStores(t)
	_, err := NewSearchEngine(config.RagConfig{
		Backend:    config.BackendQdrant,
		QdrantPath: filepath.Join(t.TempDir(), "qdrant"),
	}, stores)
	if !errors.Is(err, qdrant.ErrLocalPathUnsupported) {
		t.Fatalf("NewSearchEngine() error = %v, want ErrLocalPathUnsupported", err)
	}
}

func TestNewSearchEngineRequiresStores(t *testing.T) {
	_, err := NewSearchEngine(config.RagConfig{Backend: config.BackendQdrant}, nil)
	if err == nil || !strings.Contains(err.Error(), "shared state store") {
		t.Fatalf("NewSearchEngine() error = %v", err)
	}
}

func TestTenantBuildsFromKnowledgeBaseRow(t *testing.T) {
	qdrantURL, _ := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, _ := newFakeModelAPI(t, nil)
	engine, _ := newSearchEngineForTest(t, qdrantURL, modelURL)

	tenant, err := engine.tenant(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("tenant() error = %v", err)
	}
	if tenant.collection != "knowledge_test" {
		t.Errorf("collection = %q", tenant.collection)
	}
	if tenant.orchestrator == nil || tenant.store == nil {
		t.Fatalf("tenant resources incomplete: %+v", tenant)
	}
	if tenant.embedding.Model != "embed-a" || tenant.embedding.APIKey != "embedding-key" {
		t.Errorf("embedding connection = %+v", tenant.embedding)
	}
	if tenant.embedding.VectorSize == nil || *tenant.embedding.VectorSize != 3 {
		t.Errorf("embedding vector size = %v", tenant.embedding.VectorSize)
	}
	if tenant.reranker.Model != "rank-a" || tenant.reranker.VectorSize != nil {
		t.Errorf("reranker connection = %+v", tenant.reranker)
	}
}

func TestTenantCacheReuseAndInvalidate(t *testing.T) {
	qdrantURL, _ := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, _ := newFakeModelAPI(t, nil)
	engine, stores := newSearchEngineForTest(t, qdrantURL, modelURL)
	ctx := context.Background()

	first, err := engine.tenant(ctx, "alpha")
	if err != nil {
		t.Fatalf("tenant() error = %v", err)
	}
	second, err := engine.tenant(ctx, "alpha")
	if err != nil {
		t.Fatalf("tenant() error = %v", err)
	}
	if first != second {
		t.Fatalf("tenant cache was not reused")
	}

	// Simulate a configuration change followed by runtime invalidation.
	settings, err := stores.KnowledgeBases.Get(ctx, "alpha")
	if err != nil || settings == nil {
		t.Fatalf("KnowledgeBases.Get() = %v, %v", settings, err)
	}
	settings["collection"] = "knowledge_rebuilt"
	if err := stores.KnowledgeBases.Update(ctx, settings); err != nil {
		t.Fatalf("KnowledgeBases.Update() error = %v", err)
	}
	engine.Invalidate("alpha")
	third, err := engine.tenant(ctx, "alpha")
	if err != nil {
		t.Fatalf("tenant() after invalidate error = %v", err)
	}
	if third == first {
		t.Fatalf("Invalidate did not drop the cached tenant")
	}
	if third.collection != "knowledge_rebuilt" {
		t.Errorf("collection after invalidate = %q", third.collection)
	}
}

func TestTenantMissingKnowledgeBase(t *testing.T) {
	qdrantURL, _ := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, _ := newFakeModelAPI(t, nil)
	stores := newSearchTestStores(t)
	engine, err := NewSearchEngine(config.RagConfig{
		Backend: config.BackendQdrant, QdrantURL: qdrantURL, Collection: "unused",
	}, stores)
	if err != nil {
		t.Fatalf("NewSearchEngine() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	_ = modelURL

	_, err = engine.tenant(context.Background(), "missing")
	var validation *apierrors.KnowledgeValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("tenant() error = %v, want KnowledgeValidationError", err)
	}
	if validation.Message != kbconfig.MessageKnowledgeBaseMissing {
		t.Errorf("message = %q", validation.Message)
	}
}

func TestTenantReportsUnreadableCredentials(t *testing.T) {
	qdrantURL, _ := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, _ := newFakeModelAPI(t, nil)
	stores := newSearchTestStores(t)
	// Protect the row with a different key than the store's encryption key.
	otherKey := "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA="
	settings := testKnowledgeBaseSettings(t, "alpha", modelURL, "knowledge_test")
	protected, err := kbconfig.StoredConnectionWithKey(modelapi.ModelConnection{
		BaseURL: modelURL, Model: "embed-a", APIKey: "embedding-key",
	}, otherKey)
	if err != nil {
		t.Fatalf("StoredConnectionWithKey() error = %v", err)
	}
	settings["embedding"] = protected
	if err := stores.KnowledgeBases.Create(context.Background(), settings); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	engine, err := NewSearchEngine(config.RagConfig{
		Backend: config.BackendQdrant, QdrantURL: qdrantURL, Collection: "unused",
	}, stores)
	if err != nil {
		t.Fatalf("NewSearchEngine() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	_, err = engine.tenant(context.Background(), "alpha")
	if err == nil {
		t.Fatalf("tenant() accepted a credential protected with another key")
	}
	// Python lets the AEAD InvalidTag surface as an unhandled error (HTTP 500);
	// the Go engine keeps the same parity and never returns a partial tenant.
	if !strings.Contains(err.Error(), "cipher") && !strings.Contains(err.Error(), "authentication") {
		t.Logf("tenant() credential failure surfaced as %T: %v", err, err)
	}
}

func TestSearchEngineCloseIsIdempotentAndBlocksTenants(t *testing.T) {
	qdrantURL, _ := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, _ := newFakeModelAPI(t, nil)
	engine, _ := newSearchEngineForTest(t, qdrantURL, modelURL)
	ctx := context.Background()
	if _, err := engine.tenant(ctx, "alpha"); err != nil {
		t.Fatalf("tenant() error = %v", err)
	}
	if err := engine.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := engine.Close(ctx); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	_, err := engine.tenant(ctx, "alpha")
	var knowledge *apierrors.KnowledgeError
	if !errors.As(err, &knowledge) {
		t.Fatalf("tenant() after Close error = %v, want KnowledgeError", err)
	}
}
