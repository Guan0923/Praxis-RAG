package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/Guan0923/xinghe_rag/internal/catalog"
)

// ToolRegistry exposes the registered MCP tools and their built-in defaults.
// The MCP package implements it; a nil registry disables the admin endpoints.
type ToolRegistry interface {
	Configured(namespace string) ([]map[string]any, error)
	Defaults(namespace, name string) (catalog.ToolSettings, bool)
}

// WithToolRegistry enables /v1/admin/tools endpoints.
func WithToolRegistry(registry ToolRegistry) Option {
	return func(s *Server) { s.tools = registry }
}

// toolPatchBody tracks which fields were present so JSON null can be rejected
// like pydantic's strict model validator.
type toolPatchBody struct {
	Enabled     json.RawMessage `json:"enabled"`
	Description json.RawMessage `json:"description"`
}

func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.namespaceHeader(w, r)
	if !ok {
		return
	}
	if s.tools == nil {
		writeDetail(w, http.StatusServiceUnavailable, "MCP tool registry is not available")
		return
	}
	tools, err := s.tools.Configured(namespace)
	if err != nil {
		writeDetail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools})
}

func (s *Server) handlePatchTool(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.namespaceHeader(w, r)
	if !ok {
		return
	}
	name := pathParam(r, "name")
	if s.tools == nil {
		writeDetail(w, http.StatusServiceUnavailable, "MCP tool registry is not available")
		return
	}
	defaults, known := s.tools.Defaults(namespace, name)
	if !known {
		writeDetail(w, http.StatusNotFound, "Unknown registered tool.")
		return
	}
	var body toolPatchBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	patch, details := parseToolPatch(body)
	if details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	catalogForNamespace, ok := s.catalogFor(namespace)
	if !ok {
		writeDetail(w, http.StatusServiceUnavailable, "MCP configuration cannot be saved")
		return
	}
	changed, err := catalogForNamespace.PatchTool(r.Context(), name, patch, defaults)
	if err != nil {
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	if changed && s.notifier != nil {
		s.notifier.ToolsChanged(namespace)
	}
	tools, err := s.tools.Configured(namespace)
	if err != nil {
		writeDetail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	var selected map[string]any
	for _, tool := range tools {
		if tool["name"] == name {
			selected = tool
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tool": selected, "changed": changed})
}

func parseToolPatch(body toolPatchBody) (catalog.ToolPatch, []validationDetail) {
	patch := catalog.ToolPatch{}
	if len(body.Enabled) == 0 && len(body.Description) == 0 {
		return patch, []validationDetail{{
			Type: "value_error", Loc: []any{"body"},
			Msg: "Provide enabled and/or description; null is not accepted",
		}}
	}
	if len(body.Enabled) > 0 {
		if bytes.Equal(bytes.TrimSpace(body.Enabled), []byte("null")) {
			return patch, []validationDetail{{
				Type: "value_error", Loc: []any{"body", "enabled"},
				Msg: "Provide enabled and/or description; null is not accepted",
			}}
		}
		var enabled bool
		if err := json.Unmarshal(body.Enabled, &enabled); err != nil {
			return patch, []validationDetail{{
				Type: "bool_type", Loc: []any{"body", "enabled"},
				Msg: "Input should be a valid boolean",
			}}
		}
		patch.Enabled = &enabled
	}
	if len(body.Description) > 0 {
		if bytes.Equal(bytes.TrimSpace(body.Description), []byte("null")) {
			return patch, []validationDetail{{
				Type: "value_error", Loc: []any{"body", "description"},
				Msg: "Provide enabled and/or description; null is not accepted",
			}}
		}
		var description string
		if err := json.Unmarshal(body.Description, &description); err != nil {
			return patch, []validationDetail{{
				Type: "string_type", Loc: []any{"body", "description"},
				Msg: "Input should be a valid string",
			}}
		}
		if len([]rune(description)) > 8000 {
			return patch, []validationDetail{{
				Type: "string_too_long", Loc: []any{"body", "description"},
				Msg: "String should have at most 8000 characters",
			}}
		}
		patch.Description = &description
	}
	return patch, nil
}
