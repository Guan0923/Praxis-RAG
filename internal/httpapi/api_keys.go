package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// APIKeyStore is the credential facade implemented by internal/apikeys.Store.
type APIKeyStore interface {
	KeyLookup
	List(ctx context.Context) ([]state.APIKey, error)
	Create(ctx context.Context, name string, namespaces []string) (state.CreatedAPIKey, error)
	Update(ctx context.Context, keyID, name string, namespaces []string, enabled bool) (*state.APIKey, error)
	Delete(ctx context.Context, keyID string) (bool, error)
	SecretForNamespace(ctx context.Context, keyID, namespace string) (string, error)
}

// WithAPIKeyStore enables the stored-credential authentication and admin API.
func WithAPIKeyStore(store APIKeyStore) Option {
	return func(s *Server) {
		s.keyStore = store
		if store != nil {
			s.keys = store
		}
	}
}

type apiKeyBody struct {
	Name       string    `json:"name"`
	Namespaces *[]string `json:"namespaces"`
}

type apiKeyUpdateBody struct {
	Name       *string   `json:"name"`
	Namespaces *[]string `json:"namespaces"`
	Enabled    *bool     `json:"enabled"`
}

func cleanAPIKeyName(name string) (string, []validationDetail) {
	if name == "" {
		return "", []validationDetail{{Type: "string_too_short", Loc: []any{"body", "name"}, Msg: "String should have at least 1 character"}}
	}
	if len([]rune(name)) > 80 {
		return "", []validationDetail{{Type: "string_too_long", Loc: []any{"body", "name"}, Msg: "String should have at most 80 characters"}}
	}
	trimmed := trimSpace(name)
	if trimmed == "" {
		return "", []validationDetail{{Type: "value_error", Loc: []any{"body", "name"}, Msg: "请输入密钥名称"}}
	}
	return trimmed, nil
}

func cleanNamespaces(namespaces *[]string) ([]string, []validationDetail) {
	if namespaces == nil {
		return nil, []validationDetail{{Type: "missing", Loc: []any{"body", "namespaces"}, Msg: "Field required"}}
	}
	values := *namespaces
	if len(values) < 1 {
		return nil, []validationDetail{{Type: "too_short", Loc: []any{"body", "namespaces"}, Msg: "List should have at least 1 item after validation, not 0"}}
	}
	if len(values) > 100 {
		return nil, []validationDetail{{Type: "too_long", Loc: []any{"body", "namespaces"}, Msg: "List should have at most 100 items after validation"}}
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !namespacePattern.MatchString(value) {
			return nil, []validationDetail{{Type: "value_error", Loc: []any{"body", "namespaces"}, Msg: "Namespace 格式无效"}}
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func trimSpace(value string) string {
	start := 0
	end := len(value)
	for start < end && isSpaceByte(value[start]) {
		start++
	}
	for end > start && isSpaceByte(value[end-1]) {
		end--
	}
	return value[start:end]
}

func isSpaceByte(character byte) bool {
	switch character {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	if s.keyStore == nil {
		writeDetail(w, http.StatusServiceUnavailable, "API key store is not available")
		return
	}
	keys, err := s.keyStore.List(r.Context())
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "API Key 读取失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if s.keyStore == nil {
		writeDetail(w, http.StatusServiceUnavailable, "API key store is not available")
		return
	}
	var body apiKeyBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	name, details := cleanAPIKeyName(body.Name)
	if details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	namespaces, details := cleanNamespaces(body.Namespaces)
	if details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	created, err := s.keyStore.Create(r.Context(), name, namespaces)
	if err != nil {
		writeDetail(w, http.StatusConflict, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateAPIKey(w http.ResponseWriter, r *http.Request) {
	if s.keyStore == nil {
		writeDetail(w, http.StatusServiceUnavailable, "API key store is not available")
		return
	}
	keyID := pathParam(r, "key_id")
	var body apiKeyUpdateBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	var details []validationDetail
	if body.Name == nil {
		details = append(details, validationDetail{Type: "missing", Loc: []any{"body", "name"}, Msg: "Field required"})
	}
	if body.Enabled == nil {
		details = append(details, validationDetail{Type: "missing", Loc: []any{"body", "enabled"}, Msg: "Field required"})
	}
	if len(details) > 0 {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	name, nameDetails := cleanAPIKeyName(*body.Name)
	if nameDetails != nil {
		writeValidation(w, http.StatusUnprocessableEntity, nameDetails)
		return
	}
	namespaces, namespaceDetails := cleanNamespaces(body.Namespaces)
	if namespaceDetails != nil {
		writeValidation(w, http.StatusUnprocessableEntity, namespaceDetails)
		return
	}
	item, err := s.keyStore.Update(r.Context(), keyID, name, namespaces, *body.Enabled)
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "API Key 更新失败")
		return
	}
	if item == nil {
		writeDetail(w, http.StatusNotFound, "API Key 不存在")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if s.keyStore == nil {
		writeDetail(w, http.StatusServiceUnavailable, "API key store is not available")
		return
	}
	deleted, err := s.keyStore.Delete(r.Context(), pathParam(r, "key_id"))
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "API Key 删除失败")
		return
	}
	if !deleted {
		writeDetail(w, http.StatusNotFound, "API Key 不存在")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIKeyMCPConfig(w http.ResponseWriter, r *http.Request) {
	if s.keyStore == nil {
		writeDetail(w, http.StatusServiceUnavailable, "API key store is not available")
		return
	}
	namespace, ok := s.namespaceHeader(w, r)
	if !ok {
		return
	}
	secret, err := s.keyStore.SecretForNamespace(r.Context(), pathParam(r, "key_id"), namespace)
	if err != nil {
		status, detail := apiKeyConfigError(err)
		writeDetail(w, status, detail)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"url": scheme + "://" + r.Host + "/mcp",
		"headers": map[string]string{
			"Authorization":   "Bearer " + secret,
			"X-RAG-Namespace": namespace,
		},
	})
}

func apiKeyConfigError(err error) (int, string) {
	var lookup *state.LookupError
	if errors.As(err, &lookup) {
		return http.StatusNotFound, lookup.Message
	}
	var permission *state.PermissionError
	if errors.As(err, &permission) {
		return http.StatusForbidden, permission.Message
	}
	var valueError *state.ValueError
	if errors.As(err, &valueError) {
		return http.StatusConflict, valueError.Message
	}
	return http.StatusInternalServerError, "API Key 读取失败"
}
