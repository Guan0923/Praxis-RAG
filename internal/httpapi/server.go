package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/config"
)

// StatusProvider renders the secret-free service health payload.
type StatusProvider interface {
	Status(ctx context.Context) map[string]any
}

// Server hosts the REST API, MCP endpoint, and Console static assets.
type Server struct {
	settings config.ServiceSettings
	status   StatusProvider
	keys     KeyLookup
	logger   *slog.Logger
	router   *router
	webRoot  string
	handler  http.Handler
}

// New builds the HTTP server. A nil StatusProvider falls back to a static
// healthy payload; a nil KeyLookup disables API-key authentication.
func New(settings config.ServiceSettings, status StatusProvider, keys KeyLookup, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if status == nil {
		status = staticStatus{}
	}
	server := &Server{
		settings: settings,
		status:   status,
		keys:     keys,
		logger:   logger,
		router:   newRouter(),
		webRoot:  resolveWebRoot(settings.WebRoot),
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
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logger.Error("http handler panicked",
				"error", fmt.Sprint(recovered),
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
		namespace, ok := s.tenantFromHeaders(recorder, r)
		if !ok {
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), namespaceContextKey{}, namespace))
	case authAdmin:
		if !s.requireLocalAdmin(recorder, r) {
			return
		}
	}

	result.route.handler(recorder, r)
}

func (s *Server) logRequest(r *http.Request, recorder *statusRecorder, started time.Time) {
	s.logger.Info("http request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", recorder.status,
		"duration_ms", float64(time.Since(started).Microseconds())/1000.0,
	)
}

type namespaceContextKey struct{}

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
	return strings.HasPrefix(path, "/v1/search-results/") || strings.HasPrefix(path, "/v1/artifacts/")
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
		filepath.Join("src", "rag_mcp", "web"),
		"web",
	}
	if executable, err := os.Executable(); err == nil {
		executableDir := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(executableDir, "web"),
			filepath.Join(executableDir, "..", "src", "rag_mcp", "web"),
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
