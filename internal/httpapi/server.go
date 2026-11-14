package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/catalog"
	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/logging"
)

// StatusProvider renders the secret-free service health payload.
type StatusProvider interface {
	Status(ctx context.Context) map[string]any
}

// Option customizes the HTTP server.
type Option func(*Server)

// WithCatalogStore enables the MCP catalog administration endpoints.
func WithCatalogStore(store catalog.Store) Option {
	return func(s *Server) { s.catalog = store }
}

// WithUserAuth enables the Console login/registration flow and the
// user-management endpoints. Without it the server keeps the legacy
// token/loopback behaviour.
func WithUserAuth(users UserAuth) Option {
	return func(s *Server) { s.users = users }
}

// WithAccountProfile enables the personal-profile endpoints (e-mail
// registration, avatar, password and mail-verified e-mail changes). It is kept
// separate from UserAuth so existing fakes of UserAuth keep working.
func WithAccountProfile(profile AccountProfile) Option {
	return func(s *Server) { s.account = profile }
}

// WithMailAdmin enables the administrator SMTP settings endpoints.
func WithMailAdmin(admin MailAdmin) Option {
	return func(s *Server) { s.mailAdmin = admin }
}

// ChangeNotifier receives catalog mutations so the MCP layer can publish
// tools/list_changed and prompts/list_changed notifications, plus document
// mutations for resources/list_changed and resources/updated.
type ChangeNotifier interface {
	PromptsChanged(namespace string)
	ToolsChanged(namespace string)
}

// DocumentChangeNotifier receives document mutations so the MCP layer can
// publish resources/list_changed and resources/updated notifications.
type DocumentChangeNotifier interface {
	DocumentsChanged(namespace string, before, after []map[string]any)
}

// WithDocumentNotifier wires document-change notifications.
func WithDocumentNotifier(notifier DocumentChangeNotifier) Option {
	return func(s *Server) { s.docNotifier = notifier }
}

// WithChangeNotifier wires MCP change notifications.
func WithChangeNotifier(notifier ChangeNotifier) Option {
	return func(s *Server) { s.notifier = notifier }
}

// Server hosts the REST API, MCP endpoint, and Console static assets.
type Server struct {
	settings       config.ServiceSettings
	status         StatusProvider
	keys           KeyLookup
	keyStore       APIKeyStore
	runtime        Runtime
	mcpHandler     http.Handler
	logger         *slog.Logger
	router         *router
	webRoot        string
	catalog        catalog.Store
	tools          ToolRegistry
	kbAdmin        KnowledgeBaseAdminRuntime
	kbSelfCreate   KnowledgeBaseSelfCreateRuntime
	modelProfiles  ModelProfileRuntime
	monitor        MonitorRuntime
	notifier       ChangeNotifier
	users          UserAuth
	account        AccountProfile
	mailAdmin      MailAdmin
	access         AccessControl
	limiter        *loginLimiter
	accountLimiter *rateLimiter
	docNotifier    DocumentChangeNotifier
	uploadEvents   *uploadEventHub
	handler        http.Handler
}

// New builds the HTTP server. A nil StatusProvider falls back to a static
// healthy payload; a nil KeyLookup disables API-key authentication.
func New(settings config.ServiceSettings, status StatusProvider, keys KeyLookup, logger *slog.Logger, options ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if status == nil {
		status = staticStatus{}
	}
	server := &Server{
		settings:       settings,
		status:         status,
		keys:           keys,
		logger:         logger,
		router:         newRouter(),
		limiter:        newLoginLimiter(),
		accountLimiter: newRateLimiter(),
		webRoot:        resolveWebRoot(settings.WebRoot),
		uploadEvents:   newUploadEventHub(),
	}
	for _, option := range options {
		option(server)
	}
	server.registerRoutes()
	server.handler = server.buildHandler()
	return server
}

// WebRoot reports the resolved Console asset directory ("" when unavailable).
func (s *Server) WebRoot() string { return s.webRoot }

// Handler returns the composed HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) registerRoutes() {
	s.router.add("GET", "/health", authNone, s.handleHealth)
	s.router.add("GET", "/v1/chunking/strategies", authTenant, s.handleChunkingStrategies)
	s.router.add("GET", "/v1/admin/knowledge-bases/defaults", authAdmin, s.handleKnowledgeBaseDefaults)
	s.router.add("GET", "/v1/admin/prompts", authAdmin, s.handleListPrompts)
	s.router.add("PUT", "/v1/admin/prompts/{name}", authAdmin, s.handlePutPrompt)
	s.router.add("DELETE", "/v1/admin/prompts/{name}", authAdmin, s.handleDeletePrompt)
	s.router.add("GET", "/v1/admin/tools", authAdmin, s.handleListTools)
	s.router.add("PATCH", "/v1/admin/tools/{name}", authAdmin, s.handlePatchTool)
	s.router.add("GET", "/v1/admin/api-keys", authSessionOrLocal, s.handleListAPIKeys)
	s.router.add("POST", "/v1/admin/api-keys", authSessionOrLocal, s.handleCreateAPIKey)
	s.router.add("POST", "/v1/admin/api-keys/batch-delete", authSessionOrLocal, s.handleBatchDeleteAPIKeys)
	s.router.add("PUT", "/v1/admin/api-keys/{key_id}", authSessionOrLocal, s.handleUpdateAPIKey)
	s.router.add("DELETE", "/v1/admin/api-keys/{key_id}", authSessionOrLocal, s.handleDeleteAPIKey)
	s.router.add("GET", "/v1/admin/api-keys/{key_id}/mcp-config", authSessionOrLocal, s.handleAPIKeyMCPConfig)
	s.router.add("POST", "/v1/search", authTenant, s.handleSearch)
	s.router.add("GET", "/v1/artifacts/{artifact_id}", authTenant, s.handleDownloadArtifact)
	s.router.add("GET", "/v1/artifacts/{artifact_id}/content", authTenant, s.handleGetArtifactContent)
	s.router.add("GET", "/v1/documents", authTenant, s.handleListDocuments)
	s.router.add("POST", "/v1/documents/check", authTenant, s.handleCheckDocuments)
	s.router.add("GET", "/v1/document-jobs/{artifact_id}", authTenant, s.handleDocumentJobStatus)
	s.router.add("POST", "/v1/document-batches", authTenant, s.handleCreateDocumentBatch)
	s.router.add("GET", "/v1/document-batches", authTenant, s.handleListDocumentBatches)
	s.router.add("GET", "/v1/document-batches/upload-events/{upload_id}", authTenant, s.handleUploadEvents)
	s.router.add("GET", "/v1/document-batches/{batch_id}", authTenant, s.handleDocumentBatchStatus)
	s.router.add("GET", "/v1/documents/{name}/content", authTenant, s.handleDocumentContent)
	s.router.add("DELETE", "/v1/documents/{name}", authTenant, s.handleDeleteDocument)
	s.router.add("GET", "/v1/knowledge-base", authTenant, s.handleKnowledgeBase)
	s.router.add("GET", "/v1/namespaces", authTenantList, s.handleNamespaceList)
	s.router.add("GET", "/v1/uploads", authTenant, s.handleUploadStatusList)
	s.router.add("GET", "/v1/uploads/{operation_id}", authTenant, s.handleUploadStatusDetail)
	s.router.add("GET", "/v1/admin/published-versions", authNone, s.handlePublishedVersions)
	s.router.add("GET", "/v1/admin/upload-leases", authNone, s.handleUploadLeases)
	s.router.add("GET", "/v1/admin/monitor/overview", authAdmin, s.handleMonitorOverview)
	s.router.add("GET", "/v1/admin/monitor/logs", authAdmin, s.handleMonitorLogs)
	s.registerDocumentationRoutes()
	s.registerKnowledgeBaseAdminRoutes()
	s.registerModelProfileRoutes()
	s.registerUserAuthRoutes()
	s.registerProfileRoutes()
	s.registerAccessRoutes()
	s.router.add("POST", "/mcp", authMCP, s.handleMCP)
	s.router.add("GET", "/mcp", authMCP, s.handleMCP)
	s.router.add("DELETE", "/mcp", authMCP, s.handleMCP)

	if s.webRoot != "" {
		s.router.add("GET", "/", authNone, s.handleConsoleIndex)
		s.router.add("GET", "/console", authNone, s.handleConsoleIndex)
		s.router.add("GET", "/console/assets/*", authNone, s.handleConsoleAsset)
	}
}

func (s *Server) buildHandler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	started := time.Now()
	// Every request carries one correlation id: a validated inbound value (proxy
	// or tests) or a generated one. Handlers annotate the same context with the
	// durable operation/artifact/batch ids they create, so one upload can be
	// followed from this access line into the worker and the retry history.
	requestID := requestIDFor(r)
	w.Header().Set(requestIDHeader, requestID)
	r = r.WithContext(logging.With(r.Context(), logging.FieldRequestID, requestID))
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logger.Error("http handler panicked",
				"error_detail", logging.RedactLimit(fmt.Sprint(recovered), 300),
				"method", r.Method,
				"path", r.URL.Path,
				"stack", string(debug.Stack()),
			)
			if !recorder.wroteHeader {
				writeDetail(recorder, http.StatusInternalServerError, "Internal Server Error")
			}
		}
		s.logRequest(r, recorder, started)
	}()

	if isSnapshotPath(r.URL.Path) {
		recorder.Header().Set("Cache-Control", "no-store")
	}

	// Python's StaticFiles never serves directory listings: reject directory
	// paths (including the assets root) instead of letting http.FileServer render
	// HTML listings or fall back to webRoot/index.html.
	if s.webRoot != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		strings.HasPrefix(r.URL.Path, "/console/assets") && !consoleAssetFile(s.webRoot, r.URL.Path) {
		writeDetail(recorder, http.StatusNotFound, "Not Found")
		return
	}

	result, pathMatched, allowed := s.router.match(r.Method, r.URL.Path)
	if result.route == nil {
		if !pathMatched {
			writeDetail(recorder, http.StatusNotFound, "Not Found")
			return
		}
		recorder.Header().Set("Allow", strings.Join(allowed, ", "))
		writeDetail(recorder, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}

	switch result.route.policy {
	case authTenant:
		identity, ok := s.tenantFromRequest(recorder, r)
		if !ok {
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), namespaceContextKey{}, identity.namespace))
		if identity.session != nil {
			// Login sessions are subject to knowledge-base ownership/permission
			// filtering; API keys, the service token and session-less loopback
			// requests stay unrestricted.
			r = r.WithContext(context.WithValue(r.Context(), sessionUserContextKey{}, identity.session))
			if r = s.withAccessScope(recorder, r, identity.session.User, identity.namespace); r == nil {
				return
			}
		} else if identity.keyNamespaces != nil {
			// API keys keep their bound namespace set in the request context so
			// list endpoints such as /v1/namespaces can narrow the merged list
			// instead of exposing every knowledge base to the credential.
			r = r.WithContext(context.WithValue(r.Context(), apiKeyNamespacesContextKey{}, identity.keyNamespaces))
		}
	case authTenantList:
		// GET /v1/namespaces is namespace-agnostic: it only needs the caller's
		// credential identity, because it reports that credential's own scope.
		// A stale/revoked remembered X-RAG-Namespace must not block it, or an
		// API key rotated to new namespaces could never recover in the Console.
		// The handler filters by session/API-key itself, so no access scope is
		// installed here.
		identity, ok := s.tenantFromListingRequest(recorder, r)
		if !ok {
			return
		}
		if identity.session != nil {
			r = r.WithContext(context.WithValue(r.Context(), sessionUserContextKey{}, identity.session))
		} else if identity.keyNamespaces != nil {
			r = r.WithContext(context.WithValue(r.Context(), apiKeyNamespacesContextKey{}, identity.keyNamespaces))
		}
	case authSession:
		// The sharing endpoints always act for one Console account and never use
		// the loopback fallback; writes additionally require the CSRF header.
		recorder.Header().Set("Cache-Control", "no-store")
		session, ok := s.requireSession(recorder, r)
		if !ok {
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), sessionUserContextKey{}, session))
	case authSessionOrLocal:
		// API-key management is the one route family every signed-in account
		// uses for its own credentials. A session of any role is accepted (CSRF
		// is enforced for cookie-authenticated writes); without a session it
		// keeps the trusted local management break-glass used by automation.
		recorder.Header().Set("Cache-Control", "no-store")
		if session, viaCookie := s.requestSessionUser(r); session != nil {
			if !s.validateCSRF(r, session, viaCookie) {
				writeDetail(recorder, http.StatusForbidden, "CSRF 校验失败，请刷新页面后重试")
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), sessionUserContextKey{}, session))
			break
		}
		if !localModelRequest(r) {
			writeDetail(recorder, http.StatusForbidden, "仅允许本机直接访问或登录后访问")
			return
		}
	case authAdmin:
		if !s.requireAdmin(recorder, r) {
			return
		}
	case authMCP:
		// MCP requires credentials even on loopback; the protocol endpoint
		// answers 401 with a Bearer challenge instead of the local-admin
		// fallback used by the REST tenant routes.
		if r.Header.Get("Authorization") == "" {
			recorder.Header().Set("WWW-Authenticate", "Bearer")
			writeDetail(recorder, http.StatusUnauthorized, "MCP 请求必须提供 Authorization: Bearer <API_KEY>")
			return
		}
		identity, ok := s.tenantFromRequest(recorder, r)
		if !ok {
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), namespaceContextKey{}, identity.namespace))
		if identity.session != nil {
			// A session bearer token resolves the same scope REST uses, and the
			// MCP layer reads it through access.ScopeFromContext.
			if r = s.withAccessScope(recorder, r, identity.session.User, identity.namespace); r == nil {
				return
			}
		}
	}
	if len(result.params) > 0 {
		r = r.WithContext(context.WithValue(r.Context(), pathParamsContext{}, result.params))
	}

	result.route.handler(recorder, r)
}

func (s *Server) logRequest(r *http.Request, recorder *statusRecorder, started time.Time) {
	logger := s.logger.With(logging.Attrs(r.Context())...)
	fields := []any{
		"method", r.Method,
		"path", r.URL.Path,
		"status", recorder.status,
		"duration_ms", float64(time.Since(started).Microseconds()) / 1000.0,
	}
	if namespace, ok := NamespaceFromContext(r.Context()); ok && namespace != "" {
		fields = append(fields, logging.FieldNamespace, namespace)
	}
	logger.Info("http request", fields...)
}

// requestIDHeader is the inbound/outbound request correlation header.
const requestIDHeader = "X-Request-ID"

// requestIDPattern admits conservative proxy/test ids only: bounded length and
// no whitespace or control characters, so a header value can never forge a log
// record.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// requestIDFor reuses a valid inbound X-Request-ID or generates a 128-bit hex
// id. Generated ids are opaque and carry no user data.
func requestIDFor(r *http.Request) string {
	if candidate := strings.TrimSpace(r.Header.Get(requestIDHeader)); requestIDPattern.MatchString(candidate) {
		return candidate
	}
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		// crypto/rand only fails in broken environments; a timestamp fallback
		// still yields a unique-enough correlation id without failing the request.
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer[:])
}

// namespaceContextKey carries the authenticated tenant namespace.
type namespaceContextKey struct{}

// tenantNamespace returns the namespace authenticated by the route policy.
func (s *Server) tenantNamespace(w http.ResponseWriter, r *http.Request) (string, bool) {
	namespace, ok := NamespaceFromContext(r.Context())
	if !ok {
		writeDetail(w, http.StatusUnauthorized, "RAG service authentication failed.")
		return "", false
	}
	return namespace, true
}

// pathParamsContext carries router path parameters to handlers.
type pathParamsContext struct{}

// NamespaceFromContext returns the authenticated tenant namespace.
func NamespaceFromContext(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(namespaceContextKey{}).(string)
	return value, ok
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	payload := s.status.Status(r.Context())
	if payload == nil {
		payload = map[string]any{"status": "ok"}
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleChunkingStrategies(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, chunking.StrategyCatalog())
}

func (s *Server) handleKnowledgeBaseDefaults(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, chunking.StrategyCatalog())
}

type staticStatus struct{}

func (staticStatus) Status(context.Context) map[string]any {
	return map[string]any{"status": "ok"}
}

// isSnapshotPath reports paths that must never be cached, mirroring
// _SnapshotNoStoreMiddleware.
func isSnapshotPath(path string) bool {
	if path == "/mcp" || path == "/v1/search" {
		return true
	}
	return strings.HasPrefix(path, "/v1/artifacts/")
}

// consoleAssetFile reports whether the request maps to a regular file under the
// Console web root (not a directory, not outside the root).
func consoleAssetFile(webRoot, requestPath string) bool {
	relative := strings.TrimPrefix(requestPath, "/console/assets")
	relative = strings.TrimPrefix(relative, "/")
	if relative == "" || strings.Contains(relative, "..") {
		return false
	}
	root, err := filepath.Abs(webRoot)
	if err != nil {
		return false
	}
	target, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil || target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return false
	}
	info, err := os.Stat(target)
	return err == nil && !info.IsDir()
}

// resolveWebRoot locates the built Console. An explicit setting wins; otherwise
// the repository layout and the executable directory are probed.
func resolveWebRoot(configured string) string {
	if configured != "" {
		root := config.ExpandPath(configured)
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			return root
		}
		return ""
	}
	candidates := []string{
		filepath.Join("build", "web"),
		"web",
	}
	if executable, err := os.Executable(); err == nil {
		executableDir := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(executableDir, "web"),
			filepath.Join(executableDir, "..", "web"),
			filepath.Join(executableDir, "..", "build", "web"),
		)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(filepath.Join(candidate, "index.html")); err == nil && !info.IsDir() {
			absolute, absErr := filepath.Abs(candidate)
			if absErr == nil {
				return absolute
			}
			return candidate
		}
	}
	return ""
}

// statusRecorder captures the response status for request logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(data)
}

// Flush forwards streaming flushes when the underlying writer supports them.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// writeJSON writes an application/json response the way Starlette's
// JSONResponse does (UTF-8 preserved, no trailing newline).
func writeJSON(w http.ResponseWriter, status int, payload any) {
	buffer := &bytes.Buffer{}
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		writeDetail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	body := bytes.TrimRight(buffer.Bytes(), "\n")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeDetail writes FastAPI's {"detail": ...} error envelope.
func writeDetail(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
