package e2e

// Harness for the end-to-end async pipeline tests: a real SQLite state store, a
// real runtime.Service talking to fake model/Qdrant HTTP servers, an in-process
// Redis (miniredis) for result snapshots/transient state, and fake broker/consumer
// seams. No network or real broker/Qdrant/model is required.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/kbconfig"
	"github.com/Guan0923/xinghe_rag/internal/modelapi"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/redisstate"
	"github.com/Guan0923/xinghe_rag/internal/runtime"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/statetest"
)

// testEncryptionKey is a deterministic base64-encoded 32-byte AES key used to
// protect the knowledge-base provider credentials stored in SQLite.
var testEncryptionKey = statetest.TestEncryptionKey

// documentText is long enough for several chunks with the seeded character
// chunking configuration.
const documentText = "One document paragraph with enough text for chunking. " +
	"E03 水路供水不足的处理步骤需要排出管路空气。 " +
	"Another sentence keeps the chunk boundaries meaningful.\n\n" +
	"Second paragraph repeats the operational procedure for the water loop. " +
	"E04 压力异常时先关闭进水阀再检查过滤器。\n\n" +
	"Third paragraph documents the cooling check and the filter replacement. " +
	"E05 冷却水温过高时检查风扇与散热片。\n\n"

// searchQueryPoints mimics a Qdrant query response with chunk records the
// projector turns into search results.
const searchQueryPoints = `[
	{"id":"p1","version":1,"score":0.93,"payload":{
		"record_type":"chunk","document_id":"doc-e03","chunk_id":"chunk-e03",
		"text":"错误代码 E03 表示水路供水不足","title":"e03","source":"e03.md",
		"metadata":{"kind":"test"},"upload_version":"v1"}},
	{"id":"p2","version":1,"score":0.41,"payload":{
		"record_type":"chunk","document_id":"doc-temp","chunk_id":"chunk-temp",
		"text":"咖啡温度偏低时先预热杯子","title":"temperature","source":"temperature.md",
		"metadata":{"kind":"test"},"upload_version":"v1"}}
]`

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type recordedRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

type httpRecorder struct {
	mutex    sync.Mutex
	requests []recordedRequest
}

func (r *httpRecorder) add(request recordedRequest) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.requests = append(r.requests, request)
}

func (r *httpRecorder) all() []recordedRequest {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	copied := make([]recordedRequest, len(r.requests))
	copy(copied, r.requests)
	return copied
}

func (r *httpRecorder) count(method, suffix string) int {
	count := 0
	for _, request := range r.all() {
		if request.Method == method && strings.HasSuffix(request.Path, suffix) {
			count++
		}
	}
	return count
}

// recordAndDecode records one server request body for assertions.
func recordAndDecode(t *testing.T, recorder *httpRecorder, request *http.Request) recordedRequest {
	t.Helper()
	raw, _ := io.ReadAll(request.Body)
	recorded := recordedRequest{Method: request.Method, Path: request.URL.Path}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &recorded.Body); err != nil {
			t.Errorf("invalid request body: %v (%s)", err, raw)
		}
	}
	recorder.add(recorded)
	return recorded
}

// newFakeModelServer serves the embedding/rerank/models endpoints the retrieval
// and ingestion stacks call. Embeddings are three-dimensional to match the
// seeded knowledge-base vector size.
func newFakeModelServer(t *testing.T) (string, *httpRecorder) {
	t.Helper()
	recorder := &httpRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorded := recordAndDecode(t, recorder, request)
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/embeddings"):
			inputs, _ := recorded.Body["input"].([]any)
			data := make([]map[string]any, len(inputs))
			for index := range inputs {
				data[index] = map[string]any{"index": index, "embedding": []float64{1, 0, 0}}
			}
			encoded, _ := json.Marshal(map[string]any{"data": data})
			_, _ = writer.Write(encoded)
		case strings.HasSuffix(request.URL.Path, "/rerank"):
			documents, _ := recorded.Body["documents"].([]any)
			results := make([]map[string]any, len(documents))
			for index := range documents {
				results[index] = map[string]any{"index": index, "relevance_score": 1.0 - 0.1*float64(index)}
			}
			encoded, _ := json.Marshal(map[string]any{"results": results})
			_, _ = writer.Write(encoded)
		case strings.HasSuffix(request.URL.Path, "/models"):
			_, _ = writer.Write([]byte(`{"data":[{"id":"embed-a"},{"id":"rank-a"}]}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
			_, _ = writer.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, recorder
}

// fakeQdrant serves the REST surface used by ingestion (exists, create, upsert,
// scroll, delete) and retrieval (points/query).
type fakeQdrant struct {
	mutex       sync.Mutex
	requests    []recordedRequest
	created     map[string]bool
	queryPoints string
}

func (f *fakeQdrant) all() []recordedRequest {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	copied := make([]recordedRequest, len(f.requests))
	copy(copied, f.requests)
	return copied
}

func (f *fakeQdrant) count(method, suffix string) int {
	count := 0
	for _, request := range f.all() {
		if request.Method == method && strings.HasSuffix(request.Path, suffix) {
			count++
		}
	}
	return count
}

func (f *fakeQdrant) wasCreated(collection string) bool {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.created[collection]
}

func newFakeQdrantServer(t *testing.T, queryPoints string) (*fakeQdrant, string) {
	t.Helper()
	fake := &fakeQdrant{created: map[string]bool{}, queryPoints: queryPoints}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorded := recordAndDecode(t, &httpRecorder{}, request)
		fake.mutex.Lock()
		fake.requests = append(fake.requests, recorded)
		fake.mutex.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		path := request.URL.Path
		switch {
		case strings.HasSuffix(path, "/exists"):
			_, _ = writer.Write([]byte(`{"result":{"exists":true}}`))
		case strings.HasSuffix(path, "/points/query"):
			if fake.queryPoints == "" {
				_, _ = writer.Write([]byte(`{"result":{"points":[]}}`))
				return
			}
			_, _ = writer.Write([]byte(`{"result":{"points":` + fake.queryPoints + `}}`))
		case strings.HasSuffix(path, "/points/scroll"):
			_, _ = writer.Write([]byte(`{"result":{"points":[],"next_page_offset":null}}`))
		case strings.HasSuffix(path, "/points/count"):
			_, _ = writer.Write([]byte(`{"result":{"count":0}}`))
		case strings.HasSuffix(path, "/points/delete"):
			_, _ = writer.Write([]byte(`{"result":true}`))
		case request.Method == http.MethodPut && strings.HasSuffix(path, "/points"):
			_, _ = writer.Write([]byte(`{"result":{"status":"completed"}}`))
		case request.Method == http.MethodPut:
			name := collectionFromPath(path)
			fake.mutex.Lock()
			fake.created[name] = true
			fake.mutex.Unlock()
			_, _ = writer.Write([]byte(`{"result":true}`))
		case request.Method == http.MethodDelete:
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

func collectionFromPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/collections/")
	if index := strings.Index(trimmed, "/"); index >= 0 {
		return trimmed[:index]
	}
	return trimmed
}

// newTestStores opens the shared MySQL test store with the suite encryption key.
func newTestStores(t *testing.T) *state.Stores {
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
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	return stores
}

// seedKnowledgeBase inserts one knowledge-base row pointing at the fake model
// provider with protected credentials and the given Qdrant collection.
func seedKnowledgeBase(t *testing.T, stores *state.Stores, namespace, modelURL, collection string) {
	t.Helper()
	embedding, err := kbconfig.StoredConnectionWithKey(modelapi.ModelConnection{
		BaseURL: modelURL, Model: "embed-a", APIKey: "embedding-key",
	}, testEncryptionKey)
	if err != nil {
		t.Fatalf("StoredConnectionWithKey(embedding) error = %v", err)
	}
	reranker, err := kbconfig.StoredConnectionWithKey(modelapi.ModelConnection{
		BaseURL: modelURL, Model: "rank-a",
	}, testEncryptionKey)
	if err != nil {
		t.Fatalf("StoredConnectionWithKey(reranker) error = %v", err)
	}
	if err := stores.KnowledgeBases.Create(context.Background(), map[string]any{
		"namespace":       namespace,
		"vector_size":     3,
		"embedding_model": "embed-a",
		"reranker_model":  "rank-a",
		"embedding":       embedding,
		"reranker":        reranker,
		"collection":      collection,
		"tokenizer":       "BAAI/bge-m3",
		"strategy":        "character",
		"chunk_size":      120,
		"chunk_overlap":   20,
		"created_at":      "2026-11-08T10:00:00.000000+00:00",
	}); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
}

// harness is one isolated end-to-end environment.
type harness struct {
	Service *runtime.Service
	Stores  *state.Stores
	Qdrant  *fakeQdrant
	Model   *httpRecorder
	Cache   *redisstate.Client
	Broker  *fakeBroker
	// ModelURL is the fake embedding/reranker endpoint used by knowledge-base
	// creation and upload tests.
	ModelURL string
}

// newHarness builds the real runtime over fake model/Qdrant servers, a real
// SQLite store and an in-process Redis for snapshots.
func newHarness(t *testing.T, queryPoints string) *harness {
	t.Helper()
	modelURL, modelRecorder := newFakeModelServer(t)
	qdrant, qdrantURL := newFakeQdrantServer(t, queryPoints)
	stores := newTestStores(t)
	seedKnowledgeBase(t, stores, "alpha", modelURL, "knowledge_test")

	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	cache, err := redisstate.New(state.DefaultCacheSettings(), redisstate.WithRedisClient(redisClient))
	if err != nil {
		t.Fatalf("redisstate.New() error = %v", err)
	}
	if err := redisstate.Attach(stores, cache); err != nil {
		t.Fatalf("redisstate.Attach() error = %v", err)
	}

	service, err := runtime.New(config.RagConfig{
		Backend:    config.BackendQdrant,
		QdrantURL:  qdrantURL,
		Collection: "mini_agent_knowledge_pdf_assets_v1",
		VectorSize: 1024,
	}, stores, t.TempDir())
	if err != nil {
		t.Fatalf("runtime.New() error = %v", err)
	}
	h := &harness{
		Service:  service,
		Stores:   stores,
		Qdrant:   qdrant,
		Model:    modelRecorder,
		Cache:    cache,
		Broker:   &fakeBroker{},
		ModelURL: modelURL,
	}
	t.Cleanup(func() {
		service.StopBackground()
		if err := cache.Close(); err != nil {
			t.Logf("cache.Close() error = %v", err)
		}
		_ = service.Close(context.Background())
	})
	return h
}

// startBackgroundPublisher wires the fake broker as the queue transport, which
// enables the real Service.SubmitSearch admission path.
func (h *harness) startBackgroundPublisher(t *testing.T) {
	t.Helper()
	if err := h.Service.StartBackground(context.Background(),
		runtime.WithBackgroundPublisher(h.Broker),
		runtime.WithMaintenanceInterval(0),
		runtime.WithInitialMaintenancePass(false),
	); err != nil {
		t.Fatalf("StartBackground() error = %v", err)
	}
	t.Cleanup(h.Service.StopBackground)
}

// fakeBroker implements runtime.BackgroundPublisher: it records published search
// envelopes and document notifications instead of using a Kafka broker.
type fakeBroker struct {
	mutex     sync.Mutex
	searches  []state.SearchEnvelope
	documents []state.DocumentNotification
	searchErr error
	closed    bool
}

func (b *fakeBroker) SendSearchEnvelope(_ context.Context, envelope state.SearchEnvelope) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	if b.searchErr != nil {
		return b.searchErr
	}
	b.searches = append(b.searches, envelope)
	return nil
}

func (b *fakeBroker) SendDocumentNotification(_ context.Context, notification state.DocumentNotification) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.documents = append(b.documents, notification)
	return nil
}

func (b *fakeBroker) Close() error {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.closed = true
	return nil
}

func (b *fakeBroker) lastSearchEnvelope() (state.SearchEnvelope, bool) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	if len(b.searches) == 0 {
		return state.SearchEnvelope{}, false
	}
	return b.searches[len(b.searches)-1], true
}

func (b *fakeBroker) searchCount() int {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return len(b.searches)
}

// fakePullTransport is an mq.PullTransport fed by the test; it records acked and
// nacked deliveries so assertions can verify the worker ack policy.
type fakePullTransport struct {
	mutex    sync.Mutex
	messages []mq.ReceivedMessage
	acked    []mq.ReceivedMessage
	nacked   []mq.ReceivedMessage
}

func (f *fakePullTransport) Start() error { return nil }

func (f *fakePullTransport) Stop() error { return nil }

func (f *fakePullTransport) push(message mq.ReceivedMessage) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.messages = append(f.messages, message)
}

func (f *fakePullTransport) Receive(_ context.Context, maxMessages int32, _ time.Duration) ([]mq.ReceivedMessage, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if len(f.messages) == 0 {
		return nil, nil
	}
	count := int(maxMessages)
	if count <= 0 || count > len(f.messages) {
		count = len(f.messages)
	}
	batch := append([]mq.ReceivedMessage(nil), f.messages[:count]...)
	f.messages = f.messages[count:]
	return batch, nil
}

func (f *fakePullTransport) Ack(_ context.Context, message mq.ReceivedMessage) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.acked = append(f.acked, message)
	return nil
}

func (f *fakePullTransport) ChangeInvisibleDuration(_ context.Context, message mq.ReceivedMessage, _ time.Duration) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.nacked = append(f.nacked, message)
	return nil
}

func (f *fakePullTransport) ackCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return len(f.acked)
}

func (f *fakePullTransport) nackCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return len(f.nacked)
}

// consumerConfig returns a fast-polling consumer configuration for tests.
func consumerConfig(topic, group string) mq.ConsumerConfig {
	return mq.ConsumerConfig{
		Topic:             topic,
		Group:             group,
		BatchSize:         1,
		VisibilityTimeout: time.Second,
		AwaitDuration:     time.Second,
		PollInterval:      5 * time.Millisecond,
	}
}

// waitFor polls check until it passes or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", description)
}
