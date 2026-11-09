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
	// A login session only sees knowledge bases it owns or holds grants in;
	// management credentials keep the full list.
	if session, ok := sessionUserFromContext(r.Context()); ok && s.access != nil {
		visible, err := s.access.VisibleNamespaces(r.Context(), session.User)
		if err != nil {
			writeDetail(w, http.StatusInternalServerError, "知识库权限服务异常")
			return
		}
		allowed := make(map[string]struct{}, len(visible))
		for _, name := range visible {
			allowed[name] = struct{}{}
		}
		filtered := make([]string, 0, len(visible))
		for _, name := range namespaces {
			if _, ok := allowed[name]; ok {
				filtered = append(filtered, name)
			}
		}
		namespaces = filtered
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": namespaces})
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
	s.router.add("GET", "/v1/admin/knowledge-bases/{namespace}", authAdmin, s.handleKnowledgeBaseDetails)
	s.router.add("GET", "/v1/admin/knowledge-bases/{namespace}/key/{kind}", authAdmin, s.handleKnowledgeBaseKey)
	s.router.add("PATCH", "/v1/admin/knowledge-bases/{namespace}", authAdmin, s.handleUpdateKnowledgeBase)
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
			Msg: "Input should be 'embedding', 'reranker' or 'vlm'",
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
			Msg: "Input should be 'embedding', 'reranker' or 'vlm'",
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

func validateNamespaceField(namespace string) []validationDetail {
	if namespacePattern.MatchString(namespace) {
		return nil
	}
	return []validationDetail{{
		Type: "string_pattern_mismatch", Loc: []any{"body", "namespace"},
		Msg: "String should match pattern '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'",
	}}
}
