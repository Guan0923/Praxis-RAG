package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/config"
)

// newTestClient starts one fake Feishu origin serving both the OAuth and Open
// Platform paths.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient(config.FeishuSettings{
		AppID:           "cli_test",
		AppSecret:       "secret",
		RedirectURI:     "https://rag.example.com/v1/feishu/oauth/callback",
		APIBaseURL:      server.URL,
		AccountsBaseURL: server.URL,
		Timeout:         5 * time.Second,
	})
	return client, server
}

func TestAuthorizeURL(t *testing.T) {
	client, _ := newTestClient(t, func(http.ResponseWriter, *http.Request) {})
	raw := client.AuthorizeURL("https://rag.example.com/v1/feishu/oauth/callback", "state-value")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("authorize URL is invalid: %v", err)
	}
	if parsed.Path != "/open-apis/authen/v1/authorize" {
		t.Fatalf("authorize path = %q", parsed.Path)
	}
	query := parsed.Query()
	if query.Get("client_id") != "cli_test" || query.Get("response_type") != "code" {
		t.Fatalf("authorize query = %v", query)
	}
	if query.Get("redirect_uri") != "https://rag.example.com/v1/feishu/oauth/callback" {
		t.Fatalf("redirect_uri = %q", query.Get("redirect_uri"))
	}
	if query.Get("state") != "state-value" {
		t.Fatalf("state = %q", query.Get("state"))
	}
	if !strings.Contains(query.Get("scope"), ScopeDriveRead) ||
		!strings.Contains(query.Get("scope"), ScopeDocxRead) ||
		!strings.Contains(query.Get("scope"), ScopeOffline) {
		t.Fatalf("scope = %q", query.Get("scope"))
	}
}

func TestExchangeCodeAndRefresh(t *testing.T) {
	var form url.Values
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/v3/token" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		form = r.PostForm
		writeJSON(t, w, map[string]any{
			"code": 0, "access_token": "u-token", "refresh_token": "r-token",
			"expires_in": 7200, "refresh_token_expires_in": 2592000, "scope": ScopeDriveRead + " " + ScopeOffline,
		})
	})
	token, err := client.ExchangeCode(context.Background(), "code-1", "https://rag.example.com/cb")
	if err != nil {
		t.Fatalf("ExchangeCode() error = %v", err)
	}
	if token.AccessToken != "u-token" || token.RefreshToken != "r-token" || token.ExpiresIn != 7200 {
		t.Fatalf("token = %+v", token)
	}
	if token.RefreshExpiresIn != 2592000 {
		t.Fatalf("refresh expiry = %d", token.RefreshExpiresIn)
	}
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "code-1" ||
		form.Get("client_id") != "cli_test" || form.Get("client_secret") != "secret" ||
		form.Get("redirect_uri") != "https://rag.example.com/cb" {
		t.Fatalf("token form = %v", form)
	}

	refreshed, err := client.Refresh(context.Background(), "r-token")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if refreshed.AccessToken != "u-token" {
		t.Fatalf("refresh token = %+v", refreshed)
	}
}

func TestRefreshErrorRequiresReauth(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"code": 20073, "error": "invalid_grant", "error_description": "refresh token used",
		})
	})
	_, err := client.Refresh(context.Background(), "r-token")
	if !IsAuthError(err) {
		t.Fatalf("error = %v, want auth kind", err)
	}
}

func TestListFilesSearchUserInfoAndRawContent(t *testing.T) {
	var paths []string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer u-token" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/open-apis/authen/v1/user_info":
			writeJSON(t, w, map[string]any{
				"code": 0,
				"data": map[string]any{"open_id": "ou_1", "name": "张三", "avatar_url": "https://a"},
			})
		case "/open-apis/drive/v1/files":
			if r.URL.Query().Get("folder_token") != "fld_1" {
				t.Fatalf("folder_token = %q", r.URL.Query().Get("folder_token"))
			}
			writeJSON(t, w, map[string]any{
				"code": 0,
				"data": map[string]any{
					"files": []map[string]any{
						{"token": "doxcn1", "name": "新版", "type": "docx", "url": "https://x"},
						{"token": "fld_2", "name": "子目录", "type": "folder"},
						{"token": "nodcn1", "name": "快捷方式", "type": "shortcut",
							"shortcut_info": map[string]any{"target_token": "doxcn2", "target_type": "docx"}},
					},
					"has_more":        true,
					"next_page_token": "next",
				},
			})
		case "/open-apis/suite/docs-api/search/object":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("search body: %v", err)
			}
			if body["search_key"] != "周报" {
				t.Fatalf("search_key = %v", body["search_key"])
			}
			writeJSON(t, w, map[string]any{
				"code": 0,
				"data": map[string]any{
					"docs_entities": []map[string]any{
						{"docs_token": "doxcn3", "docs_type": "doc", "title": "周报", "owner_id": "ou_1"},
					},
					"has_more": false, "total": 1,
				},
			})
		case "/open-apis/docx/v1/documents/doxcn1/raw_content":
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"content": "正文"}})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	})
	info, err := client.UserInfo(context.Background(), "u-token")
	if err != nil || info.Name != "张三" || info.OpenID != "ou_1" {
		t.Fatalf("UserInfo() = %+v, %v", info, err)
	}
	page, err := client.ListFiles(context.Background(), "u-token", "fld_1", "", 20)
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if len(page.Files) != 3 || !page.HasMore || page.NextPageToken != "next" {
		t.Fatalf("ListFiles() = %+v", page)
	}
	search, err := client.SearchDocuments(context.Background(), "u-token", "周报", 0, 10)
	if err != nil {
		t.Fatalf("SearchDocuments() error = %v", err)
	}
	if len(search.Items) != 1 || search.Items[0].Token != "doxcn3" || search.Total != 1 {
		t.Fatalf("SearchDocuments() = %+v", search)
	}
	content, err := client.RawContent(context.Background(), "u-token", "doxcn1")
	if err != nil || content != "正文" {
		t.Fatalf("RawContent() = %q, %v", content, err)
	}
	if len(paths) != 4 {
		t.Fatalf("paths = %v", paths)
	}
}

func TestAPIErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		code   int
		kind   ErrorKind
	}{
		{401, 99991663, KindAuth},
		{403, 99991679, KindPermission},
		{404, 1770002, KindNotFound},
		{429, 99991400, KindRateLimited},
		{500, 1061001, KindAPI},
	}
	for _, testCase := range cases {
		err := mapAPIError(testCase.status, testCase.code, "boom")
		if err.Kind != testCase.kind {
			t.Fatalf("mapAPIError(%d, %d) = %q, want %q", testCase.status, testCase.code, err.Kind, testCase.kind)
		}
		if strings.Contains(err.Error(), "Bearer") {
			t.Fatalf("error leaks a token: %q", err.Error())
		}
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
