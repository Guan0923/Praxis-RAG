package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Guan0923/Praxis-RAG/internal/audit"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Guan0923/Praxis-RAG/internal/access"
	"github.com/Guan0923/Praxis-RAG/internal/logging"
)

// modernProtocolVersion is the first protocol era the SDK only serves over a
// stateless Streamable HTTP handler.
const (
	modernProtocolVersion = "2026-07-28"
	protocolVersionHeader = "Mcp-Protocol-Version"
	sessionIDHeader       = "Mcp-Session-Id"
	metaProtocolVersion   = "io.modelcontextprotocol/protocolVersion"
)

// HTTPOptions configures the /mcp Streamable HTTP handler.
type HTTPOptions struct {
	// Endpoint is required.
	Endpoint *Endpoint
	// NamespaceFromRequest resolves the authenticated tenant namespace. The
	// HTTP API layer supplies a callback that reads its own request context
	// (httpapi.NamespaceFromContext); the default reads NamespaceFromContext,
	// which is testable and free of an import cycle.
	//
	// Return ErrUnauthorized for missing/invalid credentials (401),
	// ErrForbidden for an authenticated but unauthorized tenant (403) and
	// ErrInvalidNamespace for a malformed namespace (400).
	NamespaceFromRequest func(*http.Request) (string, error)
	// AllowedHosts mirrors RAG_MCP_ALLOWED_HOSTS. Patterns may end in ":"*
	// to accept any port on the base host; an empty list rejects every request
	// (like the Python TransportSecurityMiddleware).
	AllowedHosts []string
	// AllowedOrigins mirrors RAG_MCP_ALLOWED_ORIGINS. A request without an
	// Origin header is allowed (same-origin requests).
	AllowedOrigins []string
	// MaxRequestBodyBytes bounds buffered request bodies; zero uses the
	// Python service limit 6*10 MiB + 64 KiB.
	MaxRequestBodyBytes int64
	// Logger receives transport diagnostics.
	Logger *slog.Logger
}

// Handler is the dual-era Streamable HTTP MCP endpoint. It authenticates the
// tenant from the request context, enforces the same Host/Origin allowlist as
// the Python service and dispatches each request to the stateful (legacy) or
// stateless (modern) SDK handler of that namespace.
type Handler struct {
	endpoint       *Endpoint
	namespace      func(*http.Request) (string, error)
	allowedHosts   []string
	allowedOrigins []string
	maxBody        int64
	logger         *slog.Logger
}

// NewHTTPHandler builds the /mcp handler.
func NewHTTPHandler(opts HTTPOptions) (*Handler, error) {
	if opts.Endpoint == nil {
		return nil, fmt.Errorf("mcp: HTTPOptions.Endpoint is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	namespace := opts.NamespaceFromRequest
	if namespace == nil {
		namespace = func(r *http.Request) (string, error) {
			value, ok := NamespaceFromContext(r.Context())
			if !ok || value == "" {
				return "", ErrUnauthorized
			}
			return value, nil
		}
	}
	maxBody := opts.MaxRequestBodyBytes
	if maxBody == 0 {
		maxBody = maxRequestBodyBytes
	}
	return &Handler{
		endpoint:       opts.Endpoint,
		namespace:      namespace,
		allowedHosts:   append([]string(nil), opts.AllowedHosts...),
		allowedOrigins: append([]string(nil), opts.AllowedOrigins...),
		maxBody:        maxBody,
		logger:         logger,
	}, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Authentication: the HTTP API layer already validated credentials and
	// injected the namespace into the request context.
	namespace, err := h.namespace(r)
	if err != nil {
		h.writeAuthError(w, err)
		return
	}
	if !namespacePattern.MatchString(namespace) {
		writeJSONDetail(w, http.StatusBadRequest, "Invalid RAG namespace.")
		return
	}

	// 2. Transport security: Content-Type for POST, then DNS-rebinding Host and
	// Origin allowlists.
	if r.Method == http.MethodPost && !validContentType(r.Header.Get("Content-Type")) {
		writePlain(w, http.StatusBadRequest, "Invalid Content-Type header")
		return
	}
	if !matchesAllowlist(r.Host, h.allowedHosts) {
		writePlain(w, http.StatusMisdirectedRequest, "Invalid Host header")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !matchesAllowlist(origin, h.allowedOrigins) {
		writePlain(w, http.StatusForbidden, "Invalid Origin header")
		return
	}

	// 3. Protocol era dispatch. The SDK serves protocol >= 2026-07-28 only on
	// a stateless transport, so buffer the JSON-RPC envelope when the era is
	// not already determined by the Mcp-Protocol-Version/Session headers.
	modern, body, err := h.classify(r)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writePlain(w, http.StatusRequestEntityTooLarge, "Request body too large")
			return
		}
		writePlain(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if body != nil {
		restoreBody(r, body)
	}

	transport, err := h.endpoint.transportFor(namespace)
	if err != nil {
		h.logger.Error("mcp: cannot initialize namespace server", append([]any{"namespace", namespace}, logging.ErrorAttrs(err)...)...)
		writeJSONDetail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	request := r.WithContext(WithNamespace(r.Context(), namespace))
	// Carry the caller's access scope to the SDK handlers per request. The
	// stateful (legacy) transport runs handlers on the session goroutine with
	// the initialize request's context, so context values alone would be stale;
	// the header is attached to every message as RequestExtra.Header. It always
	// overrides any client-supplied value so a restricted caller cannot forge a
	// broader scope.
	request.Header = r.Header.Clone()
	if request.Header == nil {
		request.Header = http.Header{}
	}
	request.Header.Set(actorHeaderName, base64.RawURLEncoding.EncodeToString([]byte(audit.Actor(r.Context()))))
	if scope, ok := access.ScopeFromContext(r.Context()); ok {
		request.Header.Set(scopeHeaderName, encodeScopeHeader(scope))
	} else {
		request.Header.Set(scopeHeaderName, scopeHeaderNone)
	}
	if modern {
		transport.stateless.ServeHTTP(w, request)
		return
	}
	transport.stateful.ServeHTTP(w, request)
}

// classify decides the protocol era of a request and returns the buffered POST
// body (nil when it was not consumed).
func (h *Handler) classify(r *http.Request) (modern bool, body []byte, err error) {
	if r.Method != http.MethodPost {
		// GET (standalone event stream) and DELETE (session teardown) are
		// legacy stateful transport operations.
		return false, nil, nil
	}
	buffered, err := readBody(r, h.maxBody)
	if err != nil {
		return false, nil, err
	}
	body = buffered
	// The client sets Mcp-Protocol-Version for every request once a version is
	// negotiated, including server/discover.
	if header := strings.TrimSpace(r.Header.Get(protocolVersionHeader)); header != "" {
		return header >= modernProtocolVersion, body, nil
	}
	// A session id only exists on the stateful legacy transport.
	if r.Header.Get(sessionIDHeader) != "" {
		return false, body, nil
	}
	return bodyUsesModernProtocol(body), body, nil
}

// bodyUsesModernProtocol inspects a JSON-RPC envelope for the SEP-2575
// server/discover method or a per-request _meta.protocolVersion.
func bodyUsesModernProtocol(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	type envelope struct {
		Method string `json:"method"`
		Params struct {
			Meta map[string]any `json:"_meta"`
		} `json:"params"`
	}
	decode := func(raw []byte) (envelope, bool) {
		var message envelope
		if err := json.Unmarshal(raw, &message); err != nil {
			return envelope{}, false
		}
		return message, true
	}
	check := func(message envelope) bool {
		if message.Method == "server/discover" {
			return true
		}
		version, ok := message.Params.Meta[metaProtocolVersion].(string)
		return ok && version >= modernProtocolVersion
	}
	if len(body) > 0 && body[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			return false
		}
		for _, raw := range batch {
			if message, ok := decode(raw); ok && check(message) {
				return true
			}
		}
		return false
	}
	message, ok := decode(body)
	return ok && check(message)
}

// readBody buffers a POST body, enforcing the Python service limit. A declared
// Content-Length over the limit is rejected without reading.
func readBody(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	if limit > 0 && r.ContentLength > limit {
		return nil, errBodyTooLarge
	}
	if limit <= 0 {
		return io.ReadAll(r.Body)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errBodyTooLarge
	}
	return body, nil
}

func restoreBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	r.TransferEncoding = nil
}

// errBodyTooLarge marks an oversized MCP request body (413).
var errBodyTooLarge = errors.New("request body too large")

// validContentType ports TransportSecurityMiddleware._validate_content_type.
func validContentType(value string) bool {
	return value != "" && strings.HasPrefix(strings.ToLower(value), "application/json")
}

// matchesAllowlist ports the Python wildcard-port matching: an exact value or a
// value that starts with "<base>:" for a pattern ending in ":*".
func matchesAllowlist(value string, patterns []string) bool {
	if value == "" {
		return false
	}
	if len(patterns) == 0 {
		return false
	}
	for _, pattern := range patterns {
		if pattern == value {
			return true
		}
		if strings.HasSuffix(pattern, ":*") {
			base := strings.TrimSuffix(pattern, ":*")
			if strings.HasPrefix(value, base+":") {
				return true
			}
		}
	}
	return false
}

// writeAuthError maps the namespace callback error to the HTTP contract.
func (h *Handler) writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrForbidden):
		writeJSONDetail(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrInvalidNamespace):
		writeJSONDetail(w, http.StatusBadRequest, err.Error())
	default:
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSONDetail(w, http.StatusUnauthorized, errDetail(err))
	}
}

func errDetail(err error) string {
	if err == nil || err.Error() == "" {
		return ErrUnauthorized.Error()
	}
	return err.Error()
}

// writeJSONDetail writes FastAPI's {"detail": ...} envelope without HTML
// escaping, so localized messages survive byte-for-byte.
func writeJSONDetail(w http.ResponseWriter, status int, detail string) {
	payload, err := json.Marshal(map[string]string{"detail": detail})
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// writePlain writes Starlette's text/plain error response.
func writePlain(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message)
}
