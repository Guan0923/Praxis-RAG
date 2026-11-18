package httpapi

// Per-account API-key isolation coverage. The server under test wires the real
// MySQL state, the real API-key store and the real access service together with
// fake Console sessions, so the assertions exercise the actual HTTP policy:
// a normal account only manages its own credentials and may only scope them to
// knowledge bases it owns, administrators manage every credential, and
// session-less loopback requests keep the local break-glass management surface.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/access"
	"github.com/Guan0923/Praxis-RAG/internal/apikeys"
	"github.com/Guan0923/Praxis-RAG/internal/auth"
	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/statetest"
)

// createdAPIKeyDetail is the 403 detail frozen for a normal account attempting
// to bind an API key to a knowledge base it does not own.
const createdAPIKeyDetail = "只能为本人创建的知识库创建 API Key"

// apiKeyOwnerFixture is one isolated MySQL state plus a server wired with real
// credential/access services and fake login sessions.
type apiKeyOwnerFixture struct {
	stores *state.Stores
	server *Server

	admin state.User
	alice state.User
	bob   state.User
}

// newAPIKeyOwnerFixture opens the shared MySQL test schema and seeds three
// accounts and three knowledge bases: one owned by alice, one by bob, one by bob
// and shared read-only with alice. The first account is the administrator.
func newAPIKeyOwnerFixture(t *testing.T) *apiKeyOwnerFixture {
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

	ctx := context.Background()
	admin, err := stores.Users.Create(ctx, "owner-admin", "x")
	if err != nil {
		t.Fatalf("Users.Create(admin) error = %v", err)
	}
	alice, err := stores.Users.Create(ctx, "owner-alice", "x")
	if err != nil {
		t.Fatalf("Users.Create(alice) error = %v", err)
	}
	bob, err := stores.Users.Create(ctx, "owner-bob", "x")
	if err != nil {
		t.Fatalf("Users.Create(bob) error = %v", err)
	}
	for _, knowledgeBase := range []struct {
		namespace string
		owner     string
	}{
		{"alice-kb", alice.ID},
		{"bob-kb", bob.ID},
		{"shared-kb", bob.ID},
	} {
		if err := stores.KnowledgeBases.CreateWithOwner(ctx, map[string]any{
			"namespace":  knowledgeBase.namespace,
			"collection": knowledgeBase.namespace,
		}, knowledgeBase.owner); err != nil {
			t.Fatalf("CreateWithOwner(%q) error = %v", knowledgeBase.namespace, err)
		}
	}
	// shared-kb is visible to alice through a read grant, but alice does not own
	// it: it must not be usable as an API-key binding.
	if err := stores.Permissions.Upsert(ctx, state.Permission{
		Namespace:     "shared-kb",
		GranteeUserID: alice.ID,
		Permission:    state.PermissionRead,
		GrantedBy:     bob.ID,
	}); err != nil {
		t.Fatalf("Permissions.Upsert(shared-kb) error = %v", err)
	}

	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		"token-admin": {User: admin, CSRF: "csrf-token-admin"},
		"token-alice": {User: alice, CSRF: "csrf-token-alice"},
		"token-bob":   {User: bob, CSRF: "csrf-token-bob"},
	}}
	settings := testSettings("")
	settings.AuthEnabled = true
	settings.SessionTTL = time.Hour
	knowledgeBaseRuntime := &fakeKBAdminRuntime{listPayload: []map[string]any{
		{"namespace": "alice-kb"},
		{"namespace": "bob-kb"},
		{"namespace": "shared-kb"},
	}}
	server := New(settings, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithAPIKeyStore(apikeys.New(stores.APIKeys)),
		WithUserAuth(users),
		WithAccessControl(access.NewService(stores, access.Options{InvitationTTL: time.Hour})),
		WithKnowledgeBaseAdminRuntime(knowledgeBaseRuntime),
	)
	return &apiKeyOwnerFixture{stores: stores, server: server, admin: admin, alice: alice, bob: bob}
}

// createAPIKeyOwnerKey seeds one credential through the real repository. An
// empty owner creates a legacy unowned credential.
func (f *apiKeyOwnerFixture) createAPIKeyOwnerKey(
	t *testing.T, name string, namespaces []string, ownerUserID string,
) state.CreatedAPIKey {
	t.Helper()
	created, err := f.stores.APIKeys.CreateForOwner(context.Background(), name, namespaces, ownerUserID)
	if err != nil {
		t.Fatalf("APIKeys.CreateForOwner(%q, %v) error = %v", name, namespaces, err)
	}
	return created
}

// apiKeyOwnerWriteHeaders authenticates a cookie write with its CSRF token.
func apiKeyOwnerWriteHeaders(token string) map[string]string {
	return mergeHeaders(sessionHeaders(token, ""), map[string]string{"Content-Type": "application/json"})
}

// apiKeyOwnerListEntries decodes the {"keys": [...]} list payload.
func apiKeyOwnerListEntries(t *testing.T, recorder *httptest.ResponseRecorder) []any {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	entries, ok := body["keys"].([]any)
	if !ok {
		t.Fatalf("keys = %v", body["keys"])
	}
	return entries
}

// apiKeyOwnerEntry returns one entry of a list payload by key id.
func apiKeyOwnerEntry(t *testing.T, entries []any, keyID string) map[string]any {
	t.Helper()
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry["id"] == keyID {
			return entry
		}
	}
	t.Fatalf("api key %q not found in %v", keyID, entries)
	return nil
}

// apiKeyOwnerHasKey reports whether a list payload contains one key id.
func apiKeyOwnerHasKey(entries []any, keyID string) bool {
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry["id"] == keyID {
			return true
		}
	}
	return false
}

// apiKeyOwnerDetail decodes a {"detail": "..."} error body.
func apiKeyOwnerDetail(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	detail, _ := decodeBody(t, recorder)["detail"].(string)
	return detail
}

// apiKeyOwnerStrings decodes a JSON string array (nil is an empty array here).
func apiKeyOwnerStrings(t *testing.T, raw any) []string {
	t.Helper()
	if raw == nil {
		return nil
	}
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

// assertAPIKeyOwnerStrings compares two string sets ignoring order.
func assertAPIKeyOwnerStrings(t *testing.T, got, want []string, description string) {
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

// apiKeyOwnerContains reports whether values contains want.
func apiKeyOwnerContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestAPIKeySessionUserIsolation covers the normal-account view: an account only
// lists and manages its own credentials, may only bind knowledge bases it owns,
// and cross-account access looks exactly like a missing key.
func TestAPIKeySessionUserIsolation(t *testing.T) {
	fixture := newAPIKeyOwnerFixture(t)
	handler := fixture.server.Handler()
	aliceKey := fixture.createAPIKeyOwnerKey(t, "alice-config", []string{"alice-kb"}, fixture.alice.ID)
	bobKey := fixture.createAPIKeyOwnerKey(t, "bob-config", []string{"bob-kb"}, fixture.bob.ID)
	legacyKey := fixture.createAPIKeyOwnerKey(t, "legacy-config", []string{"shared-kb"}, "")

	// A normal account sees exactly its own credential: not another account's,
	// not the unowned legacy one, and never a plaintext secret.
	listed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/api-keys", remote: "10.0.0.9:5000",
		headers: sessionHeaders("token-alice", ""),
	})
	entries := apiKeyOwnerListEntries(t, listed)
	if len(entries) != 1 {
		t.Fatalf("alice list = %v, want only her own key", entries)
	}
	entry := apiKeyOwnerEntry(t, entries, aliceKey.ID)
	if entry["owner_user_id"] != fixture.alice.ID {
		t.Fatalf("alice list owner_user_id = %v, want %q", entry["owner_user_id"], fixture.alice.ID)
	}
	if _, leaked := entry["key"]; leaked {
		t.Fatal("alice list exposed a plaintext secret")
	}
	if apiKeyOwnerHasKey(entries, bobKey.ID) || apiKeyOwnerHasKey(entries, legacyKey.ID) {
		t.Fatalf("alice list leaked another credential: %v", entries)
	}

	bobListed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/api-keys", remote: "10.0.0.9:5000",
		headers: sessionHeaders("token-bob", ""),
	})
	bobEntries := apiKeyOwnerListEntries(t, bobListed)
	if len(bobEntries) != 1 || !apiKeyOwnerHasKey(bobEntries, bobKey.ID) {
		t.Fatalf("bob list = %v, want only his own key", bobEntries)
	}

	// The owner's MCP config export works for the owner...
	ownConfig := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/api-keys/" + aliceKey.ID + "/mcp-config",
		remote: "10.0.0.9:5000", headers: sessionHeaders("token-alice", "alice-kb"),
	})
	if ownConfig.Code != http.StatusOK {
		t.Fatalf("owner mcp-config status = %d body = %s", ownConfig.Code, ownConfig.Body.String())
	}
	configHeaders, _ := decodeBody(t, ownConfig)["headers"].(map[string]any)
	if configHeaders["Authorization"] != "Bearer "+aliceKey.Key {
		t.Fatalf("owner mcp-config headers = %v", configHeaders)
	}

	// ...but another account gets the missing-key response for every operation.
	mcpPath := "/v1/admin/api-keys/" + aliceKey.ID + "/mcp-config"
	crossRead := performRequest(handler, testRequest{
		method: "GET", target: mcpPath, remote: "10.0.0.9:5000",
		headers: sessionHeaders("token-bob", "bob-kb"),
	})
	if crossRead.Code != http.StatusNotFound {
		t.Fatalf("cross-account mcp-config status = %d body = %s", crossRead.Code, crossRead.Body.String())
	}
	if detail := apiKeyOwnerDetail(t, crossRead); detail != "API Key 不存在" {
		t.Fatalf("cross-account mcp-config detail = %q", detail)
	}

	crossUpdate := performRequest(handler, testRequest{
		method: "PUT", target: "/v1/admin/api-keys/" + aliceKey.ID, remote: "10.0.0.9:5000",
		headers: apiKeyOwnerWriteHeaders("token-bob"),
		body:    `{"name":"stolen","namespaces":["bob-kb"],"enabled":true}`,
	})
	if crossUpdate.Code != http.StatusNotFound {
		t.Fatalf("cross-account update status = %d body = %s", crossUpdate.Code, crossUpdate.Body.String())
	}
	if detail := apiKeyOwnerDetail(t, crossUpdate); detail != "API Key 不存在" {
		t.Fatalf("cross-account update detail = %q", detail)
	}

	crossDelete := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/admin/api-keys/" + aliceKey.ID, remote: "10.0.0.9:5000",
		headers: sessionHeaders("token-bob", ""),
	})
	if crossDelete.Code != http.StatusNotFound {
		t.Fatalf("cross-account delete status = %d body = %s", crossDelete.Code, crossDelete.Body.String())
	}
	if detail := apiKeyOwnerDetail(t, crossDelete); detail != "API Key 不存在" {
		t.Fatalf("cross-account delete detail = %q", detail)
	}
	untouched, err := fixture.stores.APIKeys.Get(context.Background(), aliceKey.ID)
	if err != nil || untouched == nil {
		t.Fatalf("alice key after cross-account attempts = %+v, %v", untouched, err)
	}
	if untouched.Name != "alice-config" || !untouched.Enabled || untouched.OwnerUserID != fixture.alice.ID {
		t.Fatalf("cross-account attempts modified the key: %+v", untouched)
	}

	// A normal account creates a credential owned by itself and scoped to a
	// knowledge base it owns. The plaintext is returned exactly once and the
	// credential grants tenant access to exactly that namespace.
	created := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/api-keys", remote: "10.0.0.9:5000",
		headers: apiKeyOwnerWriteHeaders("token-alice"),
		body:    `{"name":"alice-new","namespaces":["alice-kb"]}`,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", created.Code, created.Body.String())
	}
	createdBody := decodeBody(t, created)
	secret, _ := createdBody["key"].(string)
	if !strings.HasPrefix(secret, "prk_") {
		t.Fatalf("created key = %v, want a prk_ secret", createdBody["key"])
	}
	if createdBody["owner_user_id"] != fixture.alice.ID {
		t.Fatalf("created owner_user_id = %v, want %q", createdBody["owner_user_id"], fixture.alice.ID)
	}
	createdID, _ := createdBody["id"].(string)
	if createdID == "" {
		t.Fatalf("created = %v", createdBody)
	}
	tenantAllowed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/chunking/strategies",
		headers: map[string]string{"Authorization": "Bearer " + secret, "X-RAG-Namespace": "alice-kb"},
	})
	if tenantAllowed.Code != http.StatusOK {
		t.Fatalf("tenant status = %d body = %s", tenantAllowed.Code, tenantAllowed.Body.String())
	}
	tenantForbidden := performRequest(handler, testRequest{
		method: "GET", target: "/v1/chunking/strategies",
		headers: map[string]string{"Authorization": "Bearer " + secret, "X-RAG-Namespace": "bob-kb"},
	})
	if tenantForbidden.Code != http.StatusForbidden {
		t.Fatalf("foreign namespace status = %d body = %s", tenantForbidden.Code, tenantForbidden.Body.String())
	}
	entries = apiKeyOwnerListEntries(t, performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/api-keys", remote: "10.0.0.9:5000",
		headers: sessionHeaders("token-alice", ""),
	}))
	if len(entries) != 2 || !apiKeyOwnerHasKey(entries, createdID) {
		t.Fatalf("alice list after create = %v", entries)
	}

	// A normal account cannot create a credential for a knowledge base it does
	// not own, with the frozen detail.
	forbiddenCreate := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/api-keys", remote: "10.0.0.9:5000",
		headers: apiKeyOwnerWriteHeaders("token-alice"),
		body:    `{"name":"bad","namespaces":["bob-kb"]}`,
	})
	if forbiddenCreate.Code != http.StatusForbidden {
		t.Fatalf("foreign-namespace create status = %d body = %s", forbiddenCreate.Code, forbiddenCreate.Body.String())
	}
	if detail := apiKeyOwnerDetail(t, forbiddenCreate); detail != createdAPIKeyDetail {
		t.Fatalf("foreign-namespace create detail = %q, want %q", detail, createdAPIKeyDetail)
	}

	// Updates must stay within the owned namespaces too: another account's
	// knowledge base and even a granted-but-not-owned one are rejected.
	for _, namespace := range []string{"bob-kb", "shared-kb"} {
		widened := performRequest(handler, testRequest{
			method: "PUT", target: "/v1/admin/api-keys/" + createdID, remote: "10.0.0.9:5000",
			headers: apiKeyOwnerWriteHeaders("token-alice"),
			body:    `{"name":"alice-new","namespaces":["` + namespace + `"],"enabled":true}`,
		})
		if widened.Code != http.StatusForbidden {
			t.Fatalf("update to %q status = %d body = %s", namespace, widened.Code, widened.Body.String())
		}
	}
	renamed := performRequest(handler, testRequest{
		method: "PUT", target: "/v1/admin/api-keys/" + createdID, remote: "10.0.0.9:5000",
		headers: apiKeyOwnerWriteHeaders("token-alice"),
		body:    `{"name":"alice-renamed","namespaces":["alice-kb"],"enabled":false}`,
	})
	if renamed.Code != http.StatusOK {
		t.Fatalf("own update status = %d body = %s", renamed.Code, renamed.Body.String())
	}
	renamedBody := decodeBody(t, renamed)
	if renamedBody["name"] != "alice-renamed" || renamedBody["enabled"] != false {
		t.Fatalf("own update body = %v", renamedBody)
	}
	if renamedBody["owner_user_id"] != fixture.alice.ID {
		t.Fatalf("own update dropped the owner: %v", renamedBody)
	}

	// Deleting the own credential works; then CSRF protection for cookie writes.
	deleted := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/admin/api-keys/" + createdID, remote: "10.0.0.9:5000",
		headers: sessionHeaders("token-alice", ""),
	})
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("own delete status = %d body = %s", deleted.Code, deleted.Body.String())
	}
	noCSRF := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/api-keys", remote: "10.0.0.9:5000",
		headers: mergeHeaders(cookieFor("token-alice"), map[string]string{"Content-Type": "application/json"}),
		body:    `{"name":"no-csrf","namespaces":["alice-kb"]}`,
	})
	if noCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing-CSRF create status = %d body = %s", noCSRF.Code, noCSRF.Body.String())
	}
	// A bearer session token cannot be sent implicitly by a browser, so it is
	// exempt from the CSRF double submit.
	bearerWrite := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/api-keys", remote: "10.0.0.9:5000",
		headers: map[string]string{"Authorization": "Bearer token-alice", "Content-Type": "application/json"},
		body:    `{"name":"bearer-key","namespaces":["alice-kb"]}`,
	})
	if bearerWrite.Code != http.StatusCreated {
		t.Fatalf("bearer create status = %d body = %s", bearerWrite.Code, bearerWrite.Body.String())
	}
	if owner := decodeBody(t, bearerWrite)["owner_user_id"]; owner != fixture.alice.ID {
		t.Fatalf("bearer create owner = %v, want %q", owner, fixture.alice.ID)
	}
}

// TestAPIKeySessionAdminVisibility covers the administrator view: every
// credential is visible with its owner's username, and the administrator may
// manage and delete any of them.
func TestAPIKeySessionAdminVisibility(t *testing.T) {
	fixture := newAPIKeyOwnerFixture(t)
	handler := fixture.server.Handler()
	aliceKey := fixture.createAPIKeyOwnerKey(t, "alice-config", []string{"alice-kb"}, fixture.alice.ID)
	bobKey := fixture.createAPIKeyOwnerKey(t, "bob-config", []string{"bob-kb"}, fixture.bob.ID)
	legacyKey := fixture.createAPIKeyOwnerKey(t, "legacy-config", []string{"shared-kb"}, "")

	listed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/api-keys", remote: "10.0.0.9:5000",
		headers: sessionHeaders("token-admin", ""),
	})
	entries := apiKeyOwnerListEntries(t, listed)
	if len(entries) != 3 {
		t.Fatalf("admin list = %v, want all three credentials", entries)
	}
	aliceEntry := apiKeyOwnerEntry(t, entries, aliceKey.ID)
	if aliceEntry["owner_user_id"] != fixture.alice.ID || aliceEntry["owner_username"] != fixture.alice.Username {
		t.Fatalf("admin list alice owner = %v/%v", aliceEntry["owner_user_id"], aliceEntry["owner_username"])
	}
	bobEntry := apiKeyOwnerEntry(t, entries, bobKey.ID)
	if bobEntry["owner_username"] != fixture.bob.Username {
		t.Fatalf("admin list bob owner_username = %v", bobEntry["owner_username"])
	}
	legacyEntry := apiKeyOwnerEntry(t, entries, legacyKey.ID)
	if owner, ok := legacyEntry["owner_user_id"]; ok && owner != "" {
		t.Fatalf("legacy key owner_user_id = %v, want empty", owner)
	}
	if owner, ok := legacyEntry["owner_username"]; ok && owner != "" {
		t.Fatalf("legacy key owner_username = %v, want empty", owner)
	}

	// The administrator may update any credential; ownership is preserved.
	updated := performRequest(handler, testRequest{
		method: "PUT", target: "/v1/admin/api-keys/" + aliceKey.ID, remote: "10.0.0.9:5000",
		headers: apiKeyOwnerWriteHeaders("token-admin"),
		body:    `{"name":"admin-renamed","namespaces":["alice-kb","bob-kb"],"enabled":false}`,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("admin update status = %d body = %s", updated.Code, updated.Body.String())
	}
	updatedBody := decodeBody(t, updated)
	if updatedBody["name"] != "admin-renamed" || updatedBody["enabled"] != false {
		t.Fatalf("admin update body = %v", updatedBody)
	}
	if updatedBody["owner_user_id"] != fixture.alice.ID || updatedBody["owner_username"] != fixture.alice.Username {
		t.Fatalf("admin update owner = %v", updatedBody)
	}
	reenabled := performRequest(handler, testRequest{
		method: "PUT", target: "/v1/admin/api-keys/" + aliceKey.ID, remote: "10.0.0.9:5000",
		headers: apiKeyOwnerWriteHeaders("token-admin"),
		body:    `{"name":"admin-renamed","namespaces":["alice-kb","bob-kb"],"enabled":true}`,
	})
	if reenabled.Code != http.StatusOK {
		t.Fatalf("admin re-enable status = %d body = %s", reenabled.Code, reenabled.Body.String())
	}
	mcpConfig := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/api-keys/" + aliceKey.ID + "/mcp-config",
		remote: "10.0.0.9:5000", headers: sessionHeaders("token-admin", "alice-kb"),
	})
	if mcpConfig.Code != http.StatusOK {
		t.Fatalf("admin mcp-config status = %d body = %s", mcpConfig.Code, mcpConfig.Body.String())
	}

	// The administrator creates credentials owned by the administrator for any
	// knowledge base, and deletes any credential.
	adminCreated := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/api-keys", remote: "10.0.0.9:5000",
		headers: apiKeyOwnerWriteHeaders("token-admin"),
		body:    `{"name":"admin-key","namespaces":["bob-kb"]}`,
	})
	if adminCreated.Code != http.StatusCreated {
		t.Fatalf("admin create status = %d body = %s", adminCreated.Code, adminCreated.Body.String())
	}
	if owner := decodeBody(t, adminCreated)["owner_user_id"]; owner != fixture.admin.ID {
		t.Fatalf("admin create owner = %v, want %q", owner, fixture.admin.ID)
	}
	deleted := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/admin/api-keys/" + bobKey.ID, remote: "10.0.0.9:5000",
		headers: sessionHeaders("token-admin", ""),
	})
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("admin delete status = %d body = %s", deleted.Code, deleted.Body.String())
	}
	entries = apiKeyOwnerListEntries(t, performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/api-keys", remote: "10.0.0.9:5000",
		headers: sessionHeaders("token-admin", ""),
	}))
	if apiKeyOwnerHasKey(entries, bobKey.ID) {
		t.Fatalf("admin list still contains the deleted key: %v", entries)
	}
}

// TestAPIKeyLocalManagementBreakGlass covers session-less loopback management:
// it sees every credential, creates unowned credentials, and manages any key,
// while remote session-less requests stay forbidden.
func TestAPIKeyLocalManagementBreakGlass(t *testing.T) {
	fixture := newAPIKeyOwnerFixture(t)
	handler := fixture.server.Handler()
	aliceKey := fixture.createAPIKeyOwnerKey(t, "alice-config", []string{"alice-kb"}, fixture.alice.ID)
	bobKey := fixture.createAPIKeyOwnerKey(t, "bob-config", []string{"bob-kb"}, fixture.bob.ID)

	listed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/api-keys", remote: "127.0.0.1:54321",
	})
	entries := apiKeyOwnerListEntries(t, listed)
	if len(entries) != 2 || !apiKeyOwnerHasKey(entries, aliceKey.ID) || !apiKeyOwnerHasKey(entries, bobKey.ID) {
		t.Fatalf("local list = %v, want every credential", entries)
	}

	// The local actor issues unowned (administrator-only) credentials and may
	// bind any namespace.
	created := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/api-keys", remote: "127.0.0.1:54321",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"name":"local-key","namespaces":["bob-kb"]}`,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("local create status = %d body = %s", created.Code, created.Body.String())
	}
	createdBody := decodeBody(t, created)
	if !strings.HasPrefix(createdBody["key"].(string), "prk_") {
		t.Fatalf("local create key = %v", createdBody["key"])
	}
	if owner, ok := createdBody["owner_user_id"]; ok && owner != "" {
		t.Fatalf("local create owner_user_id = %v, want empty", owner)
	}
	createdID, _ := createdBody["id"].(string)
	stored, err := fixture.stores.APIKeys.Get(context.Background(), createdID)
	if err != nil || stored == nil {
		t.Fatalf("stored local key = %+v, %v", stored, err)
	}
	if stored.OwnerUserID != "" || stored.OwnerUsername != "" {
		t.Fatalf("local key owner = %q/%q, want empty", stored.OwnerUserID, stored.OwnerUsername)
	}

	// The local actor updates and deletes any credential.
	updated := performRequest(handler, testRequest{
		method: "PUT", target: "/v1/admin/api-keys/" + aliceKey.ID, remote: "127.0.0.1:54321",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"name":"local-renamed","namespaces":["alice-kb","bob-kb"],"enabled":true}`,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("local update status = %d body = %s", updated.Code, updated.Body.String())
	}
	if updatedBody := decodeBody(t, updated); updatedBody["owner_user_id"] != fixture.alice.ID {
		t.Fatalf("local update dropped owner: %v", updatedBody)
	}
	deleted := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/admin/api-keys/" + bobKey.ID, remote: "127.0.0.1:54321",
	})
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("local delete status = %d body = %s", deleted.Code, deleted.Body.String())
	}

	// Remote requests without a session never reach the management surface.
	remoteRequests := []testRequest{
		{method: "GET", target: "/v1/admin/api-keys", remote: "10.0.0.5:1234"},
		{
			method: "POST", target: "/v1/admin/api-keys", remote: "10.0.0.5:1234",
			headers: map[string]string{"Content-Type": "application/json"},
			body:    `{"name":"remote","namespaces":["alice-kb"]}`,
		},
		{
			method: "PUT", target: "/v1/admin/api-keys/" + aliceKey.ID, remote: "10.0.0.5:1234",
			headers: map[string]string{"Content-Type": "application/json"},
			body:    `{"name":"remote","namespaces":["alice-kb"],"enabled":true}`,
		},
		{method: "DELETE", target: "/v1/admin/api-keys/" + aliceKey.ID, remote: "10.0.0.5:1234"},
		{
			method: "GET", target: "/v1/admin/api-keys/" + aliceKey.ID + "/mcp-config",
			remote: "10.0.0.5:1234", headers: map[string]string{"X-RAG-Namespace": "alice-kb"},
		},
	}
	for _, request := range remoteRequests {
		response := performRequest(handler, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s %s from a remote address status = %d body = %s",
				request.method, request.target, response.Code, response.Body.String())
		}
	}

	// Proxy headers and a non-loopback Host break the trusted-local trust too.
	proxied := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/api-keys", remote: "127.0.0.1:54321",
		headers: map[string]string{"X-Forwarded-For": "1.2.3.4"},
	})
	if proxied.Code != http.StatusForbidden {
		t.Fatalf("proxied local request status = %d body = %s", proxied.Code, proxied.Body.String())
	}
	foreignHost := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/api-keys", remote: "127.0.0.1:54321", host: "example.com",
	})
	if foreignHost.Code != http.StatusForbidden {
		t.Fatalf("foreign-host local request status = %d body = %s", foreignHost.Code, foreignHost.Body.String())
	}
}

// TestAPIKeyNamespacesOwnedList covers the /v1/namespaces contract: the existing
// namespaces list is unchanged while owned_namespaces reports the caller-owned
// knowledge bases (all of them for the session-less local actor).
func TestAPIKeyNamespacesOwnedList(t *testing.T) {
	fixture := newAPIKeyOwnerFixture(t)
	handler := fixture.server.Handler()

	session := performRequest(handler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
		headers: sessionHeaders("token-alice", "alice-kb"),
	})
	if session.Code != http.StatusOK {
		t.Fatalf("session namespaces status = %d body = %s", session.Code, session.Body.String())
	}
	sessionBody := decodeBody(t, session)
	namespaces := apiKeyOwnerStrings(t, sessionBody["namespaces"])
	owned := apiKeyOwnerStrings(t, sessionBody["owned_namespaces"])
	assertAPIKeyOwnerStrings(t, owned, []string{"alice-kb"}, "alice owned_namespaces")
	if !apiKeyOwnerContains(namespaces, "alice-kb") || !apiKeyOwnerContains(namespaces, "shared-kb") {
		t.Fatalf("alice namespaces = %v, want her owned and shared knowledge bases", namespaces)
	}
	if apiKeyOwnerContains(namespaces, "bob-kb") {
		t.Fatalf("alice namespaces leaked bob-kb: %v", namespaces)
	}

	admin := performRequest(handler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
		headers: sessionHeaders("token-admin", "alice-kb"),
	})
	if admin.Code != http.StatusOK {
		t.Fatalf("admin namespaces status = %d body = %s", admin.Code, admin.Body.String())
	}
	adminNamespaces := apiKeyOwnerStrings(t, decodeBody(t, admin)["namespaces"])
	assertAPIKeyOwnerStrings(t, adminNamespaces,
		[]string{"alice-kb", "bob-kb", "shared-kb"}, "admin namespaces")

	local := performRequest(handler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "127.0.0.1:54321",
		headers: map[string]string{"X-RAG-Namespace": "alice-kb"},
	})
	if local.Code != http.StatusOK {
		t.Fatalf("local namespaces status = %d body = %s", local.Code, local.Body.String())
	}
	localBody := decodeBody(t, local)
	localNamespaces := apiKeyOwnerStrings(t, localBody["namespaces"])
	localOwned := apiKeyOwnerStrings(t, localBody["owned_namespaces"])
	all := []string{"alice-kb", "bob-kb", "shared-kb"}
	assertAPIKeyOwnerStrings(t, localNamespaces, all, "local namespaces")
	assertAPIKeyOwnerStrings(t, localOwned, all, "local owned_namespaces")
}
