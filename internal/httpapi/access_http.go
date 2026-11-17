package httpapi

import (
	"context"
	"net/http"

	"github.com/Guan0923/xinghe_rag/internal/access"
	"github.com/Guan0923/xinghe_rag/internal/auth"
	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// registerAccessRoutes mounts the knowledge-base sharing surface: invite
// pickers, access views, grants, invitations and the tenant self-create route.
// Every route requires a Console login session (authSession).
func (s *Server) registerAccessRoutes() {
	s.router.add("GET", "/v1/users", authSession, s.handleListInvitableUsers)
	s.router.add("POST", "/v1/knowledge-bases", authSession, s.handleCreateKnowledgeBaseForUser)
	s.router.add("GET", "/v1/knowledge-bases/{namespace}/access", authSession, s.handleKnowledgeBaseAccess)
	s.router.add("GET", "/v1/knowledge-bases/{namespace}/permissions", authSession, s.handleListKnowledgeBasePermissions)
	s.router.add("DELETE", "/v1/knowledge-bases/{namespace}/permissions/{user_id}", authSession, s.handleRevokeKnowledgeBasePermission)
	s.router.add("GET", "/v1/knowledge-bases/{namespace}/invitations", authSession, s.handleListKnowledgeBaseInvitations)
	s.router.add("POST", "/v1/knowledge-bases/{namespace}/invitations", authSession, s.handleCreateKnowledgeBaseInvitation)
	s.router.add("GET", "/v1/invitations", authSession, s.handleInvitationInbox)
	s.router.add("POST", "/v1/invitations/preview", authSession, s.handlePreviewInvitation)
	s.router.add("POST", "/v1/invitations/accept", authSession, s.handleAcceptInvitation)
	s.router.add("POST", "/v1/invitations/decline", authSession, s.handleDeclineInvitation)
	s.router.add("DELETE", "/v1/invitations/{invitation_id}", authSession, s.handleRevokeInvitation)
}

// requireAccess reports a 503 when the sharing service is not wired.
func (s *Server) requireAccess(w http.ResponseWriter) bool {
	if s.access != nil {
		return true
	}
	writeDetail(w, http.StatusServiceUnavailable, "知识库权限服务未启用")
	return false
}

// requestSessionUserContext returns the authSession account attached to the
// context. serveHTTP guarantees it; the guard keeps direct handler calls safe.
func requestSessionUserContext(w http.ResponseWriter, r *http.Request) (*auth.SessionUser, bool) {
	session, ok := sessionUserFromContext(r.Context())
	if !ok || session == nil {
		writeDetail(w, http.StatusUnauthorized, auth.MessageSessionInvalid)
		return nil, false
	}
	return session, true
}

// handleListInvitableUsers serves the invite picker: enabled accounts only, no
// password hashes or timestamps. Only VIPs and administrators may call it.
func (s *Server) handleListInvitableUsers(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if s.access != nil {
		// Membership-aware sharing is an optional extension so existing
		// AccessControl fakes keep compiling; a role-only control still uses
		// the historical VIP check below.
		if checker, ok := s.access.(interface {
			CanShareAccount(context.Context, state.User) (bool, error)
		}); ok {
			canShare, err := checker.CanShareAccount(r.Context(), session.User)
			if err != nil {
				writeDetail(w, http.StatusInternalServerError, "知识库权限服务异常")
				return
			}
			if !canShare {
				writeDetail(w, http.StatusForbidden, access.MessageForbidden)
				return
			}
		} else if !session.User.CanShare() {
			writeDetail(w, http.StatusForbidden, access.MessageForbidden)
			return
		}
	} else if !session.User.CanShare() {
		writeDetail(w, http.StatusForbidden, access.MessageForbidden)
		return
	}
	if s.access != nil {
		users, err := s.access.ListInvitableUsers(r.Context(), session.User)
		if err != nil {
			writeAccessError(w, err)
			return
		}
		if users == nil {
			users = []access.UserView{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": users})
		return
	}
	// Servers without the sharing service still expose the picker to VIPs and
	// administrators so the Console remains usable during a staged rollout.
	if s.users == nil {
		writeDetail(w, http.StatusServiceUnavailable, "用户管理未启用")
		return
	}
	users, err := s.users.ListUsers(r.Context())
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, err.Error())
		return
	}
	payload := make([]map[string]any, 0, len(users))
	for _, user := range users {
		if !user.Enabled || user.ID == session.User.ID {
			continue
		}
		payload = append(payload, map[string]any{"id": user.ID, "username": user.Username, "role": user.Role})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": payload})
}

// selfCreateKnowledgeBaseBody is the tenant self-create payload. The provider
// fields are accepted but ignored: the default template supplies them.
type selfCreateKnowledgeBaseBody struct {
	Namespace    string `json:"namespace"`
	Strategy     string `json:"strategy"`
	ChunkSize    *int   `json:"chunk_size"`
	ChunkOverlap *int   `json:"chunk_overlap"`

	Embedding      *providerSettings `json:"embedding"`
	Reranker       *providerSettings `json:"reranker"`
	EmbeddingModel *string           `json:"embedding_model"`
	RerankerModel  string            `json:"reranker_model"`
	VLM            *providerSettings `json:"vlm"`
	VLMModel       string            `json:"vlm_model"`
}

// withProviderSettings copies the administrator-only provider fields into the
// create input. It reports false unless the embedding provider is complete, in
// which case the runtime must clone the default template instead.
func (body selfCreateKnowledgeBaseBody) withProviderSettings(input KnowledgeBaseCreateInput) (KnowledgeBaseCreateInput, bool) {
	if body.Embedding == nil || body.EmbeddingModel == nil || *body.EmbeddingModel == "" {
		return input, false
	}
	input.EmbeddingModel = *body.EmbeddingModel
	input.EmbeddingValues = body.Embedding.values()
	input.RerankerModel = body.RerankerModel
	if body.Reranker != nil {
		input.RerankerValues = body.Reranker.values()
	}
	input.VLMModel = body.VLMModel
	if body.VLM != nil {
		input.VLMValues = body.VLM.values()
	}
	return input, true
}

// handleCreateKnowledgeBaseForUser creates a knowledge base owned by the caller.
// Everyone may call it; by default the runtime clones the default template and
// assigns the session account as the owner. An administrator may also submit the
// full provider configuration (the same fields as the admin route); without it
// the template clone applies to administrators as well. Providers are never
// taken from the payload for VIPs or normal users.
func (s *Server) handleCreateKnowledgeBaseForUser(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if s.kbSelfCreate == nil {
		writeDetail(w, http.StatusServiceUnavailable, "知识库自建服务未启用")
		return
	}
	var body selfCreateKnowledgeBaseBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	if details := validateNamespaceField(body.Namespace); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
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
	input := KnowledgeBaseCreateInput{Namespace: body.Namespace, Chunking: configuration}
	isAdmin := false
	if session.User.IsAdmin() {
		if withProviders, complete := body.withProviderSettings(input); complete {
			input = withProviders
			isAdmin = true
		}
	}
	payload, err := s.kbSelfCreate.CreateKnowledgeBaseForUser(r.Context(), session.User.ID, isAdmin, input)
	if err != nil {
		writeKnowledgeBaseSelfCreateError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, payload)
}

// handleKnowledgeBaseAccess renders the caller's effective permissions so the
// Console can hide write controls and show the management entry.
func (s *Server) handleKnowledgeBaseAccess(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireAccess(w) {
		return
	}
	view, err := s.access.Describe(r.Context(), session.User, pathParam(r, "namespace"))
	if err != nil {
		writeAccessError(w, err)
		return
	}
	if !view.CanRead && !view.CanWrite && !view.CanManage {
		writeDetail(w, http.StatusForbidden, access.MessageKnowledgeAccess)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleListKnowledgeBasePermissions lists the grants of one knowledge base.
func (s *Server) handleListKnowledgeBasePermissions(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireAccess(w) {
		return
	}
	grants, err := s.access.ListGrants(r.Context(), session.User, pathParam(r, "namespace"))
	if err != nil {
		writeAccessError(w, err)
		return
	}
	if grants == nil {
		grants = []access.GrantView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"permissions": grants})
}

// handleRevokeKnowledgeBasePermission removes every grant of one account.
func (s *Server) handleRevokeKnowledgeBasePermission(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireAccess(w) {
		return
	}
	if err := s.access.RevokeGrant(r.Context(), session.User,
		pathParam(r, "namespace"), pathParam(r, "user_id")); err != nil {
		writeAccessError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListKnowledgeBaseInvitations lists the invitations issued for a
// knowledge base.
func (s *Server) handleListKnowledgeBaseInvitations(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireAccess(w) {
		return
	}
	invitations, err := s.access.ListNamespaceInvitations(r.Context(), session.User, pathParam(r, "namespace"))
	if err != nil {
		writeAccessError(w, err)
		return
	}
	if invitations == nil {
		invitations = []access.InvitationView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitations": invitations})
}

// createKnowledgeBaseInvitationBody is the frozen invitation payload.
type createKnowledgeBaseInvitationBody struct {
	Username      string   `json:"username"`
	Permission    string   `json:"permission"`
	Scope         string   `json:"scope"`
	DocumentNames []string `json:"document_names"`
}

// handleCreateKnowledgeBaseInvitation issues one share link. The raw token only
// appears in this response; the database stores its digest.
func (s *Server) handleCreateKnowledgeBaseInvitation(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireAccess(w) {
		return
	}
	var body createKnowledgeBaseInvitationBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	invitation, token, err := s.access.CreateInvitation(r.Context(), session.User, access.CreateInvitationRequest{
		Namespace:     pathParam(r, "namespace"),
		Username:      body.Username,
		Permission:    body.Permission,
		Scope:         body.Scope,
		DocumentNames: body.DocumentNames,
	})
	if err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"invitation":  invitation,
		"invite_path": "/console#/invite?token=" + token,
	})
}

// handleInvitationInbox returns the invitations addressed to the caller.
func (s *Server) handleInvitationInbox(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireAccess(w) {
		return
	}
	invitations, err := s.access.Inbox(r.Context(), session.User)
	if err != nil {
		writeAccessError(w, err)
		return
	}
	if invitations == nil {
		invitations = []access.InvitationView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitations": invitations})
}

// invitationTokenBody is the frozen token payload for preview/accept/decline.
type invitationTokenBody struct {
	Token string `json:"token"`
}

// decodeInvitationToken reads and shape-checks one share-link token.
func decodeInvitationToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body invitationTokenBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return "", false
	}
	if body.Token == "" {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Type: "missing", Loc: []any{"body", "token"}, Msg: "Field required",
		}})
		return "", false
	}
	return body.Token, true
}

// handlePreviewInvitation resolves one link for the logged-in invitee.
func (s *Server) handlePreviewInvitation(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireAccess(w) {
		return
	}
	token, ok := decodeInvitationToken(w, r)
	if !ok {
		return
	}
	invitation, err := s.access.PreviewInvitation(r.Context(), session.User, token)
	if err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitation": invitation})
}

// handleAcceptInvitation turns one pending invitation into grants.
func (s *Server) handleAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireAccess(w) {
		return
	}
	token, ok := decodeInvitationToken(w, r)
	if !ok {
		return
	}
	invitation, err := s.access.AcceptInvitation(r.Context(), session.User, token)
	if err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"invitation": invitation,
		"namespace":  invitation.Namespace,
	})
}

// handleDeclineInvitation marks one pending invitation as declined.
func (s *Server) handleDeclineInvitation(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireAccess(w) {
		return
	}
	token, ok := decodeInvitationToken(w, r)
	if !ok {
		return
	}
	invitation, err := s.access.DeclineInvitation(r.Context(), session.User, token)
	if err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitation": invitation})
}

// handleRevokeInvitation withdraws one pending invitation.
func (s *Server) handleRevokeInvitation(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireAccess(w) {
		return
	}
	if err := s.access.RevokeInvitation(r.Context(), session.User, pathParam(r, "invitation_id")); err != nil {
		writeAccessError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
