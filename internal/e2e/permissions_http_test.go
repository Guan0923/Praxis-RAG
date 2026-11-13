package e2e

// End-to-end coverage of Console accounts, knowledge-base ownership and the
// invite-based sharing rules. The test drives the real HTTP surface (login
// sessions, CSRF, roles), the real access service and the real MySQL state on
// top of the fake model/Qdrant servers, so a regression in any of those layers
// shows up as an HTTP contract failure instead of a unit-level mismatch.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/access"
	"github.com/Guan0923/xinghe_rag/internal/auth"
	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/httpapi"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/wire"
)

const (
	permissionsAdminPassword = "AdminPass123"
	permissionsVIPPassword   = "VipPass123"
	permissionsPlainPassword = "PlainPass123"
	sessionCookieName        = "rag_session"
	csrfHeaderName           = "X-CSRF-Token"
)

// permissionsHandler builds the real HTTP surface with Console accounts and the
// sharing service wired in. Account roles, ownership, invitations and the
// effective scopes all come from the real services; only the model/Qdrant
// servers are fakes from the harness.
func permissionsHandler(t *testing.T, h *harness) http.Handler {
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
	server := httpapi.New(settings, nil, nil, testLogger(),
		httpapi.WithRuntime(wire.HTTPRuntime{Service: h.Service}),
		httpapi.WithKnowledgeBaseAdminRuntime(wire.HTTPRuntime{Service: h.Service}),
		httpapi.WithKnowledgeBaseSelfCreateRuntime(wire.HTTPRuntime{Service: h.Service}),
		httpapi.WithAccessControl(sharing),
		httpapi.WithUserAuth(users),
	)
	return server.Handler()
}

// consoleClient is one logged-in Console account talking to the test handler.
type consoleClient struct {
	handler http.Handler
	t       *testing.T
	cookie  string
	csrf    string
	id      string
}

func newConsoleClient(t *testing.T, handler http.Handler) *consoleClient {
	t.Helper()
	return &consoleClient{handler: handler, t: t}
}

// do performs one request with the account's cookie and CSRF token.
func (c *consoleClient) do(method, target, namespace string, body []byte, contentType string) *httptest.ResponseRecorder {
	c.t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:5000"
	if c.cookie != "" {
		request.Header.Set("Cookie", c.cookie)
	}
	if c.csrf != "" {
		request.Header.Set(csrfHeaderName, c.csrf)
	}
	if namespace != "" {
		request.Header.Set("X-RAG-Namespace", namespace)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	c.handler.ServeHTTP(recorder, request)
	return recorder
}

// json performs one JSON request and decodes the JSON object response.
func (c *consoleClient) json(method, target, namespace string, payload any) (*httptest.ResponseRecorder, map[string]any) {
	c.t.Helper()
	var body []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			c.t.Fatalf("marshal %s %s: %v", method, target, err)
		}
		body = encoded
	}
	recorder := c.do(method, target, namespace, body, "application/json")
	decoded := map[string]any{}
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			c.t.Fatalf("decode %s %s (%d): %v body=%s", method, target, recorder.Code, err, recorder.Body.String())
		}
	}
	return recorder, decoded
}

// register registers the account and adopts its cookie, CSRF token and id.
func (c *consoleClient) register(username, password string) map[string]any {
	c.t.Helper()
	recorder, payload := c.json(http.MethodPost, "/v1/auth/register", "",
		map[string]any{"username": username, "password": password})
	if recorder.Code != http.StatusCreated {
		c.t.Fatalf("register %s status = %d body = %s", username, recorder.Code, recorder.Body.String())
	}
	user, _ := payload["user"].(map[string]any)
	c.id, _ = user["id"].(string)
	c.csrf, _ = payload["csrf_token"].(string)
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			c.cookie = cookie.Name + "=" + cookie.Value
		}
	}
	if c.id == "" || c.csrf == "" || c.cookie == "" {
		c.t.Fatalf("register %s returned an incomplete session: %v", username, payload)
	}
	return user
}

func requireStatus(t *testing.T, recorder *httptest.ResponseRecorder, want int, description string) {
	t.Helper()
	if recorder.Code != want {
		t.Fatalf("%s status = %d, want %d (body = %s)", description, recorder.Code, want, recorder.Body.String())
	}
}

func detailOf(payload map[string]any) string {
	detail, _ := payload["detail"].(string)
	return detail
}

// createLegacyKnowledgeBase creates one knowledge base through the local
// administrator route (owner stays NULL, like pre-sharing data).
func createLegacyKnowledgeBase(t *testing.T, admin *consoleClient, handler http.Handler, namespace, modelURL string) {
	t.Helper()
	client := &consoleClient{handler: handler, t: t, cookie: admin.cookie, csrf: admin.csrf}
	recorder, _ := client.json(http.MethodPost, "/v1/admin/knowledge-bases", "", map[string]any{
		"namespace":       namespace,
		"embedding":       map[string]any{"base_url": modelURL, "apikey": ""},
		"embedding_model": "embed-a",
		"reranker":        map[string]any{"base_url": modelURL, "apikey": ""},
		"reranker_model":  "rank-a",
		"strategy":        "character",
		"chunk_size":      120,
		"chunk_overlap":   20,
	})
	requireStatus(t, recorder, http.StatusCreated, "create knowledge base "+namespace)
}

// TestPermissionsInvitationFlowEndToEnd walks the complete sharing workflow:
// role management, ownership visibility, a read invitation accepted through
// its link, document read/write gates, a write grant, self-creation from the
// template and revocation.
func TestPermissionsInvitationFlowEndToEnd(t *testing.T) {
	h := newHarness(t, "")
	handler := permissionsHandler(t, h)

	admin := newConsoleClient(t, handler)
	if role := admin.register("perm-admin", permissionsAdminPassword)["role"]; role != state.RoleAdmin {
		t.Fatalf("first account role = %v, want %s", role, state.RoleAdmin)
	}
	vip := newConsoleClient(t, handler)
	if role := vip.register("perm-vip", permissionsVIPPassword)["role"]; role != state.RoleUser {
		t.Fatalf("second account role = %v, want %s", role, state.RoleUser)
	}
	plain := newConsoleClient(t, handler)
	if role := plain.register("perm-user", permissionsPlainPassword)["role"]; role != state.RoleUser {
		t.Fatalf("third account role = %v, want %s", role, state.RoleUser)
	}

	// Only administrators manage roles; VIPs and administrators may pick
	// invitation recipients.
	recorder, payload := plain.json(http.MethodGet, "/v1/users", "", nil)
	requireStatus(t, recorder, http.StatusForbidden, "normal user picks invitees")
	if detail := detailOf(payload); detail != access.MessageForbidden {
		t.Fatalf("normal user picker detail = %q", detail)
	}
	recorder, payload = admin.json(http.MethodPatch, "/v1/admin/users/"+vip.id, "",
		map[string]any{"role": state.RoleVIP, "enabled": true})
	requireStatus(t, recorder, http.StatusOK, "promote vip")
	if role := payload["user"].(map[string]any)["role"]; role != state.RoleVIP {
		t.Fatalf("promoted role = %v", role)
	}
	recorder, payload = vip.json(http.MethodGet, "/v1/users", "", nil)
	requireStatus(t, recorder, http.StatusOK, "vip picks invitees")
	if users, _ := payload["users"].([]any); len(users) != 2 {
		t.Fatalf("invitable users = %v, want 2", users)
	}

	// The administrator creates the default template (legacy route, NULL owner)
	// and one owned knowledge base through the tenant self-create route.
	createLegacyKnowledgeBase(t, admin, handler, "default", h.ModelURL)
	recorder, _ = admin.json(http.MethodPost, "/v1/knowledge-bases", "", map[string]any{
		"namespace":       "team",
		"embedding":       map[string]any{"base_url": h.ModelURL, "apikey": ""},
		"embedding_model": "embed-a",
		"reranker":        map[string]any{"base_url": h.ModelURL, "apikey": ""},
		"reranker_model":  "rank-a",
		"strategy":        "character",
		"chunk_size":      120,
		"chunk_overlap":   20,
	})
	requireStatus(t, recorder, http.StatusCreated, "admin creates owned knowledge base")

	recorder, payload = admin.json(http.MethodGet, "/v1/knowledge-bases/team/access", "", nil)
	requireStatus(t, recorder, http.StatusOK, "owner access view")
	if payload["owner"] != true || payload["can_manage"] != true || payload["can_share"] != true {
		t.Fatalf("owner access view = %v", payload)
	}

	// Ownership isolates the knowledge base before any invitation.
	recorder, payload = vip.json(http.MethodGet, "/v1/namespaces", "alpha", nil)
	requireStatus(t, recorder, http.StatusOK, "vip namespace list before invite")
	if containsString(payload["namespaces"], "team") {
		t.Fatalf("vip sees an unshared knowledge base: %v", payload)
	}
	// An account with no owned or granted knowledge base gets empty lists.
	recorder, payload = plain.json(http.MethodGet, "/v1/namespaces", "alpha", nil)
	requireStatus(t, recorder, http.StatusOK, "unaffiliated namespace list")
	if namespaces, _ := payload["namespaces"].([]any); len(namespaces) != 0 {
		t.Fatalf("unaffiliated account sees namespaces: %v", payload)
	}
	if owned, _ := payload["owned_namespaces"].([]any); len(owned) != 0 {
		t.Fatalf("unaffiliated account owns namespaces: %v", payload)
	}
	recorder, payload = vip.json(http.MethodGet, "/v1/knowledge-bases/team/access", "", nil)
	requireStatus(t, recorder, http.StatusForbidden, "vip access before invite")
	if detail := detailOf(payload); detail != access.MessageKnowledgeAccess {
		t.Fatalf("vip access detail = %q", detail)
	}

	// 1. The owner issues a whole-knowledge-base read invitation.
	recorder, payload = admin.json(http.MethodPost, "/v1/knowledge-bases/team/invitations", "", map[string]any{
		"username": "perm-vip", "permission": "read", "scope": "all",
	})
	requireStatus(t, recorder, http.StatusCreated, "create read invitation")
	invitation, _ := payload["invitation"].(map[string]any)
	if invitation["status"] != "pending" {
		t.Fatalf("new invitation = %v", invitation)
	}
	readToken := tokenFromInvitePath(t, payload["invite_path"])

	recorder, payload = vip.json(http.MethodGet, "/v1/invitations", "", nil)
	requireStatus(t, recorder, http.StatusOK, "invitee inbox")
	inbox, _ := payload["invitations"].([]any)
	if len(inbox) != 1 || inbox[0].(map[string]any)["status"] != "pending" {
		t.Fatalf("invitee inbox = %v", payload)
	}

	// Only the addressed account may preview or accept the link.
	recorder, payload = plain.json(http.MethodPost, "/v1/invitations/preview", "", map[string]any{"token": readToken})
	requireStatus(t, recorder, http.StatusForbidden, "other account previews invitation")
	if detail := detailOf(payload); detail != access.MessageInvitationOwner {
		t.Fatalf("foreign preview detail = %q", detail)
	}
	recorder, payload = vip.json(http.MethodPost, "/v1/invitations/preview", "", map[string]any{"token": readToken})
	requireStatus(t, recorder, http.StatusOK, "invitee previews invitation")
	preview, _ := payload["invitation"].(map[string]any)
	if preview["namespace"] != "team" || preview["inviter_username"] != "perm-admin" {
		t.Fatalf("invitation preview = %v", preview)
	}
	recorder, payload = vip.json(http.MethodPost, "/v1/invitations/accept", "", map[string]any{"token": readToken})
	requireStatus(t, recorder, http.StatusOK, "invitee accepts invitation")
	if accepted, _ := payload["invitation"].(map[string]any); accepted["status"] != "accepted" {
		t.Fatalf("accepted invitation = %v", payload)
	}

	// The grant is effective and read-only.
	recorder, payload = vip.json(http.MethodGet, "/v1/knowledge-bases/team/access", "", nil)
	requireStatus(t, recorder, http.StatusOK, "grantee access view")
	scope, _ := payload["scope"].(map[string]any)
	if payload["can_read"] != true || payload["can_write"] != false || payload["can_manage"] != false ||
		scope["whole_read"] != true || scope["whole_write"] != false {
		t.Fatalf("read grant access view = %v", payload)
	}
	recorder, payload = vip.json(http.MethodGet, "/v1/namespaces", "alpha", nil)
	requireStatus(t, recorder, http.StatusOK, "grantee namespace list")
	if !containsString(payload["namespaces"], "team") {
		t.Fatalf("shared knowledge base missing from grantee namespaces: %v", payload)
	}
	recorder, _ = vip.json(http.MethodGet, "/v1/documents", "team", nil)
	requireStatus(t, recorder, http.StatusOK, "grantee lists documents")
	recorder, payload = vip.json(http.MethodDelete, "/v1/documents/missing.md", "team", nil)
	requireStatus(t, recorder, http.StatusForbidden, "read-only grantee deletes")
	if detail := detailOf(payload); detail != access.MessageWriteRequired {
		t.Fatalf("read-only delete detail = %q", detail)
	}
	uploadBody, uploadType := restMultipart(t, []restUploadFile{{name: "readonly.md", content: documentText}})
	recorder = vip.do(http.MethodPost, "/v1/document-batches", "team", uploadBody, uploadType)
	requireStatus(t, recorder, http.StatusForbidden, "read-only grantee uploads")
	if detail := decodeRESTBody(t, recorder)["detail"]; detail != access.MessageWriteRequired {
		t.Fatalf("read-only upload detail = %v", detail)
	}

	// A grantee cannot share further, and a normal user cannot invite at all.
	recorder, payload = vip.json(http.MethodPost, "/v1/knowledge-bases/team/invitations", "", map[string]any{
		"username": "perm-user", "permission": "read", "scope": "all",
	})
	requireStatus(t, recorder, http.StatusForbidden, "grantee invites others")
	if detail := detailOf(payload); detail != access.MessageNotOwner {
		t.Fatalf("grantee invite detail = %q", detail)
	}
	recorder, payload = plain.json(http.MethodPost, "/v1/knowledge-bases/team/invitations", "", map[string]any{
		"username": "perm-admin", "permission": "read", "scope": "all",
	})
	requireStatus(t, recorder, http.StatusForbidden, "normal user invites")
	if detail := detailOf(payload); detail != access.MessageForbidden {
		t.Fatalf("normal user invite detail = %q", detail)
	}

	// 2. A second invitation upgrades the grant to whole-knowledge-base write;
	// the grantee can then upload and the document becomes visible.
	recorder, payload = admin.json(http.MethodPost, "/v1/knowledge-bases/team/invitations", "", map[string]any{
		"username": "perm-vip", "permission": "write", "scope": "all",
	})
	requireStatus(t, recorder, http.StatusCreated, "create write invitation")
	writeToken := tokenFromInvitePath(t, payload["invite_path"])
	recorder, _ = vip.json(http.MethodPost, "/v1/invitations/accept", "", map[string]any{"token": writeToken})
	requireStatus(t, recorder, http.StatusOK, "accept write invitation")
	recorder, payload = vip.json(http.MethodGet, "/v1/knowledge-bases/team/access", "", nil)
	requireStatus(t, recorder, http.StatusOK, "writer access view")
	if payload["can_write"] != true {
		t.Fatalf("write grant access view = %v", payload)
	}
	documentBody, documentType := restMultipart(t, []restUploadFile{{name: "shared.md", content: documentText}})
	recorder = vip.do(http.MethodPost, "/v1/document-batches", "team", documentBody, documentType)
	requireStatus(t, recorder, http.StatusAccepted, "writer uploads document")
	artifactID := firstRestJobArtifactID(t, decodeRESTBody(t, recorder))
	runDocumentWorkerUntil(t, h, "shared document indexed", func() bool {
		job, err := h.Stores.DocumentJobs.Get(context.Background(), artifactID, nil)
		return err == nil && job != nil && job.Status == state.DocumentJobSucceeded
	})
	recorder, payload = vip.json(http.MethodGet, "/v1/documents", "team", nil)
	requireStatus(t, recorder, http.StatusOK, "writer lists documents")
	if !containsDocument(payload["documents"], "shared.md") {
		t.Fatalf("uploaded document missing from list: %v", payload)
	}
	recorder, payload = admin.json(http.MethodGet, "/v1/documents", "team", nil)
	requireStatus(t, recorder, http.StatusOK, "owner lists documents")
	if !containsDocument(payload["documents"], "shared.md") {
		t.Fatalf("owner does not see the uploaded document: %v", payload)
	}
	recorder, payload = admin.json(http.MethodGet, "/v1/knowledge-bases/team/permissions", "", nil)
	requireStatus(t, recorder, http.StatusOK, "owner lists grants")
	if grants, _ := payload["permissions"].([]any); len(grants) == 0 {
		t.Fatalf("grant list = %v", payload)
	}

	// 3. VIP self-creation clones the default template and records ownership.
	recorder, _ = vip.json(http.MethodPost, "/v1/knowledge-bases", "", map[string]any{
		"namespace": "vip-kb", "strategy": "character", "chunk_size": 120, "chunk_overlap": 20,
	})
	requireStatus(t, recorder, http.StatusCreated, "vip self-creates knowledge base")
	recorder, payload = vip.json(http.MethodGet, "/v1/knowledge-bases/vip-kb/access", "", nil)
	requireStatus(t, recorder, http.StatusOK, "self-created access view")
	if payload["owner"] != true || payload["can_manage"] != true || payload["can_share"] != true {
		t.Fatalf("self-created access view = %v", payload)
	}
	recorder, payload = vip.json(http.MethodGet, "/v1/namespaces", "alpha", nil)
	requireStatus(t, recorder, http.StatusOK, "vip namespace list after self-create")
	if !containsString(payload["namespaces"], "vip-kb") {
		t.Fatalf("self-created knowledge base missing from namespaces: %v", payload)
	}

	// 4. Revocation removes every grant and the knowledge base becomes private
	// again, while the owner keeps access.
	recorder, _ = admin.json(http.MethodDelete, "/v1/knowledge-bases/team/permissions/"+vip.id, "", nil)
	requireStatus(t, recorder, http.StatusNoContent, "revoke grant")
	recorder, payload = vip.json(http.MethodGet, "/v1/knowledge-bases/team/access", "", nil)
	requireStatus(t, recorder, http.StatusForbidden, "access after revoke")
	if detail := detailOf(payload); detail != access.MessageKnowledgeAccess {
		t.Fatalf("revoked access detail = %q", detail)
	}
	recorder, payload = vip.json(http.MethodGet, "/v1/namespaces", "alpha", nil)
	requireStatus(t, recorder, http.StatusOK, "namespace list after revoke")
	if containsString(payload["namespaces"], "team") {
		t.Fatalf("revoked knowledge base still listed: %v", payload)
	}
	recorder, _ = admin.json(http.MethodGet, "/v1/knowledge-bases/team/access", "", nil)
	requireStatus(t, recorder, http.StatusOK, "owner keeps access after revoke")
}

// tokenFromInvitePath extracts the raw invitation token from the invite path.
func tokenFromInvitePath(t *testing.T, raw any) string {
	t.Helper()
	path, _ := raw.(string)
	index := strings.Index(path, "token=")
	if index < 0 {
		t.Fatalf("invite_path %q has no token", path)
	}
	token := strings.TrimSpace(path[index+len("token="):])
	if token == "" {
		t.Fatalf("invite_path %q has an empty token", path)
	}
	return token
}

func containsString(raw any, want string) bool {
	values, _ := raw.([]any)
	for _, value := range values {
		if text, _ := value.(string); text == want {
			return true
		}
	}
	return false
}

func containsDocument(raw any, name string) bool {
	documents, _ := raw.([]any)
	for _, value := range documents {
		document, _ := value.(map[string]any)
		if document["name"] == name {
			return true
		}
	}
	return false
}
