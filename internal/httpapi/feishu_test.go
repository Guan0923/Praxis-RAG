package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/access"
	"github.com/Guan0923/Praxis-RAG/internal/auth"
	"github.com/Guan0923/Praxis-RAG/internal/feishu"
)

// fakeFeishuService is an in-memory FeishuService for handler tests.
type fakeFeishuService struct {
	enabled      bool
	status       feishu.Status
	authURL      string
	authErr      error
	completeErr  error
	files        feishu.FilePageView
	filesErr     error
	searchPage   feishu.SearchPageView
	searchErr    error
	docs         map[string]string
	readErrs     map[string]error
	readMu       sync.Mutex
	readCalls    []string
	disconnected bool
}

func (f *fakeFeishuService) Enabled() bool { return f.enabled }

func (f *fakeFeishuService) Status(context.Context, string) (feishu.Status, error) {
	return f.status, nil
}

func (f *fakeFeishuService) AuthorizeURL(context.Context, string) (string, error) {
	return f.authURL, f.authErr
}

func (f *fakeFeishuService) CompleteOAuth(context.Context, string, string, string) error {
	return f.completeErr
}

func (f *fakeFeishuService) Disconnect(context.Context, string) (bool, error) {
	f.disconnected = true
	return true, nil
}

func (f *fakeFeishuService) ListFiles(context.Context, string, string, string, int) (feishu.FilePageView, error) {
	return f.files, f.filesErr
}

func (f *fakeFeishuService) Search(context.Context, string, string, int, int) (feishu.SearchPageView, error) {
	return f.searchPage, f.searchErr
}

func (f *fakeFeishuService) ReadDocument(_ context.Context, _ string, documentID string) (feishu.DocumentContent, error) {
	f.readMu.Lock()
	f.readCalls = append(f.readCalls, documentID)
	f.readMu.Unlock()
	if err := f.readErrs[documentID]; err != nil {
		return feishu.DocumentContent{}, err
	}
	return feishu.DocumentContent{DocumentID: documentID, Text: f.docs[documentID]}, nil
}

func newFeishuTestServer(t *testing.T, service FeishuService, runtime Runtime, scope access.ScopeSet) *Server {
	t.Helper()
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token")}}
	control := &fakeAccessControl{scopes: map[string]access.ScopeSet{"u-user|alpha": scope}}
	return New(authTestSettings(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithUserAuth(users), WithAccessControl(control), WithRuntime(runtime), WithFeishuService(service))
}

func TestFeishuStatusAndAuthorizeStart(t *testing.T) {
	service := &fakeFeishuService{
		enabled: true,
		status:  feishu.Status{Enabled: true, Connected: false, RequiredScopes: feishu.RequiredScopes},
		authURL: "https://accounts.feishu.cn/open-apis/authen/v1/authorize?state=1",
	}
	server := newFeishuTestServer(t, service, nil, fullTestScope())

	status := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/feishu/status", headers: sessionHeaders("user-token", ""),
	})
	if status.Code != http.StatusOK {
		t.Fatalf("status code = %d body = %s", status.Code, status.Body.String())
	}
	if enabled := decodeBody(t, status)["enabled"]; enabled != true {
		t.Fatalf("enabled = %v", enabled)
	}

	start := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/feishu/oauth/start",
		headers: sessionHeaders("user-token", ""), body: "{}",
	})
	if start.Code != http.StatusOK {
		t.Fatalf("start code = %d body = %s", start.Code, start.Body.String())
	}
	if got := decodeBody(t, start)["authorize_url"]; got == "" {
		t.Fatalf("authorize_url = %v", got)
	}
}

func TestFeishuDisabled(t *testing.T) {
	server := newFeishuTestServer(t, nil, nil, fullTestScope())
	status := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/feishu/status", headers: sessionHeaders("user-token", ""),
	})
	if status.Code != http.StatusOK || decodeBody(t, status)["enabled"] != false {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
	files := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/feishu/files", headers: sessionHeaders("user-token", ""),
	})
	if files.Code != http.StatusServiceUnavailable {
		t.Fatalf("files code = %d body = %s", files.Code, files.Body.String())
	}
}

func TestFeishuImportIsolatesReadFailures(t *testing.T) {
	runtime := newFakeDocRuntime()
	runtime.batchPayload = map[string]any{
		"batch_id": "batch-1", "status": "processing", "total": 2,
		"accepted": 1, "skipped": 1, "failed": 0,
		"items": []map[string]any{
			{"position": 0, "name": "周报.md", "status": "accepted", "reason": nil,
				"artifact_id": "artifact-1", "error": nil,
				"job": map[string]any{"status": "queued", "stage": "queued", "error": nil}},
			{"position": 1, "name": "代码规范.md", "status": "skipped", "reason": "exists",
				"artifact_id": nil, "error": "同名文档已存在", "job": nil},
		},
	}
	service := &fakeFeishuService{
		enabled: true,
		docs: map[string]string{
			"doc-1": "正文一",
			"doc-2": "正文二",
		},
		readErrs: map[string]error{
			"doc-3": &feishu.APIError{Kind: feishu.KindPermission, Message: "没有权限"},
			"doc-5": feishu.ErrEmptyDocument,
		},
	}
	server := newFeishuTestServer(t, service, runtime, fullTestScope())

	body := `{"documents":[
		{"document_id":"doc-1","title":"周报"},
		{"document_id":"doc-2","title":"代码规范"},
		{"document_id":"doc-3","title":"读不到"},
		{"document_id":"doc-1","title":"周报（重复）"},
		{"document_id":"doc-5","title":"空文档"}
	]}`
	response := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/feishu/import",
		headers: sessionHeaders("user-token", "alpha"), body: body,
	})
	if response.Code != http.StatusAccepted {
		t.Fatalf("import code = %d body = %s", response.Code, response.Body.String())
	}
	payload := decodeBody(t, response)
	if payload["batch_id"] != "batch-1" || payload["namespace"] != "alpha" {
		t.Fatalf("payload = %v", payload)
	}
	if payload["accepted"] != float64(1) || payload["skipped"] != float64(2) || payload["failed"] != float64(2) {
		t.Fatalf("counts = accepted %v skipped %v failed %v", payload["accepted"], payload["skipped"], payload["failed"])
	}
	items, _ := payload["items"].([]any)
	if len(items) != 5 {
		t.Fatalf("items = %v", items)
	}
	statuses := make([]string, len(items))
	reasons := make([]string, len(items))
	for index, raw := range items {
		item, _ := raw.(map[string]any)
		statuses[index], _ = item["status"].(string)
		reasons[index], _ = item["reason"].(string)
	}
	wantStatuses := []string{"accepted", "skipped", "failed", "skipped", "failed"}
	for index, want := range wantStatuses {
		if statuses[index] != want {
			t.Fatalf("item[%d] status = %q, want %q", index, statuses[index], want)
		}
	}
	if reasons[2] != "permission" || reasons[3] != "duplicate" || reasons[4] != "empty" {
		t.Fatalf("reasons = %v", reasons)
	}
	if len(runtime.batchFiles) != 2 {
		t.Fatalf("batch files = %d, want 2", len(runtime.batchFiles))
	}
	if runtime.batchFiles[0].Name != "周报.md" || string(runtime.batchFiles[0].Content) != "正文一" {
		t.Fatalf("batch file[0] = %q %q", runtime.batchFiles[0].Name, runtime.batchFiles[0].Content)
	}
	if runtime.batchOwner != "u-user" {
		t.Fatalf("batch owner = %q", runtime.batchOwner)
	}
	// The duplicate selection and the failed/empty reads must not be read again.
	readCounts := map[string]int{}
	for _, documentID := range service.readCalls {
		readCounts[documentID]++
	}
	if len(service.readCalls) != 4 || readCounts["doc-1"] != 1 || readCounts["doc-3"] != 1 {
		t.Fatalf("read calls = %v", service.readCalls)
	}
}

func TestFeishuImportRequiresWriteScope(t *testing.T) {
	readOnly := access.NewScopeSet()
	readOnly.WholeAllow = uint16(access.OpRead)
	runtime := newFakeDocRuntime()
	service := &fakeFeishuService{enabled: true, docs: map[string]string{"doc-1": "正文"}}
	server := newFeishuTestServer(t, service, runtime, readOnly)
	response := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/feishu/import",
		headers: sessionHeaders("user-token", "alpha"),
		body:    `{"documents":[{"document_id":"doc-1","title":"周报"}]}`,
	})
	if response.Code != http.StatusForbidden {
		t.Fatalf("code = %d body = %s", response.Code, response.Body.String())
	}
	if len(service.readCalls) != 0 {
		t.Fatalf("read calls = %v", service.readCalls)
	}
}

func TestFeishuOAuthCallbackRedirects(t *testing.T) {
	service := &fakeFeishuService{enabled: true}
	server := newFeishuTestServer(t, service, nil, fullTestScope())
	response := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/feishu/oauth/callback?code=c&state=s",
		headers: map[string]string{"Cookie": sessionCookieName + "=user-token"},
	})
	if response.Code != http.StatusFound {
		t.Fatalf("callback code = %d body = %s", response.Code, response.Body.String())
	}
	location := response.Header().Get("Location")
	if !strings.Contains(location, "feishu=connected") {
		t.Fatalf("Location = %q", location)
	}
	service.completeErr = feishu.ErrInvalidState
	response = performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/feishu/oauth/callback?code=c&state=s",
		headers: map[string]string{"Cookie": sessionCookieName + "=user-token"},
	})
	if response.Code != http.StatusFound || !strings.Contains(response.Header().Get("Location"), "feishu_reason=state") {
		t.Fatalf("callback error = %d %q", response.Code, response.Header().Get("Location"))
	}
}

func TestFeishuReauthReturnsConflict(t *testing.T) {
	service := &fakeFeishuService{
		enabled:  true,
		filesErr: feishu.ErrReauthRequired,
		readErrs: map[string]error{"doc-1": feishu.ErrReauthRequired},
	}
	server := newFeishuTestServer(t, service, newFakeDocRuntime(), fullTestScope())
	files := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/feishu/files", headers: sessionHeaders("user-token", ""),
	})
	// A Feishu grant failure must not look like an expired Console session.
	if files.Code != http.StatusConflict {
		t.Fatalf("files code = %d body = %s", files.Code, files.Body.String())
	}
	response := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/feishu/import",
		headers: sessionHeaders("user-token", "alpha"),
		body:    `{"documents":[{"document_id":"doc-1","title":"周报"}]}`,
	})
	if response.Code != http.StatusAccepted {
		t.Fatalf("import code = %d body = %s", response.Code, response.Body.String())
	}
	items, _ := decodeBody(t, response)["items"].([]any)
	item, _ := items[0].(map[string]any)
	if item["status"] != "failed" || item["reason"] != "reauth" {
		t.Fatalf("item = %v", item)
	}
}

func TestFeishuImportRejectsMoreThanTenDocuments(t *testing.T) {
	service := &fakeFeishuService{enabled: true}
	server := newFeishuTestServer(t, service, newFakeDocRuntime(), fullTestScope())
	documents := make([]string, 11)
	for index := range documents {
		documents[index] = `{"document_id":"doc-` + strconv.Itoa(index) + `","title":"文档"}`
	}
	body := `{"documents":[` + strings.Join(documents, ",") + `]}`
	response := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/feishu/import",
		headers: sessionHeaders("user-token", "alpha"), body: body,
	})
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d body = %s", response.Code, response.Body.String())
	}
	if len(service.readCalls) != 0 {
		t.Fatalf("read calls = %v", service.readCalls)
	}
}

func TestFeishuFilesAndSearch(t *testing.T) {
	service := &fakeFeishuService{
		enabled: true,
		files: feishu.FilePageView{Files: []feishu.FileView{
			{Token: "doxcn1", Name: "文档", Type: "docx", Importable: true},
		}},
		searchPage: feishu.SearchPageView{
			Query: "周报", Items: []feishu.SearchView{{Token: "doxcn1", Title: "周报", Importable: true}},
			Total: 1, Count: 20,
		},
	}
	server := newFeishuTestServer(t, service, nil, fullTestScope())
	files := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/feishu/files?page_size=20",
		headers: sessionHeaders("user-token", ""),
	})
	if files.Code != http.StatusOK || len(decodeBody(t, files)["files"].([]any)) != 1 {
		t.Fatalf("files = %d %s", files.Code, files.Body.String())
	}
	search := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/feishu/search?q=%E5%91%A8%E6%8A%A5&limit=20",
		headers: sessionHeaders("user-token", ""),
	})
	if search.Code != http.StatusOK || decodeBody(t, search)["total"] != float64(1) {
		t.Fatalf("search = %d %s", search.Code, search.Body.String())
	}
}

func TestFeishuConnectionRemainsBoundToSessionUser(t *testing.T) {
	// The handlers must use the signed-in session user, never a request body id.
	service := &fakeFeishuService{enabled: true, status: feishu.Status{Enabled: true}}
	server := newFeishuTestServer(t, service, nil, fullTestScope())
	start := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/feishu/oauth/start",
		headers: sessionHeaders("user-token", ""), body: `{}`,
	})
	if start.Code != http.StatusOK {
		t.Fatalf("start code = %d", start.Code)
	}
	// CSRF is still required for cookie-authenticated writes.
	noCSRF := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/feishu/oauth/start",
		headers: map[string]string{"Cookie": sessionCookieName + "=user-token"}, body: `{}`,
	})
	if noCSRF.Code != http.StatusForbidden {
		t.Fatalf("CSRF code = %d body = %s", noCSRF.Code, noCSRF.Body.String())
	}
}
