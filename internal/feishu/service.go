package feishu

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// oauthStateTTL bounds how long one browser authorization may take.
const oauthStateTTL = 10 * time.Minute

// tokenLockCount is the number of per-account refresh lock stripes.
const tokenLockCount = 64

// documentIDPattern accepts Feishu docx tokens and keeps them safe for a URL
// path segment.
var documentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// folderTokenPattern accepts Feishu folder tokens.
var folderTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{0,128}$`)

// Account is the Console-visible Feishu identity.
type Account struct {
	OpenID    string `json:"open_id"`
	UnionID   string `json:"union_id,omitempty"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

// Status describes one account's Feishu connection.
type Status struct {
	Enabled          bool     `json:"enabled"`
	Connected        bool     `json:"connected"`
	NeedsReauth      bool     `json:"needs_reauth"`
	ReconnectReason  string   `json:"reconnect_reason,omitempty"`
	Account          *Account `json:"account,omitempty"`
	Scopes           []string `json:"scopes"`
	MissingScopes    []string `json:"missing_scopes"`
	RequiredScopes   []string `json:"required_scopes"`
	AccessExpiresAt  int64    `json:"access_expires_at,omitempty"`
	RefreshExpiresAt int64    `json:"refresh_expires_at,omitempty"`
	Notice           string   `json:"notice,omitempty"`
}

// FileView is one folder entry projected for the Console.
type FileView struct {
	Token             string `json:"token"`
	Name              string `json:"name"`
	Type              string `json:"type"`
	TargetType        string `json:"target_type,omitempty"`
	URL               string `json:"url,omitempty"`
	ModifiedTime      string `json:"modified_time,omitempty"`
	Importable        bool   `json:"importable"`
	Navigable         bool   `json:"navigable"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`
}

// FilePageView is one folder listing.
type FilePageView struct {
	FolderToken   string     `json:"folder_token"`
	Files         []FileView `json:"files"`
	HasMore       bool       `json:"has_more"`
	NextPageToken string     `json:"next_page_token,omitempty"`
}

// SearchView is one document search result.
type SearchView struct {
	Token      string `json:"token"`
	Title      string `json:"title"`
	Type       string `json:"type"`
	OwnerID    string `json:"owner_id,omitempty"`
	Importable bool   `json:"importable"`
	Note       string `json:"note,omitempty"`
}

// SearchPageView is one page of document search results.
type SearchPageView struct {
	Query   string       `json:"query"`
	Items   []SearchView `json:"items"`
	HasMore bool         `json:"has_more"`
	Total   int          `json:"total"`
	Offset  int          `json:"offset"`
	Count   int          `json:"count"`
}

// DocumentContent is one imported document's plain text.
type DocumentContent struct {
	DocumentID string
	Text       string
}

// ConnectionStore stores one account's Feishu grant.
type ConnectionStore interface {
	Get(ctx context.Context, userID string) (*state.FeishuConnection, error)
	Upsert(ctx context.Context, connection *state.FeishuConnection) error
	Delete(ctx context.Context, userID string) (bool, error)
}

// OAuthStateStore stores short-lived browser OAuth state digests.
type OAuthStateStore interface {
	Create(ctx context.Context, oauthState *state.FeishuOAuthState) error
	Consume(ctx context.Context, digest, userID string, now int64) (*state.FeishuOAuthState, error)
}

// Service implements the Feishu integration on top of the API client and the
// shared state repositories.
type Service struct {
	settings    config.FeishuSettings
	connections ConnectionStore
	states      OAuthStateStore
	client      *Client
	now         func() time.Time

	tokenLocks [tokenLockCount]sync.Mutex
}

// NewService builds the integration. The service is disabled when the
// deployment did not configure the app; handlers then answer 503 while the
// Console still reports why.
func NewService(settings config.FeishuSettings, connections ConnectionStore, states OAuthStateStore) *Service {
	return &Service{
		settings:    settings,
		connections: connections,
		states:      states,
		client:      NewClient(settings),
		now:         time.Now,
	}
}

// Enabled reports whether the deployment configured the integration and its
// stores are wired.
func (s *Service) Enabled() bool {
	return s != nil && s.settings.Enabled() && s.connections != nil && s.states != nil
}

// Status reports the current account's connection state without touching
// Feishu. It never fails for a missing connection.
func (s *Service) Status(ctx context.Context, userID string) (Status, error) {
	status := Status{
		Enabled:        s.Enabled(),
		RequiredScopes: append([]string(nil), RequiredScopes...),
		Scopes:         []string{},
		MissingScopes:  []string{},
	}
	if !s.Enabled() {
		return status, nil
	}
	connection, err := s.connections.Get(ctx, userID)
	if err != nil {
		return Status{}, err
	}
	if connection == nil {
		return status, nil
	}
	status.Connected = true
	status.Account = &Account{
		OpenID: connection.OpenID, UnionID: connection.UnionID,
		Name: connection.Name, AvatarURL: connection.AvatarURL,
	}
	status.Scopes = splitScope(connection.Scope)
	status.MissingScopes = missingScopes(status.Scopes)
	status.AccessExpiresAt = connection.AccessExpiresAt
	status.RefreshExpiresAt = connection.RefreshExpiresAt
	now := s.now().Unix()
	accessValid := connection.AccessToken != "" && connection.AccessExpiresAt > now+60
	switch {
	case missingAPIScopes(status.MissingScopes):
		// The grant lacks a document permission: browsing cannot work even
		// though the current access token may still be valid.
		status.NeedsReauth = true
		status.ReconnectReason = "scope_missing"
	case connection.RefreshToken == "":
		if !accessValid {
			status.NeedsReauth = true
			status.ReconnectReason = "refresh_missing"
		} else {
			status.Notice = "当前授权没有 refresh token，access token 过期后需要重新连接飞书账号。"
		}
	case connection.RefreshExpiresAt > 0 && connection.RefreshExpiresAt <= now:
		if !accessValid {
			status.NeedsReauth = true
			status.ReconnectReason = "refresh_expired"
		} else {
			status.Notice = "飞书 refresh token 已过期，access token 失效后需要重新连接。"
		}
	}
	if status.NeedsReauth {
		status.Notice = "飞书授权需要更新，请重新连接飞书账号。"
	} else if containsString(status.MissingScopes, ScopeOffline) {
		status.Notice = "当前授权不包含 offline_access，access token 过期后需要重新连接飞书账号。"
	}
	return status, nil
}

// AuthorizeURL creates one OAuth state row and returns the browser URL.
func (s *Service) AuthorizeURL(ctx context.Context, userID string) (string, error) {
	if !s.Enabled() {
		return "", ErrNotConfigured
	}
	rawState, err := randomState()
	if err != nil {
		return "", err
	}
	digest := sha256Hex(rawState)
	now := s.now()
	if err := s.states.Create(ctx, &state.FeishuOAuthState{
		StateDigest: digest,
		UserID:      userID,
		RedirectURI: s.settings.RedirectURI,
		ExpiresAt:   now.Add(oauthStateTTL).Unix(),
		CreatedAt:   now.Unix(),
	}); err != nil {
		return "", err
	}
	return s.client.AuthorizeURL(s.settings.RedirectURI, rawState), nil
}

// CompleteOAuth consumes one callback state, exchanges the code and stores the
// account's grant.
func (s *Service) CompleteOAuth(ctx context.Context, userID, code, rawState string) error {
	if !s.Enabled() {
		return ErrNotConfigured
	}
	if strings.TrimSpace(code) == "" || strings.TrimSpace(rawState) == "" {
		return ErrInvalidState
	}
	digest := sha256Hex(rawState)
	oauthState, err := s.states.Consume(ctx, digest, userID, s.now().Unix())
	if err != nil {
		return err
	}
	if oauthState == nil {
		return ErrInvalidState
	}
	token, err := s.client.ExchangeCode(ctx, code, oauthState.RedirectURI)
	if err != nil {
		return err
	}
	info, err := s.client.UserInfo(ctx, token.AccessToken)
	if err != nil {
		return err
	}
	if strings.TrimSpace(info.OpenID) == "" {
		return newAPIError(KindAPI, 0, 0, "飞书未返回用户标识，请稍后重试。")
	}
	now := s.now()
	connection := &state.FeishuConnection{
		UserID:           userID,
		OpenID:           info.OpenID,
		UnionID:          info.UnionID,
		Name:             firstNonEmpty(info.Name, info.EnName, "飞书用户"),
		AvatarURL:        info.AvatarURL,
		AccessToken:      token.AccessToken,
		RefreshToken:     token.RefreshToken,
		AccessExpiresAt:  now.Add(time.Duration(tokenExpiresIn(token)) * time.Second).Unix(),
		RefreshExpiresAt: refreshExpiry(now, token),
		Scope:            token.Scope,
	}
	if err := s.connections.Upsert(ctx, connection); err != nil {
		return err
	}
	return nil
}

// Disconnect removes one account's stored grant.
func (s *Service) Disconnect(ctx context.Context, userID string) (bool, error) {
	if s.connections == nil {
		return false, ErrNotConfigured
	}
	return s.connections.Delete(ctx, userID)
}

// ListFiles lists one folder level using the account's user token.
func (s *Service) ListFiles(ctx context.Context, userID, folderToken, pageToken string, pageSize int) (FilePageView, error) {
	if !folderTokenPattern.MatchString(folderToken) {
		return FilePageView{}, &APIError{Kind: KindValidation, Message: "飞书文件夹标识无效。"}
	}
	if len(pageToken) > 512 {
		return FilePageView{}, &APIError{Kind: KindValidation, Message: "飞书分页标识无效。"}
	}
	var page FilePage
	err := s.withToken(ctx, userID, func(token string) error {
		result, callErr := s.client.ListFiles(ctx, token, folderToken, pageToken, pageSize)
		if callErr != nil {
			return callErr
		}
		page = result
		return nil
	})
	if err != nil {
		return FilePageView{}, err
	}
	views := make([]FileView, 0, len(page.Files))
	for _, file := range page.Files {
		views = append(views, fileView(file))
	}
	return FilePageView{
		FolderToken: folderToken, Files: views,
		HasMore: page.HasMore, NextPageToken: page.NextPageToken,
	}, nil
}

// Search searches the documents visible to the account.
func (s *Service) Search(ctx context.Context, userID, query string, offset, count int) (SearchPageView, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > 100 {
		return SearchPageView{}, &APIError{Kind: KindValidation, Message: "搜索关键词长度必须在 1 到 100 个字符之间。"}
	}
	if offset < 0 {
		offset = 0
	}
	if count <= 0 || count > 50 {
		count = 20
	}
	if offset+count >= 200 {
		return SearchPageView{}, &APIError{Kind: KindValidation, Message: "飞书文档搜索最多翻到前 200 条，请缩短关键词。"}
	}
	var page SearchPage
	err := s.withToken(ctx, userID, func(token string) error {
		result, callErr := s.client.SearchDocuments(ctx, token, query, offset, count)
		if callErr != nil {
			return callErr
		}
		page = result
		return nil
	})
	if err != nil {
		return SearchPageView{}, err
	}
	items := make([]SearchView, 0, len(page.Items))
	seen := make(map[string]struct{}, len(page.Items))
	for _, entity := range page.Items {
		token := strings.TrimSpace(entity.Token)
		if token == "" {
			continue
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		items = append(items, SearchView{
			Token: token, Title: firstNonEmpty(entity.Title, "未命名文档"),
			Type: entity.Type, OwnerID: entity.OwnerID, Importable: true,
			Note: "导入时读取正文；旧版或不受支持的文档会逐篇提示失败原因。",
		})
	}
	return SearchPageView{
		Query: query, Items: items, HasMore: page.HasMore,
		Total: page.Total, Offset: page.Offset, Count: page.Count,
	}, nil
}

// ReadDocument reads one document's plain text.
func (s *Service) ReadDocument(ctx context.Context, userID, documentID string) (DocumentContent, error) {
	documentID = strings.TrimSpace(documentID)
	if !documentIDPattern.MatchString(documentID) {
		return DocumentContent{}, &APIError{Kind: KindValidation, Message: "飞书文档标识无效。"}
	}
	var text string
	err := s.withToken(ctx, userID, func(token string) error {
		content, callErr := s.client.RawContent(ctx, token, documentID)
		if callErr != nil {
			return callErr
		}
		text = content
		return nil
	})
	if err != nil {
		return DocumentContent{}, err
	}
	if strings.TrimSpace(text) == "" {
		return DocumentContent{}, ErrEmptyDocument
	}
	if len(text) > MaxDocumentBytes {
		return DocumentContent{}, ErrDocumentTooLarge
	}
	return DocumentContent{DocumentID: documentID, Text: text}, nil
}

// withToken runs call with a valid access token and retries once after a
// refresh when Feishu rejects the token before its recorded expiry.
func (s *Service) withToken(ctx context.Context, userID string, call func(token string) error) error {
	token, err := s.accessToken(ctx, userID, false, "")
	if err != nil {
		return err
	}
	err = call(token)
	if err == nil || !IsAuthError(err) {
		return err
	}
	refreshed, refreshErr := s.accessToken(ctx, userID, true, token)
	if refreshErr != nil {
		return refreshErr
	}
	return call(refreshed)
}

// accessToken returns a usable token, refreshing it when needed. The per-account
// lock serializes refreshes within this process; refresh tokens are single-use,
// so a concurrent refresh would otherwise invalidate the stored grant.
//
// stale is the access token that just failed. When force is set and another
// request already replaced it, the newer token is used instead of rotating the
// refresh token again.
func (s *Service) accessToken(ctx context.Context, userID string, force bool, stale string) (string, error) {
	connection, err := s.connections.Get(ctx, userID)
	if err != nil {
		return "", err
	}
	if connection == nil {
		return "", ErrNotConnected
	}
	now := s.now().Unix()
	if !force && connection.AccessToken != "" && connection.AccessExpiresAt > now+60 {
		return connection.AccessToken, nil
	}
	lock := &s.tokenLocks[tokenLockIndex(userID)]
	lock.Lock()
	defer lock.Unlock()
	// Another request may have refreshed while this one waited.
	connection, err = s.connections.Get(ctx, userID)
	if err != nil {
		return "", err
	}
	if connection == nil {
		return "", ErrNotConnected
	}
	now = s.now().Unix()
	if connection.AccessToken != "" && connection.AccessExpiresAt > now+60 {
		if !force {
			return connection.AccessToken, nil
		}
		if stale != "" && connection.AccessToken != stale {
			return connection.AccessToken, nil
		}
	}
	if connection.RefreshToken == "" {
		return "", ErrReauthRequired
	}
	if connection.RefreshExpiresAt > 0 && connection.RefreshExpiresAt <= now {
		return "", ErrReauthRequired
	}
	token, err := s.client.Refresh(ctx, connection.RefreshToken)
	if err != nil {
		// Only a dead/revoked refresh grant requires the user to authorize
		// again. App-credential or app-setting failures must stay actionable
		// deployment errors instead of sending the user through OAuth forever.
		if IsAuthError(err) {
			return "", ErrReauthRequired
		}
		return "", err
	}
	connection.AccessToken = token.AccessToken
	if token.RefreshToken != "" {
		connection.RefreshToken = token.RefreshToken
	}
	connection.AccessExpiresAt = s.now().Add(time.Duration(tokenExpiresIn(token)) * time.Second).Unix()
	if token.RefreshExpiresIn > 0 {
		connection.RefreshExpiresAt = s.now().Add(time.Duration(token.RefreshExpiresIn) * time.Second).Unix()
	}
	if token.Scope != "" {
		connection.Scope = token.Scope
	}
	if err := s.connections.Upsert(ctx, connection); err != nil {
		return "", err
	}
	return connection.AccessToken, nil
}

// fileView projects one Feishu file entry and explains unsupported types.
func fileView(file FileItem) FileView {
	view := FileView{
		Token: file.Token, Name: firstNonEmpty(file.Name, "未命名"),
		Type: file.Type, URL: file.URL, ModifiedTime: file.ModifiedTime,
	}
	targetType := file.Type
	targetToken := file.Token
	if file.Type == "shortcut" && file.ShortcutInfo != nil {
		targetType = file.ShortcutInfo.TargetType
		targetToken = file.ShortcutInfo.TargetToken
		view.TargetType = targetType
	}
	switch targetType {
	case "folder":
		view.Navigable = true
		view.Token = targetToken
		if file.Type == "shortcut" {
			view.UnsupportedReason = "暂不支持打开快捷方式指向的文件夹。"
			view.Navigable = false
		}
	case "docx":
		view.Importable = true
		view.Token = targetToken
	case "doc":
		view.UnsupportedReason = "旧版飞书文档（doc）暂不支持，请先转换为新版文档。"
	default:
		view.UnsupportedReason = "仅支持新版飞书文档（docx）。"
	}
	return view
}

// splitScope splits a Feishu scope string. Feishu separates scopes with spaces.
func splitScope(value string) []string {
	fields := strings.Fields(value)
	result := make([]string, 0, len(fields))
	seen := map[string]struct{}{}
	for _, field := range fields {
		if _, ok := seen[field]; ok {
			continue
		}
		seen[field] = struct{}{}
		result = append(result, field)
	}
	return result
}

// missingScopes returns the required scopes the grant does not cover. Broader
// read/write variants count as their read-only counterpart, because Feishu
// grants cannot be downgraded at the OAuth permission prompt.
func missingScopes(scopes []string) []string {
	missing := make([]string, 0, len(RequiredScopes))
	for _, required := range RequiredScopes {
		switch required {
		case ScopeDriveRead:
			if containsString(scopes, ScopeDriveRead) || containsString(scopes, "drive:drive") {
				continue
			}
		case ScopeDocxRead:
			if containsString(scopes, ScopeDocxRead) || containsString(scopes, "docx:document") {
				continue
			}
		default:
			if containsString(scopes, required) {
				continue
			}
		}
		missing = append(missing, required)
	}
	return missing
}

// missingAPIScopes reports whether a required document permission is missing
// (offline_access only affects refresh, not immediate browsing).
func missingAPIScopes(missing []string) bool {
	return containsString(missing, ScopeDriveRead) || containsString(missing, ScopeDocxRead)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// tokenExpiresIn normalizes the access token lifetime.
func tokenExpiresIn(token Token) int {
	if token.ExpiresIn > 0 {
		return token.ExpiresIn
	}
	return 7200
}

// refreshExpiry computes the refresh-token deadline; 0 means "unknown".
func refreshExpiry(now time.Time, token Token) int64 {
	if token.RefreshToken == "" {
		return 0
	}
	if token.RefreshExpiresIn > 0 {
		return now.Add(time.Duration(token.RefreshExpiresIn) * time.Second).Unix()
	}
	return now.Add(30 * 24 * time.Hour).Unix()
}

// randomState generates the browser OAuth state value.
func randomState() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// sha256Hex returns the lowercase hex digest used for state rows.
func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// tokenLockIndex maps one user id to a fixed lock stripe.
func tokenLockIndex(userID string) uint32 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(userID))
	return hasher.Sum32() % uint32(tokenLockCount)
}

// IsReauthError reports whether err means the account must reconnect Feishu.
func IsReauthError(err error) bool {
	return errors.Is(err, ErrReauthRequired)
}
