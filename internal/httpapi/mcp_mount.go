package httpapi

import (
	"errors"
	"net/http"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/chunking"
)

// WithMCPHandler mounts the MCP Streamable HTTP handler on /mcp. The handler is
// dispatched only after the authMCP policy has authenticated the tenant, and it
// reads the namespace from the request context.
func WithMCPHandler(handler http.Handler) Option {
	return func(s *Server) { s.mcpHandler = handler }
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if s.mcpHandler == nil {
		writeDetail(w, http.StatusServiceUnavailable, "MCP service is not running")
		return
	}
	s.mcpHandler.ServeHTTP(w, r)
}

func (s *Server) handleKnowledgeBase(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	payload, err := s.runtime.PublicKnowledgeBase(r.Context(), namespace)
	if err != nil {
		var validation *apierrors.KnowledgeValidationError
		if errors.As(err, &validation) {
			writeDetail(w, http.StatusNotFound, validation.Message)
			return
		}
		writeRuntimeError(w, err, false)
		return
	}
	result := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		result[key] = value
	}
	result["strategies"] = chunking.StrategyCatalog().Strategies
	writeJSON(w, http.StatusOK, result)
}
