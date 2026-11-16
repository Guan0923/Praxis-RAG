// Package feishu integrates the Feishu (Lark) Open Platform with the Console.
//
// The deployment configures one Feishu app (App ID / App Secret). Every Console
// account authorizes its own Feishu user, and browse/search/read calls use that
// account's user_access_token. The app credential is only used to exchange and
// refresh tokens; it never grants document access by itself.
package feishu

import (
	"errors"
	"fmt"
	"strings"
)

// OAuth scopes requested from each user. Feishu grants permissions
// cumulatively, so re-authorizing after a scope change returns a token that
// covers both the old and the new scopes.
const (
	// ScopeDriveRead lists folders and searches the documents the user can see.
	ScopeDriveRead = "drive:drive:readonly"
	// ScopeDocxRead reads new-version document plain text.
	ScopeDocxRead = "docx:document:readonly"
	// ScopeOffline keeps a refresh token so the connection survives the
	// two-hour user_access_token lifetime.
	ScopeOffline = "offline_access"
)

// RequiredScopes are the scopes the integration needs for every feature.
var RequiredScopes = []string{ScopeDriveRead, ScopeDocxRead, ScopeOffline}

// RequestedScopeValue is the space-separated scope value sent to Feishu.
var RequestedScopeValue = strings.Join(RequiredScopes, " ")

// MaxImportDocuments bounds one import request. It mirrors the ordinary upload
// batch limit so the same admission and batch semantics apply.
const MaxImportDocuments = 10

// MaxDocumentBytes bounds one imported document's plain text. It mirrors the
// document-job admission ceiling (10 MiB).
const MaxDocumentBytes = 10 * 1024 * 1024

// Sentinels returned by Service. Handlers map them to HTTP status codes.
var (
	// ErrNotConfigured means the deployment did not set the Feishu app.
	ErrNotConfigured = errors.New("飞书集成未配置")
	// ErrNotConnected means the Console account has not authorized Feishu yet.
	ErrNotConnected = errors.New("尚未连接飞书账号")
	// ErrReauthRequired means the stored grant can no longer be used and the
	// account has to authorize again.
	ErrReauthRequired = errors.New("飞书授权已失效，请重新连接飞书账号")
	// ErrInvalidState means an OAuth callback carried an unknown, expired or
	// already consumed state value.
	ErrInvalidState = errors.New("飞书授权校验失败，请重新发起连接")
	// ErrEmptyDocument means the document has no importable plain text.
	ErrEmptyDocument = errors.New("飞书文档没有可导入的正文")
	// ErrDocumentTooLarge means the plain text exceeds MaxDocumentBytes.
	ErrDocumentTooLarge = errors.New("飞书文档正文超过 10 MiB")
)

// ErrorKind classifies one Feishu API failure.
type ErrorKind string

const (
	// KindConfig marks a missing or invalid local configuration.
	KindConfig ErrorKind = "config"
	// KindValidation marks invalid caller input such as a malformed token.
	KindValidation ErrorKind = "validation"
	// KindAuth marks an invalid or expired access token.
	KindAuth ErrorKind = "auth"
	// KindPermission marks a missing resource permission or scope.
	KindPermission ErrorKind = "permission"
	// KindNotFound marks a deleted or inaccessible resource.
	KindNotFound ErrorKind = "not_found"
	// KindRateLimited marks Feishu throttling.
	KindRateLimited ErrorKind = "rate_limited"
	// KindNetwork marks a transport failure before Feishu answered.
	KindNetwork ErrorKind = "network"
	// KindAPI marks any other Feishu API failure.
	KindAPI ErrorKind = "api"
)

// APIError is one safe, classified Feishu failure. It never contains tokens,
// request bodies or full URLs.
type APIError struct {
	Kind    ErrorKind
	Status  int
	Code    int
	Message string
}

// Error implements error.
func (e *APIError) Error() string { return e.Message }

// KindOf returns the classification of err, or "" when err is not an APIError.
func KindOf(err error) ErrorKind {
	var apiError *APIError
	if errors.As(err, &apiError) {
		return apiError.Kind
	}
	return ""
}

// IsAuthError reports whether err invalidates the stored access token.
func IsAuthError(err error) bool { return KindOf(err) == KindAuth }

// IsPermissionError reports whether err is a missing permission or scope.
func IsPermissionError(err error) bool { return KindOf(err) == KindPermission }

// newAPIError builds one classified API failure with a safe user-facing text.
func newAPIError(kind ErrorKind, status, code int, message string) *APIError {
	return &APIError{Kind: kind, Status: status, Code: code, Message: message}
}

// mapAPIError classifies one non-zero Feishu response.
//
// The message shown to users is deliberately rebuilt from the category; the
// raw Feishu text is only appended for generic failures where it helps an
// operator and cannot contain credentials (Feishu error messages are
// short human-readable strings). Tokens and request parameters are never
// included.
func mapAPIError(status, code int, rawMessage string) *APIError {
	rawMessage = strings.TrimSpace(rawMessage)
	if len(rawMessage) > 240 {
		rawMessage = rawMessage[:240] + "…"
	}
	switch {
	case status == 401 || code == 99991661 || code == 99991663 || code == 99991668 || code == 20005:
		return newAPIError(KindAuth, status, code, "飞书授权已失效，请重新连接飞书账号。")
	case status == 403 || code == 99991679 || code == 99991672 ||
		code == 1061004 || code == 1770032:
		return newAPIError(KindPermission, status, code,
			"当前飞书账号没有访问该资源的权限，或应用授权范围不足；请确认文档权限后重试，必要时重新连接飞书账号。")
	case status == 404 || code == 1061003 || code == 1770002 || code == 1770003:
		return newAPIError(KindNotFound, status, code, "飞书文档不存在或已被删除。")
	case status == 429 || code == 99991400:
		return newAPIError(KindRateLimited, status, code, "飞书接口调用过于频繁，请稍后重试。")
	case code == 1770004 || code == 1770008 || code == 1770033:
		return newAPIError(KindAPI, status, code, "飞书文档内容超过接口限制，请拆分文档后重试。")
	case code == 1770021:
		return newAPIError(KindAPI, status, code, "飞书文档版本过旧，暂不支持导入。")
	}
	detail := "飞书接口调用失败"
	if code != 0 {
		detail = fmt.Sprintf("飞书接口调用失败（错误码 %d）", code)
	}
	if rawMessage != "" {
		detail += "：" + rawMessage
	}
	return newAPIError(KindAPI, status, code, detail)
}
