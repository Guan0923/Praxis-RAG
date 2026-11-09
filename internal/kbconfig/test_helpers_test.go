package kbconfig

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/modelapi"
)

// testEncryptionKey is a deterministic base64-encoded 32-byte AES key.
const testEncryptionKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

type modelCall struct {
	Method        string
	Path          string
	Authorization string
	Body          map[string]any
}

// newFakeModelServer starts an HTTP model API stub and returns its URL plus the
// recorded requests. No real model is involved.
func newFakeModelServer(t *testing.T, handler func(call modelCall) (int, string)) (string, func() []modelCall) {
	t.Helper()
	recorder := &modelRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		call := modelCall{
			Method:        request.Method,
			Path:          request.URL.Path,
			Authorization: request.Header.Get("Authorization"),
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &call.Body); err != nil {
				t.Errorf("invalid model request body: %v (%s)", err, raw)
			}
		}
		recorder.mu.Lock()
		recorder.calls = append(recorder.calls, call)
		recorder.mu.Unlock()
		status, response := handler(call)
		if status == 0 {
			status = http.StatusOK
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return server.URL, recorder.all
}

type modelRecorder struct {
	mu    sync.Mutex
	calls []modelCall
}

func (r *modelRecorder) all() []modelCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := make([]modelCall, len(r.calls))
	copy(copied, r.calls)
	return copied
}

type fakeProber struct {
	vectorSize     int
	embeddingError error
	rerankerError  error
	embeddingCalls int
	rerankerCalls  int
	lastEmbedding  modelapi.ModelConnection
	lastReranker   modelapi.ModelConnection
}

func (p *fakeProber) EmbeddingVectorSize(_ context.Context, connection modelapi.ModelConnection) (int, error) {
	p.embeddingCalls++
	p.lastEmbedding = connection
	if p.embeddingError != nil {
		return 0, p.embeddingError
	}
	return p.vectorSize, nil
}

func (p *fakeProber) RerankerAccessible(_ context.Context, connection modelapi.ModelConnection) error {
	p.rerankerCalls++
	p.lastReranker = connection
	return p.rerankerError
}

// testStoredSettings builds a stored settings map shaped like create_settings
// output, with deterministic protected keys.
func testStoredSettings(t *testing.T, embeddingURL, embeddingKey string) map[string]any {
	t.Helper()
	embeddingStored, err := StoredConnectionWithKey(
		modelapi.ModelConnection{BaseURL: embeddingURL, Model: "embed-a", APIKey: embeddingKey},
		testEncryptionKey,
	)
	if err != nil {
		t.Fatalf("StoredConnectionWithKey() error: %v", err)
	}
	rerankerStored, err := StoredConnectionWithKey(
		modelapi.ModelConnection{BaseURL: embeddingURL, Model: "rank-a"},
		testEncryptionKey,
	)
	if err != nil {
		t.Fatalf("StoredConnectionWithKey() error: %v", err)
	}
	return map[string]any{
		"namespace":       "alpha",
		"vector_size":     3,
		"embedding_model": "embed-a",
		"reranker_model":  "rank-a",
		"embedding":       embeddingStored,
		"reranker":        rerankerStored,
		"collection":      "knowledge_0123456789abcdef0123456789abcdef",
		"tokenizer":       "BAAI/bge-m3",
		"strategy":        "character",
		"chunk_size":      120,
		"chunk_overlap":   20,
		"created_at":      "2026-11-08T10:00:00.000000+00:00",
	}
}

func newTestCreateInput(embeddingURL string) CreateInput {
	return CreateInput{
		Namespace:       "alpha",
		Chunking:        chunkingConfig("character", 120, 20),
		EmbeddingModel:  "embed-a",
		RerankerModel:   "rank-a",
		EmbeddingValues: map[string]any{"base_url": embeddingURL, "apikey": "private-key"},
		RerankerValues:  map[string]any{"base_url": embeddingURL},
		CreatedAt:       fixedTimestamp(),
		EncryptionKey:   testEncryptionKey,
	}
}

func chunkingConfig(strategy string, chunkSize, chunkOverlap int) chunking.Config {
	return chunking.Config{Strategy: strategy, ChunkSize: chunkSize, ChunkOverlap: chunkOverlap}
}

func fixedTimestamp() time.Time {
	return time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
}
