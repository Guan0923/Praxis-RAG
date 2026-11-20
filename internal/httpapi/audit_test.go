package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/audit"
	"github.com/Guan0923/Praxis-RAG/internal/auth"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

type auditCapture struct {
	entries []audit.Entry
	filter  audit.Filter
}

type testSessionAdmin struct{ actor string }

func (s *testSessionAdmin) ListSessionAccounts(context.Context, time.Time) ([]state.SessionAccount, error) {
	return []state.SessionAccount{}, nil
}
func (s *testSessionAdmin) ForceLogout(ctx context.Context, id string) (int64, error) {
	s.actor = audit.Actor(ctx)
	return 2, nil
}

func TestAuditSessionManagementAndRejections(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"admin-token": adminSession("admin-token"), "user-token": userSession("user-token")}, loginErr: &auth.Error{Kind: auth.KindUnauthorized, Message: auth.MessageCredentials, LoginAccount: "bob"}}
	settings := testSettings("")
	settings.AuthEnabled = true
	capture := &auditCapture{}
	sessions := &testSessionAdmin{}
	handler := New(settings, nil, nil, nil, WithUserAuth(users), WithSessionAdmin(sessions), WithAudit(capture)).Handler()
	for _, tc := range []struct {
		method, path, token, csrf string
		status                    int
	}{
		{"GET", "/v1/admin/session-users", "", "", 401},
		{"GET", "/v1/admin/session-users", "user-token", "", 403},
		{"POST", "/v1/admin/users/u-user/force-logout", "admin-token", "", 403},
		{"POST", "/v1/admin/users/u-admin/force-logout", "admin-token", "csrf-admin-token", 400},
		{"POST", "/v1/admin/users/u-user/force-logout", "admin-token", "csrf-admin-token", 200},
		{"GET", "/v1/admin/session-users", "admin-token", "", 200},
		{"GET", "/v1/admin/users", "revoked", "", 401},
		{"GET", "/v1/documents", "revoked", "", 401},
		{"GET", "/v1/admin/api-keys", "revoked", "", 401},
	} {
		headers := cookieFor(tc.token)
		headers[csrfHeaderName] = tc.csrf
		headers["X-RAG-Namespace"] = "manuals"
		response := performRequest(handler, testRequest{method: tc.method, target: tc.path, headers: headers, remote: "127.0.0.1:4444"})
		if response.Code != tc.status {
			t.Fatalf("%s = %d want %d", tc.path, response.Code, tc.status)
		}
	}
	if sessions.actor != "用户 alice（u-admin）" {
		t.Fatal(sessions.actor)
	}
	for i := 0; i < loginAttemptLimit+1; i++ {
		performRequest(handler, testRequest{method: "POST", target: "/v1/auth/login", body: `{"username":"bob","password":"test-password"}`})
	}
	counts := map[string]int{}
	for _, event := range capture.entries {
		if event.Outcome != "rejected" {
			t.Fatal(event)
		}
		counts[event.Action]++
	}
	if counts["access_denied"] != 1 || counts["csrf_rejected"] != 1 || counts["login_failed"] != loginAttemptLimit-1 || counts["login_throttled"] != 1 {
		t.Fatal(counts)
	}
}

func (c *auditCapture) Append(_ context.Context, entries []audit.Entry) error {
	c.entries = append(c.entries, entries...)
	return nil
}
func (c *auditCapture) List(_ context.Context, filter audit.Filter, _ int) ([]audit.Entry, error) {
	c.filter = filter
	return c.entries, nil
}

func TestAuditUserSearchAndAdminPage(t *testing.T) {
	capture := &auditCapture{}
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token"), "admin-token": adminSession("admin-token")}}
	settings := testSettings("")
	settings.AuthEnabled = true
	runtime := &fakeServiceRuntime{searchPayload: map[string]any{"results": []any{
		map[string]any{"source": "manual.pdf", "text": "private-chunk"}, map[string]any{"source": "manual.pdf"}, map[string]any{"source": "guide.md"},
	}}}
	handler := New(settings, nil, nil, nil, WithUserAuth(users), WithRuntime(runtime), WithAudit(capture)).Handler()
	response := performRequest(handler, testRequest{method: "POST", target: "/v1/search", headers: map[string]string{"Authorization": "Bearer user-token", "X-RAG-Namespace": "manuals", "X-Rag-Mcp-Audit-Actor": "伪造"}, body: `{"query":"private-query"}`})
	if response.Code != 200 || len(capture.entries) != 2 {
		t.Fatalf("search status=%d entries=%v", response.Code, capture.entries)
	}
	for i, name := range []string{"manual.pdf", "guide.md"} {
		e := capture.entries[i]
		if e.Actor != "用户 bob（u-user）" || e.Namespace != "manuals" || e.Target != name || e.Channel != "REST" || e.Action != "search" || e.Outcome != "returned" {
			t.Fatalf("event=%+v", e)
		}
	}
	denied := performRequest(handler, testRequest{method: "GET", target: "/v1/admin/audit-events", headers: cookieFor("user-token"), remote: "127.0.0.1:5000"})
	if denied.Code != 403 {
		t.Fatalf("non-admin status=%d", denied.Code)
	}
	page := performRequest(handler, testRequest{method: "GET", target: "/v1/admin/audit-events?limit=1&actor=bob", headers: cookieFor("admin-token")})
	var result struct {
		Items []audit.Entry `json:"items"`
		Next  string        `json:"next_cursor"`
	}
	if err := json.Unmarshal(page.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if page.Code != 200 || len(result.Items) != 1 || result.Next != capture.entries[0].ID || capture.filter.Actor != "bob" || page.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("page=%s", page.Body)
	}
	for _, secret := range []string{"private-query", "private-chunk", "user-token"} {
		if strings.Contains(page.Body.String(), secret) {
			t.Fatalf("audit exposed %q", secret)
		}
	}
}

func TestAuditBatchAdmissionAndDelete(t *testing.T) {
	runtime := newFakeDocRuntime()
	runtime.batchPayload = map[string]any{"items": []map[string]any{{"name": "a.md", "status": "accepted"}, {"name": "b.md", "status": "skipped"}}}
	capture := &auditCapture{}
	h := New(testSettings("test-token"), nil, nil, nil, WithRuntime(runtime), WithAudit(capture)).Handler()
	body, contentType := batchMultipartBody(t, map[string]string{"a.md": "abc", "b.md": "abc"})
	headers := tenantAuth()
	headers["Content-Type"] = contentType
	response := performRequest(h, testRequest{method: "POST", target: "/v1/document-batches", headers: headers, body: body})
	if response.Code != 202 || len(capture.entries) != 2 {
		t.Fatalf("admission=%d events=%v", response.Code, capture.entries)
	}
	if capture.entries[0].Target != "a.md" || capture.entries[0].Outcome != "accepted" || capture.entries[1].Outcome != "skipped" {
		t.Fatal(capture.entries)
	}
	deleted := performRequest(h, testRequest{method: "DELETE", target: "/v1/documents/a.md", headers: tenantAuth()})
	if deleted.Code != 200 || len(capture.entries) != 3 {
		t.Fatalf("delete=%d events=%v", deleted.Code, capture.entries)
	}
	e := capture.entries[2]
	if e.Target != "a.md" || e.Actor != "服务令牌" || e.Action != "delete_document" {
		t.Fatal(e)
	}
}
