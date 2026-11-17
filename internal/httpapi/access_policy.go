package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/Guan0923/xinghe_rag/internal/access"
	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/auth"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// AccessControl resolves what one Console account may read or modify inside a
// knowledge base, and it implements the invite-based sharing rules.
//
// The concrete implementation is *access.Service. A nil AccessControl disables
// knowledge-base ownership enforcement for tenant routes so unit tests and the
// management credentials (API key, service token, session-less loopback) keep
// their historical behaviour. The per-account API-key binding rule fails closed
// instead: a non-administrator session cannot mint a credential for any
// namespace when the sharing service is missing.
type AccessControl interface {
	ScopeSet(ctx context.Context, user state.User, namespace string) (access.ScopeSet, error)
	CanManage(ctx context.Context, user state.User, namespace string) (bool, error)
	VisibleNamespaces(ctx context.Context, user state.User) ([]string, error)
	OwnedNamespaces(ctx context.Context, user state.User) ([]string, error)
	ResolveDocumentIDs(ctx context.Context, namespace string, names []string) ([]string, error)
	Describe(ctx context.Context, user state.User, namespace string) (access.AccessView, error)
	ListGrants(ctx context.Context, user state.User, namespace string) ([]access.GrantView, error)
	RevokeGrant(ctx context.Context, user state.User, namespace, granteeUserID string) error
	ListNamespaceInvitations(ctx context.Context, user state.User, namespace string) ([]access.InvitationView, error)
	CreateInvitation(ctx context.Context, inviter state.User, request access.CreateInvitationRequest) (access.InvitationView, string, error)
	Inbox(ctx context.Context, user state.User) ([]access.InvitationView, error)
	PreviewInvitation(ctx context.Context, user state.User, token string) (access.InvitationView, error)
	AcceptInvitation(ctx context.Context, user state.User, token string) (access.InvitationView, error)
	DeclineInvitation(ctx context.Context, user state.User, token string) (access.InvitationView, error)
	RevokeInvitation(ctx context.Context, user state.User, invitationID string) error
	ListInvitableUsers(ctx context.Context, inviter state.User) ([]access.UserView, error)
}

// KnowledgeBaseSelfCreateRuntime clones the default knowledge-base template for
// one owning account. The admin route keeps the full provider configuration;
// this surface only accepts namespace/chunking fields.
type KnowledgeBaseSelfCreateRuntime interface {
	CreateKnowledgeBaseForUser(ctx context.Context, ownerUserID string, isAdmin bool,
		input KnowledgeBaseCreateInput) (map[string]any, error)
}

// WithAccessControl wires the knowledge-base authorization service. When it is
// absent, session requests to tenant routes skip ownership filtering entirely.
func WithAccessControl(control AccessControl) Option {
	return func(s *Server) { s.access = control }
}

// WithKnowledgeBaseSelfCreateRuntime wires POST /v1/knowledge-bases.
func WithKnowledgeBaseSelfCreateRuntime(runtime KnowledgeBaseSelfCreateRuntime) Option {
	return func(s *Server) { s.kbSelfCreate = runtime }
}

// sessionUserContextKey carries the authenticated Console account of an
// authSession route (and of a session-authenticated tenant route).
type sessionUserContextKey struct{}

// sessionUserFromContext returns the login session attached by serveHTTP.
func sessionUserFromContext(ctx context.Context) (*auth.SessionUser, bool) {
	session, ok := ctx.Value(sessionUserContextKey{}).(*auth.SessionUser)
	return session, ok
}

// apiKeyNamespacesContextKey carries the namespaces bound to the API-key
// credential that authenticated a tenant route.
type apiKeyNamespacesContextKey struct{}

// apiKeyNamespacesFromContext returns the API-key binding set attached by
// serveHTTP. The boolean reports whether an API key authenticated the request;
// it is absent for login sessions, the service token and session-less loopback.
func apiKeyNamespacesFromContext(ctx context.Context) ([]string, bool) {
	namespaces, ok := ctx.Value(apiKeyNamespacesContextKey{}).([]string)
	return namespaces, ok
}

// requireSession authenticates the Console login session for an authSession
// route. Unlike the legacy tenant/admin policies there is no loopback fallback:
// the sharing endpoints always act on behalf of one account.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) (*auth.SessionUser, bool) {
	if !s.authEnabled() {
		writeDetail(w, http.StatusUnauthorized, auth.MessageSessionInvalid)
		return nil, false
	}
	session, viaCookie := s.requestSessionUser(r)
	if session == nil {
		writeDetail(w, http.StatusUnauthorized, auth.MessageSessionInvalid)
		return nil, false
	}
	if !s.validateCSRF(r, session, viaCookie) {
		writeDetail(w, http.StatusForbidden, "CSRF 校验失败，请刷新页面后重试")
		return nil, false
	}
	return session, true
}

// requireAdminSession authenticates an authAdminSession route: a Console login
// session is mandatory (requireSession writes 401 without one and 403 when a
// cookie-authenticated write misses the CSRF header), and the account must be an
// administrator (403 otherwise).
//
// Unlike requireAdmin there is deliberately no loopback break-glass fallback:
// the Kubernetes monitor and rolling-restart endpoints act on the live cluster
// and must only be reachable by an explicitly signed-in administrator.
func (s *Server) requireAdminSession(w http.ResponseWriter, r *http.Request) (*auth.SessionUser, bool) {
	session, ok := s.requireSession(w, r)
	if !ok {
		return nil, false
	}
	if !session.User.IsAdmin() {
		writeDetail(w, http.StatusForbidden, "需要管理员权限")
		return nil, false
	}
	return session, true
}

// withAccessScope resolves one session account's scope for a namespace and puts
// it in the request context, so REST handlers and the MCP endpoint enforce the
// same document visibility. It returns nil after writing the error response.
func (s *Server) withAccessScope(w http.ResponseWriter, r *http.Request, user state.User, namespace string) *http.Request {
	if s.access == nil {
		return r
	}
	scope, err := s.access.ScopeSet(r.Context(), user, namespace)
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "知识库权限服务异常")
		return nil
	}
	return r.WithContext(access.ContextWithScope(r.Context(), scope))
}

// requireScopeRead enforces read visibility on one tenant read endpoint. When no
// scope was resolved (no session, or ownership enforcement disabled) the request
// keeps the historical unrestricted behaviour.
func (s *Server) requireScopeRead(w http.ResponseWriter, r *http.Request) (access.ScopeSet, bool) {
	scope, ok := access.ScopeFromContext(r.Context())
	if !ok {
		return access.NewScopeSet(), true
	}
	if !scope.AllowsAnyRead() {
		writeDetail(w, http.StatusForbidden, access.MessageKnowledgeAccess)
		return scope, false
	}
	return scope, true
}

// requireScopeReadDocument checks one document-level read grant.
func (s *Server) requireScopeReadDocument(w http.ResponseWriter, r *http.Request, name string) bool {
	scope, ok := access.ScopeFromContext(r.Context())
	if !ok || scope.CanReadDocument(name) {
		return true
	}
	writeDetail(w, http.StatusForbidden, access.MessageKnowledgeAccess)
	return false
}

// requireScopeWriteDocument checks one document-level write grant.
func (s *Server) requireScopeWriteDocument(w http.ResponseWriter, r *http.Request, name string) bool {
	scope, ok := access.ScopeFromContext(r.Context())
	if !ok || scope.CanWriteDocument(name) {
		return true
	}
	writeDetail(w, http.StatusForbidden, access.MessageWriteRequired)
	return false
}

// requireScopeWriteBatch checks every file name of one upload batch.
func (s *Server) requireScopeWriteBatch(w http.ResponseWriter, r *http.Request, files []UploadFile) bool {
	scope, ok := access.ScopeFromContext(r.Context())
	if !ok {
		return true
	}
	for _, file := range files {
		if !scope.CanWriteDocument(file.Name) {
			writeDetail(w, http.StatusForbidden, access.MessageWriteRequired)
			return false
		}
	}
	return true
}

// scopedDocuments drops documents the caller may not read. It is a no-op for an
// unrestricted scope and for management credentials (no scope in context).
func scopedDocuments(r *http.Request, documents []map[string]any) []map[string]any {
	scope, ok := access.ScopeFromContext(r.Context())
	if !ok || !scope.Restricted() {
		return documents
	}
	filtered := make([]map[string]any, 0, len(documents))
	for _, document := range documents {
		name, _ := document["name"].(string)
		if name != "" && scope.CanReadDocument(name) {
			filtered = append(filtered, document)
		}
	}
	return filtered
}

// writeAccessError maps *access.Error onto the frozen HTTP contract and keeps
// persistence failures as a generic 500.
func writeAccessError(w http.ResponseWriter, err error) {
	var accessError *access.Error
	if !errors.As(err, &accessError) {
		writeDetail(w, http.StatusInternalServerError, "知识库权限服务异常")
		return
	}
	switch accessError.Kind {
	case access.KindValidation:
		writeDetail(w, http.StatusUnprocessableEntity, accessError.Message)
	case access.KindForbidden:
		writeDetail(w, http.StatusForbidden, accessError.Message)
	case access.KindNotFound:
		writeDetail(w, http.StatusNotFound, accessError.Message)
	case access.KindConflict:
		writeDetail(w, http.StatusConflict, accessError.Message)
	default:
		writeDetail(w, http.StatusInternalServerError, "知识库权限服务异常")
	}
}

// messageKBTemplateMissing is the frozen 409 detail for a missing default
// knowledge-base template.
const messageKBTemplateMissing = "管理员尚未配置默认知识库模板，请联系管理员创建知识库"

// knowledgeConflictMessage reports the message of an
// apierrors.KnowledgeConflictError (the runtime-kb side maps a missing default
// template onto it).
func knowledgeConflictMessage(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var conflict *apierrors.KnowledgeConflictError
	if errors.As(err, &conflict) {
		return conflict.Message, true
	}
	// The template-missing conflict is pinned by the contract even if it is
	// reported through a validation error by an older runtime adapter.
	if err.Error() == messageKBTemplateMissing {
		return messageKBTemplateMissing, true
	}
	return "", false
}

// writeKnowledgeBaseSelfCreateError maps the tenant self-create failures.
func writeKnowledgeBaseSelfCreateError(w http.ResponseWriter, err error) {
	if message, ok := knowledgeConflictMessage(err); ok {
		writeDetail(w, http.StatusConflict, message)
		return
	}
	writeCreateKnowledgeBaseError(w, err)
}
