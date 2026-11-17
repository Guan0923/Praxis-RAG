package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// GrantView is the JSON shape of one knowledge-base permission row.
type GrantView struct {
	UserID            string `json:"user_id"`
	Username          string `json:"username"`
	DocumentName      string `json:"document_name"`
	Permission        string `json:"permission"`
	GrantedBy         string `json:"granted_by"`
	GrantedByUsername string `json:"granted_by_username"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

// InvitationView is the JSON shape of one share invitation.
type InvitationView struct {
	ID              string   `json:"id"`
	Namespace       string   `json:"namespace"`
	InviterUserID   string   `json:"inviter_user_id"`
	InviterUsername string   `json:"inviter_username"`
	InviteeUserID   string   `json:"invitee_user_id"`
	InviteeUsername string   `json:"invitee_username"`
	Permission      string   `json:"permission"`
	Scope           string   `json:"scope"`
	DocumentNames   []string `json:"document_names"`
	Status          string   `json:"status"`
	CreatedAt       string   `json:"created_at"`
	ExpiresAt       string   `json:"expires_at"`
	AcceptedAt      *string  `json:"accepted_at"`
}

// UserView is the minimal account projection used by the invite picker.
type UserView struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// ScopeView renders one effective scope as JSON.
type ScopeView struct {
	WholeRead      bool     `json:"whole_read"`
	WholeWrite     bool     `json:"whole_write"`
	ReadDocuments  []string `json:"read_documents"`
	WriteDocuments []string `json:"write_documents"`
}

// AccessView is the effective access of the current account in one knowledge
// base, used by the Console to decide which controls to show.
type AccessView struct {
	Namespace string    `json:"namespace"`
	Owner     bool      `json:"owner"`
	CanManage bool      `json:"can_manage"`
	CanShare  bool      `json:"can_share"`
	Role      string    `json:"role"`
	CanRead   bool      `json:"can_read"`
	CanWrite  bool      `json:"can_write"`
	Scope     ScopeView `json:"scope"`
}

// CreateInvitationRequest is one validated invitation creation payload.
type CreateInvitationRequest struct {
	Namespace     string
	Username      string
	Permission    string
	Scope         string
	DocumentNames []string
}

// Describe returns the caller's effective access view for one knowledge base.
func (s *Service) Describe(ctx context.Context, user state.User, namespace string) (AccessView, error) {
	scope, err := s.ScopeSet(ctx, user, namespace)
	if err != nil {
		return AccessView{}, err
	}
	owner, err := s.knowledgeBases.OwnerOf(ctx, namespace)
	if err != nil {
		return AccessView{}, err
	}
	manage, err := s.CanManage(ctx, user, namespace)
	if err != nil {
		return AccessView{}, err
	}
	canShare, err := s.CanShareAccount(ctx, user)
	if err != nil {
		return AccessView{}, err
	}
	return AccessView{
		Namespace: namespace,
		Owner:     owner != "" && owner == user.ID,
		CanManage: manage,
		CanShare:  canShare && manage,
		Role:      user.Role,
		CanRead:   scope.AllowsAnyRead(),
		CanWrite:  scope.HasAnyWrite(),
		Scope: ScopeView{
			WholeRead:      scope.ReadsWhole(),
			WholeWrite:     scope.WritesWhole(),
			ReadDocuments:  scope.ReadDocumentNames(),
			WriteDocuments: scope.WriteDocumentNames(),
		},
	}, nil
}

// ListGrants returns every grant of one knowledge base. Only the owner or an
// administrator may list them.
func (s *Service) ListGrants(ctx context.Context, user state.User, namespace string) ([]GrantView, error) {
	if err := s.RequireManagePermission(ctx, user, namespace); err != nil {
		return nil, err
	}
	rows, err := s.permissions.ListByNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}
	cache := map[string]string{}
	views := make([]GrantView, 0, len(rows))
	for _, row := range rows {
		views = append(views, GrantView{
			UserID:            row.GranteeUserID,
			Username:          s.username(ctx, cache, row.GranteeUserID),
			DocumentName:      row.DocumentName,
			Permission:        row.Permission,
			GrantedBy:         row.GrantedBy,
			GrantedByUsername: s.username(ctx, cache, row.GrantedBy),
			CreatedAt:         row.CreatedAt,
			UpdatedAt:         row.UpdatedAt,
		})
	}
	return views, nil
}

// RevokeGrant removes every grant of one account inside one knowledge base.
func (s *Service) RevokeGrant(ctx context.Context, user state.User, namespace, granteeUserID string) error {
	if err := s.RequireManagePermission(ctx, user, namespace); err != nil {
		return err
	}
	if strings.TrimSpace(granteeUserID) == "" {
		return NewError(KindValidation, MessageUserMissing)
	}
	removed, err := s.permissions.Delete(ctx, namespace, granteeUserID)
	if err != nil {
		return err
	}
	if removed == 0 {
		return NewError(KindNotFound, "授权不存在")
	}
	return nil
}

// ListNamespaceInvitations returns the invitations issued for one knowledge
// base. Only the owner or an administrator may list them.
func (s *Service) ListNamespaceInvitations(ctx context.Context, user state.User, namespace string) ([]InvitationView, error) {
	if err := s.RequireManagePermission(ctx, user, namespace); err != nil {
		return nil, err
	}
	rows, err := s.invitations.ListByNamespace(ctx, namespace, 200)
	if err != nil {
		return nil, err
	}
	cache := map[string]string{}
	views := make([]InvitationView, 0, len(rows))
	for _, row := range rows {
		views = append(views, s.invitationView(ctx, cache, row))
	}
	return views, nil
}

// CreateInvitation issues one share link. The returned token is the only copy of
// the raw link secret; the database stores its digest.
func (s *Service) CreateInvitation(
	ctx context.Context,
	inviter state.User,
	request CreateInvitationRequest,
) (InvitationView, string, error) {
	canShare, err := s.CanShareAccount(ctx, inviter)
	if err != nil {
		return InvitationView{}, "", err
	}
	if !canShare {
		return InvitationView{}, "", NewError(KindForbidden, MessageForbidden)
	}
	permission := strings.TrimSpace(request.Permission)
	if !state.IsPermission(permission) {
		return InvitationView{}, "", NewError(KindValidation, MessagePermissionBad)
	}
	scope := strings.TrimSpace(request.Scope)
	if !state.IsInvitationScope(scope) {
		return InvitationView{}, "", NewError(KindValidation, MessageScopeBad)
	}
	documents, err := normalizeDocumentNames(scope, request.DocumentNames)
	if err != nil {
		return InvitationView{}, "", err
	}
	namespace := strings.TrimSpace(request.Namespace)
	if namespace == "" {
		return InvitationView{}, "", NewError(KindValidation, MessageKnowledgeMissing)
	}
	if err := s.RequireSharePermission(ctx, inviter, namespace); err != nil {
		return InvitationView{}, "", err
	}
	exists, err := s.knowledgeBases.Exists(ctx, namespace)
	if err != nil {
		return InvitationView{}, "", err
	}
	if !exists {
		return InvitationView{}, "", NewError(KindNotFound, MessageKnowledgeMissing)
	}
	username := strings.ToLower(strings.TrimSpace(request.Username))
	if username == "" {
		return InvitationView{}, "", NewError(KindValidation, MessageUserMissing)
	}
	invitee, _, err := s.users.ByUsername(ctx, username)
	if err != nil {
		return InvitationView{}, "", err
	}
	if invitee == nil {
		return InvitationView{}, "", NewError(KindNotFound, MessageUserMissing)
	}
	if !invitee.Enabled {
		return InvitationView{}, "", NewError(KindConflict, MessageUserDisabled)
	}
	if invitee.ID == inviter.ID {
		return InvitationView{}, "", NewError(KindValidation, MessageSelfInvite)
	}
	// Re-issuing an invitation invalidates the older pending link so only the
	// newest message can be accepted.
	if _, err := s.invitations.RevokePending(ctx, namespace, invitee.ID); err != nil {
		return InvitationView{}, "", err
	}
	token, digest, err := newInvitationToken()
	if err != nil {
		return InvitationView{}, "", err
	}
	created, err := s.invitations.Create(ctx, state.Invitation{
		Namespace:     namespace,
		InviterUserID: inviter.ID,
		InviteeUserID: invitee.ID,
		Permission:    permission,
		Scope:         scope,
		DocumentNames: documents,
		TokenDigest:   digest,
		Status:        state.InvitationPending,
		ExpiresAt:     isoTime(s.now().UTC().Add(s.invitationTTL)),
	})
	if err != nil {
		return InvitationView{}, "", err
	}
	cache := map[string]string{}
	return s.invitationView(ctx, cache, created), token, nil
}

// Inbox returns the invitations addressed to one account, pending first.
func (s *Service) Inbox(ctx context.Context, user state.User) ([]InvitationView, error) {
	rows, err := s.invitations.ListByInvitee(ctx, user.ID, 100)
	if err != nil {
		return nil, err
	}
	cache := map[string]string{}
	views := make([]InvitationView, 0, len(rows))
	for _, row := range rows {
		views = append(views, s.invitationView(ctx, cache, row))
	}
	return views, nil
}

// PreviewInvitation resolves one link token for the logged-in invitee.
func (s *Service) PreviewInvitation(ctx context.Context, user state.User, token string) (InvitationView, error) {
	invitation, err := s.invitationByToken(ctx, user, token)
	if err != nil {
		return InvitationView{}, err
	}
	cache := map[string]string{}
	return s.invitationView(ctx, cache, *invitation), nil
}

// AcceptInvitation turns one pending invitation into effective grants.
func (s *Service) AcceptInvitation(ctx context.Context, user state.User, token string) (InvitationView, error) {
	invitation, err := s.invitationByToken(ctx, user, token)
	if err != nil {
		return InvitationView{}, err
	}
	cache := map[string]string{}
	switch invitation.Status {
	case state.InvitationAccepted:
		// Accepting twice is idempotent: the grants already exist.
		return s.invitationView(ctx, cache, *invitation), nil
	case state.InvitationDeclined, state.InvitationRevoked:
		return InvitationView{}, NewError(KindConflict, MessageInvitationUsed)
	}
	if s.expired(*invitation) {
		_, _ = s.invitations.SetStatus(ctx, invitation.ID, state.InvitationRevoked, "")
		return InvitationView{}, NewError(KindConflict, MessageInvitationExpiry)
	}
	// A paid inviter may have expired between sending and acceptance. Re-check
	// the inviter's entitlement now; a manual VIP role keeps its historical
	// sharing ability, while an expired Plus/Pro invitation must not create a
	// new grant.
	inviter, err := s.users.ByID(ctx, invitation.InviterUserID)
	if err != nil {
		return InvitationView{}, err
	}
	if inviter == nil || !inviter.Enabled {
		return InvitationView{}, NewError(KindConflict, MessageInviterExpired)
	}
	canShare, err := s.CanShareAccount(ctx, *inviter)
	if err != nil {
		return InvitationView{}, err
	}
	if !canShare {
		return InvitationView{}, NewError(KindConflict, MessageInviterExpired)
	}
	grants := make([]state.Permission, 0, len(invitation.DocumentNames)+1)
	if invitation.Scope == state.InvitationScopeAll || len(invitation.DocumentNames) == 0 {
		grants = append(grants, state.Permission{
			Namespace:     invitation.Namespace,
			GranteeUserID: user.ID,
			DocumentName:  "",
			Permission:    invitation.Permission,
			GrantedBy:     invitation.InviterUserID,
		})
	} else {
		for _, name := range invitation.DocumentNames {
			grants = append(grants, state.Permission{
				Namespace:     invitation.Namespace,
				GranteeUserID: user.ID,
				DocumentName:  name,
				Permission:    invitation.Permission,
				GrantedBy:     invitation.InviterUserID,
			})
		}
	}
	if err := s.permissions.UpsertMany(ctx, grants); err != nil {
		return InvitationView{}, err
	}
	acceptedAt := s.nowISO()
	if _, err := s.invitations.SetStatus(ctx, invitation.ID, state.InvitationAccepted, acceptedAt); err != nil {
		return InvitationView{}, err
	}
	invitation.Status = state.InvitationAccepted
	invitation.AcceptedAt = &acceptedAt
	return s.invitationView(ctx, cache, *invitation), nil
}

// DeclineInvitation marks one pending invitation as declined.
func (s *Service) DeclineInvitation(ctx context.Context, user state.User, token string) (InvitationView, error) {
	invitation, err := s.invitationByToken(ctx, user, token)
	if err != nil {
		return InvitationView{}, err
	}
	cache := map[string]string{}
	switch invitation.Status {
	case state.InvitationDeclined, state.InvitationRevoked:
		return s.invitationView(ctx, cache, *invitation), nil
	case state.InvitationAccepted:
		return InvitationView{}, NewError(KindConflict, MessageInvitationUsed)
	}
	if s.expired(*invitation) {
		_, _ = s.invitations.SetStatus(ctx, invitation.ID, state.InvitationRevoked, "")
		return InvitationView{}, NewError(KindConflict, MessageInvitationExpiry)
	}
	if _, err := s.invitations.SetStatus(ctx, invitation.ID, state.InvitationDeclined, ""); err != nil {
		return InvitationView{}, err
	}
	invitation.Status = state.InvitationDeclined
	return s.invitationView(ctx, cache, *invitation), nil
}

// RevokeInvitation withdraws one pending invitation. The inviter, the owner of
// the knowledge base, and administrators may revoke it.
func (s *Service) RevokeInvitation(ctx context.Context, user state.User, invitationID string) error {
	invitation, err := s.invitations.ByID(ctx, strings.TrimSpace(invitationID))
	if err != nil {
		return err
	}
	if invitation == nil {
		return NewError(KindNotFound, MessageInvitationGone)
	}
	manage, err := s.CanManage(ctx, user, invitation.Namespace)
	if err != nil {
		return err
	}
	if invitation.InviterUserID != user.ID && !manage {
		return NewError(KindForbidden, MessageForbidden)
	}
	if invitation.Status != state.InvitationPending {
		return NewError(KindConflict, MessageInvitationUsed)
	}
	if _, err := s.invitations.SetStatus(ctx, invitation.ID, state.InvitationRevoked, ""); err != nil {
		return err
	}
	return nil
}

// ListInvitableUsers returns the enabled accounts an inviter may pick from.
func (s *Service) ListInvitableUsers(ctx context.Context, inviter state.User) ([]UserView, error) {
	canShare, err := s.CanShareAccount(ctx, inviter)
	if err != nil {
		return nil, err
	}
	if !canShare {
		return nil, NewError(KindForbidden, MessageForbidden)
	}
	users, err := s.users.List(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]UserView, 0, len(users))
	for _, user := range users {
		if !user.Enabled || user.ID == inviter.ID {
			continue
		}
		views = append(views, UserView{ID: user.ID, Username: user.Username, Role: user.Role})
	}
	return views, nil
}

// invitationByToken resolves one raw link token and checks the invitee.
func (s *Service) invitationByToken(ctx context.Context, user state.User, token string) (*state.Invitation, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, NewError(KindValidation, MessageInvitationGone)
	}
	invitation, err := s.invitations.ByTokenDigest(ctx, tokenDigest(token))
	if err != nil {
		return nil, err
	}
	if invitation == nil {
		return nil, NewError(KindNotFound, MessageInvitationGone)
	}
	if invitation.InviteeUserID != user.ID {
		return nil, NewError(KindForbidden, MessageInvitationOwner)
	}
	return invitation, nil
}

// expired reports whether a pending invitation passed its expiry.
func (s *Service) expired(invitation state.Invitation) bool {
	return invitation.Status == state.InvitationPending &&
		invitation.ExpiresAt != "" && invitation.ExpiresAt <= s.nowISO()
}

// invitationView enriches one row with usernames and the effective status.
func (s *Service) invitationView(ctx context.Context, cache map[string]string, invitation state.Invitation) InvitationView {
	status := invitation.Status
	if s.expired(invitation) {
		status = "expired"
	}
	documents := invitation.DocumentNames
	if documents == nil {
		documents = []string{}
	}
	return InvitationView{
		ID:              invitation.ID,
		Namespace:       invitation.Namespace,
		InviterUserID:   invitation.InviterUserID,
		InviterUsername: s.username(ctx, cache, invitation.InviterUserID),
		InviteeUserID:   invitation.InviteeUserID,
		InviteeUsername: s.username(ctx, cache, invitation.InviteeUserID),
		Permission:      invitation.Permission,
		Scope:           invitation.Scope,
		DocumentNames:   documents,
		Status:          status,
		CreatedAt:       invitation.CreatedAt,
		ExpiresAt:       invitation.ExpiresAt,
		AcceptedAt:      invitation.AcceptedAt,
	}
}

// username resolves one account name with a per-call cache.
func (s *Service) username(ctx context.Context, cache map[string]string, id string) string {
	if id == "" {
		return ""
	}
	if name, ok := cache[id]; ok {
		return name
	}
	user, err := s.users.ByID(ctx, id)
	if err != nil || user == nil {
		cache[id] = ""
		return ""
	}
	cache[id] = user.Username
	return user.Username
}

// normalizeDocumentNames validates request document names for the scope.
func normalizeDocumentNames(scope string, names []string) ([]string, error) {
	if scope == state.InvitationScopeAll {
		return []string{}, nil
	}
	unique := make(map[string]struct{}, len(names))
	result := make([]string, 0, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if !IsDocumentNameValid(trimmed) {
			return nil, NewError(KindValidation, MessageDocumentsBad+trimmed)
		}
		if _, ok := unique[trimmed]; ok {
			continue
		}
		unique[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	if len(result) == 0 {
		return nil, NewError(KindValidation, MessageDocumentsEmpty)
	}
	if len(result) > 200 {
		return nil, NewError(KindValidation, MessageDocumentsTooMany)
	}
	sort.Strings(result)
	return result, nil
}

// newInvitationToken returns the raw link token and its stored digest.
func newInvitationToken() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", NewError(KindConflict, "邀请无法创建")
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, tokenDigest(token), nil
}

// tokenDigest is the SHA-256 hex digest stored for one invitation token.
func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// isoTime formats a UTC timestamp the same way the state layer stores them.
func isoTime(moment time.Time) string {
	return moment.UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

// nowISO returns the service clock as a stored timestamp.
func (s *Service) nowISO() string { return isoTime(s.now().UTC()) }
