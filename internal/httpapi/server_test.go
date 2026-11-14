package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/apikeys"
	"github.com/Guan0923/xinghe_rag/internal/catalog"
	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/statetest"
)

type fakeNotifier struct{ namespaces []string }

func (f *fakeNotifier) PromptsChanged(namespace string) {
	f.namespaces = append(f.namespaces, namespace)
}

func (f *fakeNotifier) ToolsChanged(namespace string) {
	f.namespaces = append(f.namespaces, namespace)
}

func (f *fakeNotifier) DocumentsChanged(namespace string, _, _ []map[string]any) {
	f.namespaces = append(f.namespaces, namespace)
}

type fakeStatus struct{ payload map[string]any }

func (f fakeStatus) Status(context.Context) map[string]any { return f.payload }

type fakeKeys map[string][]string

func (f fakeKeys) NamespacesForKey(_ context.Context, secret string) ([]string, error) {
	namespaces, ok := f[secret]
	if !ok {
		return nil, errors.New("无效的 API Key")
	}
	return namespaces, nil
}

func testSettings(token string) config.ServiceSettings {
	return config.ServiceSettings{
		Host:     "127.0.0.1",
		Port:     8011,
		Token:    token,
		DataRoot: "unused",
	}
}

func newTestServer(t *testing.T, settings config.ServiceSettings, keys KeyLookup) *Server {
	t.Helper()
	status := fakeStatus{payload: map[string]any{
		"status":     "ok",
		"service":    "praxis-rag",
		"backend":    "qdrant",
		"collection": "test_hybrid",
		"state":      map[string]any{"backend": "mysql"},
	}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(settings, status, keys, logger)
}

type testRequest struct {
	method  string
	target  string
	headers map[string]string
	remote  string
	host    string
	body    string
}

func performRequest(handler http.Handler, request testRequest) *httptest.ResponseRecorder {
	var body io.Reader
	if request.body != "" {
		body = strings.NewReader(request.body)
	}
	req := httptest.NewRequest(request.method, request.target, body)
	req.Host = "127.0.0.1:8011"
	if request.host != "" {
		req.Host = request.host
	}
	for key, value := range request.headers {
		req.Header.Set(key, value)
	}
	if request.remote != "" {
		req.RemoteAddr = request.remote
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return body
}

func TestHealthIsAvailableWithoutTenantCredentials(t *testing.T) {
	server := newTestServer(t, testSettings("test-token"), nil)
	response := performRequest(server.Handler(), testRequest{method: "GET", target: "/health"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := decodeBody(t, response)
	if body["service"] != "praxis-rag" {
		t.Fatalf("service = %v", body["service"])
	}
	if body["backend"] != "qdrant" || body["collection"] != "test_hybrid" {
		t.Fatalf("unexpected health payload: %v", body)
	}
}

func TestUnknownPathAndWrongMethod(t *testing.T) {
	server := newTestServer(t, testSettings("test-token"), nil)
	notFound := performRequest(server.Handler(), testRequest{method: "GET", target: "/nope"})
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("status = %d", notFound.Code)
	}
	if detail := decodeBody(t, notFound)["detail"]; detail != "Not Found" {
		t.Fatalf("detail = %v", detail)
	}

	notAllowed := performRequest(server.Handler(), testRequest{method: "POST", target: "/health"})
	if notAllowed.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", notAllowed.Code)
	}
	if allow := notAllowed.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("Allow = %q", allow)
	}
	if detail := decodeBody(t, notAllowed)["detail"]; detail != "Method Not Allowed" {
		t.Fatalf("detail = %v", detail)
	}
}

func TestTenantAuthentication(t *testing.T) {
	server := newTestServer(t, testSettings("test-token"), nil)
	// Expose the resolved namespace so the tenant boundary is observable.
	server.router.add("GET", "/test/namespace", authTenant, func(w http.ResponseWriter, r *http.Request) {
		namespace, _ := NamespaceFromContext(r.Context())
		writeJSON(w, http.StatusOK, map[string]string{"namespace": namespace})
	})

	cases := []struct {
		name     string
		headers  map[string]string
		status   int
		contains string
	}{
		{"no credentials", nil, http.StatusUnauthorized, "RAG service authentication failed."},
		{"wrong token", map[string]string{"Authorization": "Bearer wrong"}, http.StatusUnauthorized, "RAG service authentication failed."},
		{"missing namespace", map[string]string{"Authorization": "Bearer test-token"}, http.StatusBadRequest, "Invalid RAG namespace."},
		{"invalid namespace", map[string]string{"Authorization": "Bearer test-token", "X-RAG-Namespace": "bad namespace"}, http.StatusBadRequest, "Invalid RAG namespace."},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := performRequest(server.Handler(), testRequest{method: "GET", target: "/v1/chunking/strategies", headers: testCase.headers})
			if response.Code != testCase.status {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, testCase.status, response.Body.String())
			}
			if detail := decodeBody(t, response)["detail"]; detail != testCase.contains {
				t.Fatalf("detail = %v, want %q", detail, testCase.contains)
			}
		})
	}

	ok := performRequest(server.Handler(), testRequest{
		method:  "GET",
		target:  "/test/namespace",
		headers: map[string]string{"Authorization": "Bearer test-token", "X-RAG-Namespace": "user-1"},
	})
	if ok.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", ok.Code, ok.Body.String())
	}
	if namespace := decodeBody(t, ok)["namespace"]; namespace != "user-1" {
		t.Fatalf("namespace = %v", namespace)
	}

	catalog := performRequest(server.Handler(), testRequest{
		method:  "GET",
		target:  "/v1/chunking/strategies",
		headers: map[string]string{"Authorization": "Bearer test-token", "X-RAG-Namespace": "user-1"},
	})
	if catalog.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", catalog.Code, catalog.Body.String())
	}
	if catalogBody := decodeBody(t, catalog); catalogBody["default"] != "recursive" {
		t.Fatalf("catalog = %v", catalogBody)
	}
}

func TestAPIKeyAuthorization(t *testing.T) {
	server := newTestServer(t, testSettings(""), fakeKeys{"valid-key": {"ns-1"}})
	allowed := performRequest(server.Handler(), testRequest{
		method:  "GET",
		target:  "/v1/chunking/strategies",
		headers: map[string]string{"Authorization": "Bearer valid-key", "X-RAG-Namespace": "ns-1"},
	})
	if allowed.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", allowed.Code, allowed.Body.String())
	}
	forbidden := performRequest(server.Handler(), testRequest{
		method:  "GET",
		target:  "/v1/chunking/strategies",
		headers: map[string]string{"Authorization": "Bearer valid-key", "X-RAG-Namespace": "ns-2"},
	})
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", forbidden.Code, forbidden.Body.String())
	}
	if detail := decodeBody(t, forbidden)["detail"]; detail != "API Key 未获授权访问此 Namespace" {
		t.Fatalf("detail = %v", detail)
	}
	unknown := performRequest(server.Handler(), testRequest{
		method:  "GET",
		target:  "/v1/chunking/strategies",
		headers: map[string]string{"Authorization": "Bearer unknown", "X-RAG-Namespace": "ns-1"},
	})
	if unknown.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", unknown.Code)
	}
	if detail := decodeBody(t, unknown)["detail"]; detail != "无效的 API Key" {
		t.Fatalf("detail = %v", detail)
	}
}

func TestLocalAdminBoundary(t *testing.T) {
	server := newTestServer(t, testSettings(""), nil)
	target := "/v1/admin/knowledge-bases/defaults"

	local := performRequest(server.Handler(), testRequest{
		method: "GET", target: target, remote: "127.0.0.1:54321",
	})
	if local.Code != http.StatusOK {
		t.Fatalf("loopback status = %d, body = %s", local.Code, local.Body.String())
	}

	remote := performRequest(server.Handler(), testRequest{
		method: "GET", target: target, remote: "10.0.0.5:1234",
	})
	if remote.Code != http.StatusForbidden {
		t.Fatalf("remote status = %d", remote.Code)
	}
	if detail := decodeBody(t, remote)["detail"]; detail != "仅允许本机直接访问管理接口" {
		t.Fatalf("detail = %v", detail)
	}

	forwarded := performRequest(server.Handler(), testRequest{
		method:  "GET",
		target:  target,
		remote:  "127.0.0.1:54321",
		headers: map[string]string{"X-Forwarded-For": "1.2.3.4"},
	})
	if forwarded.Code != http.StatusForbidden {
		t.Fatalf("forwarded status = %d", forwarded.Code)
	}

	crossSite := performRequest(server.Handler(), testRequest{
		method:  "GET",
		target:  target,
		remote:  "127.0.0.1:54321",
		headers: map[string]string{"Sec-Fetch-Site": "cross-site"},
	})
	if crossSite.Code != http.StatusForbidden {
		t.Fatalf("cross-site status = %d", crossSite.Code)
	}

	sameOrigin := performRequest(server.Handler(), testRequest{
		method:  "GET",
		target:  target,
		remote:  "127.0.0.1:54321",
		headers: map[string]string{"Origin": "http://127.0.0.1:8011"},
	})
	if sameOrigin.Code != http.StatusOK {
		t.Fatalf("same-origin status = %d, body = %s", sameOrigin.Code, sameOrigin.Body.String())
	}

	crossOrigin := performRequest(server.Handler(), testRequest{
		method:  "GET",
		target:  target,
		remote:  "127.0.0.1:54321",
		headers: map[string]string{"Origin": "http://evil.example"},
	})
	if crossOrigin.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d", crossOrigin.Code)
	}
}

func TestSearchPathsAreNoStore(t *testing.T) {
	server := newTestServer(t, testSettings("test-token"), nil)
	for _, target := range []string{"/v1/search", "/mcp", "/v1/artifacts/abc/content"} {
		response := performRequest(server.Handler(), testRequest{method: "GET", target: target})
		if got := response.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s Cache-Control = %q, want no-store", target, got)
		}
	}
	// The removed result-snapshot path is not routed anymore.
	removed := performRequest(server.Handler(), testRequest{method: "GET", target: "/v1/search-results/abc"})
	if removed.Code != http.StatusNotFound {
		t.Fatalf("/v1/search-results/abc status = %d, want 404", removed.Code)
	}
	health := performRequest(server.Handler(), testRequest{method: "GET", target: "/health"})
	if got := health.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("/health Cache-Control = %q, want empty", got)
	}
}

func TestConsoleStaticAssets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<html>console-marker</html>"), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app.js"), []byte("console-asset"), 0o600); err != nil {
		t.Fatalf("write asset: %v", err)
	}
	settings := testSettings("")
	settings.WebRoot = root
	server := newTestServer(t, settings, nil)
	if server.WebRoot() != root {
		t.Fatalf("web root = %q", server.WebRoot())
	}

	for _, target := range []string{"/", "/console"} {
		response := performRequest(server.Handler(), testRequest{method: "GET", target: target})
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "console-marker") {
			t.Fatalf("%s status = %d body = %q", target, response.Code, response.Body.String())
		}
	}
	asset := performRequest(server.Handler(), testRequest{method: "GET", target: "/console/assets/assets/app.js"})
	if asset.Code != http.StatusOK || asset.Body.String() != "console-asset" {
		t.Fatalf("asset status = %d body = %q", asset.Code, asset.Body.String())
	}
	// Python StaticFiles never renders directory listings (and never falls back
	// to webRoot/index.html for a directory request): both the assets root and
	// subdirectories are 404.
	for _, target := range []string{"/console/assets/", "/console/assets/assets/", "/console/assets/missing.js"} {
		response := performRequest(server.Handler(), testRequest{method: "GET", target: target})
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "index.html") {
			t.Fatalf("%s status = %d body = %q", target, response.Code, response.Body.String())
		}
	}
}

func TestConsoleIsAbsentWithoutBuild(t *testing.T) {
	settings := testSettings("")
	settings.WebRoot = filepath.Join(t.TempDir(), "missing")
	server := newTestServer(t, settings, nil)
	if server.WebRoot() != "" {
		t.Fatalf("web root = %q, want empty", server.WebRoot())
	}
	response := performRequest(server.Handler(), testRequest{method: "GET", target: "/console"})
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestRouterMatching(t *testing.T) {
	rt := newRouter()
	rt.add("GET", "/v1/admin/knowledge-bases/{namespace}/key/{kind}", authNone, func(http.ResponseWriter, *http.Request) {})
	rt.add("DELETE", "/v1/admin/api-keys/{key_id}", authNone, func(http.ResponseWriter, *http.Request) {})

	result, matched, _ := rt.match("GET", "/v1/admin/knowledge-bases/user-1/key/embedding")
	if !matched || result.route == nil {
		t.Fatalf("route did not match")
	}
	if result.params["namespace"] != "user-1" || result.params["kind"] != "embedding" {
		t.Fatalf("params = %v", result.params)
	}

	_, matched, allowed := rt.match("POST", "/v1/admin/api-keys/abc")
	if !matched || len(allowed) != 1 || allowed[0] != "DELETE" {
		t.Fatalf("matched = %v allowed = %v", matched, allowed)
	}

	_, matched, _ = rt.match("GET", "/v1/admin/knowledge-bases/user-1/key")
	if matched {
		t.Fatalf("short path matched")
	}

	trailing := func() bool {
		_, matched, _ := rt.match("GET", "/v1/admin/knowledge-bases/user-1/key/embedding/")
		return matched
	}()
	if !trailing {
		t.Fatalf("trailing slash did not match")
	}
}

func newPromptAdminServer(t *testing.T) (*Server, *fakeNotifier) {
	t.Helper()
	notifier := &fakeNotifier{}
	server := New(testSettings(""), fakeStatus{payload: map[string]any{"status": "ok"}}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithCatalogStore(catalog.NewMemoryStore()),
		WithChangeNotifier(notifier),
	)
	return server, notifier
}

func adminHeaders(namespace string) map[string]string {
	return map[string]string{"X-RAG-Namespace": namespace}
}

func TestAdminPromptLifecycle(t *testing.T) {
	server, notifier := newPromptAdminServer(t)
	handler := server.Handler()

	empty := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/prompts", remote: "127.0.0.1:5000", headers: adminHeaders("user-1"),
	})
	if empty.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", empty.Code, empty.Body.String())
	}
	emptyBody := decodeBody(t, empty)
	if prompts, ok := emptyBody["prompts"].([]any); !ok || len(prompts) != 0 {
		t.Fatalf("prompts = %v", emptyBody["prompts"])
	}

	template := `{"description":"Summary prompt","arguments":[{"name":"topic","required":true}],` +
		`"messages":[{"role":"user","text":"Summarize {{topic}}"}]}`
	created := performRequest(handler, testRequest{
		method:  "PUT",
		target:  "/v1/admin/prompts/summary",
		remote:  "127.0.0.1:5000",
		headers: adminHeaders("user-1"),
		body:    template,
	})
	if created.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", created.Code, created.Body.String())
	}
	if body := decodeBody(t, created); body["changed"] != true || body["name"] != "summary" {
		t.Fatalf("put body = %v", body)
	}
	if len(notifier.namespaces) != 1 || notifier.namespaces[0] != "user-1" {
		t.Fatalf("notifier = %v", notifier.namespaces)
	}

	unchanged := performRequest(handler, testRequest{
		method:  "PUT",
		target:  "/v1/admin/prompts/summary",
		remote:  "127.0.0.1:5000",
		headers: adminHeaders("user-1"),
		body:    template,
	})
	if body := decodeBody(t, unchanged); body["changed"] != false {
		t.Fatalf("unchanged body = %v", body)
	}
	if len(notifier.namespaces) != 1 {
		t.Fatalf("no-op put notified: %v", notifier.namespaces)
	}

	listed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/prompts", remote: "127.0.0.1:5000", headers: adminHeaders("user-1"),
	})
	prompts := decodeBody(t, listed)["prompts"].([]any)
	first := prompts[0].(map[string]any)
	if first["name"] != "summary" || first["description"] != "Summary prompt" {
		t.Fatalf("prompt = %v", first)
	}
	arguments := first["arguments"].([]any)
	if len(arguments) != 1 || arguments[0].(map[string]any)["description"] != "" {
		t.Fatalf("arguments = %v", arguments)
	}

	invalid := performRequest(handler, testRequest{
		method:  "PUT",
		target:  "/v1/admin/prompts/bad",
		remote:  "127.0.0.1:5000",
		headers: adminHeaders("user-1"),
		body:    `{"messages":[{"role":"user","text":"{{undeclared}}"}]}`,
	})
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid status = %d body = %s", invalid.Code, invalid.Body.String())
	}
	if detail, ok := decodeBody(t, invalid)["detail"].([]any); !ok || len(detail) == 0 {
		t.Fatalf("invalid detail = %s", invalid.Body.String())
	}

	deleted := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/admin/prompts/summary", remote: "127.0.0.1:5000", headers: adminHeaders("user-1"),
	})
	if deleted.Code != http.StatusOK || decodeBody(t, deleted)["deleted"] != "summary" {
		t.Fatalf("delete = %d %s", deleted.Code, deleted.Body.String())
	}
	if len(notifier.namespaces) != 2 {
		t.Fatalf("notifier = %v", notifier.namespaces)
	}
	secondDelete := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/admin/prompts/summary", remote: "127.0.0.1:5000", headers: adminHeaders("user-1"),
	})
	if secondDelete.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d", secondDelete.Code)
	}
}

func TestPromptAdminRequiresNamespaceHeaderAndCatalog(t *testing.T) {
	server, _ := newPromptAdminServer(t)
	missingNamespace := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/admin/prompts", remote: "127.0.0.1:5000",
	})
	if missingNamespace.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", missingNamespace.Code)
	}
	if detail := decodeBody(t, missingNamespace)["detail"]; detail != "Invalid RAG namespace." {
		t.Fatalf("detail = %v", detail)
	}

	withoutCatalog := New(testSettings(""), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := performRequest(withoutCatalog.Handler(), testRequest{
		method: "GET", target: "/v1/admin/prompts", remote: "127.0.0.1:5000", headers: adminHeaders("user-1"),
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}

type fakeToolRegistry struct {
	defaults     map[string]catalog.ToolSettings
	descriptions map[string]string
}

func (f fakeToolRegistry) Configured(string) ([]map[string]any, error) {
	tools := make([]map[string]any, 0, len(f.defaults))
	for name, defaults := range f.defaults {
		enabled := true
		if defaults.Enabled != nil {
			enabled = *defaults.Enabled
		}
		description := ""
		if defaults.Description != nil {
			description = *defaults.Description
		}
		tools = append(tools, map[string]any{
			"name": name, "enabled": enabled, "description": description,
		})
	}
	return tools, nil
}

func (f fakeToolRegistry) Defaults(_ string, name string) (catalog.ToolSettings, bool) {
	defaults, ok := f.defaults[name]
	return defaults, ok
}

func enabledPtr(value bool) *bool { return &value }

func TestAdminToolSettings(t *testing.T) {
	registry := fakeToolRegistry{
		defaults: map[string]catalog.ToolSettings{
			"search":       {Enabled: enabledPtr(true), Description: strPtr("Search the knowledge base.")},
			"add_document": {Enabled: enabledPtr(true), Description: strPtr("Upload one new file.")},
		},
	}
	notifier := &fakeNotifier{}
	server := New(testSettings(""), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithCatalogStore(catalog.NewMemoryStore()),
		WithToolRegistry(registry),
		WithChangeNotifier(notifier),
	)
	handler := server.Handler()

	listed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/tools", remote: "127.0.0.1:5000", headers: adminHeaders("user-1"),
	})
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", listed.Code, listed.Body.String())
	}
	tools := decodeBody(t, listed)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v", tools)
	}

	patched := performRequest(handler, testRequest{
		method:  "PATCH",
		target:  "/v1/admin/tools/search",
		remote:  "127.0.0.1:5000",
		headers: adminHeaders("user-1"),
		body:    `{"enabled":false,"description":"Custom"}`,
	})
	if patched.Code != http.StatusOK {
		t.Fatalf("patch status = %d body = %s", patched.Code, patched.Body.String())
	}
	patchedBody := decodeBody(t, patched)
	if patchedBody["changed"] != true {
		t.Fatalf("patch body = %v", patchedBody)
	}
	if len(notifier.namespaces) != 1 {
		t.Fatalf("notifier = %v", notifier.namespaces)
	}

	unchanged := performRequest(handler, testRequest{
		method:  "PATCH",
		target:  "/v1/admin/tools/search",
		remote:  "127.0.0.1:5000",
		headers: adminHeaders("user-1"),
		body:    `{"enabled":false,"description":"Custom"}`,
	})
	if decodeBody(t, unchanged)["changed"] != false {
		t.Fatalf("unchanged = %s", unchanged.Body.String())
	}

	unknown := performRequest(handler, testRequest{
		method:  "PATCH",
		target:  "/v1/admin/tools/missing",
		remote:  "127.0.0.1:5000",
		headers: adminHeaders("user-1"),
		body:    `{"enabled":true}`,
	})
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown status = %d", unknown.Code)
	}
	if detail := decodeBody(t, unknown)["detail"]; detail != "Unknown registered tool." {
		t.Fatalf("detail = %v", detail)
	}

	for _, body := range []string{`{}`, `{"enabled":null}`, `{"enabled":"yes"}`, `{"description":123}`} {
		invalid := performRequest(handler, testRequest{
			method:  "PATCH",
			target:  "/v1/admin/tools/search",
			remote:  "127.0.0.1:5000",
			headers: adminHeaders("user-1"),
			body:    body,
		})
		if invalid.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %s status = %d", body, invalid.Code)
		}
	}

	withoutRegistry := New(testSettings(""), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := performRequest(withoutRegistry.Handler(), testRequest{
		method: "GET", target: "/v1/admin/tools", remote: "127.0.0.1:5000", headers: adminHeaders("user-1"),
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}

func strPtr(value string) *string { return &value }

func newStateBackedServer(t *testing.T) *Server {
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
	return New(testSettings(""), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithAPIKeyStore(apikeys.New(stores.APIKeys)),
	)
}

func TestAPIKeyLifecycleAgainstSQLiteState(t *testing.T) {
	handler := newStateBackedServer(t).Handler()

	created := performRequest(handler, testRequest{
		method:  "POST",
		target:  "/v1/admin/api-keys",
		remote:  "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"name":"app","namespaces":["alpha","beta"]}`,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", created.Code, created.Body.String())
	}
	createdBody := decodeBody(t, created)
	key, _ := createdBody["key"].(string)
	if !strings.HasPrefix(key, "prk_") {
		t.Fatalf("key = %v", createdBody["key"])
	}
	keyID, _ := createdBody["id"].(string)
	if keyID == "" {
		t.Fatalf("created = %v", createdBody)
	}

	listed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/api-keys", remote: "127.0.0.1:5000",
	})
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", listed.Code, listed.Body.String())
	}
	if strings.Contains(listed.Body.String(), key) {
		t.Fatalf("list response leaked the plaintext key")
	}
	if got := listed.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}

	tenantHeaders := map[string]string{"Authorization": "Bearer " + key, "X-RAG-Namespace": "alpha"}
	allowed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/chunking/strategies", headers: tenantHeaders,
	})
	if allowed.Code != http.StatusOK {
		t.Fatalf("tenant status = %d body = %s", allowed.Code, allowed.Body.String())
	}
	forbidden := performRequest(handler, testRequest{
		method:  "GET",
		target:  "/v1/chunking/strategies",
		headers: map[string]string{"Authorization": "Bearer " + key, "X-RAG-Namespace": "other"},
	})
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("forbidden status = %d", forbidden.Code)
	}

	updatePath := "/v1/admin/api-keys/" + keyID
	updated := performRequest(handler, testRequest{
		method: "PUT", target: updatePath, remote: "127.0.0.1:5000",
		body: `{"name":"app","namespaces":["alpha"],"enabled":false}`,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("update status = %d body = %s", updated.Code, updated.Body.String())
	}
	disabled := performRequest(handler, testRequest{
		method: "GET", target: "/v1/chunking/strategies", headers: tenantHeaders,
	})
	if disabled.Code != http.StatusUnauthorized {
		t.Fatalf("disabled status = %d", disabled.Code)
	}

	performRequest(handler, testRequest{
		method: "PUT", target: updatePath, remote: "127.0.0.1:5000",
		body: `{"name":"app","namespaces":["alpha"],"enabled":true}`,
	})
	configPath := updatePath + "/mcp-config"
	mcpConfig := performRequest(handler, testRequest{
		method:  "GET",
		target:  configPath,
		remote:  "127.0.0.1:5000",
		headers: map[string]string{"X-RAG-Namespace": "alpha"},
	})
	if mcpConfig.Code != http.StatusOK {
		t.Fatalf("mcp-config status = %d body = %s", mcpConfig.Code, mcpConfig.Body.String())
	}
	if got := mcpConfig.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("mcp-config Cache-Control = %q", got)
	}
	configBody := decodeBody(t, mcpConfig)
	headers, _ := configBody["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer "+key || headers["X-RAG-Namespace"] != "alpha" {
		t.Fatalf("headers = %v", headers)
	}
	if configBody["url"] != "http://127.0.0.1:8011/mcp" {
		t.Fatalf("url = %v", configBody["url"])
	}

	deleted := performRequest(handler, testRequest{
		method: "DELETE", target: updatePath, remote: "127.0.0.1:5000",
	})
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", deleted.Code)
	}
	gone := performRequest(handler, testRequest{
		method: "GET", target: "/v1/chunking/strategies", headers: tenantHeaders,
	})
	if gone.Code != http.StatusUnauthorized {
		t.Fatalf("deleted key status = %d", gone.Code)
	}
	missing := performRequest(handler, testRequest{
		method: "DELETE", target: updatePath, remote: "127.0.0.1:5000",
	})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing delete status = %d", missing.Code)
	}
}

func TestAPIKeyValidation(t *testing.T) {
	handler := newStateBackedServer(t).Handler()
	for _, body := range []string{
		`{"name":"","namespaces":["alpha"]}`,
		`{"name":"app","namespaces":[]}`,
		`{"name":"app","namespaces":["bad namespace"]}`,
		`{"name":"app"}`,
		`{"name":"app","namespaces":["alpha"],"unknown":true}`,
	} {
		response := performRequest(handler, testRequest{
			method: "POST", target: "/v1/admin/api-keys", remote: "127.0.0.1:5000", body: body,
		})
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %s status = %d", body, response.Code)
		}
	}
}
