package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/chunking"
)

var knowledgeBaseKinds = map[string]struct{}{"embedding": {}, "reranker": {}, "vlm": {}}

// providerSettings mirrors http_service.ProviderSettingsBody.
type providerSettings struct {
	BaseURL *string `json:"base_url"`
	APIKey  *string `json:"apikey"`
}

func (p providerSettings) values() map[string]any {
	values := map[string]any{}
	if p.BaseURL != nil {
		values["base_url"] = *p.BaseURL
	}
	if p.APIKey != nil {
		values["apikey"] = *p.APIKey
	}
	return values
}

// KnowledgeBaseCreateInput is the transport-neutral create request.
type KnowledgeBaseCreateInput struct {
	Namespace       string
	Chunking        chunking.Config
	EmbeddingModel  string
	RerankerModel   string
	EmbeddingValues map[string]any
	RerankerValues  map[string]any
	VLMModel        string
	VLMValues       map[string]any
}

// KnowledgeBaseUpdateInput is the transport-neutral patch request.
type KnowledgeBaseUpdateInput struct {
	Values  map[string]any
	Reembed bool
}

// KnowledgeBaseAdminRuntime is the knowledge-base administration surface.
type KnowledgeBaseAdminRuntime interface {
	ListKnowledgeBases(ctx context.Context) ([]map[string]any, error)
	CreateKnowledgeBase(ctx context.Context, input KnowledgeBaseCreateInput) (map[string]any, error)
	UpdateKnowledgeBase(ctx context.Context, namespace string, input KnowledgeBaseUpdateInput) (map[string]any, error)
	KnowledgeBaseKey(ctx context.Context, namespace, kind string) (string, error)
	KnowledgeBaseModels(ctx context.Context, kind string, values map[string]any, namespace *string) ([]string, error)
	// PurgeKnowledgeBase deletes every document logically and then the
	// configuration row, so the merged Console list no longer shows the
	// namespace. FailedDocuments reports per-document failures; when it is
	// non-empty the configuration row is intentionally kept.
	PurgeKnowledgeBase(ctx context.Context, namespace string) (KnowledgeBasePurgeResult, error)
}

// KnowledgeBasePurgeResult is the transport-neutral outcome of purging one
// knowledge base. Documents are removed before the configuration row.
type KnowledgeBasePurgeResult struct {
	Namespace            string
	DocumentsDeleted     int
	FailedDocuments      []map[string]string
	ConfigurationDeleted bool
}

// WithKnowledgeBaseAdminRuntime wires the knowledge-base admin handlers.
func WithKnowledgeBaseAdminRuntime(runtime KnowledgeBaseAdminRuntime) Option {
	return func(s *Server) { s.kbAdmin = runtime }
}

// handleNamespaceList returns the namespace names a logged-in session may
// browse. It is the tenant-scoped counterpart of the admin list: normal users
// need the switch list, but only admins may see provider configuration.
func (s *Server) handleNamespaceList(w http.ResponseWriter, r *http.Request) {
	if s.kbAdmin == nil {
		writeDetail(w, http.StatusServiceUnavailable, "Knowledge base admin runtime is not available")
		return
	}
	items, err := s.kbAdmin.ListKnowledgeBases(r.Context())
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, err.Error())
		return
	}
	namespaces := make([]string, 0, len(items))
	for _, item := range items {
		if name, ok := item["namespace"].(string); ok && name != "" {
			namespaces = append(namespaces, name)
		}
	}
	// A login session only sees knowledge bases it owns or holds grants in and
	// fails closed when ownership enforcement is not wired; an API key only sees
	// the namespaces bound to the credential. Management credentials (the
	// service token and session-less loopback requests) keep the full list.
	// `owned_namespaces` is the stricter binding set used by the per-account
	// API-key page (a normal account may only scope a key to knowledge bases it
	// created).
	response := map[string]any{"namespaces": namespaces, "owned_namespaces": namespaces}
	if session, ok := sessionUserFromContext(r.Context()); ok {
		if s.access == nil {
			writeDetail(w, http.StatusInternalServerError, "知识库权限服务未配置")
			return
		}
		visible, err := s.access.VisibleNamespaces(r.Context(), session.User)
		if err != nil {
			writeDetail(w, http.StatusInternalServerError, "知识库权限服务异常")
			return
		}
		owned, err := s.access.OwnedNamespaces(r.Context(), session.User)
		if err != nil {
			writeDetail(w, http.StatusInternalServerError, "知识库权限服务异常")
			return
		}
		visible = filterNamespaceNames(namespaces, visible)
		response["namespaces"] = visible
		response["owned_namespaces"] = filterNamespaceNames(visible, owned)
	} else if keyNamespaces, ok := apiKeyNamespacesFromContext(r.Context()); ok {
		scoped := filterNamespaceNames(namespaces, keyNamespaces)
		response["namespaces"] = scoped
		response["owned_namespaces"] = scoped
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

// filterNamespaceNames keeps the merged knowledge-base list order and drops
// names outside the allowed set, so concurrent credential scope and merged-list
// changes never reorder the response.
func filterNamespaceNames(namespaces, allowed []string) []string {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	filtered := make([]string, 0, len(namespaces))
	for _, name := range namespaces {
		if _, ok := allowedSet[name]; ok {
			filtered = append(filtered, name)
		}
	}
	return filtered
}

type createKnowledgeBaseBody struct {
	Namespace      string            `json:"namespace"`
	Embedding      *providerSettings `json:"embedding"`
	Reranker       *providerSettings `json:"reranker"`
	EmbeddingModel *string           `json:"embedding_model"`
	RerankerModel  string            `json:"reranker_model"`
	VLM            *providerSettings `json:"vlm"`
	VLMModel       string            `json:"vlm_model"`
	Strategy       string            `json:"strategy"`
	ChunkSize      *int              `json:"chunk_size"`
	ChunkOverlap   *int              `json:"chunk_overlap"`
}

type updateKnowledgeBaseBody struct {
	Embedding      *providerSettings `json:"embedding"`
	Reranker       *providerSettings `json:"reranker"`
	EmbeddingModel *string           `json:"embedding_model"`
	RerankerModel  *string           `json:"reranker_model"`
	VLM            *providerSettings `json:"vlm"`
	VLMModel       *string           `json:"vlm_model"`
	Reembed        bool              `json:"reembed"`
}

func (body updateKnowledgeBaseBody) values() map[string]any {
	values := map[string]any{}
	if body.Embedding != nil {
		values["embedding"] = body.Embedding.values()
	}
	if body.Reranker != nil {
		values["reranker"] = body.Reranker.values()
	}
	if body.EmbeddingModel != nil {
		values["embedding_model"] = *body.EmbeddingModel
	}
	if body.RerankerModel != nil {
		values["reranker_model"] = *body.RerankerModel
	}
	if body.VLM != nil {
		values["vlm"] = body.VLM.values()
	}
	if body.VLMModel != nil {
		values["vlm_model"] = *body.VLMModel
	}
	return values
}

type modelListBody struct {
	BaseURL   *string `json:"base_url"`
	APIKey    *string `json:"apikey"`
	Namespace *string `json:"namespace"`
}

func (s *Server) registerKnowledgeBaseAdminRoutes() {
	s.router.add("GET", "/v1/admin/knowledge-bases", authAdmin, s.handleListKnowledgeBases)
	s.router.add("POST", "/v1/admin/knowledge-bases", authAdmin, s.handleCreateKnowledgeBase)
	s.router.add("POST", "/v1/admin/knowledge-bases/models/{kind}", authAdmin, s.handleKnowledgeBaseModels)
	// The literal batch path must be registered before the {namespace} routes:
	// the router takes the first matching route, so a later literal registration
	// would be swallowed by DELETE /v1/admin/knowledge-bases/{namespace}.
	s.router.add("POST", "/v1/admin/knowledge-bases/batch-delete", authAdmin, s.handleBatchDeleteKnowledgeBases)
	s.router.add("GET", "/v1/admin/knowledge-bases/{namespace}", authAdmin, s.handleKnowledgeBaseDetails)
	s.router.add("GET", "/v1/admin/knowledge-bases/{namespace}/key/{kind}", authAdmin, s.handleKnowledgeBaseKey)
	s.router.add("PATCH", "/v1/admin/knowledge-bases/{namespace}", authAdmin, s.handleUpdateKnowledgeBase)
	s.router.add("DELETE", "/v1/admin/knowledge-bases/{namespace}", authAdmin, s.handlePurgeKnowledgeBase)
}

func (s *Server) requireKBAdmin(w http.ResponseWriter) bool {
	if s.kbAdmin != nil {
		return true
	}
	writeDetail(w, http.StatusServiceUnavailable, "Knowledge-base administration is not available")
	return false
}

func (s *Server) handleListKnowledgeBases(w http.ResponseWriter, r *http.Request) {
	if !s.requireKBAdmin(w) {
		return
	}
	items, err := s.kbAdmin.ListKnowledgeBases(r.Context())
	if err != nil {
		writeRuntimeError(w, err, true)
		return
	}
	if items == nil {
		items = []map[string]any{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"knowledge_bases": items})
}

func (s *Server) handleCreateKnowledgeBase(w http.ResponseWriter, r *http.Request) {
	if !s.requireKBAdmin(w) {
		return
	}
	var body createKnowledgeBaseBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	if details := validateNamespaceField(body.Namespace); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	// Python's CreateKnowledgeBaseBody requires `embedding` (ProviderSettingsBody)
	// and `embedding_model` (min_length=1, max_length=512), validated in field
	// declaration order before the handler runs. Match that ordering so the
	// first 422 detail has the same loc/type/msg as FastAPI.
	if body.Embedding == nil {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Type: "missing", Loc: []any{"body", "embedding"}, Msg: "Field required",
		}})
		return
	}
	if body.EmbeddingModel == nil {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Type: "missing", Loc: []any{"body", "embedding_model"}, Msg: "Field required",
		}})
		return
	}
	if len([]rune(*body.EmbeddingModel)) == 0 {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Type: "string_too_short", Loc: []any{"body", "embedding_model"},
			Msg: "String should have at least 1 character",
		}})
		return
	}
	if len([]rune(*body.EmbeddingModel)) > 512 {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Type: "string_too_long", Loc: []any{"body", "embedding_model"},
			Msg: "String should have at most 512 characters",
		}})
		return
	}
	if len([]rune(body.RerankerModel)) > 512 {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Type: "string_too_long", Loc: []any{"body", "reranker_model"},
			Msg: "String should have at most 512 characters",
		}})
		return
	}
	if len([]rune(body.VLMModel)) > 512 {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Type: "string_too_long", Loc: []any{"body", "vlm_model"},
			Msg: "String should have at most 512 characters",
		}})
		return
	}
	strategy := body.Strategy
	if strategy == "" {
		strategy = chunking.DefaultStrategy
	}
	configuration, err := chunking.ConfigFromRequest(strategy, body.ChunkSize, body.ChunkOverlap)
	if err != nil {
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	embeddingValues := body.Embedding.values()
	rerankerValues := map[string]any{}
	if body.Reranker != nil {
		rerankerValues = body.Reranker.values()
	}
	vlmValues := map[string]any{}
	if body.VLM != nil {
		vlmValues = body.VLM.values()
	}
	payload, err := s.kbAdmin.CreateKnowledgeBase(r.Context(), KnowledgeBaseCreateInput{
		Namespace:       body.Namespace,
		Chunking:        configuration,
		EmbeddingModel:  *body.EmbeddingModel,
		RerankerModel:   body.RerankerModel,
		EmbeddingValues: embeddingValues,
		RerankerValues:  rerankerValues,
		VLMModel:        body.VLMModel,
		VLMValues:       vlmValues,
	})
	if err != nil {
		writeCreateKnowledgeBaseError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, payload)
}

func writeCreateKnowledgeBaseError(w http.ResponseWriter, err error) {
	var validation *apierrors.KnowledgeValidationError
	if errors.As(err, &validation) {
		writeDetail(w, http.StatusBadRequest, validation.Message)
		return
	}
	writeRuntimeError(w, err, true)
}

func (s *Server) handleKnowledgeBaseDetails(w http.ResponseWriter, r *http.Request) {
	if !s.requireKBAdmin(w) {
		return
	}
	public, err := s.runtime.PublicKnowledgeBase(r.Context(), pathParam(r, "namespace"))
	if err != nil {
		var validation *apierrors.KnowledgeValidationError
		if errors.As(err, &validation) {
			writeDetail(w, http.StatusNotFound, validation.Message)
			return
		}
		writeRuntimeError(w, err, true)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, public)
}

func (s *Server) handleKnowledgeBaseKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireKBAdmin(w) {
		return
	}
	kind := pathParam(r, "kind")
	if _, ok := knowledgeBaseKinds[kind]; !ok {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Type: "literal_error", Loc: []any{"path", "kind"},
			Msg: "Input should be 'embedding', 'reranker', 'vlm'",
		}})
		return
	}
	key, err := s.kbAdmin.KnowledgeBaseKey(r.Context(), pathParam(r, "namespace"), kind)
	if err != nil {
		var validation *apierrors.KnowledgeValidationError
		if errors.As(err, &validation) {
			writeDetail(w, http.StatusBadRequest, validation.Message)
			return
		}
		writeRuntimeError(w, err, true)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"apikey": key})
}

func (s *Server) handleKnowledgeBaseModels(w http.ResponseWriter, r *http.Request) {
	if !s.requireKBAdmin(w) {
		return
	}
	kind := pathParam(r, "kind")
	if _, ok := knowledgeBaseKinds[kind]; !ok {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Type: "literal_error", Loc: []any{"path", "kind"},
			Msg: "Input should be 'embedding', 'reranker', 'vlm'",
		}})
		return
	}
	var body modelListBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	if body.Namespace != nil {
		if details := validateNamespaceField(*body.Namespace); details != nil {
			writeValidation(w, http.StatusUnprocessableEntity, details)
			return
		}
	}
	if body.BaseURL != nil && len([]rune(*body.BaseURL)) > 2048 {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Type: "string_too_long", Loc: []any{"body", "base_url"},
			Msg: "String should have at most 2048 characters",
		}})
		return
	}
	values := map[string]any{}
	if body.BaseURL != nil {
		values["base_url"] = *body.BaseURL
	}
	if body.APIKey != nil {
		values["apikey"] = *body.APIKey
	}
	models, err := s.kbAdmin.KnowledgeBaseModels(r.Context(), kind, values, body.Namespace)
	if err != nil {
		var validation *apierrors.KnowledgeValidationError
		if errors.As(err, &validation) {
			writeDetail(w, http.StatusBadRequest, validation.Message)
			return
		}
		writeRuntimeError(w, err, true)
		return
	}
	if models == nil {
		models = []string{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) handleUpdateKnowledgeBase(w http.ResponseWriter, r *http.Request) {
	if !s.requireKBAdmin(w) {
		return
	}
	var body updateKnowledgeBaseBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	// Python's UpdateKnowledgeBaseBody validates embedding_model (min_length=1,
	// max_length=512) before reranker_model (max_length=512); provider bodies have
	// no scalar constraints, so these are the only 422 checks that run first.
	if body.EmbeddingModel != nil {
		if len([]rune(*body.EmbeddingModel)) == 0 {
			writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
				Type: "string_too_short", Loc: []any{"body", "embedding_model"},
				Msg: "String should have at least 1 character",
			}})
			return
		}
		if len([]rune(*body.EmbeddingModel)) > 512 {
			writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
				Type: "string_too_long", Loc: []any{"body", "embedding_model"},
				Msg: "String should have at most 512 characters",
			}})
			return
		}
	}
	if body.RerankerModel != nil && len([]rune(*body.RerankerModel)) > 512 {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Type: "string_too_long", Loc: []any{"body", "reranker_model"},
			Msg: "String should have at most 512 characters",
		}})
		return
	}
	if body.VLMModel != nil && len([]rune(*body.VLMModel)) > 512 {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Type: "string_too_long", Loc: []any{"body", "vlm_model"},
			Msg: "String should have at most 512 characters",
		}})
		return
	}

	payload, err := s.kbAdmin.UpdateKnowledgeBase(r.Context(), pathParam(r, "namespace"), KnowledgeBaseUpdateInput{
		Values:  body.values(),
		Reembed: body.Reembed,
	})
	if err != nil {
		writeUpdateKnowledgeBaseError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, payload)
}

func writeUpdateKnowledgeBaseError(w http.ResponseWriter, err error) {
	var conflict *apierrors.UploadConflict
	if errors.As(err, &conflict) {
		writeDetail(w, http.StatusConflict, "Upload conflicts with another operation or idempotency key.")
		return
	}
	var validation *apierrors.KnowledgeValidationError
	if errors.As(err, &validation) {
		writeDetail(w, http.StatusConflict, validation.Message)
		return
	}
	writeRuntimeError(w, err, true)
}

// messageKnowledgeBasePurgeMissing is the stable 404 detail of the purge routes.
const messageKnowledgeBasePurgeMissing = "知识库不存在"

// messageKnowledgeBasePurgePartial is the detail used when some documents could
// not be deleted; the configuration row is kept so the knowledge base remains
// listed and can be retried.
const messageKnowledgeBasePurgePartial = "部分文档删除失败，知识库未删除"

// batchDeleteKnowledgeBasesBody is the Console multi-select delete request.
type batchDeleteKnowledgeBasesBody struct {
	Namespaces *[]string `json:"namespaces"`
}

// cleanKnowledgeBaseNamespaces validates the batch list the way FastAPI
// validates a constrained list: required, 1..100 items, each matching the
// namespace pattern, deduplicated in request order.
func cleanKnowledgeBaseNamespaces(namespaces *[]string) ([]string, []validationDetail) {
	if namespaces == nil {
		return nil, []validationDetail{{
			Type: "missing", Loc: []any{"body", "namespaces"}, Msg: "Field required",
		}}
	}
	values := *namespaces
	if len(values) == 0 {
		return nil, []validationDetail{{
			Type: "too_short", Loc: []any{"body", "namespaces"},
			Msg: "List should have at least 1 item after validation, not 0",
		}}
	}
	if len(values) > 100 {
		return nil, []validationDetail{{
			Type: "too_long", Loc: []any{"body", "namespaces"},
			Msg: "List should have at most 100 items after validation",
		}}
	}
	seen := make(map[string]struct{}, len(values))
	cleaned := make([]string, 0, len(values))
	for index, namespace := range values {
		if !namespacePattern.MatchString(namespace) {
			return nil, []validationDetail{{
				Type: "string_pattern_mismatch", Loc: []any{"body", "namespaces", index},
				Msg: "String should match pattern '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'",
			}}
		}
		if _, duplicate := seen[namespace]; duplicate {
			continue
		}
		seen[namespace] = struct{}{}
		cleaned = append(cleaned, namespace)
	}
	return cleaned, nil
}

// handlePurgeKnowledgeBase implements DELETE /v1/admin/knowledge-bases/{namespace}.
// It removes every document logically and then the configuration row; 204 means
// the namespace is gone from the merged list. A partially failed purge keeps the
// configuration row and answers 409 with the per-document failures.
func (s *Server) handlePurgeKnowledgeBase(w http.ResponseWriter, r *http.Request) {
	if !s.requireKBAdmin(w) {
		return
	}
	result, err := s.kbAdmin.PurgeKnowledgeBase(r.Context(), pathParam(r, "namespace"))
	if err != nil {
		writeRuntimeError(w, err, true)
		return
	}
	if len(result.FailedDocuments) > 0 {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusConflict, map[string]any{
			"detail":           messageKnowledgeBasePurgePartial,
			"namespace":        result.Namespace,
			"failed_documents": result.FailedDocuments,
		})
		return
	}
	if result.DocumentsDeleted == 0 && !result.ConfigurationDeleted {
		writeDetail(w, http.StatusNotFound, messageKnowledgeBasePurgeMissing)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// handleBatchDeleteKnowledgeBases implements POST /v1/admin/knowledge-bases/batch-delete.
// Every namespace is purged independently; the response separates deleted,
// already-missing and failed namespaces so the Console can keep only the failed
// rows selected.
func (s *Server) handleBatchDeleteKnowledgeBases(w http.ResponseWriter, r *http.Request) {
	if !s.requireKBAdmin(w) {
		return
	}
	var body batchDeleteKnowledgeBasesBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	namespaces, details := cleanKnowledgeBaseNamespaces(body.Namespaces)
	if details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	deleted := make([]string, 0, len(namespaces))
	missing := make([]string, 0)
	failed := make([]map[string]any, 0)
	documentsDeleted := 0
	configurationsDeleted := 0
	for _, namespace := range namespaces {
		result, err := s.kbAdmin.PurgeKnowledgeBase(r.Context(), namespace)
		if err != nil {
			_, detail := runtimeErrorStatus(err)
			failed = append(failed, map[string]any{"namespace": namespace, "error": detail})
			continue
		}
		if len(result.FailedDocuments) > 0 {
			failed = append(failed, map[string]any{
				"namespace":        namespace,
				"error":            messageKnowledgeBasePurgePartial,
				"failed_documents": result.FailedDocuments,
			})
			continue
		}
		if result.DocumentsDeleted == 0 && !result.ConfigurationDeleted {
			missing = append(missing, namespace)
			continue
		}
		deleted = append(deleted, namespace)
		documentsDeleted += result.DocumentsDeleted
		if result.ConfigurationDeleted {
			configurationsDeleted++
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":                deleted,
		"missing":                missing,
		"failed":                 failed,
		"documents_deleted":      documentsDeleted,
		"configurations_deleted": configurationsDeleted,
	})
}

func validateNamespaceField(namespace string) []validationDetail {
	if namespacePattern.MatchString(namespace) {
		return nil
	}
	return []validationDetail{{
		Type: "string_pattern_mismatch", Loc: []any{"body", "namespace"},
		Msg: "String should match pattern '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'",
	}}
}


