package lightrag

// This file implements the sidecar HTTP transport: base URL and option
// validation, one request helper that enforces the response size limit, the
// JSON Content-Type check and the classified *Error mapping, plus the public
// endpoints for health, workspace lifecycle, chunk ingest, document deletion
// and graph retrieval.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sidecar paths. Workspace endpoints are namespaced because one sidecar
// process serves every knowledge base.
const (
	// healthPath is the unauthenticated readiness probe.
	healthPath = "/healthz"
	// workspacesPath is the workspace collection.
	workspacesPath = "/v1/workspaces"
)

// DefaultTimeout bounds one sidecar call when Options.Timeout is not set. The
// graph extraction call can take a while, so the default is generous; the
// service applies RAG_LIGHTRAG_SERVICE_TIMEOUT on top of it through the
// context.
const DefaultTimeout = 60 * time.Second

// maxErrorDetail bounds the server "detail" text copied into a classified
// error, so a verbose FastAPI traceback cannot become the surface error.
const maxErrorDetail = 240

// responseLimit bounds one sidecar response body. It is a variable so tests can
// tighten it, mirroring how the document service response limit is exercised.
var responseLimit int64 = 8 << 20

// ErrorKind classifies one LightRAG sidecar failure.
type ErrorKind string

const (
	// ErrorKindUnavailable marks a transport failure, a 5xx reply, an
	// oversized body, or a reply the client cannot decode: the sidecar cannot
	// serve this call right now and the caller may retry.
	ErrorKindUnavailable ErrorKind = "unavailable"
	// ErrorKindUnauthorized marks 401/403: the shared token is missing, wrong,
	// or not allowed to use this sidecar.
	ErrorKindUnauthorized ErrorKind = "unauthorized"
	// ErrorKindInvalid marks a 4xx rejection the caller can fix and every
	// local argument validation failure.
	ErrorKindInvalid ErrorKind = "invalid"
)

// Error is one safe, classified sidecar failure. Message is a short Chinese
// sentence plus the server-provided detail; it never contains the shared token,
// the request body (which carries model API keys) or a full request URL.
type Error struct {
	// Kind is the failure category callers map to HTTP status codes.
	Kind ErrorKind
	// Message is the operator-facing explanation.
	Message string
	// Status is the HTTP status of the sidecar reply, or zero for a local or
	// transport failure.
	Status int
	// Err is the underlying cause; it stays reachable through errors.Is so a
	// cancelled call still matches context.Canceled.
	Err error
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// Unwrap exposes the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// Options configures one Client.
type Options struct {
	// BaseURL is the sidecar root URL (RAG_LIGHTRAG_SERVICE_URL), for example
	// http://127.0.0.1:8093. It must be an absolute http(s) URL without a
	// query or fragment.
	BaseURL string
	// Token is the shared bearer token. Empty sends no Authorization header,
	// which is the loopback deployment default.
	Token string
	// Timeout bounds one call. Values <= 0 use DefaultTimeout.
	Timeout time.Duration
	// HTTPClient overrides the transport. nil builds a client with Timeout;
	// a provided client keeps its own timeout.
	HTTPClient *http.Client
}

// Client is the LightRAG sidecar transport. It is safe for concurrent use
// because every call only reads the immutable fields.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient validates options and builds the sidecar client. It returns an
// error when BaseURL is empty or is not an absolute http(s) URL without a query
// or fragment; a non-positive Timeout falls back to DefaultTimeout.
func NewClient(options Options) (*Client, error) {
	baseURL, err := normalizeBaseURL(options.BaseURL)
	if err != nil {
		return nil, err
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	return &Client{baseURL: baseURL, token: options.Token, httpClient: httpClient}, nil
}

// BaseURL returns the normalized sidecar root, without a trailing slash.
func (c *Client) BaseURL() string { return c.baseURL }

// Health reports sidecar readiness (GET /healthz). The probe is
// unauthenticated, so the shared token is not sent.
func (c *Client) Health(ctx context.Context) (*HealthStatus, error) {
	var status HealthStatus
	if err := c.do(ctx, http.MethodGet, healthPath, nil, &status, false); err != nil {
		return nil, err
	}
	return &status, nil
}

// EnsureWorkspace creates or reuses the workspace of one knowledge base
// (POST /v1/workspaces). cfg.Namespace is trimmed and URL escaped by the
// client; a changed cfg.ConfigFingerprint makes the sidecar rebuild the stored
// graph and report it through WorkspaceStatus.Rebuilt.
func (c *Client) EnsureWorkspace(ctx context.Context, cfg WorkspaceConfig) (*WorkspaceStatus, error) {
	namespace := strings.TrimSpace(cfg.Namespace)
	if namespace == "" {
		return nil, invalidArgument("workspace namespace must not be empty")
	}
	cfg.Namespace = namespace
	var status WorkspaceStatus
	if err := c.do(ctx, http.MethodPost, workspacesPath, cfg, &status, true); err != nil {
		return nil, err
	}
	return &status, nil
}

// IngestChunks stores the chunks of one document in the workspace
// (POST /v1/workspaces/{namespace}/chunks). namespace selects the workspace and
// request.DocumentID identifies the document whose chunks are replaced.
func (c *Client) IngestChunks(ctx context.Context, namespace string, request IngestRequest) (*IngestResult, error) {
	workspace, err := workspacePath(namespace)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.DocumentID) == "" {
		return nil, invalidArgument("ingest document_id must not be empty")
	}
	var result IngestResult
	if err := c.do(ctx, http.MethodPost, workspace+"/chunks", request, &result, true); err != nil {
		return nil, err
	}
	return &result, nil
}

// DeleteDocument removes every chunk and graph element of one document
// (DELETE /v1/workspaces/{namespace}/documents/{document_id}). documentID is URL
// escaped so an arbitrary identifier stays one path segment.
func (c *Client) DeleteDocument(ctx context.Context, namespace, documentID string) error {
	workspace, err := workspacePath(namespace)
	if err != nil {
		return err
	}
	if strings.TrimSpace(documentID) == "" {
		return invalidArgument("document id must not be empty")
	}
	return c.do(ctx, http.MethodDelete, workspace+"/documents/"+url.PathEscape(documentID), nil, nil, true)
}

// Query runs one graph retrieval (POST /v1/workspaces/{namespace}/query).
// namespace selects the workspace and request carries the question, the
// document restriction and the top-k bound; the reply is graph evidence, never
// a generated answer.
func (c *Client) Query(ctx context.Context, namespace string, request QueryRequest) (*QueryResult, error) {
	workspace, err := workspacePath(namespace)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Query) == "" {
		return nil, invalidArgument("query must not be empty")
	}
	var result QueryResult
	if err := c.do(ctx, http.MethodPost, workspace+"/query", request, &result, true); err != nil {
		return nil, err
	}
	return &result, nil
}

// ClearWorkspace drops every chunk and graph element of one workspace
// (DELETE /v1/workspaces/{namespace}) so a knowledge-base re-index starts from
// an empty graph.
func (c *Client) ClearWorkspace(ctx context.Context, namespace string) error {
	workspace, err := workspacePath(namespace)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, workspace, nil, nil, true)
}

// do performs one sidecar request and decodes a JSON reply into out when out is
// non-nil. authenticated adds the shared bearer token; /healthz omits it. Every
// failure is an *Error: transport and reply problems are Unavailable, 401/403
// are Unauthorized, other 4xx are Invalid.
func (c *Client) do(ctx context.Context, method, path string, payload any, out any, authenticated bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			// The payload carries model API keys, so the cause is kept but the
			// body is never quoted.
			return &Error{Kind: ErrorKindInvalid, Message: "LightRAG 请求体无法编码", Err: err}
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return &Error{Kind: ErrorKindInvalid, Message: "LightRAG 请求地址无效", Err: err}
	}
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authenticated && c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return c.transportError(ctx, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return c.statusError(response)
	}
	if out == nil {
		return nil
	}
	return c.decodeJSON(response, out)
}

// decodeJSON checks the reply is JSON, bounds it and decodes it into out.
func (c *Client) decodeJSON(response *http.Response, out any) error {
	if !isJSONContentType(response.Header.Get("Content-Type")) {
		return &Error{
			Kind:    ErrorKindUnavailable,
			Message: fmt.Sprintf("LightRAG 服务返回了非 JSON 响应（HTTP %d）", response.StatusCode),
			Status:  response.StatusCode,
		}
	}
	raw, err := readBounded(response.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &Error{
			Kind:    ErrorKindUnavailable,
			Message: "LightRAG 服务返回的 JSON 无效",
			Status:  response.StatusCode,
			Err:     err,
		}
	}
	return nil
}

// statusError maps one non-2xx reply to a classified *Error. It recovers the
// FastAPI {"detail": "..."} text so an operator sees why the sidecar refused
// the call, but caps it, strips the shared token and never quotes the request
// body.
func (c *Client) statusError(response *http.Response) error {
	kind := ErrorKindInvalid
	label := "LightRAG 服务拒绝了请求"
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		kind = ErrorKindUnauthorized
		label = "LightRAG 服务鉴权失败"
	case response.StatusCode >= http.StatusInternalServerError:
		kind = ErrorKindUnavailable
		label = "LightRAG 服务不可用"
	}
	message := fmt.Sprintf("%s（HTTP %d）", label, response.StatusCode)
	if detail := c.errorDetail(response); detail != "" {
		message += "：" + detail
	}
	return &Error{Kind: kind, Message: message, Status: response.StatusCode}
}

// errorDetail extracts the FastAPI detail field from an error reply and
// sanitizes it for an error message.
func (c *Client) errorDetail(response *http.Response) string {
	raw, err := readBounded(response.Body)
	if err != nil || len(raw) == 0 {
		return ""
	}
	var payload struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	detail := strings.Join(strings.Fields(payload.Detail), " ")
	if c.token != "" {
		detail = strings.ReplaceAll(detail, c.token, "***")
	}
	runes := []rune(detail)
	if len(runes) > maxErrorDetail {
		return string(runes[:maxErrorDetail]) + "…"
	}
	return detail
}

// transportError classifies a transport failure and keeps deadline and
// cancellation discoverable through errors.Is.
func (c *Client) transportError(ctx context.Context, cause error) *Error {
	joined := errors.Join(ctx.Err(), cause)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(cause, context.DeadlineExceeded):
		return &Error{Kind: ErrorKindUnavailable, Message: "LightRAG 服务请求超时", Err: joined}
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(cause, context.Canceled):
		return &Error{Kind: ErrorKindUnavailable, Message: "LightRAG 服务请求已取消", Err: joined}
	default:
		return &Error{Kind: ErrorKindUnavailable, Message: "LightRAG 服务不可用", Err: joined}
	}
}

// normalizeBaseURL validates the sidecar root URL: it must be an absolute
// http(s) URL without credentials, a query or a fragment because the client
// appends fixed paths. An empty value reports the environment variable that
// configures it.
func normalizeBaseURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", &Error{
			Kind:    ErrorKindInvalid,
			Message: "LightRAG 侧车地址未配置：请设置 RAG_LIGHTRAG_SERVICE_URL",
		}
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", &Error{
			Kind:    ErrorKindInvalid,
			Message: "RAG_LIGHTRAG_SERVICE_URL must be an http(s) URL, for example http://127.0.0.1:8093",
		}
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", &Error{
			Kind:    ErrorKindInvalid,
			Message: "RAG_LIGHTRAG_SERVICE_URL must not include a query or fragment",
		}
	}
	if parsed.User != nil {
		return "", &Error{
			Kind:    ErrorKindInvalid,
			Message: "RAG_LIGHTRAG_SERVICE_URL must not embed credentials; configure the sidecar token instead",
		}
	}
	return strings.TrimRight(value, "/"), nil
}

// workspacePath validates namespace and returns its escaped
// /v1/workspaces/{namespace} collection path.
func workspacePath(namespace string) (string, error) {
	value := strings.TrimSpace(namespace)
	if value == "" {
		return "", invalidArgument("workspace namespace must not be empty")
	}
	return workspacesPath + "/" + url.PathEscape(value), nil
}

// invalidArgument builds a local parameter validation failure.
func invalidArgument(message string) *Error {
	return &Error{Kind: ErrorKindInvalid, Message: message}
}

// isJSONContentType reports whether the Content-Type header is JSON. The media
// type is parsed so "application/json; charset=utf-8" passes, while a missing
// or different type does not: a proxy error page must not be decoded as a
// result.
func isJSONContentType(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

// readBounded reads at most responseLimit+1 bytes so an oversized body is
// detected without buffering it whole.
func readBounded(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, responseLimit+1))
	if err != nil {
		return nil, &Error{Kind: ErrorKindUnavailable, Message: "LightRAG 服务响应读取失败", Err: err}
	}
	if int64(len(raw)) > responseLimit {
		return nil, &Error{
			Kind:    ErrorKindUnavailable,
			Message: fmt.Sprintf("LightRAG 服务响应超过 %d 字节上限", responseLimit),
		}
	}
	return raw, nil
}
