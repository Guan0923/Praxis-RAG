package access

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sort"
	"strings"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// User-group message constants, kept stable across handlers and tests.
const (
	MessageGroupCreateForbidden = "需要 VIP 或管理员权限才能创建用户组"
	MessageGroupNotFound        = "用户组不存在"
	MessageGroupDissolved       = "用户组已解散"
	MessageGroupNameBad         = "用户组名称不合法"
	MessageGroupNameTaken       = "同名用户组已存在"
	MessageGroupNotOwner        = "仅组创建者可以管理该用户组"
	MessageGroupMemberMissing   = "该用户不是组成员"
	MessageGroupInviteSelf      = "不能邀请自己加入用户组"
	MessageGroupInviteDisabled  = "该账号已被禁用"
	MessageGroupUserMissing     = "用户不存在"
	MessageGroupMaskBad         = "权限位只能是 1、2、4、8 的非空组合"
	MessageGroupCreateFileOnly  = "新增权限只能设置在整库授权上"
	MessageGroupNamespaceOwner  = "只能为自己创建的知识库设置用户组权限"
	MessageGroupScopeBad        = "范围只能是 all 或 files"
	MessageGroupFilesEmpty      = "请选择要授权的文件"
	MessageGroupFilesBad        = "文件名不合法："
)

// GroupView is the JSON shape of one user group.
type GroupView struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	OwnerUserID    string `json:"owner_user_id"`
	OwnerUsername  string `json:"owner_username"`
	Status         string `json:"status"`
	MemberCount    int    `json:"member_count"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	DissolvedAt    *string `json:"dissolved_at"`
}

// GroupMemberView is the JSON shape of one membership row.
type GroupMemberView struct {
	UserID    string `json:"user_id"`
	Username string `json:"username"`
	Role      string `json:"role"`
	Enabled   bool   `json:"enabled"`
	JoinedAt  string `json:"joined_at"`
}

// GroupInvitationView is the JSON shape of one membership invitation.
type GroupInvitationView struct {
	ID              string   `json:"id"`
	GroupID         string   `json:"group_id"`
	GroupName       string   `json:"group_name"`
	InviterUserID   string   `json:"inviter_user_id"`
	InviterUsername string   `json:"inviter_username"`
	InviteeUserID   string   `json:"invitee_user_id"`
	InviteeUsername string   `json:"invitee_username"`
	Status          string   `json:"status"`
	CreatedAt       string   `json:"created_at"`
	ExpiresAt       string   `json:"expires_at"`
	AcceptedAt      *string  `json:"accepted_at"`
}

// GroupGrantView is the JSON shape of one knowledge-base group grant row.
type GroupGrantView struct {
	GroupID          string `json:"group_id"`
	GroupName        string `json:"group_name"`
	Namespace        string `json:"namespace"`
	DocumentName     string `json:"document_name"`
	AllowMask        int    `json:"allow_mask"`
	GrantedBy        string `json:"granted_by"`
	GrantedByUsername string `json:"granted_by_username"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

// PermissionSourceView explains one bit of the effective permission with its
// origin, so an administrator can trace why access remains after removing a
// group.
type PermissionSourceView struct {
	Bit        int    `json:"bit"`
	BitName    string `json:"bit_name"`
	Kind       string `json:"kind"` // "role" | "personal" | "group"
	Origin     string `json:"origin"`
	DocumentName string `json:"document_name"`
}

// CreateGroupRequest is one validated group creation payload.
type CreateGroupRequest struct {
	Name string
}

// CreateGroupInvitationRequest is one validated group invite payload.
type CreateGroupInvitationRequest struct {
	GroupID  string
	Username string
}

// SetGroupGrantRequest is one validated group grant payload.
type SetGroupGrantRequest struct {
	Namespace    string
	GroupID      string
	DocumentNames []string
	AllowMask    int
}

// maxGroupNameLength bounds the group name; one rune count keeps multibyte
// names predictable.
const maxGroupNameLength = 100

// IsGroupNameValid applies the shared group-name rules.
func IsGroupNameValid(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || len([]rune(trimmed)) > maxGroupNameLength {
		return false
	}
	return !strings.ContainsAny(trimmed, "\x00\n\r")
}

// mayManageGroups reports whether the account may create groups and invite
// members: enabled vip/admin roles only. A demoted owner keeps its
// administrative rights (remove/revoke/dissolve) through requireGroupOwner.
func (s *Service) mayManageGroups(user state.User) bool {
	return user.Enabled && user.CanShare()
}

// CreateGroup creates one group owned by the caller. The creator is the group
// administrator; they do not need to be a member.
func (s *Service) CreateGroup(ctx context.Context, user state.User, request CreateGroupRequest) (GroupView, error) {
	if !s.mayManageGroups(user) {
		return GroupView{}, NewError(KindForbidden, MessageGroupCreateForbidden)
	}
	name := strings.TrimSpace(request.Name)
	if !IsGroupNameValid(name) {
		return GroupView{}, NewError(KindValidation, MessageGroupNameBad)
	}
	// One account cannot hold two active groups with the same name.
	owned, err := s.groups.ListByOwner(ctx, user.ID)
	if err != nil {
		return GroupView{}, err
	}
	for _, group := range owned {
		if group.Status == state.UserGroupActive && group.Name == name {
			return GroupView{}, NewError(KindConflict, MessageGroupNameTaken)
		}
	}
	created, err := s.groups.Create(ctx, state.UserGroup{
		Name:        name,
		OwnerUserID: user.ID,
		Status:      state.UserGroupActive,
	})
	if err != nil {
		return GroupView{}, err
	}
	return s.groupView(ctx, map[string]string{}, created, 0), nil
}

// ListGroups returns the groups the caller created or joined. Every account
// may call it: members need their joined list, owners need administration.
func (s *Service) ListGroups(ctx context.Context, user state.User) (owned []GroupView, joined []GroupView, err error) {
	ownedRows, err := s.groups.ListByOwner(ctx, user.ID)
	if err != nil {
		return nil, nil, err
	}
	cache := map[string]string{}
	owned = make([]GroupView, 0, len(ownedRows))
	for _, row := range ownedRows {
		members, membersErr := s.members.ListByGroup(ctx, row.ID)
		if membersErr != nil {
			return nil, nil, membersErr
		}
		owned = append(owned, s.groupView(ctx, cache, row, len(members)))
	}
	joinedRows, err := s.members.ListJoinedByUser(ctx, user.ID)
	if err != nil {
		return nil, nil, err
	}
	// The same group can be both owned and joined; drop owned duplicates from
	// the joined list so the Console does not double-render administration.
	ownedByID := make(map[string]struct{}, len(ownedRows))
	for _, row := range ownedRows {
		ownedByID[row.ID] = struct{}{}
	}
	joined = make([]GroupView, 0, len(joinedRows))
	for _, row := range joinedRows {
		if _, ok := ownedByID[row.ID]; ok {
			continue
		}
		members, membersErr := s.members.ListByGroup(ctx, row.ID)
		if membersErr != nil {
			return nil, nil, membersErr
		}
		joined = append(joined, s.groupView(ctx, cache, row, len(members)))
	}
	return owned, joined, nil
}

// GroupDetail returns one group with its members and pending invitations. The
// owner administers; a member sees the roster; anyone else gets a 404.
func (s *Service) GroupDetail(ctx context.Context, user state.User, groupID string) (GroupView, []GroupMemberView, []GroupInvitationView, error) {
	group, err := s.groups.ByID(ctx, strings.TrimSpace(groupID))
	if err != nil {
		return GroupView{}, nil, nil, err
	}
	if group == nil {
		return GroupView{}, nil, nil, NewError(KindNotFound, MessageGroupNotFound)
	}
	owner := group.OwnerUserID == user.ID
	if !owner {
		member, memberErr := s.members.Exists(ctx, group.ID, user.ID)
		if memberErr != nil {
			return GroupView{}, nil, nil, memberErr
		}
		if !member {
			return GroupView{}, nil, nil, NewError(KindNotFound, MessageGroupNotFound)
		}
	}
	cache := map[string]string{}
	members, err := s.members.ListByGroup(ctx, group.ID)
	if err != nil {
		return GroupView{}, nil, nil, err
	}
	memberViews := make([]GroupMemberView, 0, len(members))
	for _, member := range members {
		memberViews = append(memberViews, s.groupMemberView(ctx, cache, member))
	}
	var invitations []GroupInvitationView
	if owner {
		invitations, err = s.listGroupInvitations(ctx, cache, group)
		if err != nil {
			return GroupView{}, nil, nil, err
		}
	}
	return s.groupView(ctx, cache, *group, len(members)), memberViews, invitations, nil
}

// CreateGroupInvitation issues one membership invitation link. Only a group
// owner who still holds the vip/admin role may invite; the invitee must be an
// enabled account distinct from the inviter. Re-inviting the same account
// revokes its older pending link first.
func (s *Service) CreateGroupInvitation(
	ctx context.Context,
	inviter state.User,
	request CreateGroupInvitationRequest,
) (GroupInvitationView, string, error) {
	group, err := s.groups.ByID(ctx, strings.TrimSpace(request.GroupID))
	if err != nil {
		return GroupInvitationView{}, "", err
	}
	if group == nil {
		return GroupInvitationView{}, "", NewError(KindNotFound, MessageGroupNotFound)
	}
	if group.Status != state.UserGroupActive {
		return GroupInvitationView{}, "", NewError(KindConflict, MessageGroupDissolved)
	}
	if group.OwnerUserID != inviter.ID {
		return GroupInvitationView{}, "", NewError(KindForbidden, MessageGroupNotOwner)
	}
	if !s.mayManageGroups(inviter) {
		return GroupInvitationView{}, "", NewError(KindForbidden, MessageGroupCreateForbidden)
	}
	username := strings.ToLower(strings.TrimSpace(request.Username))
	if username == "" {
		return GroupInvitationView{}, "", NewError(KindValidation, MessageGroupUserMissing)
	}
	invitee, _, err := s.users.ByUsername(ctx, username)
	if err != nil {
		return GroupInvitationView{}, "", err
	}
	if invitee == nil {
		return GroupInvitationView{}, "", NewError(KindNotFound, MessageGroupUserMissing)
	}
	if !invitee.Enabled {
		return GroupInvitationView{}, "", NewError(KindConflict, MessageGroupInviteDisabled)
	}
	if invitee.ID == inviter.ID {
		return GroupInvitationView{}, "", NewError(KindValidation, MessageGroupInviteSelf)
	}
	// Re-inviting invalidates the older pending link so only the newest one
	// can be accepted.
	if _, err := s.groupInvites.RevokePending(ctx, group.ID, invitee.ID); err != nil {
		return GroupInvitationView{}, "", err
	}
	token, digest, err := newGroupInvitationToken()
	if err != nil {
		return GroupInvitationView{}, "", err
	}
	created, err := s.groupInvites.Create(ctx, state.GroupInvitation{
		GroupID:       group.ID,
		InviterUserID: inviter.ID,
		InviteeUserID: invitee.ID,
		TokenDigest:   digest,
		Status:        state.GroupInvitationPending,
		ExpiresAt:     isoTime(s.now().UTC().Add(s.invitationTTL)),
	})
	if err != nil {
		return GroupInvitationView{}, "", err
	}
	cache := map[string]string{}
	view := s.groupInvitationView(ctx, cache, *group, created)
	return view, token, nil
}

// GroupInvitationInbox returns the group invitations addressed to one
// account, pending first.
func (s *Service) GroupInvitationInbox(ctx context.Context, user state.User) ([]GroupInvitationView, error) {
	rows, err := s.groupInvites.ListByInvitee(ctx, user.ID, 100)
	if err != nil {
		return nil, err
	}
	cache := map[string]string{}
	views := make([]GroupInvitationView, 0, len(rows))
	for _, row := range rows {
		group, groupErr := s.groups.ByID(ctx, row.GroupID)
		if groupErr != nil {
			return nil, groupErr
		}
		if group == nil {
			continue
		}
		views = append(views, s.groupInvitationView(ctx, cache, *group, row))
	}
	return views, nil
}

// PreviewGroupInvitation resolves one membership link for its invitee.
func (s *Service) PreviewGroupInvitation(ctx context.Context, user state.User, token string) (GroupInvitationView, error) {
	invitation, err := s.groupInvitationByToken(ctx, user, token)
	if err != nil {
		return GroupInvitationView{}, err
	}
	group, err := s.groups.ByID(ctx, invitation.GroupID)
	if err != nil {
		return GroupInvitationView{}, err
	}
	if group == nil {
		return GroupInvitationView{}, NewError(KindNotFound, MessageGroupNotFound)
	}
	cache := map[string]string{}
	return s.groupInvitationView(ctx, cache, *group, *invitation), nil
}

// AcceptGroupInvitation turns one pending membership invitation into a
// membership inside the repository's single transaction. Idempotent retries of
// an already-accepted link never re-add a member that left or was removed.
func (s *Service) AcceptGroupInvitation(ctx context.Context, user state.User, token string) (GroupInvitationView, error) {
	invitation, err := s.groupInvitationByToken(ctx, user, token)
	if err != nil {
		return GroupInvitationView{}, err
	}
	group, err := s.groups.ByID(ctx, invitation.GroupID)
	if err != nil {
		return GroupInvitationView{}, err
	}
	if group == nil {
		return GroupInvitationView{}, NewError(KindNotFound, MessageGroupNotFound)
	}
	cache := map[string]string{}
	switch invitation.Status {
	case state.GroupInvitationAccepted:
		return s.groupInvitationView(ctx, cache, *group, *invitation), nil
	case state.GroupInvitationDeclined, state.GroupInvitationRevoked:
		return GroupInvitationView{}, NewError(KindConflict, MessageInvitationUsed)
	}
	if groupInvitationExpired(*invitation, s.nowISO()) {
		_, _ = s.groupInvites.RevokeByID(ctx, invitation.ID)
		return GroupInvitationView{}, NewError(KindConflict, MessageInvitationExpiry)
	}
	final, err := s.groupInvites.AcceptPending(ctx, invitation.ID, s.nowISO(), s.nowISO())
	if err != nil {
		return GroupInvitationView{}, mapGroupInvitationError(err)
	}
	if final == nil {
		return GroupInvitationView{}, NewError(KindNotFound, MessageInvitationGone)
	}
	if final.Status != state.GroupInvitationAccepted {
		return GroupInvitationView{}, NewError(KindConflict, MessageInvitationUsed)
	}
	return s.groupInvitationView(ctx, cache, *group, *final), nil
}

// DeclineGroupInvitation marks one pending membership invitation as declined.
func (s *Service) DeclineGroupInvitation(ctx context.Context, user state.User, token string) (GroupInvitationView, error) {
	invitation, err := s.groupInvitationByToken(ctx, user, token)
	if err != nil {
		return GroupInvitationView{}, err
	}
	group, err := s.groups.ByID(ctx, invitation.GroupID)
	if err != nil {
		return GroupInvitationView{}, err
	}
	if group == nil {
		return GroupInvitationView{}, NewError(KindNotFound, MessageGroupNotFound)
	}
	cache := map[string]string{}
	switch invitation.Status {
	case state.GroupInvitationDeclined, state.GroupInvitationRevoked:
		return s.groupInvitationView(ctx, cache, *group, *invitation), nil
	case state.GroupInvitationAccepted:
		return GroupInvitationView{}, NewError(KindConflict, MessageInvitationUsed)
	}
	if groupInvitationExpired(*invitation, s.nowISO()) {
		_, _ = s.groupInvites.RevokeByID(ctx, invitation.ID)
		return GroupInvitationView{}, NewError(KindConflict, MessageInvitationExpiry)
	}
	changed, err := s.groupInvites.DeclinePending(ctx, invitation.ID)
	if err != nil {
		return GroupInvitationView{}, err
	}
	if !changed {
		return GroupInvitationView{}, NewError(KindConflict, MessageInvitationUsed)
	}
	invitation.Status = state.GroupInvitationDeclined
	return s.groupInvitationView(ctx, cache, *group, *invitation), nil
}

// RevokeGroupInvitation withdraws one pending membership invitation. The
// group owner may revoke it even after losing the vip/admin role.
func (s *Service) RevokeGroupInvitation(ctx context.Context, user state.User, invitationID string) error {
	invitation, err := s.groupInvites.ByID(ctx, strings.TrimSpace(invitationID))
	if err != nil {
		return err
	}
	if invitation == nil {
		return NewError(KindNotFound, MessageInvitationGone)
	}
	if err := s.requireGroupOwner(ctx, user, invitation.GroupID); err != nil {
		return err
	}
	changed, err := s.groupInvites.RevokeByID(ctx, invitation.ID)
	if err != nil {
		return err
	}
	if !changed {
		return NewError(KindConflict, MessageInvitationUsed)
	}
	return nil
}

// RemoveGroupMember removes one member from one group. The group owner may do
// it even after being demoted to a plain user.
func (s *Service) RemoveGroupMember(ctx context.Context, user state.User, groupID, memberUserID string) error {
	group, err := s.requireGroupOwnerGroup(ctx, user, groupID)
	if err != nil {
		return err
	}
	if group.Status != state.UserGroupActive {
		return NewError(KindConflict, MessageGroupDissolved)
	}
	memberUserID = strings.TrimSpace(memberUserID)
	if memberUserID == "" {
		return NewError(KindValidation, MessageGroupUserMissing)
	}
	removed, err := s.members.Remove(ctx, group.ID, memberUserID)
	if err != nil {
		return err
	}
	if !removed {
		return NewError(KindNotFound, MessageGroupMemberMissing)
	}
	return nil
}

// LeaveGroup removes the caller's own membership. Every member may leave,
// including across a dissolved group (the row is already gone then).
func (s *Service) LeaveGroup(ctx context.Context, user state.User, groupID string) error {
	group, err := s.groups.ByID(ctx, strings.TrimSpace(groupID))
	if err != nil {
		return err
	}
	if group == nil {
		return NewError(KindNotFound, MessageGroupNotFound)
	}
	removed, err := s.members.Remove(ctx, group.ID, user.ID)
	if err != nil {
		return err
	}
	if !removed {
		return NewError(KindNotFound, MessageGroupMemberMissing)
	}
	return nil
}

// DissolveGroup deactivates one group and clears its members, pending
// invitations and knowledge-base grants. The owner may dissolve even after
// being demoted to a plain user.
func (s *Service) DissolveGroup(ctx context.Context, user state.User, groupID string) error {
	if err := s.requireGroupOwner(ctx, user, groupID); err != nil {
		return err
	}
	changed, err := s.groups.Dissolve(ctx, strings.TrimSpace(groupID))
	if err != nil {
		return err
	}
	if !changed {
		return NewError(KindConflict, MessageGroupDissolved)
	}
	return nil
}

// ListGroupGrants returns every group grant of one knowledge base. The
// knowledge-base owner or administrator may list them.
func (s *Service) ListGroupGrants(ctx context.Context, user state.User, namespace string) ([]GroupGrantView, error) {
	if err := s.RequireManagePermission(ctx, user, namespace); err != nil {
		return nil, err
	}
	rows, err := s.groupPerms.ListByNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}
	cache := map[string]string{}
	views := make([]GroupGrantView, 0, len(rows))
	for _, row := range rows {
		group, groupErr := s.groups.ByID(ctx, row.GroupID)
		if groupErr != nil {
			return nil, groupErr
		}
		if group == nil {
			continue
		}
		views = append(views, GroupGrantView{
			GroupID:           row.GroupID,
			GroupName:         group.Name,
			Namespace:         row.Namespace,
			DocumentName:      row.DocumentName,
			AllowMask:         row.AllowMask,
			GrantedBy:         row.GrantedBy,
			GrantedByUsername: s.username(ctx, cache, row.GrantedBy),
			CreatedAt:         row.CreatedAt,
			UpdatedAt:         row.UpdatedAt,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].GroupName != views[j].GroupName {
			return views[i].GroupName < views[j].GroupName
		}
		return views[i].DocumentName < views[j].DocumentName
	})
	return views, nil
}

// SetGroupGrant writes one whole-library or per-file group grant. The caller
// must still hold the vip/admin role, own the group, and own the knowledge
// base (administrators manage every knowledge base).
func (s *Service) SetGroupGrant(ctx context.Context, user state.User, request SetGroupGrantRequest) error {
	if !s.mayManageGroups(user) {
		return NewError(KindForbidden, MessageGroupCreateForbidden)
	}
	namespace := strings.TrimSpace(request.Namespace)
	if namespace == "" {
		return NewError(KindValidation, MessageKnowledgeMissing)
	}
	if !IsValidAllowMask(request.AllowMask) {
		return NewError(KindValidation, MessageGroupMaskBad)
	}
	// The knowledge base must exist and belong to the caller (an
	// administrator manages every knowledge base).
	if err := s.RequireManagePermission(ctx, user, namespace); err != nil {
		return err
	}
	exists, err := s.knowledgeBases.Exists(ctx, namespace)
	if err != nil {
		return err
	}
	if !exists {
		return NewError(KindNotFound, MessageKnowledgeMissing)
	}
	group, err := s.groups.ByID(ctx, strings.TrimSpace(request.GroupID))
	if err != nil {
		return err
	}
	if group == nil {
		return NewError(KindNotFound, MessageGroupNotFound)
	}
	if group.Status != state.UserGroupActive {
		return NewError(KindConflict, MessageGroupDissolved)
	}
	if group.OwnerUserID != user.ID {
		return NewError(KindForbidden, MessageGroupNotOwner)
	}
	for _, name := range request.DocumentNames {
		if !IsDocumentNameValid(name) {
			return NewError(KindValidation, MessageGroupFilesBad+name)
		}
		if _, err := s.documents.Get(ctx, namespace, name); err != nil {
			return err
		}
	}
	if request.AllowMask&PermissionBitCreate != 0 && len(request.DocumentNames) > 0 {
		return NewError(KindValidation, MessageGroupCreateFileOnly)
	}
	if len(request.DocumentNames) == 0 {
		return s.groupPerms.Upsert(ctx, state.KBGroupPermission{
			Namespace:    namespace,
			GroupID:      group.ID,
			DocumentName: "",
			AllowMask:    request.AllowMask,
			GrantedBy:    user.ID,
		})
	}
	for _, name := range request.DocumentNames {
		mask := request.AllowMask &^ PermissionBitCreate
		if mask == 0 {
			// create-only was already refused above; a zero file mask cannot
			// happen, but keep the repository honest.
			continue
		}
		if err := s.groupPerms.Upsert(ctx, state.KBGroupPermission{
			Namespace:    namespace,
			GroupID:      group.ID,
			DocumentName: name,
			AllowMask:    mask,
			GrantedBy:    user.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// RevokeGroupGrant removes every group grant of one group inside one
// knowledge base. The knowledge-base owner may clean up even without the
// vip/admin role, mirroring personal grant revocation.
func (s *Service) RevokeGroupGrant(ctx context.Context, user state.User, namespace, groupID string) error {
	if err := s.RequireManagePermission(ctx, user, namespace); err != nil {
		return err
	}
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return NewError(KindValidation, MessageGroupNotFound)
	}
	removed, err := s.groupPerms.Delete(ctx, namespace, groupID)
	if err != nil {
		return err
	}
	if removed == 0 {
		return NewError(KindNotFound, "组授权不存在")
	}
	return nil
}

// DescribePermissionSources explains every set bit of the caller's effective
// document mask with its origin: role, personal grant, or a named group. It
// helps an administrator understand why removing one group left other access.
func (s *Service) DescribePermissionSources(ctx context.Context, user state.User, namespace, documentName string) ([]PermissionSourceView, error) {
	scope, err := s.ScopeSet(ctx, user, namespace)
	if err != nil {
		return nil, err
	}
	owner, err := s.knowledgeBases.OwnerOf(ctx, namespace)
	if err != nil {
		return nil, err
	}
	mask := scope.DocumentAllowMask(documentName)
	var sources []PermissionSourceView
	isOwner := owner != "" && owner == user.ID
	if (isOwner || user.IsAdmin()) && user.Enabled {
		if mask&PermissionBitRead != 0 {
			sources = append(sources, PermissionSourceView{
				Bit: PermissionBitRead, BitName: "read", Kind: "role",
				Origin: roleOwnerLabel(isOwner, user.Role), DocumentName: documentName,
			})
		}
		if mask&PermissionBitCreate != 0 {
			sources = append(sources, PermissionSourceView{
				Bit: PermissionBitCreate, BitName: "create", Kind: "role",
				Origin: roleOwnerLabel(isOwner, user.Role), DocumentName: documentName,
			})
		}
		if mask&PermissionBitUpdate != 0 {
			sources = append(sources, PermissionSourceView{
				Bit: PermissionBitUpdate, BitName: "update", Kind: "role",
				Origin: roleOwnerLabel(isOwner, user.Role), DocumentName: documentName,
			})
		}
		if mask&PermissionBitDelete != 0 {
			sources = append(sources, PermissionSourceView{
				Bit: PermissionBitDelete, BitName: "delete", Kind: "role",
				Origin: roleOwnerLabel(isOwner, user.Role), DocumentName: documentName,
			})
		}
		return sources, nil
	}
	// Personal grants.
	grants, err := s.permissions.ListFor(ctx, namespace, user.ID)
	if err != nil {
		return nil, err
	}
	personalMask := 0
	for _, grant := range grants {
		if grant.DocumentName == "" || grant.DocumentName == documentName {
			if grant.Permission == state.PermissionWrite {
				personalMask |= PermissionBitRead | PermissionBitUpdate | PermissionBitDelete
			} else {
				personalMask |= PermissionBitRead
			}
		}
	}
	// Whole-library personal create rides on a whole-library write grant; the
	// loop above already folded it when documentName is empty.
	for _, bit := range []struct {
		value int
		name  string
	}{
		{PermissionBitRead, "read"}, {PermissionBitCreate, "create"},
		{PermissionBitUpdate, "update"}, {PermissionBitDelete, "delete"},
	} {
		if personalMask&bit.value != 0 {
			sources = append(sources, PermissionSourceView{
				Bit: bit.value, BitName: bit.name, Kind: "personal",
				Origin: "个人授权", DocumentName: documentName,
			})
		}
	}
	// Group grants.
	groupScope, err := s.groupScopeDetail(ctx, user.ID, namespace, documentName)
	if err != nil {
		return nil, err
	}
	for _, detail := range groupScope {
		sources = append(sources, detail)
	}
	return sources, nil
}

// groupScopeDetail lists per-group contribution to one document's mask.
func (s *Service) groupScopeDetail(ctx context.Context, userID, namespace, documentName string) ([]PermissionSourceView, error) {
	if s.groups == nil || s.groupPerms == nil || s.members == nil {
		return nil, nil
	}
	groupIDs, err := s.members.ActiveGroupIDsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(groupIDs) == 0 {
		return nil, nil
	}
	grants, err := s.groupPerms.ListForGroups(ctx, namespace, groupIDs)
	if err != nil {
		return nil, err
	}
	var sources []PermissionSourceView
	for _, grant := range grants {
		if grant.DocumentName != "" && grant.DocumentName != documentName {
			continue
		}
		group, groupErr := s.groups.ByID(ctx, grant.GroupID)
		if groupErr != nil {
			return nil, groupErr
		}
		if group == nil {
			continue
		}
		origin := "来自用户组「" + group.Name + "」"
		for _, bit := range []struct {
			value int
			name  string
		}{
			{PermissionBitRead, "read"}, {PermissionBitCreate, "create"},
			{PermissionBitUpdate, "update"}, {PermissionBitDelete, "delete"},
		} {
			if grant.AllowMask&bit.value != 0 {
				sources = append(sources, PermissionSourceView{
					Bit: bit.value, BitName: bit.name, Kind: "group",
					Origin: origin, DocumentName: grant.DocumentName,
				})
			}
		}
	}
	return sources, nil
}

// roleOwnerLabel renders the role/ownership origin.
func roleOwnerLabel(isOwner bool, role string) string {
	if isOwner {
		return "知识库创建者"
	}
	return "管理员角色"
}

// requireGroupOwner loads the group and enforces creator ownership. A demoted
// owner keeps the right to administer (remove members, revoke, dissolve).
func (s *Service) requireGroupOwnerGroup(ctx context.Context, user state.User, groupID string) (*state.UserGroup, error) {
	group, err := s.groups.ByID(ctx, strings.TrimSpace(groupID))
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, NewError(KindNotFound, MessageGroupNotFound)
	}
	if group.OwnerUserID != user.ID {
		return nil, NewError(KindForbidden, MessageGroupNotOwner)
	}
	return group, nil
}

// requireGroupOwner enforces ownership without loading the group row for the
// caller.
func (s *Service) requireGroupOwner(ctx context.Context, user state.User, groupID string) error {
	_, err := s.requireGroupOwnerGroup(ctx, user, groupID)
	return err
}

// groupInvitationByToken resolves one link for its bound invitee only.
func (s *Service) groupInvitationByToken(ctx context.Context, user state.User, token string) (*state.GroupInvitation, error) {
	digest := groupTokenDigest(token)
	invitation, err := s.groupInvites.ByTokenDigest(ctx, digest)
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

// groupInvitationExpired reports whether a pending link's TTL elapsed.
func groupInvitationExpired(invitation state.GroupInvitation, nowISO string) bool {
	return invitation.Status == state.GroupInvitationPending &&
		invitation.ExpiresAt != "" && invitation.ExpiresAt <= nowISO
}

// mapGroupInvitationError converts repository conflicts onto typed failures.
func mapGroupInvitationError(err error) error {
	switch {
	case errors.Is(err, state.ErrGroupInvitationExpired):
		return NewError(KindConflict, MessageInvitationExpiry)
	case errors.Is(err, state.ErrGroupDissolved):
		return NewError(KindConflict, MessageGroupDissolved)
	case errors.Is(err, state.ErrGroupInviterUnqualified):
		return NewError(KindConflict, MessageInviterExpired)
	default:
		return err
	}
}

// groupView enriches one group row with its owner username.
func (s *Service) groupView(ctx context.Context, cache map[string]string, group state.UserGroup, memberCount int) GroupView {
	return GroupView{
		ID:            group.ID,
		Name:          group.Name,
		OwnerUserID:   group.OwnerUserID,
		OwnerUsername: s.username(ctx, cache, group.OwnerUserID),
		Status:        group.Status,
		MemberCount:   memberCount,
		CreatedAt:     group.CreatedAt,
		UpdatedAt:     group.UpdatedAt,
		DissolvedAt:   group.DissolvedAt,
	}
}

// groupMemberView enriches one membership with the account projection.
func (s *Service) groupMemberView(ctx context.Context, cache map[string]string, member state.UserGroupMember) GroupMemberView {
	view := GroupMemberView{
		UserID:   member.UserID,
		JoinedAt: member.JoinedAt,
	}
	account, err := s.users.ByID(ctx, member.UserID)
	if err == nil && account != nil {
		view.Username = account.Username
		view.Role = account.Role
		view.Enabled = account.Enabled
	}
	return view
}

// groupInvitationView enriches one invitation row with usernames and the
// effective (expiry-aware) status.
func (s *Service) groupInvitationView(ctx context.Context, cache map[string]string, group state.UserGroup, invitation state.GroupInvitation) GroupInvitationView {
	status := invitation.Status
	if groupInvitationExpired(invitation, s.nowISO()) {
		status = "expired"
	}
	return GroupInvitationView{
		ID:              invitation.ID,
		GroupID:         invitation.GroupID,
		GroupName:       group.Name,
		InviterUserID:   invitation.InviterUserID,
		InviterUsername: s.username(ctx, cache, invitation.InviterUserID),
		InviteeUserID:   invitation.InviteeUserID,
		InviteeUsername: s.username(ctx, cache, invitation.InviteeUserID),
		Status:          status,
		CreatedAt:       invitation.CreatedAt,
		ExpiresAt:       invitation.ExpiresAt,
		AcceptedAt:      invitation.AcceptedAt,
	}
}

// listGroupInvitations renders the invitations of one group, pending first.
func (s *Service) listGroupInvitations(ctx context.Context, cache map[string]string, group *state.UserGroup) ([]GroupInvitationView, error) {
	rows, err := s.groupInvites.ListByGroup(ctx, group.ID, 200)
	if err != nil {
		return nil, err
	}
	views := make([]GroupInvitationView, 0, len(rows))
	for _, row := range rows {
		views = append(views, s.groupInvitationView(ctx, cache, *group, row))
	}
	return views, nil
}

// newGroupInvitationToken mints one link token and its SHA-256 digest.
func newGroupInvitationToken() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", NewError(KindConflict, "邀请无法创建")
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, groupTokenDigest(token), nil
}

// groupTokenDigest is the stored digest of one group invitation token.
func groupTokenDigest(token string) string {
	return tokenDigest(token)
}
