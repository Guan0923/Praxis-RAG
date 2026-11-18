package state

import (
	"context"
	"database/sql"
)

// User-group statuses. A dissolved group keeps its row (so invitation history
// stays addressable) but its members and knowledge-base grants are removed and
// no new member may join.
const (
	UserGroupActive    = "active"
	UserGroupDissolved = "dissolved"
)

// User-group invitation statuses, mirroring kb_invitations: "expired" is
// computed from expires_at and never persisted.
const (
	GroupInvitationPending  = "pending"
	GroupInvitationAccepted = "accepted"
	GroupInvitationDeclined = "declined"
	GroupInvitationRevoked  = "revoked"
)

// UserGroup is one user group created and administered by its owner.
type UserGroup struct {
	ID          string
	Name        string
	OwnerUserID string
	Status      string
	CreatedAt   string
	UpdatedAt   string
	DissolvedAt *string
}

// UserGroupMember is one effective membership row. The creator administers
// the group without being a member.
type UserGroupMember struct {
	ID        string
	GroupID   string
	UserID    string
	JoinedAt  string
	CreatedAt string
}

// GroupInvitation is one membership invitation. Only the SHA-256 digest of the
// link token is stored, exactly like kb_invitations and user_sessions.
type GroupInvitation struct {
	ID            string
	GroupID       string
	InviterUserID string
	InviteeUserID string
	TokenDigest   string
	Status        string
	CreatedAt     string
	ExpiresAt     string
	AcceptedAt    *string
	UpdatedAt     string
}

// UserGroupRepository persists the groups and their memberships.
type UserGroupRepository struct{ db *sql.DB }

func newUserGroupRepository(db *sql.DB) *UserGroupRepository {
	return &UserGroupRepository{db: db}
}

// userGroupSelectColumns is the SELECT list shared by every group read. The
// un-prefixed form is used inside INSERT/DDL; reads that join other tables use
// userGroupQualifiedColumns instead.
const (
	userGroupColumns         = "id, name, owner_user_id, status, created_at, updated_at, dissolved_at"
	userGroupQualifiedColumns = "g.id, g.name, g.owner_user_id, g.status, g.created_at, g.updated_at, g.dissolved_at"
)

// Create inserts one group owned by the creating account.
func (r *UserGroupRepository) Create(ctx context.Context, group UserGroup) (UserGroup, error) {
	if group.ID == "" {
		generated, err := randomHex(16)
		if err != nil {
			return UserGroup{}, newStateError("用户组无法创建")
		}
		group.ID = generated
	}
	timestamp := nowISO()
	if group.CreatedAt == "" {
		group.CreatedAt = timestamp
	}
	if group.Status == "" {
		group.Status = UserGroupActive
	}
	group.UpdatedAt = timestamp
	if _, err := r.db.ExecContext(ctx,
		"INSERT INTO user_groups (id, name, owner_user_id, status, created_at, updated_at, dissolved_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, NULL)",
		group.ID, group.Name, group.OwnerUserID, group.Status, group.CreatedAt, group.UpdatedAt); err != nil {
		return UserGroup{}, newStateError("用户组无法创建")
	}
	return group, nil
}

// ByID returns one group, or nil when it does not exist.
func (r *UserGroupRepository) ByID(ctx context.Context, id string) (*UserGroup, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+userGroupColumns+" FROM user_groups WHERE id = ?", id)
	return scanUserGroup(row)
}

// ListByOwner returns the groups one account created, newest first. Dissolved
// groups are included so the Console can keep showing their outcome; every
// management operation on them fails with a conflict instead.
func (r *UserGroupRepository) ListByOwner(ctx context.Context, ownerUserID string) ([]UserGroup, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+userGroupColumns+" FROM user_groups WHERE owner_user_id = ? ORDER BY created_at DESC, id",
		ownerUserID)
	if err != nil {
		return nil, newStateError("用户组读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanUserGroups(rows)
}

// Dissolve deactivates one active group and removes its members, pending
// invitations and every knowledge-base grant it held, in one transaction. It
// reports false when the group is missing or already dissolved.
func (r *UserGroupRepository) Dissolve(ctx context.Context, id string) (bool, error) {
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, newStateError("用户组无法解散")
	}
	defer func() { _ = transaction.Rollback() }()
	timestamp := nowISO()
	result, err := transaction.ExecContext(ctx,
		"UPDATE user_groups SET status = ?, dissolved_at = ?, updated_at = ? WHERE id = ? AND status = ?",
		UserGroupDissolved, timestamp, timestamp, id, UserGroupActive)
	if err != nil {
		return false, newStateError("用户组无法解散")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, newStateError("用户组无法解散")
	}
	if changed == 0 {
		return false, nil
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE user_group_invitations SET status = ?, updated_at = ? WHERE group_id = ? AND status = ?",
		GroupInvitationRevoked, timestamp, id, GroupInvitationPending); err != nil {
		return false, newStateError("用户组无法解散")
	}
	if _, err := transaction.ExecContext(ctx,
		"DELETE FROM user_group_members WHERE group_id = ?", id); err != nil {
		return false, newStateError("用户组无法解散")
	}
	if _, err := transaction.ExecContext(ctx,
		"DELETE FROM kb_group_permissions WHERE group_id = ?", id); err != nil {
		return false, newStateError("用户组无法解散")
	}
	if err := transaction.Commit(); err != nil {
		return false, newStateError("用户组无法解散")
	}
	return true, nil
}

func scanUserGroup(row *sql.Row) (*UserGroup, error) {
	var group UserGroup
	if err := row.Scan(&group.ID, &group.Name, &group.OwnerUserID, &group.Status,
		&group.CreatedAt, &group.UpdatedAt, &group.DissolvedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, newStateError("用户组读取失败")
	}
	return &group, nil
}

func scanUserGroups(rows *sql.Rows) ([]UserGroup, error) {
	groups := make([]UserGroup, 0, 8)
	for rows.Next() {
		var group UserGroup
		if err := rows.Scan(&group.ID, &group.Name, &group.OwnerUserID, &group.Status,
			&group.CreatedAt, &group.UpdatedAt, &group.DissolvedAt); err != nil {
			return nil, newStateError("用户组读取失败")
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("用户组读取失败")
	}
	return groups, nil
}

// UserGroupMemberRepository persists the memberships.
type UserGroupMemberRepository struct{ db *sql.DB }

func newUserGroupMemberRepository(db *sql.DB) *UserGroupMemberRepository {
	return &UserGroupMemberRepository{db: db}
}

const userGroupMemberColumns = "id, group_id, user_id, joined_at, created_at"

// Add inserts one membership. A duplicate (group, user) pair is a conflict.
func (r *UserGroupMemberRepository) Add(ctx context.Context, member UserGroupMember) error {
	if member.ID == "" {
		generated, err := randomHex(16)
		if err != nil {
			return newStateError("组成员无法保存")
		}
		member.ID = generated
	}
	timestamp := nowISO()
	if member.JoinedAt == "" {
		member.JoinedAt = timestamp
	}
	if member.CreatedAt == "" {
		member.CreatedAt = timestamp
	}
	if _, err := r.db.ExecContext(ctx,
		"INSERT INTO user_group_members ("+userGroupMemberColumns+") VALUES (?, ?, ?, ?, ?)",
		member.ID, member.GroupID, member.UserID, member.JoinedAt, member.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return newStateConflict("该用户已是组成员")
		}
		return newStateError("组成员无法保存")
	}
	return nil
}

// Remove deletes one membership and reports whether a row changed.
func (r *UserGroupMemberRepository) Remove(ctx context.Context, groupID, userID string) (bool, error) {
	result, err := r.db.ExecContext(ctx,
		"DELETE FROM user_group_members WHERE group_id = ? AND user_id = ?", groupID, userID)
	if err != nil {
		return false, newStateError("组成员无法移除")
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, newStateError("组成员无法移除")
	}
	return removed > 0, nil
}

// Exists reports whether one account is a member of one group.
func (r *UserGroupMemberRepository) Exists(ctx context.Context, groupID, userID string) (bool, error) {
	var present bool
	if err := r.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM user_group_members WHERE group_id = ? AND user_id = ?)",
		groupID, userID).Scan(&present); err != nil {
		return false, newStateError("组成员读取失败")
	}
	return present, nil
}

// ListByGroup returns the members of one group, join order.
func (r *UserGroupMemberRepository) ListByGroup(ctx context.Context, groupID string) ([]UserGroupMember, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+userGroupMemberColumns+" FROM user_group_members WHERE group_id = ? "+
			"ORDER BY joined_at, id", groupID)
	if err != nil {
		return nil, newStateError("组成员读取失败")
	}
	defer func() { _ = rows.Close() }()
	members := make([]UserGroupMember, 0, 8)
	for rows.Next() {
		var member UserGroupMember
		if err := rows.Scan(&member.ID, &member.GroupID, &member.UserID,
			&member.JoinedAt, &member.CreatedAt); err != nil {
			return nil, newStateError("组成员读取失败")
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("组成员读取失败")
	}
	return members, nil
}

// ListJoinedByUser returns the active groups one account joined, newest first.
func (r *UserGroupMemberRepository) ListJoinedByUser(ctx context.Context, userID string) ([]UserGroup, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+userGroupQualifiedColumns+" FROM user_groups g JOIN user_group_members m ON m.group_id = g.id "+
			"WHERE m.user_id = ? AND g.status = ? ORDER BY g.created_at DESC, g.id",
		userID, UserGroupActive)
	if err != nil {
		return nil, newStateError("用户组读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanUserGroups(rows)
}

// ActiveGroupIDsForUser returns the ids of the active groups one account
// belongs to. It is the "当前有效成员关系" input of the permission merge.
func (r *UserGroupMemberRepository) ActiveGroupIDsForUser(ctx context.Context, userID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT g.id FROM user_groups g JOIN user_group_members m ON m.group_id = g.id "+
			"WHERE m.user_id = ? AND g.status = ? ORDER BY g.id",
		userID, UserGroupActive)
	if err != nil {
		return nil, newStateError("用户组读取失败")
	}
	defer func() { _ = rows.Close() }()
	ids := make([]string, 0, 8)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, newStateError("用户组读取失败")
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("用户组读取失败")
	}
	return ids, nil
}

// GroupInvitationRepository persists the membership invitations.
type GroupInvitationRepository struct{ db *sql.DB }

func newGroupInvitationRepository(db *sql.DB) *GroupInvitationRepository {
	return &GroupInvitationRepository{db: db}
}

const groupInvitationColumns = "id, group_id, inviter_user_id, invitee_user_id, " +
	"token_digest, status, created_at, expires_at, accepted_at, updated_at"

// Typed invitation-acceptance conflicts so the access layer can map each
// failure onto a stable user-facing message.
var (
	// ErrGroupInvitationGone means the invitation row no longer allows a
	// pending→accepted transition (already handled, or a concurrent action
	// won the state change).
	ErrGroupInvitationGone = newStateConflict("邀请已处理或已失效")
	// ErrGroupInvitationExpired means the link expired before acceptance.
	ErrGroupInvitationExpired = newStateConflict("邀请已过期")
	// ErrGroupDissolved means the inviting group was dissolved.
	ErrGroupDissolved = newStateConflict("用户组已解散")
	// ErrGroupInviterUnqualified means the inviter lost the vip/admin role.
	ErrGroupInviterUnqualified = newStateConflict("邀请人已不具备邀请资格")
)

// Create inserts one invitation, generating id and timestamps. The raw token
// never reaches the repository.
func (r *GroupInvitationRepository) Create(ctx context.Context, invitation GroupInvitation) (GroupInvitation, error) {
	if invitation.ID == "" {
		generated, err := randomHex(16)
		if err != nil {
			return GroupInvitation{}, newStateError("邀请无法创建")
		}
		invitation.ID = generated
	}
	timestamp := nowISO()
	if invitation.CreatedAt == "" {
		invitation.CreatedAt = timestamp
	}
	if invitation.Status == "" {
		invitation.Status = GroupInvitationPending
	}
	invitation.UpdatedAt = timestamp
	if _, err := r.db.ExecContext(ctx,
		"INSERT INTO user_group_invitations ("+groupInvitationColumns+") "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)",
		invitation.ID, invitation.GroupID, invitation.InviterUserID, invitation.InviteeUserID,
		invitation.TokenDigest, invitation.Status, invitation.CreatedAt, invitation.ExpiresAt,
		invitation.UpdatedAt); err != nil {
		if isUniqueViolation(err) {
			return GroupInvitation{}, newStateConflict("邀请已存在")
		}
		return GroupInvitation{}, newStateError("邀请无法创建")
	}
	return invitation, nil
}

// ByID returns one invitation, or nil when it does not exist.
func (r *GroupInvitationRepository) ByID(ctx context.Context, id string) (*GroupInvitation, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+groupInvitationColumns+" FROM user_group_invitations WHERE id = ?", id)
	return scanGroupInvitation(row)
}

// ByTokenDigest returns the invitation behind one link digest, or nil.
func (r *GroupInvitationRepository) ByTokenDigest(ctx context.Context, digest string) (*GroupInvitation, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+groupInvitationColumns+" FROM user_group_invitations WHERE token_digest = ?", digest)
	return scanGroupInvitation(row)
}

// ListByGroup returns the invitations of one group, pending first.
func (r *GroupInvitationRepository) ListByGroup(ctx context.Context, groupID string, limit int) ([]GroupInvitation, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+groupInvitationColumns+" FROM user_group_invitations WHERE group_id = ? "+
			"ORDER BY CASE WHEN status = 'pending' THEN 0 ELSE 1 END, created_at DESC LIMIT ?",
		groupID, limit)
	if err != nil {
		return nil, newStateError("邀请读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanGroupInvitations(rows)
}

// ListByInvitee returns the invitations addressed to one account, pending first.
func (r *GroupInvitationRepository) ListByInvitee(ctx context.Context, inviteeUserID string, limit int) ([]GroupInvitation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+groupInvitationColumns+" FROM user_group_invitations WHERE invitee_user_id = ? "+
			"ORDER BY CASE WHEN status = 'pending' THEN 0 ELSE 1 END, created_at DESC LIMIT ?",
		inviteeUserID, limit)
	if err != nil {
		return nil, newStateError("邀请读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanGroupInvitations(rows)
}

// RevokePending invalidates the pending invitations for one invitee of one
// group, so re-inviting keeps only the newest link acceptable.
func (r *GroupInvitationRepository) RevokePending(ctx context.Context, groupID, inviteeUserID string) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		"UPDATE user_group_invitations SET status = ?, updated_at = ? WHERE group_id = ? AND invitee_user_id = ? AND status = ?",
		GroupInvitationRevoked, nowISO(), groupID, inviteeUserID, GroupInvitationPending)
	if err != nil {
		return 0, newStateError("邀请无法更新")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, newStateError("邀请无法更新")
	}
	return changed, nil
}

// DeclinePending turns one pending invitation into declined. It reports
// false when the row is missing or already handled, so a concurrent accept can
// win only one transition.
func (r *GroupInvitationRepository) DeclinePending(ctx context.Context, id string) (bool, error) {
	result, err := r.db.ExecContext(ctx,
		"UPDATE user_group_invitations SET status = ?, updated_at = ? WHERE id = ? AND status = ?",
		GroupInvitationDeclined, nowISO(), id, GroupInvitationPending)
	if err != nil {
		return false, newStateError("邀请无法更新")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, newStateError("邀请无法更新")
	}
	return changed > 0, nil
}

// RevokeByID withdraws one pending invitation; only the pending→revoked
// transition counts, so a concurrent accept (or decline) wins instead.
func (r *GroupInvitationRepository) RevokeByID(ctx context.Context, id string) (bool, error) {
	result, err := r.db.ExecContext(ctx,
		"UPDATE user_group_invitations SET status = ?, updated_at = ? WHERE id = ? AND status = ?",
		GroupInvitationRevoked, nowISO(), id, GroupInvitationPending)
	if err != nil {
		return false, newStateError("邀请无法更新")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, newStateError("邀请无法更新")
	}
	return changed > 0, nil
}

// AcceptPending turns one pending invitation into a membership inside a
// single transaction. While holding the invitation row lock it re-checks every
// precondition: the group is still active, the invitation is still pending and
// unexpired (nowISO is the service clock string), and the inviter still holds
// an enabled vip/admin role. Only then does it flip the status and insert the
// member row; either both land or neither does.
//
// The already-accepted case is idempotent and returns the row without
// touching members: retrying an old link must never re-add an account that
// later left or was removed from the group.
func (r *GroupInvitationRepository) AcceptPending(ctx context.Context, id, acceptedAt, nowISOValue string) (*GroupInvitation, error) {
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, newStateError("邀请无法更新")
	}
	defer func() { _ = transaction.Rollback() }()
	invitation, err := scanGroupInvitation(transaction.QueryRowContext(ctx,
		"SELECT "+groupInvitationColumns+" FROM user_group_invitations WHERE id = ? FOR UPDATE", id))
	if err != nil {
		return nil, err
	}
	if invitation == nil {
		return nil, nil
	}
	if invitation.Status != GroupInvitationPending {
		if invitation.Status == GroupInvitationAccepted {
			// Idempotent retry: no membership write.
			return invitation, nil
		}
		return invitation, ErrGroupInvitationGone
	}
	if invitation.ExpiresAt != "" && invitation.ExpiresAt <= nowISOValue {
		// Persist the expiry so the link cannot be replayed, then refuse.
		if _, err := transaction.ExecContext(ctx,
			"UPDATE user_group_invitations SET status = ?, updated_at = ? WHERE id = ? AND status = ?",
			GroupInvitationRevoked, nowISO(), id, GroupInvitationPending); err != nil {
			return nil, newStateError("邀请无法更新")
		}
		if err := transaction.Commit(); err != nil {
			return nil, newStateError("邀请无法更新")
		}
		return invitation, ErrGroupInvitationExpired
	}
	// The group must still be active; the row lock serializes against a
	// concurrent dissolve.
	var groupStatus string
	if err := transaction.QueryRowContext(ctx,
		"SELECT status FROM user_groups WHERE id = ? FOR UPDATE", invitation.GroupID).
		Scan(&groupStatus); err != nil {
		if err == sql.ErrNoRows {
			return invitation, ErrGroupDissolved
		}
		return nil, newStateError("邀请无法更新")
	}
	if groupStatus != UserGroupActive {
		return invitation, ErrGroupDissolved
	}
	// The inviter must still be an enabled vip/admin account, so a demoted or
	// disabled inviter stops adding members immediately.
	var role string
	var enabled bool
	if err := transaction.QueryRowContext(ctx,
		"SELECT role, enabled FROM users WHERE id = ?", invitation.InviterUserID).
		Scan(&role, &enabled); err != nil {
		if err == sql.ErrNoRows {
			return invitation, ErrGroupInviterUnqualified
		}
		return nil, newStateError("邀请无法更新")
	}
	if !enabled || (role != RoleVIP && role != RoleAdmin) {
		return invitation, ErrGroupInviterUnqualified
	}
	timestamp := nowISO()
	var accepted any
	if acceptedAt != "" {
		accepted = acceptedAt
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE user_group_invitations SET status = ?, accepted_at = COALESCE(?, accepted_at), updated_at = ? "+
			"WHERE id = ? AND status = ?",
		GroupInvitationAccepted, accepted, timestamp, id, GroupInvitationPending); err != nil {
		return nil, newStateError("邀请无法更新")
	}
	// A member row may already exist (the account was invited again without
	// leaving); keep the original join date.
	var existing int
	if err := transaction.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM user_group_members WHERE group_id = ? AND user_id = ?)",
		invitation.GroupID, invitation.InviteeUserID).Scan(&existing); err != nil {
		return nil, newStateError("邀请无法更新")
	}
	if existing == 0 {
		memberID, err := randomHex(16)
		if err != nil {
			return nil, newStateError("邀请无法更新")
		}
		if _, err := transaction.ExecContext(ctx,
			"INSERT INTO user_group_members ("+userGroupMemberColumns+") VALUES (?, ?, ?, ?, ?)",
			memberID, invitation.GroupID, invitation.InviteeUserID, timestamp, timestamp); err != nil {
			if isUniqueViolation(err) {
				// A concurrent invite for the same pair won; keep it.
				if err := transaction.Rollback(); err != nil {
					return nil, newStateError("邀请无法更新")
				}
				return invitation, ErrGroupInvitationGone
			}
			return nil, newStateError("邀请无法更新")
		}
	}
	if err := transaction.Commit(); err != nil {
		return nil, newStateError("邀请无法更新")
	}
	invitation.Status = GroupInvitationAccepted
	if acceptedAt != "" {
		acceptedValue := acceptedAt
		invitation.AcceptedAt = &acceptedValue
	}
	return invitation, nil
}

func scanGroupInvitation(row *sql.Row) (*GroupInvitation, error) {
	var invitation GroupInvitation
	if err := row.Scan(&invitation.ID, &invitation.GroupID, &invitation.InviterUserID,
		&invitation.InviteeUserID, &invitation.TokenDigest, &invitation.Status,
		&invitation.CreatedAt, &invitation.ExpiresAt, &invitation.AcceptedAt, &invitation.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, newStateError("邀请读取失败")
	}
	return &invitation, nil
}

func scanGroupInvitations(rows *sql.Rows) ([]GroupInvitation, error) {
	invitations := make([]GroupInvitation, 0, 8)
	for rows.Next() {
		var invitation GroupInvitation
		if err := rows.Scan(&invitation.ID, &invitation.GroupID, &invitation.InviterUserID,
			&invitation.InviteeUserID, &invitation.TokenDigest, &invitation.Status,
			&invitation.CreatedAt, &invitation.ExpiresAt, &invitation.AcceptedAt, &invitation.UpdatedAt); err != nil {
			return nil, newStateError("邀请读取失败")
		}
		invitations = append(invitations, invitation)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("邀请读取失败")
	}
	return invitations, nil
}
