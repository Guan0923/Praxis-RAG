package e2e

// End-to-end coverage of per-account API-key isolation: real Console login
// sessions, the real access service, the real API-key store and the real MySQL
// state behind the real HTTP surface. The knowledge-base ownership is seeded
// through the state repository; every API-key assertion goes through HTTP.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/access"
	"github.com/Guan0923/Praxis-RAG/internal/apikeys"
	"github.com/Guan0923/Praxis-RAG/internal/auth"
	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/httpapi"
	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/wire"
)

// apiKeysEndToEndDetail is the frozen 403 detail when a normal account binds an
// API key to a knowledge base it does not own.
const apiKeysEndToEndDetail = "只能为本人创建的知识库创建 API Key"

// apiKeysHandler builds the real HTTP surface with Console accounts, the sharing
// service and the stored-credential API wired in.
func apiKeysHandler(t *testing.T, h *harness) http.Handler {
	t.Helper()
	settings := config.ServiceSettings{
		Host:                "127.0.0.1",
		Port:                8011,
		DataRoot:            "unused",
		AuthEnabled:         true,
		RegistrationEnabled: true,
		SessionTTL:          time.Hour,
	}
	users := auth.NewService(h.Stores.Users, h.Stores.Sessions, auth.Options{
		RegistrationEnabled: true,
		SessionTTL:          time.Hour,
	})
	sharing := access.NewService(h.Stores, access.Options{InvitationTTL: time.Hour})
	// The stored-credential service is wired both as the management store and as
	// the tenant KeyLookup, so Bearer API keys authenticate and scope tenant
	// routes exactly like the real service does.
	server := httpapi.New(settings, nil, apikeys.New(h.Stores.APIKeys), testLogger(),
		httpapi.WithRuntime(wire.HTTPRuntime{Service: h.Service}),
		httpapi.WithKnowledgeBaseAdminRuntime(wire.HTTPRuntime{Service: h.Service}),
		httpapi.WithKnowledgeBaseSelfCreateRuntime(wire.HTTPRuntime{Service: h.Service}),
		httpapi.WithAccessControl(sharing),
		httpapi.WithUserAuth(users),
		httpapi.WithAPIKeyStore(apikeys.New(h.Stores.APIKeys)),
	)
	return server.Handler()
}

// apiKeyBearerRequest performs one tenant request authenticated with a stored
// API-key secret. The remote address is deliberately non-loopback so the
// assertion proves the credential path instead of the local break-glass route.
func apiKeyBearerRequest(
	t *testing.T, handler http.Handler, method, target, secret string, headers map[string]string,
) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request.RemoteAddr = "10.0.0.9:5000"
	request.Host = "127.0.0.1:8011"
	request.Header.Set("Authorization", "Bearer "+secret)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	decoded := map[string]any{}
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode %s %s (%d): %v body=%s", method, target, recorder.Code, err, recorder.Body.String())
		}
	}
	return recorder, decoded
}

// localAPIKeysRequest performs one session-less loopback management request.
func localAPIKeysRequest(
	t *testing.T, handler http.Handler, method, target string, headers map[string]string, payload any,
) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var body []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s %s: %v", method, target, err)
		}
		body = encoded
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:5000"
	request.Host = "127.0.0.1:8011"
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	decoded := map[string]any{}
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode %s %s (%d): %v body=%s", method, target, recorder.Code, err, recorder.Body.String())
		}
	}
	return recorder, decoded
}

// apiKeysStringArray decodes a JSON string array.
func apiKeysStringArray(t *testing.T, raw any) []string {
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

// assertAPIKeysStringSet compares two string sets ignoring order.
func assertAPIKeysStringSet(t *testing.T, got, want []string, description string) {
	t.Helper()
	sortedGot := append([]string(nil), got...)
	sort.Strings(sortedGot)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if len(sortedGot) != len(sortedWant) {
		t.Fatalf("%s = %v, want %v", description, got, want)
	}
	for index := range sortedWant {
		if sortedGot[index] != sortedWant[index] {
			t.Fatalf("%s = %v, want %v", description, got, want)
		}
	}
}

// TestAPIKeyIsolationEndToEnd walks the per-account API-key workflow across the
// real HTTP surface: self-service creation, cross-account 404s, the
// administrator view, the local break-glass surface and the namespace
// owned_namespaces projection.
func TestAPIKeyIsolationEndToEnd(t *testing.T) {
	h := newHarness(t, "")
	handler := apiKeysHandler(t, h)

	admin := newConsoleClient(t, handler)
	if role := admin.register("e2e-admin", "AdminPass123")["role"]; role != state.RoleAdmin {
		t.Fatalf("first account role = %v, want %s", role, state.RoleAdmin)
	}
	alice := newConsoleClient(t, handler)
	alice.register("e2e-alice", "AlicePass123")
	bob := newConsoleClient(t, handler)
	bob.register("e2e-bob", "BobPass123")

	// Seed ownership directly: alice owns alice-kb, bob owns bob-kb and grants
	// alice a read grant on bob-kb (visible, but not owned).
	ctx := context.Background()
	for namespace, owner := range map[string]string{"alice-kb": alice.id, "bob-kb": bob.id} {
		if err := h.Stores.KnowledgeBases.CreateWithOwner(ctx, map[string]any{
			"namespace":  namespace,
			"collection": namespace,
		}, owner); err != nil {
			t.Fatalf("CreateWithOwner(%q) error = %v", namespace, err)
		}
	}
	if err := h.Stores.Permissions.Upsert(ctx, state.Permission{
		Namespace:     "bob-kb",
		GranteeUserID: alice.id,
		Permission:    state.PermissionRead,
		GrantedBy:     bob.id,
	}); err != nil {
		t.Fatalf("Permissions.Upsert(bob-kb) error = %v", err)
	}

	// Fresh accounts start with no credentials of their own.
	recorder, payload := alice.json(http.MethodGet, "/v1/admin/api-keys", "", nil)
	requireStatus(t, recorder, http.StatusOK, "alice initial key list")
	if keys, _ := payload["keys"].([]any); len(keys) != 0 {
		t.Fatalf("alice initial keys = %v", payload["keys"])
	}

	// A normal account may only bind its own knowledge bases.
	recorder, payload = bob.json(http.MethodPost, "/v1/admin/api-keys", "",
		map[string]any{"name": "bob-steal", "namespaces": []string{"alice-kb"}})
	requireStatus(t, recorder, http.StatusForbidden, "bob creates a key for alice-kb")
	if detail := detailOf(payload); detail != apiKeysEndToEndDetail {
		t.Fatalf("foreign-namespace detail = %q, want %q", detail, apiKeysEndToEndDetail)
	}

	// Alice creates her own credential; the plaintext is returned once.
	recorder, payload = alice.json(http.MethodPost, "/v1/admin/api-keys", "",
		map[string]any{"name": "alice-config", "namespaces": []string{"alice-kb"}})
	requireStatus(t, recorder, http.StatusCreated, "alice creates her key")
	aliceKeyID, _ := payload["id"].(string)
	aliceSecret, _ := payload["key"].(string)
	if aliceKeyID == "" || len(aliceSecret) < 5 || aliceSecret[:4] != "prk_" {
		t.Fatalf("alice created key = %v", payload)
	}
	if payload["owner_user_id"] != alice.id {
		t.Fatalf("alice key owner_user_id = %v, want %q", payload["owner_user_id"], alice.id)
	}

	// The Bearer credential only sees the knowledge bases it is bound to: the
	// namespace list is intersected with the key binding in both fields, so the
	// full tenant list never leaks through owned_namespaces.
	recorder, payload = apiKeyBearerRequest(t, handler, http.MethodGet, "/v1/namespaces",
		aliceSecret, map[string]string{"X-RAG-Namespace": "alice-kb"})
	requireStatus(t, recorder, http.StatusOK, "alice key namespace list")
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("alice key namespace list Cache-Control = %q", got)
	}
	assertAPIKeysStringSet(t, apiKeysStringArray(t, payload["namespaces"]),
		[]string{"alice-kb"}, "alice key namespaces")
	assertAPIKeysStringSet(t, apiKeysStringArray(t, payload["owned_namespaces"]),
		[]string{"alice-kb"}, "alice key owned_namespaces")
	// The list is namespace-agnostic: no X-RAG-Namespace (or a stale one) still
	// returns exactly the credential binding, so a rotated key can recover.
	recorder, payload = apiKeyBearerRequest(t, handler, http.MethodGet, "/v1/namespaces",
		aliceSecret, nil)
	requireStatus(t, recorder, http.StatusOK, "alice key headerless namespace list")
	assertAPIKeysStringSet(t, apiKeysStringArray(t, payload["namespaces"]),
		[]string{"alice-kb"}, "alice key headerless namespaces")
	assertAPIKeysStringSet(t, apiKeysStringArray(t, payload["owned_namespaces"]),
		[]string{"alice-kb"}, "alice key headerless owned_namespaces")
	// Tenant routes keep the strict namespace binding: only the listing is exempt.
	recorder, payload = apiKeyBearerRequest(t, handler, http.MethodGet, "/v1/documents",
		aliceSecret, map[string]string{"X-RAG-Namespace": "bob-kb"})
	requireStatus(t, recorder, http.StatusForbidden, "alice key reaches bob-kb")
	if detail := detailOf(payload); detail != "API Key 未获授权访问此 Namespace" {
		t.Fatalf("alice key foreign namespace detail = %q", detail)
	}

	// Both accounts only see their own list.
	recorder, payload = alice.json(http.MethodGet, "/v1/admin/api-keys", "", nil)
	requireStatus(t, recorder, http.StatusOK, "alice key list")
	aliceKeys, _ := payload["keys"].([]any)
	if len(aliceKeys) != 1 || aliceKeys[0].(map[string]any)["id"] != aliceKeyID {
		t.Fatalf("alice key list = %v", payload["keys"])
	}
	recorder, payload = bob.json(http.MethodGet, "/v1/admin/api-keys", "", nil)
	requireStatus(t, recorder, http.StatusOK, "bob key list")
	if keys, _ := payload["keys"].([]any); len(keys) != 0 {
		t.Fatalf("bob sees alice's key: %v", payload["keys"])
	}

	// Every cross-account operation reports a missing key.
	keyPath := "/v1/admin/api-keys/" + aliceKeyID
	recorder, payload = bob.json(http.MethodGet, keyPath+"/mcp-config", "bob-kb", nil)
	requireStatus(t, recorder, http.StatusNotFound, "bob reads alice's mcp-config")
	if detail := detailOf(payload); detail != "API Key 不存在" {
		t.Fatalf("bob mcp-config detail = %q", detail)
	}
	recorder, payload = bob.json(http.MethodPut, keyPath, "",
		map[string]any{"name": "stolen", "namespaces": []string{"bob-kb"}, "enabled": true})
	requireStatus(t, recorder, http.StatusNotFound, "bob updates alice's key")
	if detail := detailOf(payload); detail != "API Key 不存在" {
		t.Fatalf("bob update detail = %q", detail)
	}
	recorder, payload = bob.json(http.MethodDelete, keyPath, "", nil)
	requireStatus(t, recorder, http.StatusNotFound, "bob deletes alice's key")
	if detail := detailOf(payload); detail != "API Key 不存在" {
		t.Fatalf("bob delete detail = %q", detail)
	}

	// The owner can export the MCP configuration for her own key.
	recorder, payload = alice.json(http.MethodGet, keyPath+"/mcp-config", "alice-kb", nil)
	requireStatus(t, recorder, http.StatusOK, "alice reads her mcp-config")
	configHeaders, _ := payload["headers"].(map[string]any)
	if configHeaders["Authorization"] != "Bearer "+aliceSecret {
		t.Fatalf("alice mcp-config headers = %v", configHeaders)
	}

	// The administrator sees every credential with its owner username and can
	// manage either account's key.
	recorder, payload = admin.json(http.MethodGet, "/v1/admin/api-keys", "", nil)
	requireStatus(t, recorder, http.StatusOK, "admin key list")
	adminEntries, _ := payload["keys"].([]any)
	if len(adminEntries) != 1 {
		t.Fatalf("admin key list = %v", payload["keys"])
	}
	adminEntry, _ := adminEntries[0].(map[string]any)
	if adminEntry["owner_user_id"] != alice.id || adminEntry["owner_username"] != "e2e-alice" {
		t.Fatalf("admin key owner projection = %v", adminEntry)
	}
	recorder, _ = admin.json(http.MethodPut, keyPath, "",
		map[string]any{"name": "admin-renamed", "namespaces": []string{"alice-kb", "bob-kb"}, "enabled": true})
	requireStatus(t, recorder, http.StatusOK, "admin updates alice's key")
	recorder, payload = admin.json(http.MethodPost, "/v1/admin/api-keys", "",
		map[string]any{"name": "admin-key", "namespaces": []string{"bob-kb"}})
	requireStatus(t, recorder, http.StatusCreated, "admin creates a key for bob-kb")
	adminKeyID, _ := payload["id"].(string)
	if payload["owner_user_id"] != admin.id {
		t.Fatalf("admin key owner_user_id = %v, want %q", payload["owner_user_id"], admin.id)
	}
	recorder, _ = admin.json(http.MethodDelete, keyPath, "", nil)
	requireStatus(t, recorder, http.StatusNoContent, "admin deletes alice's key")
	recorder, payload = alice.json(http.MethodGet, "/v1/admin/api-keys", "", nil)
	requireStatus(t, recorder, http.StatusOK, "alice key list after admin delete")
	if keys, _ := payload["keys"].([]any); len(keys) != 0 {
		t.Fatalf("alice still lists keys deleted by the admin: %v", payload["keys"])
	}

	// Session-less loopback management sees every key and creates unowned keys.
	recorder, payload = localAPIKeysRequest(t, handler, http.MethodGet, "/v1/admin/api-keys", nil, nil)
	requireStatus(t, recorder, http.StatusOK, "local key list")
	localEntries, _ := payload["keys"].([]any)
	if len(localEntries) != 1 || localEntries[0].(map[string]any)["id"] != adminKeyID {
		t.Fatalf("local key list = %v", payload["keys"])
	}
	recorder, payload = localAPIKeysRequest(t, handler, http.MethodPost, "/v1/admin/api-keys", nil,
		map[string]any{"name": "local-key", "namespaces": []string{"bob-kb"}})
	requireStatus(t, recorder, http.StatusCreated, "local key create")
	localKeyID, _ := payload["id"].(string)
	if owner, ok := payload["owner_user_id"]; ok && owner != "" {
		t.Fatalf("local key owner_user_id = %v, want empty", owner)
	}
	if localKeyID == "" {
		t.Fatalf("local key create payload = %v", payload)
	}

	// The namespace projection separates "visible" from "owned".
	recorder, payload = alice.json(http.MethodGet, "/v1/namespaces", "alice-kb", nil)
	requireStatus(t, recorder, http.StatusOK, "alice namespace list")
	aliceNamespaces := apiKeysStringArray(t, payload["namespaces"])
	aliceOwned := apiKeysStringArray(t, payload["owned_namespaces"])
	assertAPIKeysStringSet(t, aliceOwned, []string{"alice-kb"}, "alice owned_namespaces")
	if !containsString(payload["namespaces"], "alice-kb") || !containsString(payload["namespaces"], "bob-kb") {
		t.Fatalf("alice namespaces = %v, want owned and shared knowledge bases", aliceNamespaces)
	}

	recorder, payload = localAPIKeysRequest(t, handler, http.MethodGet, "/v1/namespaces",
		map[string]string{"X-RAG-Namespace": "alice-kb"}, nil)
	requireStatus(t, recorder, http.StatusOK, "local namespace list")
	localNamespaces := apiKeysStringArray(t, payload["namespaces"])
	localOwned := apiKeysStringArray(t, payload["owned_namespaces"])
	assertAPIKeysStringSet(t, localOwned, localNamespaces, "local owned_namespaces")
}
