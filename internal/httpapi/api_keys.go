package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/Guan0923/xinghe_rag/internal/auth"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// APIKeyStore is the credential facade implemented by internal/apikeys.Store.
type APIKeyStore interface {
	KeyLookup
	List(ctx context.Context) ([]state.APIKey, error)
	ListByOwner(ctx context.Context, ownerUserID string) ([]state.APIKey, error)
	Get(ctx context.Context, keyID string) (*state.APIKey, error)
	Create(ctx context.Context, name string, namespaces []string) (state.CreatedAPIKey, error)
	CreateForOwner(ctx context.Context, name string, namespaces []string, ownerUserID string) (state.CreatedAPIKey, error)
	Update(ctx context.Context, keyID, name string, namespaces []string, enabled bool) (*state.APIKey, error)
	Delete(ctx context.Context, keyID string) (bool, error)
	// DeleteMany removes several credentials inside one transaction. When
	// ownerUserID is non-empty the batch only affects that account, and ids it
	// may not delete land in the missing bucket.
	DeleteMany(ctx context.Context, keyIDs []string, ownerUserID string) ([]string, []string, error)
	SecretForNamespace(ctx context.Context, keyID, namespace string) (string, error)
}

// WithAPIKeyStore enables the stored-credential authentication and the per-account
// API-key management API.
func WithAPIKeyStore(store APIKeyStore) Option {
	return func(s *Server) {
		s.keyStore = store
		if store != nil {
			s.keys = store
		}
	}
}

// Messages of the per-account credential rules. They stay stable so the Console
// can show them verbatim.
const (
	messageAPIKeyNotFound        = "API Key 不存在"
	messageAPIKeyNamespaceDenied = "只能为本人创建的知识库创建 API Key"
)

// apiKeyCaller returns the signed-in account of an authSessionOrLocal request,
// or nil for the trusted local management surface.
func apiKeyCaller(r *http.Request) *auth.SessionUser {
	session, _ := sessionUserFromContext(r.Context())
	return session
}

// apiKeyCallerIsAdmin reports whether the caller may manage every account's
// keys: a session administrator, or the trusted local management surface.
func apiKeyCallerIsAdmin(r *http.Request) bool {
	session := apiKeyCaller(r)
	return session == nil || session.User.IsAdmin()
}

// apiKeyNewNamespaces returns the requested namespaces the existing key does not
// already carry. Only an addition needs the ownership check: a key issued while
// the caller was an administrator may still bind foreign knowledge bases, and
// its owner must be able to rename, disable or delete it after a demotion.
func apiKeyNewNamespaces(existing *state.APIKey, requested []string) []string {
	if existing == nil {
		return requested
	}
	current := make(map[string]struct{}, len(existing.Namespaces))
	for _, namespace := range existing.Namespaces {
		current[namespace] = struct{}{}
	}
	added := make([]string, 0, len(requested))
	for _, namespace := range requested {
		if _, ok := current[namespace]; !ok {
			added = append(added, namespace)
		}
	}
	return added
}

// apiKeyCallerOwns reports whether one key belongs to the caller. It is only
// consulted for non-administrator sessions; existence of a foreign key is
// indistinguishable from a missing one on purpose.
func apiKeyCallerOwns(r *http.Request, key *state.APIKey) bool {
	session := apiKeyCaller(r)
	if session == nil || session.User.IsAdmin() {
		return true
	}
	return key != nil && key.OwnerUserID != "" && key.OwnerUserID == session.User.ID
}

// apiKeyNamespacesAllowed enforces the credential binding rule for normal
// accounts: an API key bypasses knowledge-base/document authorization, so a
// session account may only bind knowledge bases it owns. Administrators and the
// local management surface are unrestricted. The rule fails closed when the
// sharing service is not wired: silently skipping it would turn a missing
// dependency into a privilege escalation.
func (s *Server) apiKeyNamespacesAllowed(w http.ResponseWriter, r *http.Request, namespaces []string) bool {
	session := apiKeyCaller(r)
	if session == nil || session.User.IsAdmin() {
		return true
	}
	if s.access == nil {
		writeDetail(w, http.StatusInternalServerError, "知识库权限服务异常")
		return false
	}
	owned, err := s.access.OwnedNamespaces(r.Context(), session.User)
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "知识库权限服务异常")
		return false
	}
	allowed := make(map[string]struct{}, len(owned))
	for _, namespace := range owned {
		allowed[namespace] = struct{}{}
	}
	for _, namespace := range namespaces {
		if _, ok := allowed[namespace]; !ok {
			writeDetail(w, http.StatusForbidden, messageAPIKeyNamespaceDenied)
			return false
		}
	}
	return true
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
	// A normal account only ever lists its own credentials; administrators and
	// the local management surface see every key (including legacy unowned ones).
	var (
		keys []state.APIKey
		err  error
	)
	if session := apiKeyCaller(r); session != nil && !session.User.IsAdmin() {
		keys, err = s.keyStore.ListByOwner(r.Context(), session.User.ID)
	} else {
		keys, err = s.keyStore.List(r.Context())
	}
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
	// The issuer becomes the owner. A session account may only bind knowledge
	// bases it owns; the local management surface keeps creating unowned keys.
	ownerUserID := ""
	if session := apiKeyCaller(r); session != nil {
		ownerUserID = session.User.ID
		if !apiKeyCallerIsAdmin(r) && !s.apiKeyNamespacesAllowed(w, r, namespaces) {
			return
		}
	}
	created, err := s.keyStore.CreateForOwner(r.Context(), name, namespaces, ownerUserID)
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
	existing, err := s.keyStore.Get(r.Context(), keyID)
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "API Key 读取失败")
		return
	}
	if !apiKeyCallerOwns(r, existing) {
		writeDetail(w, http.StatusNotFound, messageAPIKeyNotFound)
		return
	}
	if !apiKeyCallerIsAdmin(r) && !s.apiKeyNamespacesAllowed(w, r, apiKeyNewNamespaces(existing, namespaces)) {
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
	keyID := pathParam(r, "key_id")
	existing, err := s.keyStore.Get(r.Context(), keyID)
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "API Key 读取失败")
		return
	}
	if !apiKeyCallerOwns(r, existing) {
		writeDetail(w, http.StatusNotFound, messageAPIKeyNotFound)
		return
	}
	deleted, err := s.keyStore.Delete(r.Context(), keyID)
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "API Key 删除失败")
		return
	}
	if !deleted {
		writeDetail(w, http.StatusNotFound, messageAPIKeyNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiKeyBatchDeleteBody is the request body of the batch-delete endpoint. A nil
// pointer means the field was absent (or null), which the existing handlers
// report as a missing required field.
type apiKeyBatchDeleteBody struct {
	IDs *[]string `json:"ids"`
}

// apiKeyBatchDeleteFailure is the per-id failure shape of the batch response.
// The atomic implementation reports database errors as a whole-batch 500, so
// the list stays empty; it is kept so the published response shape (and the
// Console contract) does not change.
type apiKeyBatchDeleteFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// apiKeyBatchDeleteResponse is the 200 payload: every submitted id lands in
// exactly one bucket, and deleted_count mirrors len(deleted). failed only
// carries entries when a caller keeps the legacy per-id error list.
type apiKeyBatchDeleteResponse struct {
	Deleted      []string                   `json:"deleted"`
	NotFound     []string                   `json:"not_found"`
	Failed       []apiKeyBatchDeleteFailure `json:"failed"`
	DeletedCount int                        `json:"deleted_count"`
}

// cleanAPIKeyBatchIDs validates the batch id list the way the sibling handlers
// validate names and namespaces. Duplicates collapse to their first occurrence
// so one key is never processed twice inside the same batch.
func cleanAPIKeyBatchIDs(ids *[]string) ([]string, []validationDetail) {
	if ids == nil {
		return nil, []validationDetail{{Type: "missing", Loc: []any{"body", "ids"}, Msg: "Field required"}}
	}
	values := *ids
	if len(values) < 1 {
		return nil, []validationDetail{{Type: "too_short", Loc: []any{"body", "ids"}, Msg: "List should have at least 1 item after validation, not 0"}}
	}
	if len(values) > 100 {
		return nil, []validationDetail{{Type: "too_long", Loc: []any{"body", "ids"}, Msg: "List should have at most 100 items after validation"}}
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	var details []validationDetail
	for index, value := range values {
		if value == "" {
			details = append(details, validationDetail{
				Type: "string_too_short",
				Loc:  []any{"body", "ids", index},
				Msg:  "String should have at least 1 character",
			})
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if len(details) > 0 {
		return nil, details
	}
	return result, nil
}

// handleBatchDeleteAPIKeys deletes several credentials in one transaction.
// A single SELECT classifies the ids (missing or not owned by the caller become
// not_found), one multi-row DELETE removes the rest, and one commit applies the
// whole batch atomically; any database error fails the batch as a whole.
func (s *Server) handleBatchDeleteAPIKeys(w http.ResponseWriter, r *http.Request) {
	if s.keyStore == nil {
		writeDetail(w, http.StatusServiceUnavailable, "API key store is not available")
		return
	}
	var body apiKeyBatchDeleteBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	ids, details := cleanAPIKeyBatchIDs(body.IDs)
	if details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	ownerUserID := ""
	if session := apiKeyCaller(r); session != nil && !session.User.IsAdmin() {
		ownerUserID = session.User.ID
	}
	deleted, notFound, err := s.keyStore.DeleteMany(r.Context(), ids, ownerUserID)
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "API Key 删除失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, apiKeyBatchDeleteResponse{
		Deleted:      deleted,
		NotFound:     notFound,
		Failed:       make([]apiKeyBatchDeleteFailure, 0),
		DeletedCount: len(deleted),
	})
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
	keyID := pathParam(r, "key_id")
	existing, err := s.keyStore.Get(r.Context(), keyID)
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "API Key 读取失败")
		return
	}
	if !apiKeyCallerOwns(r, existing) {
		writeDetail(w, http.StatusNotFound, messageAPIKeyNotFound)
		return
	}
	secret, err := s.keyStore.SecretForNamespace(r.Context(), keyID, namespace)
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
