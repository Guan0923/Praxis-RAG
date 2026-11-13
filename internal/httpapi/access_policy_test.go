package httpapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/access"
	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/auth"
	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// fakeAccessControl is an in-memory AccessControl for handler tests.
type fakeAccessControl struct {
	scopes   map[string]access.ScopeSet
	scopeErr error

	scopeUser state.User
	scopeNS   string

	visible    map[string][]string
	visibleErr error

	owned    map[string][]string
	ownedErr error

	resolvedNames []string
	resolvedIDs   []string
	resolveErr    error

	describe    access.AccessView
	describeErr error

	grants    []access.GrantView
	grantsErr error

	revokedGrantee string
	revokeGrantErr error

	namespaceInvitations []access.InvitationView
	namespaceInvErr      error

	createVal   access.InvitationView
	createToken string
	createErr   error
	lastCreate  access.CreateInvitationRequest

	inbox    []access.InvitationView
	inboxErr error

	previewVal access.InvitationView
	previewErr error

	acceptVal access.InvitationView
	acceptErr error

	declineVal access.InvitationView
	declineErr error

	revokedInvitationID string
	revokeInvitationErr error

	invitable    []access.UserView
	invitableErr error

	scopeCalls int
}

func (f *fakeAccessControl) ScopeSet(_ context.Context, user state.User, namespace string) (access.ScopeSet, error) {
	f.scopeCalls++
	f.scopeUser = user
	f.scopeNS = namespace
	if f.scopeErr != nil {
		return access.NewScopeSet(), f.scopeErr
	}
	scope, ok := f.scopes[user.ID+"|"+namespace]
	if !ok {
		return access.NewScopeSet(), nil
	}
	return scope, nil
}

func (f *fakeAccessControl) CanManage(context.Context, state.User, string) (bool, error) {
	return false, nil
}

func (f *fakeAccessControl) VisibleNamespaces(_ context.Context, user state.User) ([]string, error) {
	if f.visibleErr != nil {
		return nil, f.visibleErr
	}
	return f.visible[user.ID], nil
}

func (f *fakeAccessControl) OwnedNamespaces(_ context.Context, user state.User) ([]string, error) {
	if f.ownedErr != nil {
		return nil, f.ownedErr
	}
	return f.owned[user.ID], nil
}

func (f *fakeAccessControl) ResolveDocumentIDs(_ context.Context, _ string, names []string) ([]string, error) {
	f.resolvedNames = append([]string(nil), names...)
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	return f.resolvedIDs, nil
}

func (f *fakeAccessControl) Describe(context.Context, state.User, string) (access.AccessView, error) {
	return f.describe, f.describeErr
}

func (f *fakeAccessControl) ListGrants(context.Context, state.User, string) ([]access.GrantView, error) {
	return f.grants, f.grantsErr
}

func (f *fakeAccessControl) RevokeGrant(_ context.Context, _ state.User, _, granteeUserID string) error {
	f.revokedGrantee = granteeUserID
	return f.revokeGrantErr
}

func (f *fakeAccessControl) ListNamespaceInvitations(context.Context, state.User, string) ([]access.InvitationView, error) {
	return f.namespaceInvitations, f.namespaceInvErr
}

func (f *fakeAccessControl) CreateInvitation(
	_ context.Context, _ state.User, request access.CreateInvitationRequest,
) (access.InvitationView, string, error) {
	f.lastCreate = request
	return f.createVal, f.createToken, f.createErr
}

func (f *fakeAccessControl) Inbox(context.Context, state.User) ([]access.InvitationView, error) {
	return f.inbox, f.inboxErr
}

func (f *fakeAccessControl) PreviewInvitation(context.Context, state.User, string) (access.InvitationView, error) {
	return f.previewVal, f.previewErr
}

func (f *fakeAccessControl) AcceptInvitation(context.Context, state.User, string) (access.InvitationView, error) {
	return f.acceptVal, f.acceptErr
}

func (f *fakeAccessControl) DeclineInvitation(context.Context, state.User, string) (access.InvitationView, error) {
	return f.declineVal, f.declineErr
}

func (f *fakeAccessControl) RevokeInvitation(_ context.Context, _ state.User, invitationID string) error {
	f.revokedInvitationID = invitationID
	return f.revokeInvitationErr
}

func (f *fakeAccessControl) ListInvitableUsers(context.Context, state.User) ([]access.UserView, error) {
	return f.invitable, f.invitableErr
}

// fakeSelfCreateRuntime captures POST /v1/knowledge-bases calls.
type fakeSelfCreateRuntime struct {
	ownerUserID string
	isAdmin     bool
	input       KnowledgeBaseCreateInput
	payload     map[string]any
	err         error
}

func (f *fakeSelfCreateRuntime) CreateKnowledgeBaseForUser(
	_ context.Context, ownerUserID string, isAdmin bool, input KnowledgeBaseCreateInput,
) (map[string]any, error) {
	f.ownerUserID = ownerUserID
	f.isAdmin = isAdmin
	f.input = input
	return f.payload, f.err
}

func authTestSettings() config.ServiceSettings {
	settings := testSettings("")
	settings.AuthEnabled = true
	settings.SessionTTL = time.Hour
	return settings
}

func newAccessTestServer(t *testing.T, users UserAuth, control AccessControl, runtime Runtime, options ...Option) *Server {
	t.Helper()
	all := []Option{WithUserAuth(users)}
	if control != nil {
		all = append(all, WithAccessControl(control))
	}
	if runtime != nil {
		all = append(all, WithRuntime(runtime))
	}
	all = append(all, options...)
	return New(authTestSettings(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), all...)
}

func vipSession(token string) *auth.SessionUser {
	return &auth.SessionUser{
		User: state.User{
			ID: "u-vip", Username: "carol", Role: state.RoleVIP, Enabled: true,
			CreatedAt: "2026-11-09T10:00:00.000000+00:00",
		},
		CSRF: "csrf-" + token,
	}
}

// sessionHeaders authenticates one test request with a login session.
func sessionHeaders(token, namespace string) map[string]string {
	headers := map[string]string{
		"Cookie":       sessionCookieName + "=" + token,
		csrfHeaderName: "csrf-" + token,
	}
	if namespace != "" {
		headers["X-RAG-Namespace"] = namespace
	}
	return headers
}

func fullTestScope() access.ScopeSet {
	scope := access.NewScopeSet()
	scope.WholeRead = true
	scope.WholeWrite = true
	return scope
}

func readFilesScope(names ...string) access.ScopeSet {
	scope := access.NewScopeSet()
	for _, name := range names {
		scope.ReadDocuments[name] = struct{}{}
	}
	return scope
}

func writeFilesScope(names ...string) access.ScopeSet {
	scope := access.NewScopeSet()
	for _, name := range names {
		scope.WriteDocuments[name] = struct{}{}
	}
	return scope
}

func TestSharingRoutesRequireSessionWithoutLoopbackFallback(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		"vip-token":  vipSession("vip-token"),
		"user-token": userSession("user-token"),
	}}
	control := &fakeAccessControl{acceptVal: access.InvitationView{ID: "inv-1", Namespace: "kb1"}}
	handler := newAccessTestServer(t, users, control, nil).Handler()

	anonymous := performRequest(handler, testRequest{
		method: "GET", target: "/v1/invitations", remote: "127.0.0.1:5000",
	})
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d body = %s", anonymous.Code, anonymous.Body.String())
	}
	if got := anonymous.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("anonymous Cache-Control = %q", got)
	}

	// Sessions never fall back to loopback trust: the sharing endpoints always
	// act for one account.
	loopbackCreate := performRequest(handler, testRequest{
		method: "POST", target: "/v1/knowledge-bases", remote: "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"namespace":"kb1"}`,
	})
	if loopbackCreate.Code != http.StatusUnauthorized {
		t.Fatalf("loopback anonymous status = %d body = %s", loopbackCreate.Code, loopbackCreate.Body.String())
	}

	inbox := performRequest(handler, testRequest{
		method: "GET", target: "/v1/invitations", remote: "10.0.0.9:5000",
		headers: sessionHeaders("vip-token", ""),
	})
	if inbox.Code != http.StatusOK {
		t.Fatalf("inbox status = %d body = %s", inbox.Code, inbox.Body.String())
	}
	if body := decodeBody(t, inbox); body["invitations"] == nil {
		t.Fatalf("inbox body = %v", body)
	}

	// Cookie sessions must echo the CSRF token on writes.
	missingCSRF := performRequest(handler, testRequest{
		method: "POST", target: "/v1/invitations/accept", remote: "10.0.0.9:5000",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Cookie":       sessionCookieName + "=vip-token",
		},
		body: `{"token":"share-token"}`,
	})
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d body = %s", missingCSRF.Code, missingCSRF.Body.String())
	}

	accepted := performRequest(handler, testRequest{
		method: "POST", target: "/v1/invitations/accept", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("vip-token", ""), map[string]string{"Content-Type": "application/json"}),
		body:    `{"token":"share-token"}`,
	})
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept status = %d body = %s", accepted.Code, accepted.Body.String())
	}
	if body := decodeBody(t, accepted); body["namespace"] != "kb1" {
		t.Fatalf("accept body = %v", body)
	}
}

func TestListInvitableUsersRequiresShareRole(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		"admin-token": adminSession("admin-token"),
		"vip-token":   vipSession("vip-token"),
		"user-token":  userSession("user-token"),
	}}
	control := &fakeAccessControl{
		invitable: []access.UserView{{ID: "u-vip", Username: "carol", Role: state.RoleVIP}},
	}
	handler := newAccessTestServer(t, users, control, nil).Handler()

	normal := performRequest(handler, testRequest{
		method: "GET", target: "/v1/users", remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", ""),
	})
	if normal.Code != http.StatusForbidden {
		t.Fatalf("normal user status = %d body = %s", normal.Code, normal.Body.String())
	}
	if detail := decodeBody(t, normal)["detail"]; detail != access.MessageForbidden {
		t.Fatalf("normal detail = %v", detail)
	}

	for _, token := range []string{"vip-token", "admin-token"} {
		response := performRequest(handler, testRequest{
			method: "GET", target: "/v1/users", remote: "10.0.0.9:5000",
			headers: sessionHeaders(token, ""),
		})
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d body = %s", token, response.Code, response.Body.String())
		}
		payload, _ := decodeBody(t, response)["users"].([]any)
		if len(payload) != 1 {
			t.Fatalf("%s users = %v", token, payload)
		}
		entry, _ := payload[0].(map[string]any)
		if entry["id"] != "u-vip" || entry["role"] != state.RoleVIP {
			t.Fatalf("%s user entry = %v", token, entry)
		}
		if _, present := entry["created_at"]; present {
			t.Fatalf("%s leaked timestamps: %v", token, entry)
		}
	}
}

func TestKnowledgeBaseAccessEndpoint(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token")}}
	control := &fakeAccessControl{describe: access.AccessView{
		Namespace: "kb1", Owner: false, CanManage: false, CanShare: false, Role: state.RoleUser,
		CanRead: true, CanWrite: true,
		Scope: access.ScopeView{ReadDocuments: []string{"a.md"}, WriteDocuments: []string{"a.md"}},
	}}
	handler := newAccessTestServer(t, users, control, nil).Handler()

	response := performRequest(handler, testRequest{
		method: "GET", target: "/v1/knowledge-bases/kb1/access", remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", ""),
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	body := decodeBody(t, response)
	if body["namespace"] != "kb1" || body["can_manage"] != false || body["can_read"] != true {
		t.Fatalf("body = %v", body)
	}
	scope, _ := body["scope"].(map[string]any)
	documents, _ := scope["read_documents"].([]any)
	if len(documents) != 1 || documents[0] != "a.md" {
		t.Fatalf("scope = %v", scope)
	}

	// An account with no grant at all gets the frozen 403 detail.
	control.describe = access.AccessView{Namespace: "kb1"}
	denied := performRequest(handler, testRequest{
		method: "GET", target: "/v1/knowledge-bases/kb1/access", remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", ""),
	})
	if denied.Code != http.StatusForbidden || decodeBody(t, denied)["detail"] != access.MessageKnowledgeAccess {
		t.Fatalf("denied status = %d body = %s", denied.Code, denied.Body.String())
	}
}

func TestPermissionsAndInvitationEndpoints(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"admin-token": adminSession("admin-token")}}
	control := &fakeAccessControl{
		grants: []access.GrantView{{
			UserID: "u-user", Username: "bob", DocumentName: "a.md", Permission: "read",
			GrantedBy: "u-admin", GrantedByUsername: "alice",
			CreatedAt: "2026-11-09T10:00:00.000000+00:00", UpdatedAt: "2026-11-09T10:00:00.000000+00:00",
		}},
		namespaceInvitations: []access.InvitationView{{ID: "inv-1", Namespace: "kb1", Status: "pending"}},
		createVal:            access.InvitationView{ID: "inv-2", Namespace: "kb1", Status: "pending"},
		createToken:          "raw-token",
		inbox:                []access.InvitationView{{ID: "inv-3", Namespace: "kb2", Status: "pending"}},
		previewVal:           access.InvitationView{ID: "inv-3", Namespace: "kb2"},
		acceptVal:            access.InvitationView{ID: "inv-3", Namespace: "kb2"},
		declineVal:           access.InvitationView{ID: "inv-3", Namespace: "kb2"},
	}
	handler := newAccessTestServer(t, users, control, nil).Handler()
	headers := sessionHeaders("admin-token", "")

	permissions := performRequest(handler, testRequest{
		method: "GET", target: "/v1/knowledge-bases/kb1/permissions", remote: "10.0.0.9:5000",
		headers: headers,
	})
	if permissions.Code != http.StatusOK {
		t.Fatalf("permissions status = %d body = %s", permissions.Code, permissions.Body.String())
	}
	items, _ := decodeBody(t, permissions)["permissions"].([]any)
	first, _ := items[0].(map[string]any)
	if first["user_id"] != "u-user" || first["document_name"] != "a.md" || first["granted_by_username"] != "alice" {
		t.Fatalf("permissions = %v", items)
	}

	revoked := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/knowledge-bases/kb1/permissions/u-user", remote: "10.0.0.9:5000",
		headers: headers,
	})
	if revoked.Code != http.StatusNoContent || control.revokedGrantee != "u-user" {
		t.Fatalf("revoke status = %d grantee = %q", revoked.Code, control.revokedGrantee)
	}

	created := performRequest(handler, testRequest{
		method: "POST", target: "/v1/knowledge-bases/kb1/invitations", remote: "10.0.0.9:5000",
		headers: mergeHeaders(headers, map[string]string{"Content-Type": "application/json"}),
		body:    `{"username":"bob","permission":"read","scope":"files","document_names":["a.md"]}`,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create invitation status = %d body = %s", created.Code, created.Body.String())
	}
	createdBody := decodeBody(t, created)
	if createdBody["invite_path"] != "/console#/invite?token=raw-token" {
		t.Fatalf("invite_path = %v", createdBody["invite_path"])
	}
	if invitation, _ := createdBody["invitation"].(map[string]any); invitation["id"] != "inv-2" {
		t.Fatalf("invitation = %v", createdBody["invitation"])
	}
	if control.lastCreate.Namespace != "kb1" || control.lastCreate.Username != "bob" ||
		control.lastCreate.Permission != "read" || control.lastCreate.Scope != "files" ||
		len(control.lastCreate.DocumentNames) != 1 || control.lastCreate.DocumentNames[0] != "a.md" {
		t.Fatalf("create request = %+v", control.lastCreate)
	}

	namespaceInvitations := performRequest(handler, testRequest{
		method: "GET", target: "/v1/knowledge-bases/kb1/invitations", remote: "10.0.0.9:5000",
		headers: headers,
	})
	if namespaceInvitations.Code != http.StatusOK {
		t.Fatalf("namespace invitations status = %d", namespaceInvitations.Code)
	}

	for _, route := range []struct {
		target string
		body   string
	}{
		{"/v1/invitations/preview", `{"token":"raw-token"}`},
		{"/v1/invitations/decline", `{"token":"raw-token"}`},
	} {
		response := performRequest(handler, testRequest{
			method: "POST", target: route.target, remote: "10.0.0.9:5000",
			headers: mergeHeaders(headers, map[string]string{"Content-Type": "application/json"}),
			body:    route.body,
		})
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d body = %s", route.target, response.Code, response.Body.String())
		}
		if _, present := decodeBody(t, response)["invitation"]; !present {
			t.Fatalf("%s body = %s", route.target, response.Body.String())
		}
	}

	accept := performRequest(handler, testRequest{
		method: "POST", target: "/v1/invitations/accept", remote: "10.0.0.9:5000",
		headers: mergeHeaders(headers, map[string]string{"Content-Type": "application/json"}),
		body:    `{"token":"raw-token"}`,
	})
	if accept.Code != http.StatusOK {
		t.Fatalf("accept status = %d body = %s", accept.Code, accept.Body.String())
	}
	if body := decodeBody(t, accept); body["namespace"] != "kb2" {
		t.Fatalf("accept body = %v", body)
	}

	revokeInvitation := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/invitations/inv-1", remote: "10.0.0.9:5000",
		headers: headers,
	})
	if revokeInvitation.Code != http.StatusNoContent || control.revokedInvitationID != "inv-1" {
		t.Fatalf("revoke invitation status = %d id = %q", revokeInvitation.Code, control.revokedInvitationID)
	}

	missingToken := performRequest(handler, testRequest{
		method: "POST", target: "/v1/invitations/preview", remote: "10.0.0.9:5000",
		headers: mergeHeaders(headers, map[string]string{"Content-Type": "application/json"}),
		body:    `{}`,
	})
	if missingToken.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing token status = %d body = %s", missingToken.Code, missingToken.Body.String())
	}
}

func TestAccessErrorMapping(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"admin-token": adminSession("admin-token")}}
	control := &fakeAccessControl{}
	handler := newAccessTestServer(t, users, control, nil).Handler()
	headers := sessionHeaders("admin-token", "")

	testCases := []struct {
		kind   access.ErrorKind
		status int
	}{
		{access.KindValidation, http.StatusUnprocessableEntity},
		{access.KindForbidden, http.StatusForbidden},
		{access.KindNotFound, http.StatusNotFound},
		{access.KindConflict, http.StatusConflict},
	}
	for _, testCase := range testCases {
		control.grantsErr = access.NewError(testCase.kind, "共享错误")
		response := performRequest(handler, testRequest{
			method: "GET", target: "/v1/knowledge-bases/kb1/permissions", remote: "10.0.0.9:5000",
			headers: headers,
		})
		if response.Code != testCase.status {
			t.Fatalf("kind %d status = %d body = %s", testCase.kind, response.Code, response.Body.String())
		}
		if detail := decodeBody(t, response)["detail"]; detail != "共享错误" {
			t.Fatalf("kind %d detail = %v", testCase.kind, detail)
		}
	}
}

func TestTenantReadRoutesHonourScope(t *testing.T) {
	runtime := newScopedDocRuntime()
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		"user-token":  userSession("user-token"),
		"admin-token": adminSession("admin-token"),
	}}
	control := &fakeAccessControl{
		scopes: map[string]access.ScopeSet{
			"u-user|alpha":  readFilesScope("a.md"),
			"u-admin|alpha": fullTestScope(),
		},
		resolvedIDs: []string{"doc-a"},
	}
	handler := newAccessTestServer(t, users, control, runtime).Handler()

	listed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/documents", remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", "alpha"),
	})
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", listed.Code, listed.Body.String())
	}
	documents, _ := decodeBody(t, listed)["documents"].([]any)
	if len(documents) != 1 {
		t.Fatalf("documents = %v, want only the granted file", documents)
	}
	if document, _ := documents[0].(map[string]any); document["name"] != "a.md" {
		t.Fatalf("document = %v", documents[0])
	}

	allowedContent := performRequest(handler, testRequest{
		method: "GET", target: "/v1/documents/a.md/content", remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", "alpha"),
	})
	if allowedContent.Code != http.StatusOK {
		t.Fatalf("allowed content status = %d body = %s", allowedContent.Code, allowedContent.Body.String())
	}

	deniedContent := performRequest(handler, testRequest{
		method: "GET", target: "/v1/documents/b.md/content", remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", "alpha"),
	})
	if deniedContent.Code != http.StatusForbidden ||
		decodeBody(t, deniedContent)["detail"] != access.MessageKnowledgeAccess {
		t.Fatalf("denied content status = %d body = %s", deniedContent.Code, deniedContent.Body.String())
	}

	deniedDelete := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/documents/a.md", remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", "alpha"),
	})
	if deniedDelete.Code != http.StatusForbidden ||
		decodeBody(t, deniedDelete)["detail"] != access.MessageWriteRequired {
		t.Fatalf("denied delete status = %d body = %s", deniedDelete.Code, deniedDelete.Body.String())
	}

	// POST /v1/search carries the resolved document ids to the runtime.
	search := performRequest(handler, testRequest{
		method: "POST", target: "/v1/search", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", "alpha"), map[string]string{"Content-Type": "application/json"}),
		body:    `{"query":"E03"}`,
	})
	if search.Code != http.StatusOK {
		t.Fatalf("search status = %d body = %s", search.Code, search.Body.String())
	}
	if len(runtime.searchRequest.DocumentIDs) != 1 || runtime.searchRequest.DocumentIDs[0] != "doc-a" {
		t.Fatalf("search DocumentIDs = %v", runtime.searchRequest.DocumentIDs)
	}
	if len(control.resolvedNames) != 1 || control.resolvedNames[0] != "a.md" {
		t.Fatalf("resolved names = %v", control.resolvedNames)
	}

	// Administrators get the whole knowledge base.
	adminList := performRequest(handler, testRequest{
		method: "GET", target: "/v1/documents", remote: "10.0.0.9:5000",
		headers: sessionHeaders("admin-token", "alpha"),
	})
	adminDocuments, _ := decodeBody(t, adminList)["documents"].([]any)
	if len(adminDocuments) != 2 {
		t.Fatalf("admin documents = %v", adminDocuments)
	}
	adminSearch := performRequest(handler, testRequest{
		method: "POST", target: "/v1/search", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("admin-token", "alpha"), map[string]string{"Content-Type": "application/json"}),
		body:    `{"query":"E03"}`,
	})
	if adminSearch.Code != http.StatusOK || runtime.searchRequest.DocumentIDs != nil {
		t.Fatalf("admin search status = %d DocumentIDs = %v", adminSearch.Code, runtime.searchRequest.DocumentIDs)
	}
}

func TestTenantRoutesRejectAccountWithoutRead(t *testing.T) {
	runtime := newScopedDocRuntime()
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token")}}
	control := &fakeAccessControl{scopes: map[string]access.ScopeSet{}}
	handler := newAccessTestServer(t, users, control, runtime).Handler()

	for _, request := range []testRequest{
		{method: "GET", target: "/v1/documents", headers: sessionHeaders("user-token", "alpha")},
		{method: "GET", target: "/v1/knowledge-base", headers: sessionHeaders("user-token", "alpha")},
		{method: "GET", target: "/v1/search-results/123e4567-e89b-12d3-a456-426614174000", headers: sessionHeaders("user-token", "alpha")},
		{method: "GET", target: "/v1/uploads", headers: sessionHeaders("user-token", "alpha")},
	} {
		request.remote = "10.0.0.9:5000"
		response := performRequest(handler, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s %s status = %d body = %s", request.method, request.target, response.Code, response.Body.String())
		}
		if detail := decodeBody(t, response)["detail"]; detail != access.MessageKnowledgeAccess {
			t.Fatalf("%s %s detail = %v", request.method, request.target, detail)
		}
	}
}

func TestTenantWriteRoutesRequirePerFileWrite(t *testing.T) {
	runtime := newScopedDocRuntime()
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token")}}
	control := &fakeAccessControl{
		scopes: map[string]access.ScopeSet{"u-user|alpha": writeFilesScope("b.md")},
	}
	handler := newAccessTestServer(t, users, control, runtime).Handler()

	// Uploading a file outside the write grants is rejected before the runtime.
	body, contentType := multipartBody(t, nil, "a.md", "hello")
	denied := performRequest(handler, testRequest{
		method: "POST", target: "/v1/document-batches", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", "alpha"), map[string]string{"Content-Type": contentType}),
		body:    body,
	})
	if denied.Code != http.StatusForbidden ||
		decodeBody(t, denied)["detail"] != access.MessageWriteRequired {
		t.Fatalf("denied upload status = %d body = %s", denied.Code, denied.Body.String())
	}
	if len(runtime.batchFiles) != 0 {
		t.Fatalf("runtime received files = %v", runtime.batchFiles)
	}

	allowedBody, allowedType := multipartBody(t, nil, "b.md", "hello")
	allowed := performRequest(handler, testRequest{
		method: "POST", target: "/v1/document-batches", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", "alpha"), map[string]string{"Content-Type": allowedType}),
		body:    allowedBody,
	})
	if allowed.Code != http.StatusAccepted || len(runtime.batchFiles) != 1 || runtime.batchFiles[0].Name != "b.md" {
		t.Fatalf("allowed upload status = %d body = %s files = %v", allowed.Code, allowed.Body.String(), runtime.batchFiles)
	}

	// Management credentials keep the unrestricted behaviour.
	tokenRuntime := newScopedDocRuntime()
	tokenServer := New(testSettings("test-token"), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithRuntime(tokenRuntime), WithAccessControl(control))
	tokenList := performRequest(tokenServer.Handler(), testRequest{
		method: "GET", target: "/v1/documents", remote: "10.0.0.9:5000",
		headers: map[string]string{"Authorization": "Bearer test-token", "X-RAG-Namespace": "alpha"},
	})
	if tokenList.Code != http.StatusOK {
		t.Fatalf("token list status = %d body = %s", tokenList.Code, tokenList.Body.String())
	}
	if documents, _ := decodeBody(t, tokenList)["documents"].([]any); len(documents) != 2 {
		t.Fatalf("token documents = %v", documents)
	}
}

func TestTenantRoutesWithoutAccessControlKeepLegacyBehaviour(t *testing.T) {
	runtime := newScopedDocRuntime()
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token")}}
	handler := newAccessTestServer(t, users, nil, runtime).Handler()

	listed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/documents", remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", "alpha"),
	})
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", listed.Code, listed.Body.String())
	}
	if documents, _ := decodeBody(t, listed)["documents"].([]any); len(documents) != 2 {
		t.Fatalf("documents = %v", documents)
	}

	search := performRequest(handler, testRequest{
		method: "POST", target: "/v1/search", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", "alpha"), map[string]string{"Content-Type": "application/json"}),
		body:    `{"query":"E03"}`,
	})
	if search.Code != http.StatusOK || runtime.searchRequest.DocumentIDs != nil {
		t.Fatalf("search status = %d DocumentIDs = %v", search.Code, runtime.searchRequest.DocumentIDs)
	}
}

func TestNamespaceListFilteredByAccess(t *testing.T) {
	runtime := &fakeKBAdminRuntime{
		fakeDocRuntime: newFakeDocRuntime(),
		listPayload:    []map[string]any{{"namespace": "kb1"}, {"namespace": "kb2"}},
	}
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		"user-token":  userSession("user-token"),
		"admin-token": adminSession("admin-token"),
	}}
	control := &fakeAccessControl{visible: map[string][]string{
		"u-user":  {"kb2"},
		"u-admin": {"kb1", "kb2"},
	}}
	server := New(authTestSettings(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithUserAuth(users), WithAccessControl(control),
		WithRuntime(runtime), WithKnowledgeBaseAdminRuntime(runtime))
	handler := server.Handler()

	userList := performRequest(handler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", "alpha"),
	})
	if userList.Code != http.StatusOK {
		t.Fatalf("user list status = %d body = %s", userList.Code, userList.Body.String())
	}
	if namespaces, _ := decodeBody(t, userList)["namespaces"].([]any); len(namespaces) != 1 || namespaces[0] != "kb2" {
		t.Fatalf("user namespaces = %v", namespaces)
	}

	adminList := performRequest(handler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "10.0.0.9:5000",
		headers: sessionHeaders("admin-token", "alpha"),
	})
	if namespaces, _ := decodeBody(t, adminList)["namespaces"].([]any); len(namespaces) != 2 {
		t.Fatalf("admin namespaces = %v", namespaces)
	}

	tokenList := performRequest(handler, testRequest{
		method: "GET", target: "/v1/namespaces", remote: "127.0.0.1:5000",
		headers: map[string]string{"X-RAG-Namespace": "alpha"},
	})
	if tokenList.Code != http.StatusOK {
		t.Fatalf("loopback list status = %d body = %s", tokenList.Code, tokenList.Body.String())
	}
	if namespaces, _ := decodeBody(t, tokenList)["namespaces"].([]any); len(namespaces) != 2 {
		t.Fatalf("loopback namespaces = %v", namespaces)
	}
}

func TestCreateKnowledgeBaseForUser(t *testing.T) {
	selfCreate := &fakeSelfCreateRuntime{payload: map[string]any{"namespace": "newkb", "version": 1}}
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		"user-token":  userSession("user-token"),
		"admin-token": adminSession("admin-token"),
	}}
	server := New(authTestSettings(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithUserAuth(users), WithKnowledgeBaseSelfCreateRuntime(selfCreate))
	handler := server.Handler()

	body := `{"namespace":"newkb","strategy":"token","chunk_size":256,"chunk_overlap":32,` +
		`"embedding":{"base_url":"http://ignored","apikey":"secret"},"embedding_model":"ignored-model"}`
	created := performRequest(handler, testRequest{
		method: "POST", target: "/v1/knowledge-bases", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", ""), map[string]string{"Content-Type": "application/json"}),
		body:    body,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", created.Code, created.Body.String())
	}
	if selfCreate.ownerUserID != "u-user" || selfCreate.isAdmin {
		t.Fatalf("owner = %q isAdmin = %v", selfCreate.ownerUserID, selfCreate.isAdmin)
	}
	if selfCreate.input.Namespace != "newkb" || selfCreate.input.Chunking.Strategy != "token" ||
		selfCreate.input.Chunking.ChunkSize != 256 || selfCreate.input.Chunking.ChunkOverlap != 32 {
		t.Fatalf("input = %+v", selfCreate.input)
	}
	// Provider fields from the admin form are ignored on the tenant route.
	if selfCreate.input.EmbeddingModel != "" || len(selfCreate.input.EmbeddingValues) != 0 ||
		len(selfCreate.input.RerankerValues) != 0 || len(selfCreate.input.VLMValues) != 0 {
		t.Fatalf("provider fields leaked: %+v", selfCreate.input)
	}

	adminCreate := performRequest(handler, testRequest{
		method: "POST", target: "/v1/knowledge-bases", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("admin-token", ""), map[string]string{"Content-Type": "application/json"}),
		body:    `{"namespace":"adminkb"}`,
	})
	// An administrator using the simplified tenant form also gets the template
	// clone, owned by their own account.
	if adminCreate.Code != http.StatusCreated || selfCreate.isAdmin || selfCreate.ownerUserID != "u-admin" {
		t.Fatalf("admin create status = %d isAdmin = %v owner = %q", adminCreate.Code, selfCreate.isAdmin, selfCreate.ownerUserID)
	}

	// An administrator may submit the full provider configuration; only then is
	// the admin (probing) create path used.
	adminFull := performRequest(handler, testRequest{
		method: "POST", target: "/v1/knowledge-bases", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("admin-token", ""), map[string]string{"Content-Type": "application/json"}),
		body: `{"namespace":"adminfull","embedding":{"base_url":"http://embedding.local","apikey":"k1"},` +
			`"embedding_model":"admin-model","reranker_model":"rr","vlm_model":"vv"}`,
	})
	if adminFull.Code != http.StatusCreated || !selfCreate.isAdmin || selfCreate.ownerUserID != "u-admin" {
		t.Fatalf("admin full status = %d isAdmin = %v owner = %q", adminFull.Code, selfCreate.isAdmin, selfCreate.ownerUserID)
	}
	if selfCreate.input.EmbeddingModel != "admin-model" ||
		selfCreate.input.EmbeddingValues["base_url"] != "http://embedding.local" ||
		selfCreate.input.RerankerModel != "rr" || selfCreate.input.VLMModel != "vv" {
		t.Fatalf("admin provider input = %+v", selfCreate.input)
	}

	missingNamespace := performRequest(handler, testRequest{
		method: "POST", target: "/v1/knowledge-bases", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", ""), map[string]string{"Content-Type": "application/json"}),
		body:    `{}`,
	})
	if missingNamespace.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing namespace status = %d body = %s", missingNamespace.Code, missingNamespace.Body.String())
	}

	unknownField := performRequest(handler, testRequest{
		method: "POST", target: "/v1/knowledge-bases", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", ""), map[string]string{"Content-Type": "application/json"}),
		body:    `{"namespace":"newkb","unknown":"x"}`,
	})
	if unknownField.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown field status = %d body = %s", unknownField.Code, unknownField.Body.String())
	}
}

func TestCreateKnowledgeBaseForUserConflictAndUnavailable(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token")}}
	selfCreate := &fakeSelfCreateRuntime{
		err: &apierrors.KnowledgeConflictError{Message: messageKBTemplateMissing},
	}
	server := New(authTestSettings(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithUserAuth(users), WithKnowledgeBaseSelfCreateRuntime(selfCreate))
	handler := server.Handler()

	conflict := performRequest(handler, testRequest{
		method: "POST", target: "/v1/knowledge-bases", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", ""), map[string]string{"Content-Type": "application/json"}),
		body:    `{"namespace":"newkb"}`,
	})
	if conflict.Code != http.StatusConflict || decodeBody(t, conflict)["detail"] != messageKBTemplateMissing {
		t.Fatalf("conflict status = %d body = %s", conflict.Code, conflict.Body.String())
	}

	unavailable := New(authTestSettings(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithUserAuth(users))
	response := performRequest(unavailable.Handler(), testRequest{
		method: "POST", target: "/v1/knowledge-bases", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", ""), map[string]string{"Content-Type": "application/json"}),
		body:    `{"namespace":"newkb"}`,
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status = %d body = %s", response.Code, response.Body.String())
	}
}

// newScopedDocRuntime is a document runtime exposing two documents for list
// filtering tests.
func newScopedDocRuntime() *fakeDocRuntime {
	runtime := newFakeDocRuntime()
	runtime.documents = []map[string]any{{"name": "a.md"}, {"name": "b.md"}}
	runtime.searchPayload = map[string]any{"results": []any{}, "evidence_status": "no_results"}
	return runtime
}

// multipartFiles builds one upload body with the given ordered file names.
func multipartFiles(t *testing.T, names ...string) (string, string) {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)
	for _, name := range names {
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write([]byte("content of " + name)); err != nil {
			t.Fatalf("write content: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buffer.String(), writer.FormDataContentType()
}

func TestCheckDocumentsRouteHonoursScope(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		"user-token":  userSession("user-token"),
		"admin-token": adminSession("admin-token"),
	}}
	runtime := newFakeDocRuntime()
	control := &fakeAccessControl{scopes: map[string]access.ScopeSet{
		"u-user|alpha":  readFilesScope("a.md"),
		"u-admin|alpha": fullTestScope(),
	}}
	handler := newAccessTestServer(t, users, control, runtime).Handler()
	checkBody := `{"files":[{"name":"a.md","sha256":"` + strings.Repeat("a", 64) + `"}]}`

	withRead := performRequest(handler, testRequest{
		method: "POST", target: "/v1/documents/check", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", "alpha"), map[string]string{"Content-Type": "application/json"}),
		body:    checkBody,
	})
	if withRead.Code != http.StatusOK {
		t.Fatalf("read scope status = %d body = %s", withRead.Code, withRead.Body.String())
	}
	if len(runtime.checkFiles) != 1 || runtime.checkFiles[0].Name != "a.md" {
		t.Fatalf("check files = %+v", runtime.checkFiles)
	}

	// The same route with no read grant at all is rejected before the runtime
	// probes document existence.
	control.scopes["u-user|alpha"] = access.NewScopeSet()
	denied := performRequest(handler, testRequest{
		method: "POST", target: "/v1/documents/check", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", "alpha"), map[string]string{"Content-Type": "application/json"}),
		body:    checkBody,
	})
	if denied.Code != http.StatusForbidden || decodeBody(t, denied)["detail"] != access.MessageKnowledgeAccess {
		t.Fatalf("denied status = %d body = %s", denied.Code, denied.Body.String())
	}

	// Administrators keep full access.
	admin := performRequest(handler, testRequest{
		method: "POST", target: "/v1/documents/check", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("admin-token", "alpha"), map[string]string{"Content-Type": "application/json"}),
		body:    checkBody,
	})
	if admin.Code != http.StatusOK {
		t.Fatalf("admin status = %d body = %s", admin.Code, admin.Body.String())
	}

	// Without the access service the historical behaviour is preserved.
	legacy := newAccessTestServer(t, users, nil, runtime).Handler()
	legacyCheck := performRequest(legacy, testRequest{
		method: "POST", target: "/v1/documents/check", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", "alpha"), map[string]string{"Content-Type": "application/json"}),
		body:    checkBody,
	})
	if legacyCheck.Code != http.StatusOK {
		t.Fatalf("legacy status = %d body = %s", legacyCheck.Code, legacyCheck.Body.String())
	}
}

func TestUploadWriteGateRunsBeforeAnyUploadSideEffect(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token")}}
	// A read-only grant on one file has no write permission at all.
	control := &fakeAccessControl{scopes: map[string]access.ScopeSet{
		"u-user|alpha": readFilesScope("a.md"),
	}}

	runtime := newFakeDocRuntime()
	notifier := &fakeNotifier{}
	server := New(authTestSettings(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithUserAuth(users), WithAccessControl(control), WithRuntime(runtime),
		WithDocumentNotifier(notifier))
	body, contentType := multipartFiles(t, "a.md")
	denied := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/document-batches", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", "alpha"), map[string]string{"Content-Type": contentType}),
		body:    body,
	})
	if denied.Code != http.StatusForbidden || decodeBody(t, denied)["detail"] != access.MessageWriteRequired {
		t.Fatalf("denied status = %d body = %s", denied.Code, denied.Body.String())
	}
	if len(runtime.batchFiles) != 0 {
		t.Fatalf("runtime received files = %+v", runtime.batchFiles)
	}
	if len(notifier.namespaces) != 0 {
		t.Fatalf("denied upload emitted document changes = %v", notifier.namespaces)
	}

	// A batch is rejected as a whole when any file is outside the write grants;
	// the allowed prefix is never uploaded.
	runtime.batchFiles = nil
	batchBody, batchType := multipartFiles(t, "a.md", "b.md")
	control.scopes["u-user|alpha"] = writeFilesScope("a.md")
	batch := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/document-batches", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", "alpha"), map[string]string{"Content-Type": batchType}),
		body:    batchBody,
	})
	if batch.Code != http.StatusForbidden {
		t.Fatalf("batch status = %d body = %s", batch.Code, batch.Body.String())
	}
	if len(runtime.batchFiles) != 0 {
		t.Fatalf("batch leaked side effects: files=%+v", runtime.batchFiles)
	}

}
