package retrieval

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/qdrant"
)

// Test helpers: a minimal fake Qdrant REST server shared by the retriever and
// orchestrator tests (no real Qdrant or model downloads).

type testRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   map[string]any
}

type testQdrant struct {
	mu       sync.Mutex
	requests []testRequest
}

func (q *testQdrant) all() []testRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	copied := make([]testRequest, len(q.requests))
	copy(copied, q.requests)
	return copied
}

func (q *testQdrant) find(method, suffix string) []testRequest {
	matches := make([]testRequest, 0, 2)
	for _, request := range q.all() {
		if request.Method == method && strings.HasSuffix(request.Path, suffix) {
			matches = append(matches, request)
		}
	}
	return matches
}

func newTestQdrant(t *testing.T, handler func(testRequest) (int, string)) (*testQdrant, *httptest.Server) {
	t.Helper()
	fake := &testQdrant{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		recorded := testRequest{Method: request.Method, Path: request.URL.Path, Query: request.URL.Query()}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &recorded.Body); err != nil {
				t.Errorf("invalid request body: %v (%s)", err, raw)
			}
		}
		fake.mu.Lock()
		fake.requests = append(fake.requests, recorded)
		fake.mu.Unlock()
		status, response := 0, ""
		if handler != nil {
			status, response = handler(recorded)
		}
		if status == 0 {
			status = http.StatusOK
		}
		if response == "" {
			response = defaultTestResponse(recorded)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return fake, server
}

func defaultTestResponse(request testRequest) string {
	switch {
	case strings.HasSuffix(request.Path, "/exists"):
		return `{"result":{"exists":true}}`
	case strings.HasSuffix(request.Path, "/points/query"):
		return `{"result":{"points":[]}}`
	case request.Method == http.MethodGet && strings.HasPrefix(request.Path, "/collections/"):
		return `{"result":{"config":{"params":{"sparse_vectors":{"sparse":{"modifier":"idf"}}}}}}`
	default:
		return `{"result":true}`
	}
}

func newTestStore(t *testing.T, server *httptest.Server) *qdrant.Store {
	t.Helper()
	store, err := qdrant.NewStore(qdrant.StoreConfig{
		Collection: "knowledge",
		URL:        server.URL,
		VectorSize: 4,
	})
	if err != nil {
		t.Fatalf("qdrant.NewStore() error: %v", err)
	}
	return store
}

type fakeQueryEmbedder struct {
	vector []float64
	err    error
	calls  int
}

func (f *fakeQueryEmbedder) EmbedQuery(context.Context, string) ([]float64, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.vector, nil
}

func queryResult(pointsJSON string) func(testRequest) (int, string) {
	return func(request testRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/points/query") {
			return http.StatusOK, `{"result":{"points":` + pointsJSON + `}}`
		}
		return 0, ""
	}
}

type modelRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

type modelRecorder struct {
	mu       sync.Mutex
	requests []modelRequest
}

func (r *modelRecorder) all() []modelRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := make([]modelRequest, len(r.requests))
	copy(copied, r.requests)
	return copied
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func newFakeModelServer(t *testing.T, handler func(body map[string]any) (int, string)) (string, *modelRecorder) {
	t.Helper()
	recorder := &modelRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("invalid model request body: %v (%s)", err, raw)
			}
		}
		recorder.mu.Lock()
		recorder.requests = append(recorder.requests, modelRequest{Method: request.Method, Path: request.URL.Path, Body: body})
		recorder.mu.Unlock()
		status, response := handler(body)
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
