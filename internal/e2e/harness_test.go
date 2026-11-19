package e2e

// Harness for the end-to-end pipeline tests: a real MySQL test state store, a
// real runtime.Service talking to fake model/Qdrant HTTP servers, an in-process
// Redis (miniredis) backing the read-model cache, and an in-process fake Kafka
// transport standing in for Debezium-delivered document notifications. No
// network, real broker, Qdrant or model server is required.
//
// Task delivery is owned by Debezium in production. The tests therefore build
// the identifier-only schema-4 document_jobs notifications that Debezium emits
// and feed them to the worker handlers either directly (Handle) or through
// mq.PullConsumer over the fake transport, exactly like the deployed workers
// do. Search runs synchronously in rag-service and has no notification path.

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

	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/documentworker"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/mq"
	"github.com/Guan0923/Praxis-RAG/internal/redisstate"
	"github.com/Guan0923/Praxis-RAG/internal/runtime"
	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/statelocktest"
	"github.com/Guan0923/Praxis-RAG/internal/statetest"
)

// testEncryptionKey is a deterministic base64-encoded 32-byte AES key used to
// protect the knowledge-base provider credentials stored in SQLite.
var testEncryptionKey = statetest.TestEncryptionKey

// documentText is long enough for several chunks with the seeded character
// chunking configuration.
const documentText = "One document paragraph with enough text for chunking. " +
	"索引任务负责将上传的文档切分为可检索的片段。 " +
	"Another sentence keeps the chunk boundaries meaningful.\n\n" +
	"Second paragraph repeats the steps for preparing a document index. " +
	"索引任务完成后即可按片段检索。\n\n" +
	"Third paragraph documents how to trace evidence to its source. " +
	"检索结果会标注来源文件和片段编号。\n\n"

// searchQueryPoints mimics a Qdrant query response with chunk records the
// projector turns into search results.
const searchQueryPoints = `[
	{"id":"p1","version":1,"score":0.93,"payload":{
		"record_type":"chunk","document_id":"doc-index","chunk_id":"chunk-index",
		"text":"文档索引包含分块和向量化","title":"index","source":"index.md",
		"metadata":{"kind":"test"},"upload_version":"v1"}},
	{"id":"p2","version":1,"score":0.41,"payload":{
		"record_type":"chunk","document_id":"doc-access","chunk_id":"chunk-access",
		"text":"知识库可以按文档分配读取权限","title":"permissions","source":"permissions.md",
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
	// Redis is the in-process miniredis backing Cache; FlushAll() proves the
	// MySQL fact source can rebuild a read-model view after a cache loss.
	Redis *miniredis.Miniredis

	mutex sync.Mutex
	// artifacts lists the document artifact ids whose notifications the fake
	// Debezium pump re-emits when their schedule_version changes.
	artifacts []string

	// ModelURL is the fake embedding/reranker endpoint used by knowledge-base
	// creation and upload tests.
	ModelURL string
}

// trackDocumentArtifact registers one admitted artifact so the fake Debezium
// pump delivers its notification.
func (h *harness) trackDocumentArtifact(artifactID string) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	for _, tracked := range h.artifacts {
		if tracked == artifactID {
			return
		}
	}
	h.artifacts = append(h.artifacts, artifactID)
}

func (h *harness) trackedDocumentArtifacts() []string {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return append([]string(nil), h.artifacts...)
}

// newHarness builds the real runtime over fake model/Qdrant servers, a real
// MySQL test store and an in-process Redis for the read-model cache.
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
		Redis:    mr,
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

// newDocumentProcessor builds the real document worker (ingestion enabled) the
// rag-document-worker binary runs.
func newDocumentProcessor(t *testing.T, h *harness) *documentworker.Processor {
	t.Helper()
	worker, err := documentworker.New(documentworker.Options{
		Jobs:            h.Stores.DocumentJobs,
		Ingester:        h.Service,
		JournalRecovery: h.Service,
		Logger:          testLogger(),
	})
	if err != nil {
		t.Fatalf("documentworker.New() error = %v", err)
	}
	return worker
}

// newDocumentDeadLetterProcessor builds the ingestion-less processor the
// dead-letter worker runs: Handle can never publish a document, so broker
// retries keep failing until the delivery is exhausted.
func newDocumentDeadLetterProcessor(t *testing.T, h *harness) *documentworker.Processor {
	t.Helper()
	worker, err := documentworker.New(documentworker.Options{
		Jobs:   h.Stores.DocumentJobs,
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("documentworker.New() error = %v", err)
	}
	return worker
}

// documentNotification returns the identifier-only schema-4 notification
// Debezium emits for the job's current schedule_version.
func documentNotification(t *testing.T, h *harness, artifactID string) state.DocumentNotification {
	t.Helper()
	job, err := h.Stores.DocumentJobs.Get(context.Background(), artifactID, nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Get(%s) error = %v", artifactID, err)
	}
	if job == nil {
		t.Fatalf("DocumentJobs.Get(%s) = nil", artifactID)
	}
	notification, err := state.DocumentJobEnvelope(job)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope() error = %v", err)
	}
	return notification
}

// encodeNotification renders the application-native envelope, which is exactly
// the payload the worker decoder accepts (and what tests push to the fake
// transport or the dead-letter topic).
func encodeNotification(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	return body
}

// fakePullTransport is an mq.PullTransport fed by the test; it records acked and
// nacked deliveries so assertions can verify the worker ack policy.
type fakePullTransport struct {
	mutex    sync.Mutex
	messages []mq.ReceivedMessage
	acked    []mq.ReceivedMessage
	nacked   []mq.ReceivedMessage
	// offsets assigns deterministic per-topic offsets to pushes that carry none,
	// so the consumer's commit tracking sees a realistic record sequence.
	offsets map[string]int64
}

func (f *fakePullTransport) Start() error { return nil }

func (f *fakePullTransport) Stop() error { return nil }

func (f *fakePullTransport) push(message mq.ReceivedMessage) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.offsets == nil {
		f.offsets = make(map[string]int64)
	}
	if message.Offset == 0 {
		f.offsets[message.Topic]++
		message.Offset = f.offsets[message.Topic]
	} else if message.Offset > f.offsets[message.Topic] {
		f.offsets[message.Topic] = message.Offset
	}
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

// runDocumentWorkerUntil starts a real mq.PullConsumer over the in-process
// transport and re-emits each tracked artifact's current schema-4 notification
// whenever its schedule_version changes — the same signal Debezium's
// document_jobs capture produces — until done() holds, then stops the consumer.
// A job that fails retryably has its version bumped inside MySQL, so the pump
// naturally delivers the replacement notification.
func runDocumentWorkerUntil(t *testing.T, h *harness, description string, done func() bool) {
	t.Helper()
	worker := newDocumentProcessor(t, h)
	transport := &fakePullTransport{}
	consumer := mq.NewPullConsumer(
		consumerConfig("praxis_document_ingest", "praxis_document_ingest_workers"),
		transport, worker.MessageHandler(),
	)
	runCtx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- worker.RunConsumer(runCtx, consumer) }()

	delivered := make(map[string]int64)
	pump := func() {
		for _, artifactID := range h.trackedDocumentArtifacts() {
			job, err := h.Stores.DocumentJobs.Get(context.Background(), artifactID, nil)
			if err != nil || job == nil {
				continue
			}
			if delivered[artifactID] == job.ScheduleVersion {
				continue
			}
			notification, err := state.DocumentJobEnvelope(job)
			if err != nil {
				t.Fatalf("DocumentJobEnvelope(%s) error = %v", artifactID, err)
			}
			transport.push(mq.ReceivedMessage{
				Topic:           "praxis_document_ingest",
				Body:            encodeNotification(t, notification),
				DeliveryAttempt: 1,
			})
			delivered[artifactID] = job.ScheduleVersion
		}
	}
	waitFor(t, 90*time.Second, description, func() bool {
		if done() {
			return true
		}
		pump()
		return done()
	})
	cancel()
	select {
	case runErr := <-finished:
		if runErr != nil {
			t.Fatalf("RunConsumer() error = %v", runErr)
		}
	case <-time.After(90 * time.Second):
		t.Fatalf("document worker did not stop")
	}
	// The write path no longer flushes Redis synchronously; replay the cache
	// connector's revision-watermark refresh so views read after the upload
	// observe the published document.
	convergeDocumentViews(t, h)
}

// convergeAllViews simulates the cache connector catching up after a direct
// write: revisions are bumped for every known kind and refreshed from MySQL.
// The write path intentionally no longer flushes Redis synchronously.
func convergeAllViews(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.Stores.CacheSync.MarkAllDirty(ctx); err != nil {
		t.Fatalf("CacheSync.MarkAllDirty() error = %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := h.Stores.CacheSync.RefreshDirty(ctx, 100); err != nil {
			t.Fatalf("CacheSync.RefreshDirty() error = %v", err)
		}
		dirty, err := h.Stores.CacheSync.DirtyKeys(ctx, 1000)
		if err != nil {
			t.Fatalf("CacheSync.DirtyKeys() error = %v", err)
		}
		if len(dirty) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache views did not converge: %d dirty keys", len(dirty))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// convergeDocumentViews simulates the cache connector catching up with the
// documents written by the worker: it bumps and refreshes the same cache keys
// the CDC event for a documents row maps to.
func convergeDocumentViews(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	seen := make(map[string]struct{})
	keys := make([]state.CacheKey, 0, 8)
	for _, artifactID := range h.trackedDocumentArtifacts() {
		job, err := h.Stores.DocumentJobs.Get(ctx, artifactID, nil)
		if err != nil || job == nil {
			continue
		}
		if _, ok := seen[job.Namespace]; ok {
			continue
		}
		seen[job.Namespace] = struct{}{}
		keys = append(keys,
			state.CacheKey{Kind: "kb_list", Scope: ""},
			state.CacheKey{Kind: "kb_public", Scope: job.Namespace},
			state.CacheKey{Kind: "documents", Scope: job.Namespace},
		)
	}
	if len(keys) == 0 {
		return
	}
	if err := h.Stores.CacheSync.Revalidate(ctx, keys); err != nil {
		t.Fatalf("CacheSync.Revalidate() error = %v", err)
	}
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
