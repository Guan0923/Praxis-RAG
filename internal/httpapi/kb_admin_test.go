package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
)

type fakeKBAdminRuntime struct {
	*fakeDocRuntime

	listPayload []map[string]any
	listErr     error

	createInput   KnowledgeBaseCreateInput
	createPayload map[string]any
	createErr     error

	updateNamespace string
	updateInput     KnowledgeBaseUpdateInput
	updatePayload   map[string]any
	updateErr       error

	keyPayload string
	keyErr     error

	modelsKind      string
	modelsValues    map[string]any
	modelsNamespace *string
	modelsPayload   []string
	modelsErr       error
}

func (f *fakeKBAdminRuntime) ListKnowledgeBases(context.Context) ([]map[string]any, error) {
	return f.listPayload, f.listErr
}

func (f *fakeKBAdminRuntime) CreateKnowledgeBase(_ context.Context, input KnowledgeBaseCreateInput) (map[string]any, error) {
	f.createInput = input
	return f.createPayload, f.createErr
}

func (f *fakeKBAdminRuntime) UpdateKnowledgeBase(_ context.Context, namespace string, input KnowledgeBaseUpdateInput) (map[string]any, error) {
	f.updateNamespace = namespace
	f.updateInput = input
	return f.updatePayload, f.updateErr
}

func (f *fakeKBAdminRuntime) KnowledgeBaseKey(context.Context, string, string) (string, error) {
	return f.keyPayload, f.keyErr
}

func (f *fakeKBAdminRuntime) KnowledgeBaseModels(_ context.Context, kind string, values map[string]any, namespace *string) ([]string, error) {
	f.modelsKind = kind
	f.modelsValues = values
	f.modelsNamespace = namespace
	return f.modelsPayload, f.modelsErr
}

func newKBAdminServer(t *testing.T, runtime *fakeKBAdminRuntime) http.Handler {
	t.Helper()
	server := New(testSettings(""), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithRuntime(runtime),
		WithKnowledgeBaseAdminRuntime(runtime),
	)
	return server.Handler()
}

func TestKnowledgeBaseAdminListAndDetails(t *testing.T) {
	runtime := &fakeKBAdminRuntime{
		fakeDocRuntime: newFakeDocRuntime(),
		listPayload:    []map[string]any{{"namespace": "alpha", "collection": "knowledge_x"}},
		keyPayload:     "secret-key",
	}
	handler := newKBAdminServer(t, runtime)

	listed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/knowledge-bases", remote: "127.0.0.1:5000",
	})
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", listed.Code, listed.Body.String())
	}
	if items, ok := decodeBody(t, listed)["knowledge_bases"].([]any); !ok || len(items) != 1 {
		t.Fatalf("knowledge_bases = %v", decodeBody(t, listed)["knowledge_bases"])
	}
	if got := listed.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}

	details := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/knowledge-bases/alpha", remote: "127.0.0.1:5000",
	})
	if details.Code != http.StatusOK {
		t.Fatalf("details status = %d body = %s", details.Code, details.Body.String())
	}
	if body := decodeBody(t, details); body["namespace"] != "alpha" {
		t.Fatalf("details = %v", body)
	}

	key := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/knowledge-bases/alpha/key/embedding", remote: "127.0.0.1:5000",
	})
	if key.Code != http.StatusOK {
		t.Fatalf("key status = %d body = %s", key.Code, key.Body.String())
	}
	if body := decodeBody(t, key); body["apikey"] != "secret-key" {
		t.Fatalf("key = %v", body)
	}
	invalidKind := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/knowledge-bases/alpha/key/other", remote: "127.0.0.1:5000",
	})
	if invalidKind.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid kind status = %d", invalidKind.Code)
	}
}

func TestKnowledgeBaseAdminCreate(t *testing.T) {
	runtime := &fakeKBAdminRuntime{
		fakeDocRuntime: newFakeDocRuntime(),
		createPayload:  map[string]any{"namespace": "alpha", "collection": "knowledge_new"},
	}
	handler := newKBAdminServer(t, runtime)

	body := `{"namespace":"alpha","embedding":{"base_url":"http://model.local/v1","apikey":"sekret"},` +
		`"embedding_model":"BAAI/bge-m3","reranker_model":"BAAI/bge-reranker-base",` +
		`"strategy":"character","chunk_size":700,"chunk_overlap":100}`
	created := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/knowledge-bases", remote: "127.0.0.1:5000", body: body,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", created.Code, created.Body.String())
	}
	if body := decodeBody(t, created); body["collection"] != "knowledge_new" {
		t.Fatalf("created body = %v", body)
	}
	input := runtime.createInput
	if input.Namespace != "alpha" || input.EmbeddingModel != "BAAI/bge-m3" ||
		input.Chunking.Strategy != "character" || input.Chunking.ChunkSize != 700 {
		t.Fatalf("input = %+v", input)
	}
	if input.EmbeddingValues["base_url"] != "http://model.local/v1" || input.EmbeddingValues["apikey"] != "sekret" {
		t.Fatalf("embedding values = %v", input.EmbeddingValues)
	}

	invalidNamespace := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/knowledge-bases", remote: "127.0.0.1:5000",
		body: `{"namespace":"bad namespace","embedding_model":"x"}`,
	})
	if invalidNamespace.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid namespace status = %d", invalidNamespace.Code)
	}

	missingModel := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/knowledge-bases", remote: "127.0.0.1:5000",
		body: `{"namespace":"alpha"}`,
	})
	if missingModel.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing model status = %d", missingModel.Code)
	}

	badStrategy := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/knowledge-bases", remote: "127.0.0.1:5000",
		body: `{"namespace":"alpha","embedding_model":"x","strategy":"unknown"}`,
	})
	if badStrategy.Code != http.StatusBadRequest {
		t.Fatalf("bad strategy status = %d body = %s", badStrategy.Code, badStrategy.Body.String())
	}

	runtime.createErr = &apierrors.KnowledgeValidationError{Message: "该知识库已存在，请在知识库页面修改配置"}
	conflict := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/knowledge-bases", remote: "127.0.0.1:5000",
		body: `{"namespace":"alpha","embedding_model":"x"}`,
	})
	if conflict.Code != http.StatusBadRequest {
		t.Fatalf("conflict status = %d", conflict.Code)
	}
}

func TestKnowledgeBaseAdminModels(t *testing.T) {
	runtime := &fakeKBAdminRuntime{
		fakeDocRuntime: newFakeDocRuntime(),
		modelsPayload:  []string{"model-a", "model-b"},
	}
	handler := newKBAdminServer(t, runtime)

	models := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/knowledge-bases/models/embedding", remote: "127.0.0.1:5000",
		body: `{"base_url":"http://model.local/v1","namespace":"alpha"}`,
	})
	if models.Code != http.StatusOK {
		t.Fatalf("models status = %d body = %s", models.Code, models.Body.String())
	}
	if values, ok := decodeBody(t, models)["models"].([]any); !ok || len(values) != 2 {
		t.Fatalf("models = %v", decodeBody(t, models)["models"])
	}
	if runtime.modelsKind != "embedding" || runtime.modelsValues["base_url"] != "http://model.local/v1" {
		t.Fatalf("captured = %q %v", runtime.modelsKind, runtime.modelsValues)
	}
	if runtime.modelsNamespace == nil || *runtime.modelsNamespace != "alpha" {
		t.Fatalf("namespace = %v", runtime.modelsNamespace)
	}
	badKind := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/knowledge-bases/models/sql", remote: "127.0.0.1:5000",
		body: `{}`,
	})
	if badKind.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad kind status = %d", badKind.Code)
	}
}

func TestKnowledgeBaseAdminUpdate(t *testing.T) {
	runtime := &fakeKBAdminRuntime{
		fakeDocRuntime: newFakeDocRuntime(),
		updatePayload:  map[string]any{"namespace": "alpha", "collection": "knowledge_v2"},
	}
	handler := newKBAdminServer(t, runtime)

	updated := performRequest(handler, testRequest{
		method: "PATCH", target: "/v1/admin/knowledge-bases/alpha", remote: "127.0.0.1:5000",
		body: `{"embedding":{"base_url":"http://new.local/v1"},"embedding_model":"BAAI/bge-m3","reembed":true}`,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("update status = %d body = %s", updated.Code, updated.Body.String())
	}
	if runtime.updateNamespace != "alpha" || !runtime.updateInput.Reembed {
		t.Fatalf("captured = %q %+v", runtime.updateNamespace, runtime.updateInput)
	}
	if runtime.updateInput.Values["embedding_model"] != "BAAI/bge-m3" {
		t.Fatalf("values = %v", runtime.updateInput.Values)
	}
	embedding, _ := runtime.updateInput.Values["embedding"].(map[string]any)
	if embedding["base_url"] != "http://new.local/v1" {
		t.Fatalf("embedding values = %v", embedding)
	}

	runtime.updateErr = &apierrors.KnowledgeValidationError{Message: "修改 Embedding URL 或模型前，必须重新向量化（re embedding）"}
	rejected := performRequest(handler, testRequest{
		method: "PATCH", target: "/v1/admin/knowledge-bases/alpha", remote: "127.0.0.1:5000",
		body: `{"embedding_model":"other"}`,
	})
	if rejected.Code != http.StatusConflict {
		t.Fatalf("rejected status = %d", rejected.Code)
	}
}

func TestKnowledgeBaseAdminRequiresRuntime(t *testing.T) {
	server := New(testSettings(""), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/admin/knowledge-bases", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}
