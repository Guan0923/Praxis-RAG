package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/config"
)

// contentReadLimit is the raw-content response budget: the plain text limit plus
// JSON envelope overhead. A larger response is reported as too large instead of
// being silently truncated.
const contentReadLimit = MaxDocumentBytes*2 + 64*1024

// envelope is the shared Feishu response shape. The token endpoint returns
// fields at the top level on success and an error object on failure, so its
// response is decoded separately.
type envelope struct {
	Code             int             `json:"code"`
	Msg              string          `json:"msg"`
	Error            string          `json:"error"`
	ErrorDescription string          `json:"error_description"`
	Data             json.RawMessage `json:"data"`
}

// Token is one user token response.
type Token struct {
	AccessToken      string
	RefreshToken     string
	ExpiresIn        int
	RefreshExpiresIn int
	Scope            string
}

// UserInfo is the authorized Feishu account projection used by the Console.
type UserInfo struct {
	OpenID    string `json:"open_id"`
	UnionID   string `json:"union_id"`
	Name      string `json:"name"`
	EnName    string `json:"en_name"`
	AvatarURL string `json:"avatar_url"`
}

// ShortcutInfo describes what a shortcut file points at.
type ShortcutInfo struct {
	TargetToken string `json:"target_token"`
	TargetType  string `json:"target_type"`
}

// FileItem is one entry of a Feishu folder listing.
type FileItem struct {
	Token        string        `json:"token"`
	Name         string        `json:"name"`
	Type         string        `json:"type"`
	ParentToken  string        `json:"parent_token"`
	URL          string        `json:"url"`
	OwnerID      string        `json:"owner_id"`
	CreatedTime  string        `json:"created_time"`
	ModifiedTime string        `json:"modified_time"`
	ShortcutInfo *ShortcutInfo `json:"shortcut_info"`
}

// FilePage is one page of a Feishu folder listing.
type FilePage struct {
	Files         []FileItem
	HasMore       bool
	NextPageToken string
}

// SearchEntity is one result of the Feishu document search API. The API reports
// the legacy docs_type "doc" for both old and new documents; the import read
// step determines whether the token is a new-version document.
type SearchEntity struct {
	Token   string `json:"docs_token"`
	Type    string `json:"docs_type"`
	Title   string `json:"title"`
	OwnerID string `json:"owner_id"`
}

// SearchPage is one page of Feishu document search results.
type SearchPage struct {
	Items   []SearchEntity
	HasMore bool
	Total   int
	Offset  int
	Count   int
}

// Client calls the Feishu Open Platform. It is safe for concurrent use.
type Client struct {
	httpClient      *http.Client
	apiBaseURL      string
	accountsBaseURL string
	appID           string
	appSecret       string
}

// NewClient builds the HTTP client from the deployment settings.
func NewClient(settings config.FeishuSettings) *Client {
	timeout := settings.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &Client{
		httpClient:      &http.Client{Timeout: timeout},
		apiBaseURL:      strings.TrimRight(settings.APIBaseURL, "/"),
		accountsBaseURL: strings.TrimRight(settings.AccountsBaseURL, "/"),
		appID:           settings.AppID,
		appSecret:       settings.AppSecret,
	}
}

// AuthorizeURL builds the browser authorization URL for one OAuth state value.
func (c *Client) AuthorizeURL(redirectURI, state string) string {
	values := url.Values{}
	values.Set("client_id", c.appID)
	values.Set("response_type", "code")
	values.Set("redirect_uri", redirectURI)
	values.Set("scope", RequestedScopeValue)
	values.Set("state", state)
	// Force the consent screen so a reconnect after a scope change visibly
	// grants the new permissions instead of silently reusing the old grant.
	values.Set("prompt", "consent")
	return c.accountsBaseURL + "/open-apis/authen/v1/authorize?" + values.Encode()
}

// ExchangeCode exchanges one authorization code for its first user token.
func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI string) (Token, error) {
	values := url.Values{}
	values.Set("grant_type", "authorization_code")
	values.Set("client_id", c.appID)
	values.Set("client_secret", c.appSecret)
	values.Set("code", code)
	values.Set("redirect_uri", redirectURI)
	return c.requestToken(ctx, values)
}

// Refresh exchanges one refresh token for a new access/refresh token pair.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Token, error) {
	values := url.Values{}
	values.Set("grant_type", "refresh_token")
	values.Set("client_id", c.appID)
	values.Set("client_secret", c.appSecret)
	values.Set("refresh_token", refreshToken)
	return c.requestToken(ctx, values)
}

// requestToken posts one OAuth token request. Feishu's v3 endpoint accepts both
// form and JSON; form is the documented primary encoding.
func (c *Client) requestToken(ctx context.Context, values url.Values) (Token, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.accountsBaseURL+"/oauth/v3/token", strings.NewReader(values.Encode()))
	if err != nil {
		return Token{}, newAPIError(KindConfig, 0, 0, "无法构建飞书授权请求")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Token{}, networkError(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Token{}, networkError(err)
	}
	var payload struct {
		Code             int    `json:"code"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		RefreshExpiresIn int    `json:"refresh_expires_in"`
		// Feishu's v3 token endpoint names this field refresh_token_expires_in;
		// the shorter alias is kept for compatibility with older responses.
		RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
		Scope                 string `json:"scope"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Token{}, newAPIError(KindAPI, response.StatusCode, 0, "飞书授权服务返回了无法解析的响应")
	}
	if response.StatusCode >= 400 || payload.Code != 0 {
		message := strings.TrimSpace(payload.ErrorDescription)
		if message == "" {
			message = strings.TrimSpace(payload.Error)
		}
		return Token{}, mapOAuthError(response.StatusCode, payload.Code, message)
	}
	if payload.AccessToken == "" {
		return Token{}, newAPIError(KindAPI, response.StatusCode, 0, "飞书授权服务未返回 access_token")
	}
	refreshExpiresIn := payload.RefreshExpiresIn
	if refreshExpiresIn <= 0 {
		refreshExpiresIn = payload.RefreshTokenExpiresIn
	}
	return Token{
		AccessToken:      payload.AccessToken,
		RefreshToken:     payload.RefreshToken,
		ExpiresIn:        payload.ExpiresIn,
		RefreshExpiresIn: refreshExpiresIn,
		Scope:            payload.Scope,
	}, nil
}

// mapOAuthError classifies OAuth token failures. Refresh-token errors (single
// use, revoked, expired) require a new authorization; invalid app credentials
// are a deployment problem.
func mapOAuthError(status, code int, rawMessage string) *APIError {
	switch code {
	case 20026, 20037, 20064, 20073:
		return newAPIError(KindAuth, status, code, "飞书授权已失效，请重新连接飞书账号。")
	case 20002, 20024:
		return newAPIError(KindConfig, status, code, "飞书应用凭据配置无效，请联系部署方检查 App ID/App Secret。")
	case 20009, 20010, 20069:
		return newAPIError(KindPermission, status, code,
			"当前账号无权使用该飞书应用，或应用未启用；请联系飞书管理员确认应用可用范围。")
	case 20074:
		return newAPIError(KindConfig, status, code,
			"飞书应用未开启刷新 user_access_token；请在开放平台「安全设置」中开启并发布应用。")
	}
	return mapAPIError(status, code, rawMessage)
}

// UserInfo reads the authorized account's profile.
func (c *Client) UserInfo(ctx context.Context, accessToken string) (UserInfo, error) {
	var info UserInfo
	err := c.getJSON(ctx, "/open-apis/authen/v1/user_info", accessToken, &info)
	return info, err
}

// ListFiles lists one folder level. An empty folderToken reads the user's cloud
// space root, which Feishu does not paginate.
func (c *Client) ListFiles(ctx context.Context, accessToken, folderToken, pageToken string, pageSize int) (FilePage, error) {
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 100
	}
	values := url.Values{}
	values.Set("page_size", strconv.Itoa(pageSize))
	if folderToken != "" {
		values.Set("folder_token", folderToken)
	}
	if pageToken != "" {
		values.Set("page_token", pageToken)
	}
	values.Set("order_by", "EditedTime")
	values.Set("direction", "DESC")
	var payload struct {
		Files         []FileItem `json:"files"`
		HasMore       bool       `json:"has_more"`
		NextPageToken string     `json:"next_page_token"`
	}
	if err := c.getJSON(ctx, "/open-apis/drive/v1/files?"+values.Encode(), accessToken, &payload); err != nil {
		return FilePage{}, err
	}
	return FilePage{Files: payload.Files, HasMore: payload.HasMore, NextPageToken: payload.NextPageToken}, nil
}

// SearchDocuments searches the documents visible to the authorized user. The
// current API covers cloud documents and returns doc/docx under docs_type doc.
func (c *Client) SearchDocuments(ctx context.Context, accessToken, query string, offset, count int) (SearchPage, error) {
	if count <= 0 || count > 50 {
		count = 20
	}
	if offset < 0 || offset+count >= 200 {
		offset = 0
	}
	body := map[string]any{
		"search_key": query,
		"count":      count,
		"offset":     offset,
		"docs_types": []string{"doc"},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return SearchPage{}, newAPIError(KindConfig, 0, 0, "无法构建飞书搜索请求")
	}
	var payload struct {
		Entities []SearchEntity `json:"docs_entities"`
		HasMore  bool           `json:"has_more"`
		Total    int            `json:"total"`
	}
	if err := c.postJSON(ctx, "/open-apis/suite/docs-api/search/object", accessToken, encoded, &payload); err != nil {
		return SearchPage{}, err
	}
	return SearchPage{Items: payload.Entities, HasMore: payload.HasMore, Total: payload.Total, Offset: offset, Count: count}, nil
}

// RawContent reads one new-version document's plain text.
func (c *Client) RawContent(ctx context.Context, accessToken, documentID string) (string, error) {
	path := "/open-apis/docx/v1/documents/" + url.PathEscape(documentID) + "/raw_content?lang=0"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBaseURL+path, nil)
	if err != nil {
		return "", newAPIError(KindConfig, 0, 0, "无法构建飞书文档读取请求")
	}
	c.setHeaders(request, accessToken)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", networkError(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, contentReadLimit+1))
	if err != nil {
		return "", networkError(err)
	}
	if len(body) > contentReadLimit {
		return "", ErrDocumentTooLarge
	}
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", newAPIError(KindAPI, response.StatusCode, 0, "飞书文档接口返回了无法解析的响应")
	}
	if response.StatusCode >= 400 || payload.Code != 0 {
		return "", mapAPIError(response.StatusCode, payload.Code, payload.Msg)
	}
	return payload.Data.Content, nil
}

// getJSON performs one authenticated GET and decodes data into target.
func (c *Client) getJSON(ctx context.Context, path, accessToken string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBaseURL+path, nil)
	if err != nil {
		return newAPIError(KindConfig, 0, 0, "无法构建飞书请求")
	}
	c.setHeaders(request, accessToken)
	return c.do(request, target)
}

// postJSON performs one authenticated JSON POST and decodes data into target.
func (c *Client) postJSON(ctx context.Context, path, accessToken string, body []byte, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBaseURL+path, bytes.NewReader(body))
	if err != nil {
		return newAPIError(KindConfig, 0, 0, "无法构建飞书请求")
	}
	c.setHeaders(request, accessToken)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	return c.do(request, target)
}

// do executes one Feishu API request and checks the shared envelope.
func (c *Client) do(request *http.Request, target any) error {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return networkError(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return networkError(err)
	}
	var payload envelope
	if err := json.Unmarshal(body, &payload); err != nil {
		return newAPIError(KindAPI, response.StatusCode, 0, "飞书接口返回了无法解析的响应")
	}
	if response.StatusCode >= 400 || payload.Code != 0 {
		message := strings.TrimSpace(payload.Msg)
		if message == "" {
			message = strings.TrimSpace(payload.ErrorDescription)
		}
		return mapAPIError(response.StatusCode, payload.Code, message)
	}
	if target != nil && len(payload.Data) > 0 {
		if err := json.Unmarshal(payload.Data, target); err != nil {
			return newAPIError(KindAPI, response.StatusCode, 0, "飞书接口返回了无法解析的数据")
		}
	}
	return nil
}

// setHeaders applies the shared authentication/content headers.
func (c *Client) setHeaders(request *http.Request, accessToken string) {
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "praxis-rag/feishu")
}

// networkError turns a transport failure into a safe classified error without
// exposing the request URL (which may contain a code or token in tests).
func networkError(err error) *APIError {
	if err == nil {
		return newAPIError(KindNetwork, 0, 0, "无法连接飞书服务，请检查网络后重试。")
	}
	return newAPIError(KindNetwork, 0, 0, "无法连接飞书服务，请检查网络后重试。")
}
