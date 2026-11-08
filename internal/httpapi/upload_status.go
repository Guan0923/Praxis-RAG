package httpapi

import (
	"context"
	"net/http"
	"regexp"
)

var uploadOperationPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// UploadStatusRuntime exposes display-only upload/version/lease read models.
type UploadStatusRuntime interface {
	UploadOperations(ctx context.Context, namespace string) ([]map[string]any, error)
	UploadOperation(ctx context.Context, namespace, operationID string) (map[string]any, bool, error)
	PublishedVersions(ctx context.Context, namespace string) (map[string]string, error)
	UploadLeases(ctx context.Context, namespace string) ([]map[string]any, error)
}

func (s *Server) handleUploadStatusList(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	operations, err := s.runtime.UploadOperations(r.Context(), namespace)
	if err != nil {
		writeRuntimeError(w, err, true)
		return
	}
	if operations == nil {
		operations = []map[string]any{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"operations": operations, "limit": 100, "display_only": true,
	})
}

func (s *Server) handleUploadStatusDetail(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	operationID := pathParam(r, "operation_id")
	if !uploadOperationPattern.MatchString(operationID) {
		writeDetail(w, http.StatusNotFound, "Upload operation does not exist")
		return
	}
	operation, found, err := s.runtime.UploadOperation(r.Context(), namespace, operationID)
	if err != nil {
		writeRuntimeError(w, err, true)
		return
	}
	if !found {
		writeDetail(w, http.StatusNotFound, "Upload operation does not exist")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"operation": operation, "display_only": true})
}

// adminTenant enforces both the local-management boundary and tenant
// credentials (Python's published-versions/upload-leases dependencies).
func (s *Server) adminTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.requireLocalAdmin(w, r) {
		return "", false
	}
	return s.tenantFromHeaders(w, r)
}

func (s *Server) handlePublishedVersions(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.adminTenant(w, r)
	if !ok {
		return
	}
	versions, err := s.runtime.PublishedVersions(r.Context(), namespace)
	if err != nil {
		writeRuntimeError(w, err, true)
		return
	}
	if versions == nil {
		versions = map[string]string{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions, "display_only": true})
}

func (s *Server) handleUploadLeases(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.adminTenant(w, r)
	if !ok {
		return
	}
	leases, err := s.runtime.UploadLeases(r.Context(), namespace)
	if err != nil {
		writeRuntimeError(w, err, true)
		return
	}
	if leases == nil {
		leases = []map[string]any{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"leases": leases, "display_only": true})
}
