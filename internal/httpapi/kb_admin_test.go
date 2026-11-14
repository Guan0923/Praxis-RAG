package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/auth"
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

	purgeResults  map[string]KnowledgeBasePurgeResult
	purgeErrors   map[string]error
	purgeBatchErr error
	purgeCalls    []string
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

func (f *fakeKBAdminRuntime) PurgeKnowledgeBases(_ context.Context, namespaces []string) ([]KnowledgeBasePurgeResult, error) {
	for _, namespace := range namespaces {
		f.purgeCalls = append(f.purgeCalls, namespace)
	}
	if f.purgeBatchErr != nil {
		return nil, f.purgeBatchErr
	}
	results := make([]KnowledgeBasePurgeResult, 0, len(namespaces))
	for _, namespace := range namespaces {
		result := f.purgeResults[namespace]
		result.Namespace = namespace
		if err := f.purgeErrors[namespace]; err != nil {
			message := "知识库删除失败"
			var knowledge *apierrors.KnowledgeError
			if errors.As(err, &knowledge) {
				message = knowledge.Message
			}
			result.FailedDocuments = append(result.FailedDocuments, map[string]string{
				"name": "", "error": message,
			})
		}
		results = append(results, result)
	}
	return results, nil
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

	vlmBody := `{"namespace":"beta","embedding":{"base_url":"http://model.local/v1"},` +
		`"embedding_model":"BAAI/bge-m3","vlm":{"base_url":"http://vlm.local/v1","apikey":"vlm-sekret"},` +
		`"vlm_model":"qwen2.5-vl-7b","strategy":"character","chunk_size":700,"chunk_overlap":100}`
	if created := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/knowledge-bases", remote: "127.0.0.1:5000", body: vlmBody,
	}); created.Code != http.StatusCreated {
		t.Fatalf("vlm create status = %d body = %s", created.Code, created.Body.String())
	}
	if input := runtime.createInput; input.VLMModel != "qwen2.5-vl-7b" ||
		input.VLMValues["base_url"] != "http://vlm.local/v1" || input.VLMValues["apikey"] != "vlm-sekret" {
		t.Fatalf("vlm input = %+v values = %v", input, input.VLMValues)
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
		body: `{"namespace":"alpha","embedding":{},"embedding_model":"x","strategy":"unknown"}`,
	})
	if badStrategy.Code != http.StatusBadRequest {
		t.Fatalf("bad strategy status = %d body = %s", badStrategy.Code, badStrategy.Body.String())
	}

	runtime.createErr = &apierrors.KnowledgeValidationError{Message: "该知识库已存在，请在知识库页面修改配置"}
	conflict := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/knowledge-bases", remote: "127.0.0.1:5000",
		body: `{"namespace":"alpha","embedding":{},"embedding_model":"x"}`,
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
	// "jev" is a saved-model kind only and stays rejected for knowledge bases.
	jevKind := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/knowledge-bases/models/jev", remote: "127.0.0.1:5000",
		body: `{}`,
	})
	if jevKind.Code != http.StatusUnprocessableEntity {
		t.Fatalf("jev kind status = %d", jevKind.Code)
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
		body: `{"embedding":{"base_url":"http://new.local/v1"},"embedding_model":"BAAI/bge-m3",` +
			`"reembed":true}`,
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

func TestKnowledgeBaseAdminValidationParity(t *testing.T) {
	runtime := &fakeKBAdminRuntime{fakeDocRuntime: newFakeDocRuntime()}
	handler := newKBAdminServer(t, runtime)
	longText := func(size int) string {
		buffer := make([]byte, size)
		for index := range buffer {
			buffer[index] = 'x'
		}
		return string(buffer)
	}
	cases := []struct {
		name     string
		body     string
		location string
		wantType string
		patch    bool
	}{
		{"missing embedding", `{"namespace":"alpha","embedding_model":"x"}`, "embedding", "missing", false},
		{"missing embedding_model", `{"namespace":"alpha","embedding":{}}`, "embedding_model", "missing", false},
		{"empty embedding_model", `{"namespace":"alpha","embedding":{},"embedding_model":""}`, "embedding_model", "string_too_short", false},
		{"long embedding_model", `{"namespace":"alpha","embedding":{},"embedding_model":"` + longText(513) + `"}`, "embedding_model", "string_too_long", false},
		{"long reranker_model", `{"namespace":"alpha","embedding":{},"embedding_model":"m","reranker_model":"` + longText(513) + `"}`, "reranker_model", "string_too_long", false},

		{"patch empty embedding_model", `{"embedding_model":""}`, "embedding_model", "string_too_short", true},
		{"patch long embedding_model", `{"embedding_model":"` + longText(513) + `"}`, "embedding_model", "string_too_long", true},
		{"patch long reranker_model", `{"reranker_model":"` + longText(513) + `"}`, "reranker_model", "string_too_long", true},

	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			method, target := "POST", "/v1/admin/knowledge-bases"
			if testCase.patch {
				method, target = "PATCH", "/v1/admin/knowledge-bases/alpha"
			}
			response := performRequest(handler, testRequest{
				method: method, target: target, remote: "127.0.0.1:5000", body: testCase.body,
			})
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
			detail, _ := decodeBody(t, response)["detail"].([]any)
			if len(detail) == 0 {
				t.Fatalf("missing detail: %s", response.Body.String())
			}
			first, _ := detail[0].(map[string]any)
			location, _ := first["loc"].([]any)
			if len(location) != 2 || location[0] != "body" || location[1] != testCase.location {
				t.Fatalf("loc = %v, want body.%s", first["loc"], testCase.location)
			}
			if first["type"] != testCase.wantType {
				t.Fatalf("type = %v, want %s", first["type"], testCase.wantType)
			}
		})
	}
	if runtime.createInput.Namespace != "" {
		t.Fatalf("invalid create reached the runtime: %+v", runtime.createInput)
	}
	if runtime.updateNamespace != "" {
		t.Fatalf("invalid patch reached the runtime: %q", runtime.updateNamespace)
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
func TestNamespaceListReturnsNamesForTenantSessions(t *testing.T) {
	adminRuntime := &fakeKBAdminRuntime{
		fakeDocRuntime: newFakeDocRuntime(),
		listPayload: []map[string]any{
			{"namespace": "alpha", "collection": "knowledge_x"},
			{"namespace": "beta"},
			{"namespace": ""},
		},
	}
	settings := testSettings("")
	settings.AuthEnabled = true
	users := &fakeUserAuth{
		sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token")},
	}
	control := &fakeAccessControl{
		visible: map[string][]string{"u-user": {"alpha", "beta"}},
		owned:   map[string][]string{"u-user": {"alpha"}},
	}
	handler := New(settings, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithKnowledgeBaseAdminRuntime(adminRuntime),
		WithUserAuth(users),
		WithAccessControl(control),
	).Handler()

	anonymous := performRequest(handler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
		headers: map[string]string{"X-RAG-Namespace": "alpha"},
	})
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", anonymous.Code)
	}

	response := performRequest(handler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
		headers: mergeHeaders(cookieFor("user-token"), map[string]string{"X-RAG-Namespace": "alpha"}),
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	body := decodeBody(t, response)
	namespaces, _ := body["namespaces"].([]any)
	if len(namespaces) != 2 || namespaces[0] != "alpha" || namespaces[1] != "beta" {
		t.Fatalf("namespaces = %v", body["namespaces"])
	}
	owned, _ := body["owned_namespaces"].([]any)
	if len(owned) != 1 || owned[0] != "alpha" {
		t.Fatalf("owned_namespaces = %v", body["owned_namespaces"])
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	// Sessions also list without X-RAG-Namespace: the route only needs the
	// session identity, so a stale remembered namespace cannot block the list.
	headerless := performRequest(handler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
		headers: cookieFor("user-token"),
	})
	if headerless.Code != http.StatusOK {
		t.Fatalf("headerless session status = %d body = %s", headerless.Code, headerless.Body.String())
	}
	headerlessBody := decodeBody(t, headerless)
	if namespaces, _ := headerlessBody["namespaces"].([]any); len(namespaces) != 2 ||
		namespaces[0] != "alpha" || namespaces[1] != "beta" {
		t.Fatalf("headerless session namespaces = %v", headerlessBody["namespaces"])
	}
	if owned, _ := headerlessBody["owned_namespaces"].([]any); len(owned) != 1 || owned[0] != "alpha" {
		t.Fatalf("headerless session owned_namespaces = %v", headerlessBody["owned_namespaces"])
	}
}

// namespaceListRuntime builds the knowledge-base list used by the /v1/namespaces
// credential-scope tests.
func namespaceListRuntime() *fakeKBAdminRuntime {
	return &fakeKBAdminRuntime{
		fakeDocRuntime: newFakeDocRuntime(),
		listPayload: []map[string]any{
			{"namespace": "alpha"},
			{"namespace": "beta"},
			{"namespace": "gamma"},
		},
	}
}

func namespaceListStrings(t *testing.T, raw any) []string {
	t.Helper()
	values, ok := raw.([]any)
	if !ok {
		t.Fatalf("expected a string array, got %v", raw)
	}
	decoded := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("expected a string array, got %v", raw)
		}
		decoded = append(decoded, text)
	}
	return decoded
}

// TestNamespaceListAPIKeyScope covers the API-key branch: a Bearer credential
// only sees the knowledge bases it is bound to in both fields.
func TestNamespaceListAPIKeyScope(t *testing.T) {
	adminRuntime := namespaceListRuntime()
	keys := fakeKeys{"prk_alice": {"gamma", "alpha", "missing-kb"}}
	handler := New(testSettings(""), nil, keys, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithKnowledgeBaseAdminRuntime(adminRuntime),
	).Handler()

	scoped := performRequest(handler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
		headers: map[string]string{
			"Authorization":   "Bearer prk_alice",
			"X-RAG-Namespace": "alpha",
		},
	})
	if scoped.Code != http.StatusOK {
		t.Fatalf("scoped status = %d body = %s", scoped.Code, scoped.Body.String())
	}
	if got := scoped.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	body := decodeBody(t, scoped)
	if namespaces := namespaceListStrings(t, body["namespaces"]); len(namespaces) != 2 ||
		namespaces[0] != "alpha" || namespaces[1] != "gamma" {
		t.Fatalf("namespaces = %v, want merged-list order of the key binding", body["namespaces"])
	}
	if owned := namespaceListStrings(t, body["owned_namespaces"]); len(owned) != 2 ||
		owned[0] != "alpha" || owned[1] != "gamma" {
		t.Fatalf("owned_namespaces = %v, want the key binding", body["owned_namespaces"])
	}

	// GET /v1/namespaces is namespace-agnostic: it must answer without
	// X-RAG-Namespace (and ignore a stale one), otherwise a rotated API key
	// could never recover its scope in the Console.
	headerless := performRequest(handler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
		headers: map[string]string{"Authorization": "Bearer prk_alice"},
	})
	if headerless.Code != http.StatusOK {
		t.Fatalf("headerless status = %d body = %s", headerless.Code, headerless.Body.String())
	}
	headerlessBody := decodeBody(t, headerless)
	if namespaces := namespaceListStrings(t, headerlessBody["namespaces"]); len(namespaces) != 2 ||
		namespaces[0] != "alpha" || namespaces[1] != "gamma" {
		t.Fatalf("headerless namespaces = %v, want the key binding", headerlessBody["namespaces"])
	}

	stale := performRequest(handler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
		headers: map[string]string{
			"Authorization":   "Bearer prk_alice",
			"X-RAG-Namespace": "beta",
		},
	})
	if stale.Code != http.StatusOK {
		t.Fatalf("stale-header status = %d body = %s", stale.Code, stale.Body.String())
	}
	staleBody := decodeBody(t, stale)
	if namespaces := namespaceListStrings(t, staleBody["namespaces"]); len(namespaces) != 2 ||
		namespaces[0] != "alpha" || namespaces[1] != "gamma" {
		t.Fatalf("stale-header namespaces = %v, want the key binding", staleBody["namespaces"])
	}

	// Tenant routes keep the strict header binding; only the namespace listing
	// is exempt, so the credential still cannot authenticate a foreign namespace.
	denied := performRequest(handler, testRequest{
		method: "GET", target: "/v1/documents", remote: "10.0.0.9:5000",
		headers: map[string]string{
			"Authorization":   "Bearer prk_alice",
			"X-RAG-Namespace": "beta",
		},
	})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("denied status = %d body = %s", denied.Code, denied.Body.String())
	}
	if detail := decodeBody(t, denied)["detail"]; detail != "API Key 未获授权访问此 Namespace" {
		t.Fatalf("denied detail = %v", detail)
	}
}

// TestNamespaceListManagementCredentialsUnfiltered keeps the service token and
// session-less loopback behaviour: both fields list every merged knowledge base.
func TestNamespaceListManagementCredentialsUnfiltered(t *testing.T) {
	adminRuntime := namespaceListRuntime()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	tokenHandler := New(testSettings("test-token"), nil, nil, logger,
		WithKnowledgeBaseAdminRuntime(adminRuntime)).Handler()
	tokenList := performRequest(tokenHandler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
		headers: map[string]string{
			"Authorization":   "Bearer test-token",
			"X-RAG-Namespace": "alpha",
		},
	})
	if tokenList.Code != http.StatusOK {
		t.Fatalf("service-token status = %d body = %s", tokenList.Code, tokenList.Body.String())
	}
	tokenBody := decodeBody(t, tokenList)
	for _, field := range []string{"namespaces", "owned_namespaces"} {
		if namespaces := namespaceListStrings(t, tokenBody[field]); len(namespaces) != 3 ||
			namespaces[0] != "alpha" || namespaces[1] != "beta" || namespaces[2] != "gamma" {
			t.Fatalf("service-token %s = %v, want the full merged list", field, tokenBody[field])
		}
	}

	loopbackHandler := New(testSettings(""), nil, nil, logger,
		WithKnowledgeBaseAdminRuntime(adminRuntime)).Handler()
	loopbackList := performRequest(loopbackHandler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "127.0.0.1:5000",
		headers: map[string]string{"X-RAG-Namespace": "alpha"},
	})
	if loopbackList.Code != http.StatusOK {
		t.Fatalf("loopback status = %d body = %s", loopbackList.Code, loopbackList.Body.String())
	}
	loopbackBody := decodeBody(t, loopbackList)
	for _, field := range []string{"namespaces", "owned_namespaces"} {
		if namespaces := namespaceListStrings(t, loopbackBody[field]); len(namespaces) != 3 {
			t.Fatalf("loopback %s = %v, want the full merged list", field, loopbackBody[field])
		}
	}

	// Management credentials also work without X-RAG-Namespace.
	tokenHeaderless := performRequest(tokenHandler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
		headers: map[string]string{"Authorization": "Bearer test-token"},
	})
	if tokenHeaderless.Code != http.StatusOK {
		t.Fatalf("service-token headerless status = %d body = %s", tokenHeaderless.Code, tokenHeaderless.Body.String())
	}
	for _, field := range []string{"namespaces", "owned_namespaces"} {
		if namespaces := namespaceListStrings(t, decodeBody(t, tokenHeaderless)[field]); len(namespaces) != 3 {
			t.Fatalf("service-token headerless %s = %v, want the full merged list", field, namespaces)
		}
	}
	loopbackHeaderless := performRequest(loopbackHandler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "127.0.0.1:5000",
	})
	if loopbackHeaderless.Code != http.StatusOK {
		t.Fatalf("loopback headerless status = %d body = %s", loopbackHeaderless.Code, loopbackHeaderless.Body.String())
	}
	for _, field := range []string{"namespaces", "owned_namespaces"} {
		if namespaces := namespaceListStrings(t, decodeBody(t, loopbackHeaderless)[field]); len(namespaces) != 3 {
			t.Fatalf("loopback headerless %s = %v, want the full merged list", field, namespaces)
		}
	}
}

// TestNamespaceListFailsClosedWithoutAccessControl pins the explicit 500 when a
// login session reaches the namespace list without the ownership service.
func TestNamespaceListFailsClosedWithoutAccessControl(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token")}}
	handler := New(authTestSettings(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithKnowledgeBaseAdminRuntime(namespaceListRuntime()),
		WithUserAuth(users),
	).Handler()

	response := performRequest(handler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", "alpha"),
	})
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if detail := decodeBody(t, response)["detail"]; detail != "知识库权限服务未配置" {
		t.Fatalf("detail = %v", detail)
	}
}

// TestNamespaceListAccessServiceErrors pins the explicit 500 for both
// permission-service lookups instead of silently returning the full list.
func TestNamespaceListAccessServiceErrors(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token")}}
	cases := []struct {
		name    string
		control *fakeAccessControl
	}{
		{
			name:    "visible namespaces error",
			control: &fakeAccessControl{visibleErr: errors.New("visible failed")},
		},
		{
			name: "owned namespaces error",
			control: &fakeAccessControl{
				visible:  map[string][]string{"u-user": {"alpha"}},
				ownedErr: errors.New("owned failed"),
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := New(authTestSettings(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
				WithKnowledgeBaseAdminRuntime(namespaceListRuntime()),
				WithUserAuth(users),
				WithAccessControl(testCase.control),
			).Handler()
			response := performRequest(handler, testRequest{
				method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
				headers: sessionHeaders("user-token", "alpha"),
			})
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
			if detail := decodeBody(t, response)["detail"]; detail != "知识库权限服务异常" {
				t.Fatalf("detail = %v", detail)
			}
		})
	}
}
