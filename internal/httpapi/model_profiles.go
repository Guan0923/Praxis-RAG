package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
)

// ModelProfileInput is the transport-neutral create/patch request for one saved
// model configuration. HasAPIKey reports whether the request carried an apikey
// field, so an omitted key keeps the stored one on update.
type ModelProfileInput struct {
	Kind      string
	Name      string
	BaseURL   string
	Model     string
	APIKey    string
	HasAPIKey bool
}

// ModelProfileRuntime is the saved-model-configuration surface. Profiles are
// per-account: every authenticated caller manages its own list, and an
// administrator additionally gets a read-only view of every account's list.
// The viewer identity is therefore part of every call
// (viewerUserID, isAdmin).
type ModelProfileRuntime interface {
	ListModelProfiles(ctx context.Context, viewerUserID string, isAdmin bool) ([]map[string]any, error)
	ModelProfile(ctx context.Context, viewerUserID string, isAdmin bool, id string) (map[string]any, error)
	CreateModelProfile(ctx context.Context, viewerUserID string, isAdmin bool, input ModelProfileInput) (map[string]any, error)
	UpdateModelProfile(ctx context.Context, viewerUserID string, isAdmin bool, id string, input ModelProfileInput) (map[string]any, error)
	DeleteModelProfile(ctx context.Context, viewerUserID string, isAdmin bool, id string) (bool, error)
	ModelProfileKey(ctx context.Context, viewerUserID string, isAdmin bool, id string) (string, error)
	ModelProfileModels(ctx context.Context, viewerUserID string, isAdmin bool, id string) ([]string, error)
}

// WithModelProfileRuntime wires the 模型配置 (saved model configuration) admin
// handlers.
func WithModelProfileRuntime(runtime ModelProfileRuntime) Option {
	return func(s *Server) { s.modelProfiles = runtime }
}

// modelProfileCreateBody mirrors the FastAPI create body. Pointer fields
// distinguish "absent" (422 missing) from an explicit empty value.
type modelProfileCreateBody struct {
	Kind    string  `json:"kind"`
	Name    *string `json:"name"`
	BaseURL *string `json:"base_url"`
	APIKey  *string `json:"apikey"`
	Model   *string `json:"model"`
}

// modelProfileUpdateBody mirrors the FastAPI patch body: every field is
// optional and an omitted field keeps the stored value. The kind is immutable,
// so it is not accepted here (FastAPI-style extra_forbidden otherwise).
type modelProfileUpdateBody struct {
	Name    *string `json:"name"`
	BaseURL *string `json:"base_url"`
	APIKey  *string `json:"apikey"`
	Model   *string `json:"model"`
}

// modelProfileModelsBody is accepted for symmetry with the knowledge-base model
// listing; the stored profile is authoritative, so the values are ignored.
type modelProfileModelsBody struct {
	BaseURL *string `json:"base_url"`
	APIKey  *string `json:"apikey"`
}

func (s *Server) registerModelProfileRoutes() {
	// Session-scoped routes: every signed-in account manages its own profiles.
	// The /v1/admin/* paths stay as the administrator's entry point; they resolve
	// to the same owner-scoped handlers, with the administrator additionally
	// seeing every account's list.
	s.router.add("GET", "/v1/model-profiles", authSession, s.handleListModelProfiles)
	s.router.add("POST", "/v1/model-profiles", authSession, s.handleCreateModelProfile)
	s.router.add("GET", "/v1/model-profiles/{profile_id}", authSession, s.handleModelProfileDetails)
	s.router.add("PATCH", "/v1/model-profiles/{profile_id}", authSession, s.handleUpdateModelProfile)
	s.router.add("DELETE", "/v1/model-profiles/{profile_id}", authSession, s.handleDeleteModelProfile)
	s.router.add("GET", "/v1/model-profiles/{profile_id}/key", authSession, s.handleModelProfileKey)
	s.router.add("POST", "/v1/model-profiles/{profile_id}/models", authSession, s.handleModelProfileModels)
	s.router.add("GET", "/v1/admin/model-profiles", authAdmin, s.handleListModelProfiles)
	s.router.add("POST", "/v1/admin/model-profiles", authAdmin, s.handleCreateModelProfile)
	s.router.add("GET", "/v1/admin/model-profiles/{profile_id}", authAdmin, s.handleModelProfileDetails)
	s.router.add("PATCH", "/v1/admin/model-profiles/{profile_id}", authAdmin, s.handleUpdateModelProfile)
	s.router.add("DELETE", "/v1/admin/model-profiles/{profile_id}", authAdmin, s.handleDeleteModelProfile)
	s.router.add("GET", "/v1/admin/model-profiles/{profile_id}/key", authAdmin, s.handleModelProfileKey)
	s.router.add("POST", "/v1/admin/model-profiles/{profile_id}/models", authAdmin, s.handleModelProfileModels)
}

// modelProfileViewer resolves the calling account for a profile operation. The
// session layer guarantees a user for authSession/authAdmin routes; a nil
// session (loopback break-glass on the admin paths) is treated as the
// administrator so the existing local-console workflow keeps working.
func (s *Server) modelProfileViewer(r *http.Request) (string, bool) {
	session, ok := sessionUserFromContext(r.Context())
	if !ok || session == nil {
		return "", true
	}
	return session.User.ID, session.User.IsAdmin()
}

func (s *Server) requireModelProfiles(w http.ResponseWriter) bool {
	if s.modelProfiles != nil {
		return true
	}
	writeDetail(w, http.StatusServiceUnavailable, "Model profile administration is not available")
	return false
}

func (s *Server) handleListModelProfiles(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelProfiles(w) {
		return
	}
	viewerUserID, isAdmin := s.modelProfileViewer(r)
	items, err := s.modelProfiles.ListModelProfiles(r.Context(), viewerUserID, isAdmin)
	if err != nil {
		writeModelProfileError(w, err)
		return
	}
	if items == nil {
		items = []map[string]any{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"model_profiles": items})
}

func (s *Server) handleCreateModelProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelProfiles(w) {
		return
	}
	var body modelProfileCreateBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	if details := validateModelProfileBody(body.Kind, body.Name, body.BaseURL, body.Model); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	viewerUserID, isAdmin := s.modelProfileViewer(r)
	payload, err := s.modelProfiles.CreateModelProfile(r.Context(), viewerUserID, isAdmin, ModelProfileInput{
		Kind:      body.Kind,
		Name:      *body.Name,
		BaseURL:   stringOrEmpty(body.BaseURL),
		Model:     *body.Model,
		APIKey:    stringOrEmpty(body.APIKey),
		HasAPIKey: body.APIKey != nil,
	})
	if err != nil {
		writeModelProfileError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"profile": payload})
}

func (s *Server) handleModelProfileDetails(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelProfiles(w) {
		return
	}
	viewerUserID, isAdmin := s.modelProfileViewer(r)
	payload, err := s.modelProfiles.ModelProfile(r.Context(), viewerUserID, isAdmin, pathParam(r, "profile_id"))
	if err != nil {
		writeModelProfileError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"profile": payload})
}

func (s *Server) handleUpdateModelProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelProfiles(w) {
		return
	}
	var body modelProfileUpdateBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	// PATCH validates only the fields it carries; absent fields keep the stored
	// values, so their length is not re-checked here.
	if body.Name != nil {
		if details := validateModelProfileName(*body.Name); details != nil {
			writeValidation(w, http.StatusUnprocessableEntity, details)
			return
		}
	}
	if body.Model != nil {
		if details := validateModelProfileModel(*body.Model); details != nil {
			writeValidation(w, http.StatusUnprocessableEntity, details)
			return
		}
	}
	if details := validateModelProfileBaseURL(body.BaseURL); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	viewerUserID, isAdmin := s.modelProfileViewer(r)
	payload, err := s.modelProfiles.UpdateModelProfile(r.Context(), viewerUserID, isAdmin, pathParam(r, "profile_id"), ModelProfileInput{
		Name:      stringOrEmpty(body.Name),
		BaseURL:   stringOrEmpty(body.BaseURL),
		Model:     stringOrEmpty(body.Model),
		APIKey:    stringOrEmpty(body.APIKey),
		HasAPIKey: body.APIKey != nil,
	})
	if err != nil {
		writeModelProfileError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"profile": payload})
}

func (s *Server) handleDeleteModelProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelProfiles(w) {
		return
	}
	viewerUserID, isAdmin := s.modelProfileViewer(r)
	deleted, err := s.modelProfiles.DeleteModelProfile(r.Context(), viewerUserID, isAdmin, pathParam(r, "profile_id"))
	if err != nil {
		writeModelProfileError(w, err)
		return
	}
	if !deleted {
		writeDetail(w, http.StatusNotFound, kbconfig.MessageModelProfileMissing)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleModelProfileKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelProfiles(w) {
		return
	}
	viewerUserID, isAdmin := s.modelProfileViewer(r)
	key, err := s.modelProfiles.ModelProfileKey(r.Context(), viewerUserID, isAdmin, pathParam(r, "profile_id"))
	if err != nil {
		writeModelProfileError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"apikey": key})
}

func (s *Server) handleModelProfileModels(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelProfiles(w) {
		return
	}
	// The Console POSTs without a body (no overrides are supported); tolerate a
	// missing body, EOF, or an explicit JSON object alike.
	if details := decodeOptionalJSONBody(r, &modelProfileModelsBody{}); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	viewerUserID, isAdmin := s.modelProfileViewer(r)
	models, err := s.modelProfiles.ModelProfileModels(r.Context(), viewerUserID, isAdmin, pathParam(r, "profile_id"))
	if err != nil {
		writeModelProfileError(w, err)
		return
	}
	if models == nil {
		models = []string{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// modelProfileKinds lists the kinds the 模型配置 (saved model configuration)
// surface accepts. It is intentionally separate from knowledgeBaseKinds: "jev"
// (TypeSafe System One decision model) is a Console-only configuration and is
// not selectable as a knowledge-base model.
var modelProfileKinds = map[string]struct{}{"embedding": {}, "reranker": {}, "vlm": {}, "jev": {}}

// validateModelProfileBody reproduces the FastAPI validation order of the create
// body: kind (required Literal), name (required, 1..80), model (required,
// 1..512), then the optional base_url length bound.
func validateModelProfileBody(kind string, name, baseURL, model *string) []validationDetail {
	if kind == "" {
		return []validationDetail{{Type: "missing", Loc: []any{"body", "kind"}, Msg: "Field required"}}
	}
	if _, ok := modelProfileKinds[kind]; !ok {
		return []validationDetail{{
			Type: "literal_error", Loc: []any{"body", "kind"},
			Msg: "Input should be 'embedding', 'reranker', 'vlm' or 'jev'",
		}}
	}
	if name == nil {
		return []validationDetail{{Type: "missing", Loc: []any{"body", "name"}, Msg: "Field required"}}
	}
	if details := validateModelProfileName(*name); details != nil {
		return details
	}
	if model == nil {
		return []validationDetail{{Type: "missing", Loc: []any{"body", "model"}, Msg: "Field required"}}
	}
	if details := validateModelProfileModel(*model); details != nil {
		return details
	}
	return validateModelProfileBaseURL(baseURL)
}

func validateModelProfileName(name string) []validationDetail {
	length := len([]rune(name))
	if length == 0 {
		return []validationDetail{{
			Type: "string_too_short", Loc: []any{"body", "name"},
			Msg: "String should have at least 1 character",
		}}
	}
	if length > kbconfig.ModelProfileNameMaxLength {
		return []validationDetail{{
			Type: "string_too_long", Loc: []any{"body", "name"},
			Msg: "String should have at most 80 characters",
		}}
	}
	return nil
}

func validateModelProfileModel(model string) []validationDetail {
	length := len([]rune(model))
	if length == 0 {
		return []validationDetail{{
			Type: "string_too_short", Loc: []any{"body", "model"},
			Msg: "String should have at least 1 character",
		}}
	}
	if length > kbconfig.ModelProfileModelMaxLength {
		return []validationDetail{{
			Type: "string_too_long", Loc: []any{"body", "model"},
			Msg: "String should have at most 512 characters",
		}}
	}
	return nil
}

func validateModelProfileBaseURL(baseURL *string) []validationDetail {
	if baseURL == nil || len([]rune(*baseURL)) <= 2048 {
		return nil
	}
	return []validationDetail{{
		Type: "string_too_long", Loc: []any{"body", "base_url"},
		Msg: "String should have at most 2048 characters",
	}}
}

// writeModelProfileError renders model-profile runtime failures like the
// knowledge-base admin routes: a duplicate (kind, name) is a 409 conflict, a
// missing profile is a 404 on every by-id route, any other validation failure is
// a 400, and state/model failures keep their runtime status (503).
func writeModelProfileError(w http.ResponseWriter, err error) {
	var conflict *apierrors.KnowledgeConflictError
	if errors.As(err, &conflict) {
		writeDetail(w, http.StatusConflict, conflict.Message)
		return
	}
	var validation *apierrors.KnowledgeValidationError
	if errors.As(err, &validation) {
		status := http.StatusBadRequest
		if validation.Message == kbconfig.MessageModelProfileMissing {
			status = http.StatusNotFound
		}
		writeDetail(w, status, validation.Message)
		return
	}
	writeRuntimeError(w, err, true)
}

// stringOrEmpty dereferences an optional JSON string field.
func stringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// decodeOptionalJSONBody accepts an absent or empty JSON body as {}, while a
// present body still gets the strict FastAPI-shaped validation. It exists for
// routes the Console calls with a bare POST, such as the model-profile model
// listing.
func decodeOptionalJSONBody(r *http.Request, target any) []validationDetail {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	details := decodeJSONBody(r, target)
	if len(details) == 1 && details[0].Type == "missing" && len(details[0].Loc) == 1 {
		return nil
	}
	return details
}
