package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/chunking"
	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/statelocktest"
)

var knowledgeCollectionPattern = regexp.MustCompile(`^knowledge_[0-9a-f]{32}$`)

// kbModelHandler serves embeddings whose dimension depends on the model name:
// embed-a -> 3, embed-b -> 5. Sizes can be overridden per test.
func kbModelHandler(sizes map[string]int) func(path string, body map[string]any) (int, string) {
	if sizes == nil {
		sizes = map[string]int{"embed-a": 3, "embed-b": 5}
	}
	return func(path string, body map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/embeddings"):
			inputs, _ := body["input"].([]any)
			modelName, _ := body["model"].(string)
			size := sizes[modelName]
			if size == 0 {
				size = 3
			}
			data := make([]map[string]any, len(inputs))
			for index := range inputs {
				vector := make([]float64, size)
				vector[0] = 1
				data[index] = map[string]any{"index": index, "embedding": vector}
			}
			encoded, _ := json.Marshal(map[string]any{"data": data})
			return http.StatusOK, string(encoded)
		case strings.HasSuffix(path, "/rerank"):
			documents, _ := body["documents"].([]any)
			results := make([]map[string]any, len(documents))
			for index := range documents {
				results[index] = map[string]any{"index": index, "relevance_score": 0.5}
			}
			encoded, _ := json.Marshal(map[string]any{"results": results})
			return http.StatusOK, string(encoded)
		case strings.HasSuffix(path, "/models"):
			return http.StatusOK, `{"data":[{"id":"embed-a"},{"id":"embed-b"},{"id":"rank-a"}]}`
		default:
			return http.StatusNotFound, `{}`
		}
	}
}

// fakeKBQdrant models the collection lifecycle used by reembed_into: exists,
// create, scroll, upsert, delete. Source collections are registered by the
// test; staging collections only exist after a create request. The latest
// staged points are served by points/query so an upload-and-search test can
// observe the content that was actually indexed.
type fakeKBQdrant struct {
	mu         sync.Mutex
	requests   []recordedHTTP
	created    map[string]bool
	sources    map[string]bool
	scroll     string
	failUpsert bool
	staged     []map[string]any
}

func (f *fakeKBQdrant) all() []recordedHTTP {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := make([]recordedHTTP, len(f.requests))
	copy(copied, f.requests)
	return copied
}

func (f *fakeKBQdrant) find(method, suffix string) []recordedHTTP {
	matches := make([]recordedHTTP, 0, 2)
	for _, request := range f.all() {
		if request.Method == method && strings.HasSuffix(request.Path, suffix) {
			matches = append(matches, request)
		}
	}
	return matches
}

func (f *fakeKBQdrant) addSource(collection string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sources[collection] = true
}

func (f *fakeKBQdrant) wasCreated(collection string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created[collection]
}

func newFakeKBQdrant(t *testing.T, scrollJSON string) (*fakeKBQdrant, string) {
	t.Helper()
	fake := &fakeKBQdrant{created: map[string]bool{}, sources: map[string]bool{}, scroll: scrollJSON}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		recorded := recordedHTTP{Method: request.Method, Path: request.URL.Path, Header: request.Header.Clone()}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &recorded.Body); err != nil {
				t.Errorf("invalid Qdrant request body: %v (%s)", err, raw)
			}
		}
		fake.mu.Lock()
		fake.requests = append(fake.requests, recorded)
		fake.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		path := request.URL.Path
		switch {
		case strings.HasSuffix(path, "/exists"):
			name := collectionFromPath(path)
			fake.mu.Lock()
			exists := fake.sources[name] || fake.created[name]
			fake.mu.Unlock()
			encoded, _ := json.Marshal(map[string]any{"result": map[string]any{"exists": exists}})
			_, _ = writer.Write(encoded)
		case request.Method == http.MethodPut && strings.HasSuffix(path, "/points"):
			fake.mu.Lock()
			fail := fake.failUpsert
			fake.mu.Unlock()
			if fail {
				writer.WriteHeader(http.StatusInternalServerError)
				_, _ = writer.Write([]byte(`{"status":{"error":"upsert failed"}}`))
				return
			}
			// Remember the last staged version so points/query can return the
			// chunks this upload actually indexed.
			if points, ok := recorded.Body["points"].([]any); ok {
				staged := make([]map[string]any, 0, len(points))
				for _, item := range points {
					if point, ok := item.(map[string]any); ok {
						staged = append(staged, point)
					}
				}
				fake.mu.Lock()
				fake.staged = staged
				fake.mu.Unlock()
			}
			_, _ = writer.Write([]byte(`{"result":{"status":"completed"}}`))
		case strings.HasSuffix(path, "/points/query"):
			fake.mu.Lock()
			staged := make([]map[string]any, 0, len(fake.staged))
			for index, point := range fake.staged {
				scored := make(map[string]any, len(point)+1)
				for key, value := range point {
					scored[key] = value
				}
				scored["score"] = 1.0 - float64(index)*0.01
				staged = append(staged, scored)
			}
			fake.mu.Unlock()
			encoded, _ := json.Marshal(map[string]any{"result": map[string]any{"points": staged}})
			_, _ = writer.Write(encoded)
		case request.Method == http.MethodDelete:
			name := collectionFromPath(path)
			fake.mu.Lock()
			delete(fake.created, name)
			fake.mu.Unlock()
			_, _ = writer.Write([]byte(`{"result":true}`))
		case strings.HasSuffix(path, "/points/scroll"):
			fake.mu.Lock()
			scroll := fake.scroll
			fake.mu.Unlock()
			if scroll == "" {
				_, _ = writer.Write([]byte(`{"result":{"points":[],"next_page_offset":null}}`))
				return
			}
			_, _ = writer.Write([]byte(`{"result":{"points":` + scroll + `,"next_page_offset":null}}`))
		case request.Method == http.MethodPut:
			name := collectionFromPath(path)
			fake.mu.Lock()
			fake.created[name] = true
			fake.mu.Unlock()
			_, _ = writer.Write([]byte(`{"result":true}`))
		case request.Method == http.MethodGet && strings.HasPrefix(path, "/collections/"):
			_, _ = writer.Write([]byte(`{"result":{"config":{"params":{"sparse_vectors":{"sparse":{"modifier":"idf"}}}}}}`))
		default:
			_, _ = writer.Write([]byte(`{"result":true}`))
		}
	}))
	t.Cleanup(server.Close)
	return fake, server.URL
}

// zeroNumber reports whether value is a numeric zero (read views decode
// numbers from cache JSON as float64).
func zeroNumber(value any) bool {
	switch typed := value.(type) {
	case int:
		return typed == 0
	case int64:
		return typed == 0
	case float64:
		return typed == 0
	default:
		return false
	}
}

func collectionFromPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/collections/")
	if index := strings.Index(trimmed, "/"); index >= 0 {
		return trimmed[:index]
	}
	return trimmed
}

// newKBService builds a Service over a real SQLite store.
func newKBService(t *testing.T, qdrantURL string) (*Service, *state.Stores) {
	t.Helper()
	stores := newSearchTestStores(t)
	service, err := New(config.RagConfig{
		Backend:    config.BackendQdrant,
		QdrantURL:  qdrantURL,
		Collection: "mini_agent_knowledge_pdf_assets_v1",
		VectorSize: 1024,
	}, stores, t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	return service, stores
}

func testCreateInput(namespace, modelURL string) kbconfig.CreateInput {
	return kbconfig.CreateInput{
		Namespace:      namespace,
		Chunking:       chunking.Config{Strategy: "character", ChunkSize: 120, ChunkOverlap: 20},
		EmbeddingModel: "embed-a",
		EmbeddingValues: map[string]any{
			"base_url": modelURL, "apikey": "private-key",
		},
		CreatedAt: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC),
	}
}

func TestCreateKnowledgeBase(t *testing.T) {
	modelURL, modelCalls := newFakeModelAPI(t, kbModelHandler(nil))
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()

	public, err := service.CreateKnowledgeBase(ctx, testCreateInput("alpha", modelURL))
	if err != nil {
		t.Fatalf("CreateKnowledgeBase() error = %v", err)
	}
	if public["namespace"] != "alpha" || public["embedding_model"] != "embed-a" {
		t.Errorf("public settings = %v", public)
	}
	if public["vector_size"] != 3 {
		t.Errorf("vector_size = %v (%T)", public["vector_size"], public["vector_size"])
	}
	collection, _ := public["collection"].(string)
	if !knowledgeCollectionPattern.MatchString(collection) {
		t.Errorf("collection = %q", collection)
	}
	if public["strategy"] != "character" || public["chunk_size"] != 120 || public["chunk_overlap"] != 20 {
		t.Errorf("chunking = %v", public)
	}
	if public["created_at"] != "2026-11-08T10:00:00.000000+00:00" {
		t.Errorf("created_at = %v", public["created_at"])
	}
	embedding, _ := public["embedding"].(map[string]any)
	if embedding["base_url"] != modelURL || embedding["api_key_set"] != true {
		t.Errorf("public embedding = %v", embedding)
	}
	if _, present := public["ocr_model"]; present {
		t.Errorf("public settings still expose ocr_model = %v", public["ocr_model"])
	}
	if _, leaked := public["protected_key"]; leaked {
		t.Errorf("public settings leaked protected_key")
	}

	stored, err := stores.KnowledgeBases.Get(ctx, "alpha")
	if err != nil || stored == nil {
		t.Fatalf("KnowledgeBases.Get() = %v, %v", stored, err)
	}
	storedEmbedding, _ := stored["embedding"].(map[string]any)
	protected, _ := storedEmbedding["protected_key"].(string)
	if !strings.HasPrefix(protected, "enc:v1:") || strings.Contains(protected, "private-key") {
		t.Fatalf("stored protected_key = %q", protected)
	}
	connection, err := kbconfig.ModelConnectionForKey(stored, "embedding", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey() error = %v", err)
	}
	if connection.APIKey != "private-key" || connection.Model != "embed-a" {
		t.Errorf("stored connection = %+v", connection)
	}

	embeddings := modelCalls.find(http.MethodPost, "/embeddings")
	if len(embeddings) != 1 {
		t.Fatalf("embedding probe calls = %d", len(embeddings))
	}
	if embeddings[0].Header.Get("Authorization") != "Bearer private-key" {
		t.Errorf("probe Authorization = %q", embeddings[0].Header.Get("Authorization"))
	}
	if embeddings[0].Body["model"] != "embed-a" {
		t.Errorf("probe model = %v", embeddings[0].Body["model"])
	}
}

func TestCreateKnowledgeBaseWithRerankerProbe(t *testing.T) {
	modelURL, modelCalls := newFakeModelAPI(t, kbModelHandler(nil))
	service, _ := newKBService(t, "http://127.0.0.1:1")
	input := testCreateInput("alpha", modelURL)
	input.RerankerModel = "rank-a"
	input.RerankerValues = map[string]any{"base_url": modelURL, "apikey": "reranker-key"}
	public, err := service.CreateKnowledgeBase(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateKnowledgeBase() error = %v", err)
	}
	reranker, _ := public["reranker"].(map[string]any)
	if reranker["base_url"] != modelURL || reranker["api_key_set"] != true {
		t.Errorf("public reranker = %v", reranker)
	}
	rerankCalls := modelCalls.find(http.MethodPost, "/rerank")
	if len(rerankCalls) != 1 {
		t.Fatalf("reranker probe calls = %d", len(rerankCalls))
	}
	if rerankCalls[0].Header.Get("Authorization") != "Bearer reranker-key" {
		t.Errorf("reranker probe Authorization = %q", rerankCalls[0].Header.Get("Authorization"))
	}
}

func TestCreateKnowledgeBaseRejectsDuplicatesAndDocumentWorkspaces(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	if _, err := service.CreateKnowledgeBase(ctx, testCreateInput("alpha", modelURL)); err != nil {
		t.Fatalf("first CreateKnowledgeBase() error = %v", err)
	}
	duplicate := testCreateInput("alpha", modelURL)
	duplicate.Chunking.ChunkSize = 300
	_, err := service.CreateKnowledgeBase(ctx, duplicate)
	var validation *apierrors.KnowledgeValidationError
	if !errors.As(err, &validation) || validation.Message != kbconfig.MessageKnowledgeBaseExists {
		t.Fatalf("duplicate error = %v", err)
	}
	stored, _ := stores.KnowledgeBases.Get(ctx, "alpha")
	if stored["chunk_size"] != float64(120) {
		t.Errorf("existing configuration was overwritten: %v", stored["chunk_size"])
	}

	// A namespace with document workspaces but no configuration row is rejected.
	record := state.DocumentRecord{
		Name: "existing.txt", DocumentID: "doc-existing", Size: 4, ModifiedAt: "2026-11-08T10:00:00",
		IndexedAt: "2026-11-08T10:00:00", ChunkCount: 1, Collection: "old", SHA256: "abc",
		Chunking: map[string]any{},
	}
	if err := stores.Documents.Upsert(ctx, "orphan", record); err != nil {
		t.Fatalf("Documents.Upsert() error = %v", err)
	}
	_, err = service.CreateKnowledgeBase(ctx, testCreateInput("orphan", modelURL))
	if !errors.As(err, &validation) || validation.Message != kbconfig.MessageNamespaceHasDocuments {
		t.Fatalf("orphan namespace error = %v", err)
	}
	if stored, _ := stores.KnowledgeBases.Get(ctx, "orphan"); stored != nil {
		t.Fatalf("orphan knowledge base was created")
	}
}

func TestCreateKnowledgeBaseValidationAndProbeErrors(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	service, _ := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()

	invalid := testCreateInput("bad namespace", modelURL)
	if _, err := service.CreateKnowledgeBase(ctx, invalid); err == nil {
		t.Fatalf("invalid namespace was accepted")
	} else {
		var validation *apierrors.KnowledgeValidationError
		if !errors.As(err, &validation) || validation.Message != kbconfig.MessageNamespaceInvalid {
			t.Errorf("invalid namespace error = %v", err)
		}
	}

	emptyModel := testCreateInput("alpha", modelURL)
	emptyModel.EmbeddingModel = " "
	if _, err := service.CreateKnowledgeBase(ctx, emptyModel); err == nil {
		t.Fatalf("empty embedding model was accepted")
	} else {
		var validation *apierrors.KnowledgeValidationError
		if !errors.As(err, &validation) || validation.Message != kbconfig.MessageEmbeddingModelRequired {
			t.Errorf("empty model error = %v", err)
		}
	}

	failingURL, _ := newFakeModelAPI(t, func(string, map[string]any) (int, string) {
		return http.StatusUnauthorized, `{"detail":"private upstream error"}`
	})
	if _, err := service.CreateKnowledgeBase(ctx, testCreateInput("beta", failingURL)); err == nil {
		t.Fatalf("failing provider was accepted")
	} else {
		var validation *apierrors.KnowledgeValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("provider error = %v, want KnowledgeValidationError (Python ModelAPIError -> 400)", err)
		}
		if validation.Message != "HTTP 401：认证失败，请检查 apikey" {
			t.Errorf("provider message = %q", validation.Message)
		}
	}
}

func TestListKnowledgeBasesReturnsPublicProjection(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	service, _ := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()

	empty, err := service.ListKnowledgeBases(ctx)
	if err != nil {
		t.Fatalf("ListKnowledgeBases() error = %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("initial list = %v", empty)
	}
	for _, namespace := range []string{"beta", "alpha"} {
		if _, err := service.CreateKnowledgeBase(ctx, testCreateInput(namespace, modelURL)); err != nil {
			t.Fatalf("CreateKnowledgeBase(%s) error = %v", namespace, err)
		}
	}
	items, err := service.ListKnowledgeBases(ctx)
	if err != nil {
		t.Fatalf("ListKnowledgeBases() error = %v", err)
	}
	if len(items) != 2 || items[0]["namespace"] != "alpha" || items[1]["namespace"] != "beta" {
		t.Fatalf("items = %v", items)
	}
	first := items[0]
	configured, _ := first["configured"].(bool)
	if !configured || zeroNumber(first["document_count"]) == false {
		t.Errorf("merged fields = %v", first)
	}
	embedding, _ := first["embedding"].(map[string]any)
	if embedding["api_key_set"] != true {
		t.Errorf("public embedding = %v", embedding)
	}
	if _, present := first["ocr_model"]; present {
		t.Errorf("listing still exposes ocr_model = %v", first["ocr_model"])
	}
	encoded, _ := json.Marshal(items)
	if strings.Contains(string(encoded), "enc:v1:") || strings.Contains(string(encoded), "private-key") {
		t.Errorf("listing leaked credentials: %s", encoded)
	}
}

func TestUpdateKnowledgeBaseKeyOnlyDoesNotReembed(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	service, _ := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	created, err := service.CreateKnowledgeBase(ctx, testCreateInput("alpha", modelURL))
	if err != nil {
		t.Fatalf("CreateKnowledgeBase() error = %v", err)
	}
	updated, err := service.UpdateKnowledgeBase(ctx, "alpha", kbconfig.UpdateInput{
		Values: map[string]any{"embedding": map[string]any{"apikey": "new-key"}},
	})
	if err != nil {
		t.Fatalf("UpdateKnowledgeBase() error = %v", err)
	}
	if updated["collection"] != created["collection"] {
		t.Errorf("key-only update changed the collection: %v", updated["collection"])
	}
	if _, present := updated["reembedded_at"]; present {
		t.Errorf("key-only update recorded reembedded_at")
	}
	key, err := service.KnowledgeBaseKey(ctx, "alpha", "embedding")
	if err != nil {
		t.Fatalf("KnowledgeBaseKey() error = %v", err)
	}
	if key != "new-key" {
		t.Errorf("key = %q", key)
	}
}

func TestUpdateKnowledgeBaseRejectsEmbeddingChangeWithoutReembed(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	created, err := service.CreateKnowledgeBase(ctx, testCreateInput("alpha", modelURL))
	if err != nil {
		t.Fatalf("CreateKnowledgeBase() error = %v", err)
	}
	_, err = service.UpdateKnowledgeBase(ctx, "alpha", kbconfig.UpdateInput{
		Values: map[string]any{"embedding_model": "embed-b"},
	})
	var validation *apierrors.KnowledgeValidationError
	if !errors.As(err, &validation) || validation.Message != kbconfig.MessageEmbeddingChangeRequiresReembed {
		t.Fatalf("update error = %v", err)
	}
	stored, _ := stores.KnowledgeBases.Get(ctx, "alpha")
	if stored["embedding_model"] != "embed-a" || stored["collection"] != created["collection"] {
		t.Errorf("rejected update changed settings: %v", stored)
	}
}

const kbScrollPoints = `[
	{"id":"11111111-1111-5111-8111-111111111111",
	 "payload":{"record_type":"chunk","document_id":"Doc","chunk_id":"chunk-1","text":"active",
		"upload_version":"active"},
	 "vector":{"dense":[0,0,0,1],"sparse":{"indices":[7],"values":[1.0]}}},
	{"id":"22222222-2222-5222-8222-222222222222",
	 "payload":{"record_type":"asset","asset_id":"a1","document_id":"Doc","text":"asset",
		"upload_version":"active"},
	 "vector":{}},
	{"id":"33333333-3333-5333-8333-333333333333",
	 "payload":{"record_type":"chunk","document_id":"doc","chunk_id":"chunk-2","text":"legacy"},
	 "vector":{"dense":[0,0,1,0]}}
]`

func seedReembedDocuments(t *testing.T, stores *state.Stores) {
	t.Helper()
	for _, record := range []state.DocumentRecord{
		{Name: "Doc.txt", DocumentID: "Doc", ChunkCount: 1, Chunking: map[string]any{}},
		{Name: "doc.txt", DocumentID: "doc", ChunkCount: 1, Chunking: map[string]any{}},
	} {
		if err := stores.Documents.Upsert(context.Background(), "alpha", record); err != nil {
			t.Fatalf("Documents.Upsert() error = %v", err)
		}
	}
}

func TestUpdateKnowledgeBaseWithReembedPublishesStaging(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	qdrant, qdrantURL := newFakeKBQdrant(t, kbScrollPoints)
	service, stores := newKBService(t, qdrantURL)
	ctx := context.Background()
	created, err := service.CreateKnowledgeBase(ctx, testCreateInput("alpha", modelURL))
	if err != nil {
		t.Fatalf("CreateKnowledgeBase() error = %v", err)
	}
	oldCollection := created["collection"].(string)
	qdrant.addSource(oldCollection)
	seedReembedDocuments(t, stores)

	// Warm the tenant cache so Invalidate has something to drop.
	if _, err := service.search.tenant(ctx, "alpha"); err != nil {
		t.Fatalf("tenant() error = %v", err)
	}

	updated, err := service.UpdateKnowledgeBase(ctx, "alpha", kbconfig.UpdateInput{
		Values:  map[string]any{"embedding_model": "embed-b"},
		Reembed: true,
	})
	if err != nil {
		t.Fatalf("UpdateKnowledgeBase() error = %v", err)
	}
	staging, _ := updated["collection"].(string)
	if staging == oldCollection || !knowledgeCollectionPattern.MatchString(staging) {
		t.Fatalf("collection after reembed = %q (old %q)", staging, oldCollection)
	}
	if updated["embedding_model"] != "embed-b" || updated["vector_size"] != 5 {
		t.Errorf("updated model/vector = %v/%v", updated["embedding_model"], updated["vector_size"])
	}
	if reembeddedAt, ok := updated["reembedded_at"].(string); !ok || reembeddedAt == "" {
		t.Errorf("reembedded_at = %v", updated["reembedded_at"])
	}
	stored, _ := stores.KnowledgeBases.Get(ctx, "alpha")
	if stored["collection"] != staging || stored["embedding_model"] != "embed-b" {
		t.Errorf("stored settings = %v", stored)
	}

	// The staging collection was created and received re-embedded points; the
	// old collection was not deleted or rewritten.
	if !qdrant.wasCreated(staging) {
		t.Errorf("staging collection %q was not created", staging)
	}
	upserts := qdrant.find(http.MethodPut, "/collections/"+staging+"/points")
	if len(upserts) == 0 {
		t.Fatalf("no staging upsert recorded")
	}
	points, _ := upserts[0].Body["points"].([]any)
	if len(points) != 3 {
		t.Fatalf("staging points = %v", upserts[0].Body["points"])
	}
	first := points[0].(map[string]any)
	vector, _ := first["vector"].(map[string]any)
	dense, _ := vector["dense"].([]any)
	if len(dense) != 5 {
		t.Errorf("re-embedded dense length = %d, want 5", len(dense))
	}
	if _, ok := vector["sparse"]; !ok {
		t.Errorf("sparse vector was not preserved: %v", vector)
	}
	payload, _ := first["payload"].(map[string]any)
	if payload["document_id"] != "Doc" {
		t.Errorf("staging payload = %v", payload)
	}
	if len(qdrant.find(http.MethodDelete, "/collections/"+oldCollection)) != 0 {
		t.Errorf("old collection must be retained")
	}

	// Invalidate dropped the cached tenant: the next tenant points at staging.
	tenant, err := service.search.tenant(ctx, "alpha")
	if err != nil {
		t.Fatalf("tenant() after update error = %v", err)
	}
	if tenant.collection != staging {
		t.Errorf("tenant collection = %q, want staging %q", tenant.collection, staging)
	}
	if tenant.embedding.VectorSize == nil || *tenant.embedding.VectorSize != 5 {
		t.Errorf("tenant embedding vector size = %v", tenant.embedding.VectorSize)
	}
}

func TestUpdateKnowledgeBaseReembedFailureDiscardsStaging(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	qdrant, qdrantURL := newFakeKBQdrant(t, kbScrollPoints)
	qdrant.failUpsert = true
	service, stores := newKBService(t, qdrantURL)
	ctx := context.Background()
	created, err := service.CreateKnowledgeBase(ctx, testCreateInput("alpha", modelURL))
	if err != nil {
		t.Fatalf("CreateKnowledgeBase() error = %v", err)
	}
	oldCollection := created["collection"].(string)
	qdrant.addSource(oldCollection)
	seedReembedDocuments(t, stores)

	_, err = service.UpdateKnowledgeBase(ctx, "alpha", kbconfig.UpdateInput{
		Values:  map[string]any{"embedding_model": "embed-b"},
		Reembed: true,
	})
	if err == nil {
		t.Fatalf("UpdateKnowledgeBase() accepted a failing staging write")
	}
	var knowledge *apierrors.KnowledgeError
	if !errors.As(err, &knowledge) {
		t.Fatalf("update error = %v, want KnowledgeError", err)
	}
	stored, _ := stores.KnowledgeBases.Get(ctx, "alpha")
	if stored["collection"] != oldCollection || stored["embedding_model"] != "embed-a" {
		t.Errorf("failed update changed settings: %v", stored)
	}
	// The unpublished staging collection is deleted exactly once.
	deleted := false
	for _, request := range qdrant.all() {
		if request.Method != http.MethodDelete || !strings.HasPrefix(request.Path, "/collections/") {
			continue
		}
		if request.Path == "/collections/"+oldCollection {
			t.Errorf("old collection was deleted")
			continue
		}
		deleted = true
		if !strings.HasPrefix(request.Path, "/collections/knowledge_") {
			t.Errorf("unexpected delete path %q", request.Path)
		}
	}
	if !deleted {
		t.Errorf("staging collection was not discarded")
	}
}

func TestUpdateKnowledgeBaseMissingRecord(t *testing.T) {
	service, _ := newKBService(t, "http://127.0.0.1:1")
	_, err := service.UpdateKnowledgeBase(context.Background(), "missing", kbconfig.UpdateInput{
		Values: map[string]any{},
	})
	var validation *apierrors.KnowledgeValidationError
	if !errors.As(err, &validation) || validation.Message != kbconfig.MessageKnowledgeBaseMissing {
		t.Fatalf("update error = %v", err)
	}
}

func TestDeleteKnowledgeBaseKeepsDocumentsAndInvalidates(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	if _, err := service.CreateKnowledgeBase(ctx, testCreateInput("alpha", modelURL)); err != nil {
		t.Fatalf("CreateKnowledgeBase() error = %v", err)
	}
	record := state.DocumentRecord{
		Name: "keep.txt", DocumentID: "doc-keep", ChunkCount: 1, Chunking: map[string]any{},
	}
	if err := stores.Documents.Upsert(ctx, "alpha", record); err != nil {
		t.Fatalf("Documents.Upsert() error = %v", err)
	}
	if _, err := service.search.tenant(ctx, "alpha"); err != nil {
		t.Fatalf("tenant() error = %v", err)
	}
	deleted, err := service.DeleteKnowledgeBase(ctx, "alpha")
	if err != nil || !deleted {
		t.Fatalf("DeleteKnowledgeBase() = %v, %v", deleted, err)
	}
	if stored, _ := stores.KnowledgeBases.Get(ctx, "alpha"); stored != nil {
		t.Fatalf("configuration row still exists")
	}
	documents, err := stores.Documents.List(ctx, "alpha")
	if err != nil || len(documents) != 1 {
		t.Fatalf("documents after delete = %v, %v", documents, err)
	}
	if _, err := service.search.tenant(ctx, "alpha"); err == nil {
		t.Fatalf("tenant cache was not invalidated after delete")
	}
	if again, err := service.DeleteKnowledgeBase(ctx, "alpha"); err != nil || again {
		t.Fatalf("second DeleteKnowledgeBase() = %v, %v", again, err)
	}
}

func TestPurgeKnowledgeBaseDeletesDocumentsThenConfiguration(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	if _, err := service.CreateKnowledgeBase(ctx, testCreateInput("alpha", modelURL)); err != nil {
		t.Fatalf("CreateKnowledgeBase() error = %v", err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		record := state.DocumentRecord{
			Name: name, DocumentID: "doc-" + name, ChunkCount: 1, Chunking: map[string]any{},
		}
		if err := stores.Documents.Upsert(ctx, "alpha", record); err != nil {
			t.Fatalf("Documents.Upsert(%s) error = %v", name, err)
		}
	}
	if _, err := service.search.tenant(ctx, "alpha"); err != nil {
		t.Fatalf("tenant() error = %v", err)
	}

	result, err := service.PurgeKnowledgeBase(ctx, "alpha")
	if err != nil {
		t.Fatalf("PurgeKnowledgeBase() error = %v", err)
	}
	if result.Namespace != "alpha" || result.DocumentsDeleted != 2 || !result.ConfigurationDeleted {
		t.Fatalf("PurgeKnowledgeBase() = %+v", result)
	}
	if len(result.FailedDocuments) != 0 {
		t.Fatalf("failed documents = %v", result.FailedDocuments)
	}
	if stored, _ := stores.KnowledgeBases.Get(ctx, "alpha"); stored != nil {
		t.Fatalf("configuration row still exists: %v", stored)
	}
	documents, err := stores.Documents.List(ctx, "alpha")
	if err != nil || len(documents) != 0 {
		t.Fatalf("documents after purge = %v, %v", documents, err)
	}
	if _, err := service.search.tenant(ctx, "alpha"); err == nil {
		t.Fatalf("tenant cache was not invalidated after purge")
	}
	// The merged Console list must no longer expose the workspace.
	listed, err := service.ListKnowledgeBases(ctx)
	if err != nil {
		t.Fatalf("ListKnowledgeBases() error = %v", err)
	}
	for _, item := range listed {
		if item["namespace"] == "alpha" {
			t.Fatalf("purged namespace still listed: %v", item)
		}
	}
}

func TestPurgeKnowledgeBaseKeepsConfigurationWhenUploadLeaseIsActive(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	if _, err := service.CreateKnowledgeBase(ctx, testCreateInput("alpha", modelURL)); err != nil {
		t.Fatalf("CreateKnowledgeBase() error = %v", err)
	}
	// A live upload lease blocks the purge: the namespace is reported as failed
	// and the configuration row is kept for a retry once the upload finishes.
	// The lease is live in the backend (Redis in production, the fake here);
	// the SQL registration alone never blocks anything.
	fakeLock, ok := stores.DocumentLocks.(*statelocktest.FakeDocumentLocker)
	if !ok || fakeLock == nil {
		t.Fatalf("test store has no fake document locker (got %T)", stores.DocumentLocks)
	}
	if err := fakeLock.Acquire(ctx, "alpha",
		[]string{state.UploadNamespaceResource}, "owner", 60*time.Second); err != nil {
		t.Fatalf("fake acquire error = %v", err)
	}

	result, err := service.PurgeKnowledgeBase(ctx, "alpha")
	if err != nil {
		t.Fatalf("PurgeKnowledgeBase() error = %v", err)
	}
	if result.ConfigurationDeleted {
		t.Fatalf("configuration row was deleted despite an active upload lease: %+v", result)
	}
	if len(result.FailedDocuments) != 1 {
		t.Fatalf("failed documents = %v", result.FailedDocuments)
	}
	if stored, _ := stores.KnowledgeBases.Get(ctx, "alpha"); stored == nil {
		t.Fatalf("configuration row was removed; the purge must stay retryable")
	}
}

func TestPurgeKnowledgeBaseUnconfiguredWorkspaceAndMissingNamespace(t *testing.T) {
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	record := state.DocumentRecord{
		Name: "orphan.txt", DocumentID: "doc-orphan", ChunkCount: 1, Chunking: map[string]any{},
	}
	if err := stores.Documents.Upsert(ctx, "orphan", record); err != nil {
		t.Fatalf("Documents.Upsert() error = %v", err)
	}

	result, err := service.PurgeKnowledgeBase(ctx, "orphan")
	if err != nil {
		t.Fatalf("PurgeKnowledgeBase(orphan) error = %v", err)
	}
	if result.DocumentsDeleted != 1 || result.ConfigurationDeleted {
		t.Fatalf("PurgeKnowledgeBase(orphan) = %+v", result)
	}

	missing, err := service.PurgeKnowledgeBase(ctx, "missing")
	if err != nil {
		t.Fatalf("PurgeKnowledgeBase(missing) error = %v", err)
	}
	if missing.DocumentsDeleted != 0 || missing.ConfigurationDeleted || len(missing.FailedDocuments) != 0 {
		t.Fatalf("PurgeKnowledgeBase(missing) = %+v", missing)
	}
}

func TestKnowledgeBaseKeyRevealAndErrors(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	service, _ := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	if _, err := service.CreateKnowledgeBase(ctx, testCreateInput("alpha", modelURL)); err != nil {
		t.Fatalf("CreateKnowledgeBase() error = %v", err)
	}
	key, err := service.KnowledgeBaseKey(ctx, "alpha", "embedding")
	if err != nil || key != "private-key" {
		t.Fatalf("KnowledgeBaseKey() = %q, %v", key, err)
	}
	rerankerKey, err := service.KnowledgeBaseKey(ctx, "alpha", "reranker")
	if err != nil || rerankerKey != "" {
		t.Fatalf("reranker key = %q, %v", rerankerKey, err)
	}
	if _, err := service.KnowledgeBaseKey(ctx, "missing", "embedding"); err == nil {
		t.Fatalf("missing namespace key was revealed")
	} else {
		var knowledge *apierrors.KnowledgeError
		if !errors.As(err, &knowledge) || knowledge.Message != kbconfig.MessageKnowledgeBaseMissing {
			t.Errorf("missing namespace error = %v", err)
		}
	}
	if _, err := service.KnowledgeBaseKey(ctx, "alpha", "sql"); err == nil {
		t.Fatalf("invalid kind was accepted")
	}
}

func TestKnowledgeBaseModelsDraftAndStoredKeys(t *testing.T) {
	t.Setenv("RAG_STATE_ENCRYPTION_KEY", testEncryptionKey)
	modelURL, calls := newFakeModelAPI(t, kbModelHandler(nil))
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	settings := testKnowledgeBaseSettings(t, "alpha", modelURL, "knowledge_test")
	if err := stores.KnowledgeBases.Create(ctx, settings); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}

	// Create form: no namespace, explicit draft key.
	models, err := service.KnowledgeBaseModels(ctx, "embedding", map[string]any{
		"base_url": modelURL, "apikey": "draft-key",
	}, nil)
	if err != nil {
		t.Fatalf("KnowledgeBaseModels() error = %v", err)
	}
	if len(models) != 3 || models[2] != "rank-a" {
		t.Fatalf("models = %v", models)
	}
	recorded := calls.find(http.MethodGet, "/models")
	if len(recorded) != 1 || recorded[0].Header.Get("Authorization") != "Bearer draft-key" {
		t.Fatalf("model request = %+v", recorded)
	}

	// Existing namespace with the same base URL reuses the stored key.
	namespace := "alpha"
	if _, err := service.KnowledgeBaseModels(ctx, "embedding",
		map[string]any{"base_url": modelURL}, &namespace); err != nil {
		t.Fatalf("KnowledgeBaseModels() error = %v", err)
	}
	recorded = calls.find(http.MethodGet, "/models")
	if recorded[len(recorded)-1].Header.Get("Authorization") != "Bearer embedding-key" {
		t.Errorf("stored key was not reused: %q", recorded[len(recorded)-1].Header.Get("Authorization"))
	}

	// A different base URL drops the stored key.
	if _, err := service.KnowledgeBaseModels(ctx, "embedding",
		map[string]any{"base_url": modelURL + "/other"}, &namespace); err != nil {
		t.Fatalf("KnowledgeBaseModels() error = %v", err)
	}
	recorded = calls.find(http.MethodGet, "/models")
	if authorization := recorded[len(recorded)-1].Header.Get("Authorization"); authorization != "" {
		t.Errorf("changed base_url kept the stored key: %q", authorization)
	}

	missing := "missing"
	if _, err := service.KnowledgeBaseModels(ctx, "embedding", map[string]any{}, &missing); err == nil {
		t.Fatalf("missing namespace models were accepted")
	} else {
		var knowledge *apierrors.KnowledgeError
		if !errors.As(err, &knowledge) || knowledge.Message != kbconfig.MessageKnowledgeBaseMissing {
			t.Errorf("missing namespace error = %v", err)
		}
	}
	if _, err := service.KnowledgeBaseModels(ctx, "sql", nil, nil); err == nil {
		t.Fatalf("invalid kind was accepted")
	}
}

// TestAdminErrorCategories pins the error mapping used by the HTTP routes.
func TestAdminErrorCategories(t *testing.T) {
	validation := adminError(&kbconfig.ValidationError{Message: "知识库标识无效"})
	var knowledgeValidation *apierrors.KnowledgeValidationError
	if !errors.As(validation, &knowledgeValidation) || knowledgeValidation.Message != "知识库标识无效" {
		t.Errorf("validation mapping = %v", validation)
	}
	modelError := adminError(&modelapi.ModelAPIError{Message: "模型服务请求超时"})
	if !errors.As(modelError, &knowledgeValidation) || knowledgeValidation.Message != "模型服务请求超时" {
		t.Errorf("model error mapping = %v", modelError)
	}
	conflict := adminError(&state.UploadConflict{Message: "Upload resource is leased"})
	var uploadConflict *apierrors.UploadConflict
	if !errors.As(conflict, &uploadConflict) || uploadConflict.Message != uploadConflictMessage {
		t.Errorf("conflict mapping = %v", conflict)
	}
	operation := adminError(contracts.NewRagOperationError("Qdrant unavailable"))
	var ragOperation *contracts.RagOperationError
	if !errors.As(operation, &ragOperation) {
		t.Errorf("operation mapping = %v", operation)
	}
	generic := adminError(errors.New("sql: connection refused with a secret"))
	var knowledge *apierrors.KnowledgeError
	if !errors.As(generic, &knowledge) || knowledge.Message != genericKnowledgeBaseMessage {
		t.Errorf("generic mapping = %v", generic)
	}
	if strings.Contains(generic.Error(), "secret") {
		t.Errorf("generic mapping leaked internals: %v", generic)
	}
	updateModel := updateError(&modelapi.ModelAPIError{Message: "模型服务请求超时"})
	if !errors.As(updateModel, &ragOperation) {
		t.Errorf("update model error mapping = %v", updateModel)
	}
	if got := adminError(nil); got != nil {
		t.Errorf("nil mapping = %v", got)
	}

}
