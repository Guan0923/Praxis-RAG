package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/audit"
	"github.com/Guan0923/Praxis-RAG/internal/auth"
)

type auditCapture struct {
	entries []audit.Entry
	filter  audit.Filter
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
