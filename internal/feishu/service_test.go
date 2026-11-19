package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

type fakeConnections struct {
	mu          sync.Mutex
	connections map[string]*state.FeishuConnection
}

func newFakeConnections() *fakeConnections {
	return &fakeConnections{connections: map[string]*state.FeishuConnection{}}
}

func (f *fakeConnections) Get(_ context.Context, userID string) (*state.FeishuConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	connection, ok := f.connections[userID]
	if !ok {
		return nil, nil
	}
	copy := *connection
	return &copy, nil
}

func (f *fakeConnections) Upsert(_ context.Context, connection *state.FeishuConnection) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	copy := *connection
	f.connections[connection.UserID] = &copy
	return nil
}

func (f *fakeConnections) Delete(_ context.Context, userID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.connections[userID]
	delete(f.connections, userID)
	return ok, nil
}

type fakeStates struct {
	mu     sync.Mutex
	states map[string]*state.FeishuOAuthState
}

func newFakeStates() *fakeStates {
	return &fakeStates{states: map[string]*state.FeishuOAuthState{}}
}

func (f *fakeStates) Create(_ context.Context, oauthState *state.FeishuOAuthState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	copy := *oauthState
	f.states[oauthState.StateDigest] = &copy
	return nil
}

func (f *fakeStates) Consume(_ context.Context, digest, userID string, now int64) (*state.FeishuOAuthState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	oauthState, ok := f.states[digest]
	if !ok || oauthState.UserID != userID || oauthState.Consumed || oauthState.ExpiresAt <= now {
		return nil, nil
	}
	oauthState.Consumed = true
	copy := *oauthState
	return &copy, nil
}

func testService(t *testing.T, handler http.HandlerFunc) (*Service, *fakeConnections, *fakeStates, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	connections := newFakeConnections()
	states := newFakeStates()
	service := NewService(config.FeishuSettings{
		AppID: "cli_test", AppSecret: "secret",
		RedirectURI: "https://rag.example.com/v1/feishu/oauth/callback",
		APIBaseURL:  server.URL, AccountsBaseURL: server.URL,
		Timeout: 5 * time.Second,
	}, connections, states)
	return service, connections, states, server
}

func TestStatusDisabled(t *testing.T) {
	service := NewService(config.FeishuSettings{}, newFakeConnections(), newFakeStates())
	status, err := service.Status(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.Enabled || status.Connected {
		t.Fatalf("status = %+v", status)
	}
}

func TestAuthorizeAndCompleteOAuth(t *testing.T) {
	service, connections, states, _ := testService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			if r.PostForm.Get("code") != "code-1" {
				t.Fatalf("code = %q", r.PostForm.Get("code"))
			}
			writeJSON(t, w, map[string]any{
				"code": 0, "access_token": "u-token", "refresh_token": "r-token",
				"expires_in": 7200, "refresh_expires_in": 2592000,
				"scope": ScopeDriveRead + " " + ScopeDocxRead + " " + ScopeOffline,
			})
		case "/open-apis/authen/v1/user_info":
			if r.Header.Get("Authorization") != "Bearer u-token" {
				t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
			}
			writeJSON(t, w, map[string]any{
				"code": 0,
				"data": map[string]any{"open_id": "ou_1", "name": "张三", "avatar_url": "https://a"},
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	})
	authorizeURL, err := service.AuthorizeURL(context.Background(), "u1")
	if err != nil {
		t.Fatalf("AuthorizeURL() error = %v", err)
	}
	parsed, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("authorize URL: %v", err)
	}
	rawState := parsed.Query().Get("state")
	if rawState == "" {
		t.Fatal("authorize URL has no state")
	}
	if len(states.states) != 1 {
		t.Fatalf("stored states = %v", states.states)
	}
	if err := service.CompleteOAuth(context.Background(), "u1", "code-1", rawState); err != nil {
		t.Fatalf("CompleteOAuth() error = %v", err)
	}
	connection, err := connections.Get(context.Background(), "u1")
	if err != nil || connection == nil {
		t.Fatalf("connection = %+v, %v", connection, err)
	}
	if connection.AccessToken != "u-token" || connection.RefreshToken != "r-token" {
		t.Fatalf("stored tokens = %+v", connection)
	}
	if connection.Name != "张三" || connection.OpenID != "ou_1" {
		t.Fatalf("stored account = %+v", connection)
	}
	if connection.AccessExpiresAt <= time.Now().Unix() {
		t.Fatalf("access expiry = %d", connection.AccessExpiresAt)
	}
	status, err := service.Status(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !status.Enabled || !status.Connected || status.NeedsReauth || len(status.MissingScopes) != 0 {
		t.Fatalf("status = %+v", status)
	}
	// The state is single-use.
	if err := service.CompleteOAuth(context.Background(), "u1", "code-1", rawState); err != ErrInvalidState {
		t.Fatalf("second CompleteOAuth() error = %v", err)
	}
}

func TestStatusRequiresReauthWhenScopeMissing(t *testing.T) {
	service, connections, _, _ := testService(t, func(http.ResponseWriter, *http.Request) {})
	connections.Upsert(context.Background(), &state.FeishuConnection{
		UserID: "u1", Name: "张三", AccessToken: "u-token", RefreshToken: "r-token",
		AccessExpiresAt:  time.Now().Add(time.Hour).Unix(),
		RefreshExpiresAt: time.Now().Add(time.Hour).Unix(),
		Scope:            ScopeDriveRead,
	})
	status, err := service.Status(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !status.NeedsReauth || status.ReconnectReason != "scope_missing" {
		t.Fatalf("status = %+v", status)
	}
	if len(status.MissingScopes) != 2 {
		t.Fatalf("missing scopes = %v", status.MissingScopes)
	}
}

func TestStatusWithoutRefreshTokenWhileAccessValid(t *testing.T) {
	service, connections, _, _ := testService(t, func(http.ResponseWriter, *http.Request) {})
	connections.Upsert(context.Background(), &state.FeishuConnection{
		UserID: "u1", Name: "张三", AccessToken: "u-token",
		AccessExpiresAt: time.Now().Add(time.Hour).Unix(),
		Scope:           ScopeDriveRead + " " + ScopeDocxRead + " " + ScopeOffline,
	})
	status, err := service.Status(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	// The current access token still works; reauthorization is required only
	// after it expires, so the modal stays usable now.
	if status.NeedsReauth {
		t.Fatalf("status = %+v", status)
	}
	if status.Notice == "" {
		t.Fatal("expected a notice about the missing refresh token")
	}
	connections.connections["u1"].AccessExpiresAt = time.Now().Add(-time.Minute).Unix()
	status, err = service.Status(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !status.NeedsReauth || status.ReconnectReason != "refresh_missing" {
		t.Fatalf("expired status = %+v", status)
	}
}

func TestListFilesProjectsSupportedTypes(t *testing.T) {
	service, connections, _, _ := testService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/open-apis/drive/v1/files" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"files": []map[string]any{
					{"token": "doxcn1", "name": "新版", "type": "docx"},
					{"token": "fld_2", "name": "子目录", "type": "folder"},
					{"token": "doccn1", "name": "旧版", "type": "doc"},
					{"token": "shtcn1", "name": "表格", "type": "sheet"},
					{"token": "nodcn1", "name": "快捷方式", "type": "shortcut",
						"shortcut_info": map[string]any{"target_token": "doxcn2", "target_type": "docx"}},
				},
				"has_more": false,
			},
		})
	})
	connections.Upsert(context.Background(), &state.FeishuConnection{
		UserID: "u1", AccessToken: "u-token",
		AccessExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	page, err := service.ListFiles(context.Background(), "u1", "", "", 20)
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if len(page.Files) != 5 {
		t.Fatalf("files = %+v", page.Files)
	}
	if !page.Files[0].Importable || page.Files[0].Token != "doxcn1" {
		t.Fatalf("docx view = %+v", page.Files[0])
	}
	if !page.Files[1].Navigable || page.Files[1].Token != "fld_2" {
		t.Fatalf("folder view = %+v", page.Files[1])
	}
	if page.Files[2].Importable || !strings.Contains(page.Files[2].UnsupportedReason, "旧版") {
		t.Fatalf("doc view = %+v", page.Files[2])
	}
	if page.Files[3].Importable {
		t.Fatalf("sheet view = %+v", page.Files[3])
	}
	if !page.Files[4].Importable || page.Files[4].Token != "doxcn2" || page.Files[4].TargetType != "docx" {
		t.Fatalf("shortcut view = %+v", page.Files[4])
	}
}

func TestAccessTokenRefreshesAndRetriesAuthFailure(t *testing.T) {
	service, connections, _, _ := testService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token":
			writeJSON(t, w, map[string]any{
				"code": 0, "access_token": "u-new", "refresh_token": "r-new",
				"expires_in": 7200, "refresh_expires_in": 2592000,
				"scope": ScopeDriveRead + " " + ScopeDocxRead + " " + ScopeOffline,
			})
		case "/open-apis/docx/v1/documents/doxcn1/raw_content":
			if r.Header.Get("Authorization") == "Bearer u-old" {
				writeJSON(t, w, map[string]any{"code": 99991663, "msg": "token invalid"})
				return
			}
			if r.Header.Get("Authorization") != "Bearer u-new" {
				t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
			}
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"content": "正文"}})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	})
	connections.Upsert(context.Background(), &state.FeishuConnection{
		UserID: "u1", AccessToken: "u-old", RefreshToken: "r-old",
		AccessExpiresAt:  time.Now().Add(time.Hour).Unix(),
		RefreshExpiresAt: time.Now().Add(time.Hour).Unix(),
		Scope:            ScopeDriveRead + " " + ScopeDocxRead + " " + ScopeOffline,
	})
	content, err := service.ReadDocument(context.Background(), "u1", "doxcn1")
	if err != nil {
		t.Fatalf("ReadDocument() error = %v", err)
	}
	if content.Text != "正文" {
		t.Fatalf("content = %q", content.Text)
	}
	connection, _ := connections.Get(context.Background(), "u1")
	if connection.AccessToken != "u-new" || connection.RefreshToken != "r-new" {
		t.Fatalf("refreshed connection = %+v", connection)
	}
}

func TestReadDocumentRejectsEmptyContent(t *testing.T) {
	service, connections, _, _ := testService(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"content": "  \n"}})
	})
	connections.Upsert(context.Background(), &state.FeishuConnection{
		UserID: "u1", AccessToken: "u-token",
		AccessExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if _, err := service.ReadDocument(context.Background(), "u1", "doxcn1"); err != ErrEmptyDocument {
		t.Fatalf("ReadDocument() error = %v", err)
	}
}

func TestSearchProjectsEntities(t *testing.T) {
	service, connections, _, _ := testService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/open-apis/suite/docs-api/search/object" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["search_key"] != "周报" {
			t.Fatalf("search_key = %v", body["search_key"])
		}
		writeJSON(t, w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"docs_entities": []map[string]any{
					{"docs_token": "doxcn1", "docs_type": "doc", "title": "周报", "owner_id": "ou_1"},
				},
				"has_more": true, "total": 30,
			},
		})
	})
	connections.Upsert(context.Background(), &state.FeishuConnection{
		UserID: "u1", AccessToken: "u-token",
		AccessExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	page, err := service.Search(context.Background(), "u1", " 周报 ", 0, 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Token != "doxcn1" || !page.Items[0].Importable {
		t.Fatalf("search page = %+v", page)
	}
	if !page.HasMore || page.Total != 30 || page.Query != "周报" {
		t.Fatalf("search metadata = %+v", page)
	}
}

// TestAccessTokenRefreshMapsAuthFailureToReauth pins the refresh-failure
// classification: a revoked refresh grant maps to ErrReauthRequired so the
// user is sent back through OAuth, while an app-credential failure stays an
// actionable deployment error instead of a re-auth loop.
func TestAccessTokenRefreshMapsAuthFailureToReauth(t *testing.T) {
	seed := func(t *testing.T, connections *fakeConnections) {
		t.Helper()
		if err := connections.Upsert(context.Background(), &state.FeishuConnection{
			UserID: "u1", AccessToken: "u-expired", RefreshToken: "r-old",
			AccessExpiresAt:  time.Now().Add(-time.Hour).Unix(),
			RefreshExpiresAt: time.Now().Add(time.Hour).Unix(),
			Scope:            ScopeDriveRead + " " + ScopeDocxRead + " " + ScopeOffline,
		}); err != nil {
			t.Fatalf("Upsert() error = %v", err)
		}
	}

	// A revoked refresh token (code 20026) requires re-authorization.
	service, connections, _, _ := testService(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"code": 20026, "error": "refresh token revoked"})
	})
	seed(t, connections)
	if _, err := service.ReadDocument(context.Background(), "u1", "doxcn1"); err != ErrReauthRequired {
		t.Fatalf("ReadDocument(revoked refresh) = %v, want ErrReauthRequired", err)
	}

	// Invalid app credentials (code 20002) stay a deployment error.
	deployService, deployConnections, _, _ := testService(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"code": 20002, "error": "invalid client_id"})
	})
	seed(t, deployConnections)
	_, err := deployService.ReadDocument(context.Background(), "u1", "doxcn1")
	if err == ErrReauthRequired {
		t.Fatalf("ReadDocument(bad credentials) = ErrReauthRequired, want the deployment error to pass through")
	}
	if err == nil || !strings.Contains(err.Error(), "App ID/App Secret") {
		t.Fatalf("ReadDocument(bad credentials) = %v, want the app-credential message", err)
	}
}
