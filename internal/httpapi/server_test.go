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

	"github.com/Guan0923/xinghe_rag/internal/config"
)

type fakeStatus struct{ payload map[string]any }

func (f fakeStatus) Status(context.Context) map[string]any { return f.payload }

type fakeKeys map[string][]string

func (f fakeKeys) NamespacesForKey(secret string) ([]string, error) {
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
		"state":      map[string]any{"backend": "sqlite"},
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
}

func performRequest(handler http.Handler, request testRequest) *httptest.ResponseRecorder {
	req := httptest.NewRequest(request.method, request.target, nil)
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

func TestSnapshotPathsAreNoStore(t *testing.T) {
	server := newTestServer(t, testSettings("test-token"), nil)
	for _, target := range []string{"/v1/search", "/mcp", "/v1/search-results/abc", "/v1/artifacts/abc/content"} {
		response := performRequest(server.Handler(), testRequest{method: "GET", target: target})
		if got := response.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s Cache-Control = %q, want no-store", target, got)
		}
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
