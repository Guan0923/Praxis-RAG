package state

import (
	"context"
	"database/sql"
	"errors"
)

// Knowledge-base invitation scopes: the whole knowledge base or an explicit list
// of document names.
const (
	InvitationScopeAll   = "all"
	InvitationScopeFiles = "files"
)

// IsInvitationScope reports whether value is a supported invitation scope.
func IsInvitationScope(value string) bool {
	return value == InvitationScopeAll || value == InvitationScopeFiles
}

// Knowledge-base invitation statuses.
const (
	InvitationPending  = "pending"
	InvitationAccepted = "accepted"
	InvitationDeclined = "declined"
	InvitationRevoked  = "revoked"
)

// Invitation is one issued share invitation. Only the SHA-256 digest of the
// link token is stored, so a database leak cannot replay a pending link.
type Invitation struct {
	ID            string
	Namespace     string
	InviterUserID string
	InviteeUserID string
	Permission    string
	Scope         string
	DocumentNames []string
	TokenDigest   string
	Status        string
	CreatedAt     string
	ExpiresAt     string
	AcceptedAt    *string
	UpdatedAt     string
}

// InvitationRepository persists knowledge-base share invitations.
type InvitationRepository struct{ db *sql.DB }

func newInvitationRepository(db *sql.DB) *InvitationRepository {
	return &InvitationRepository{db: db}
}

const invitationColumns = "id, namespace, inviter_user_id, invitee_user_id, permission, scope, " +
	"document_names, token_digest, status, created_at, expires_at, accepted_at, updated_at"

// Create inserts one invitation, generating its id/timestamps and validating the
// document-name JSON payload. The raw token is never passed to the repository.
func (r *InvitationRepository) Create(ctx context.Context, invitation Invitation) (Invitation, error) {
	if invitation.ID == "" {
		generated, err := randomHex(16)
		if err != nil {
			return Invitation{}, newStateError("邀请无法创建")
		}
		invitation.ID = generated
	}
	timestamp := nowISO()
	if invitation.CreatedAt == "" {
		invitation.CreatedAt = timestamp
	}
	invitation.UpdatedAt = timestamp
	if invitation.Status == "" {
		invitation.Status = InvitationPending
	}
	encoded, err := marshalStringList(invitation.DocumentNames)
	if err != nil {
		return Invitation{}, newStateError("邀请无法创建")
	}
	if _, err := r.db.ExecContext(ctx,
		"INSERT INTO kb_invitations ("+invitationColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)",
		invitation.ID, invitation.Namespace, invitation.InviterUserID, invitation.InviteeUserID,
		invitation.Permission, invitation.Scope, encoded, invitation.TokenDigest, invitation.Status,
		invitation.CreatedAt, invitation.ExpiresAt, invitation.UpdatedAt); err != nil {
		if isUniqueViolation(err) {
			return Invitation{}, newStateConflict("邀请已存在")
		}
		return Invitation{}, newStateError("邀请无法创建")
	}
	return invitation, nil
}

// ByID returns one invitation, or nil when it does not exist.
func (r *InvitationRepository) ByID(ctx context.Context, id string) (*Invitation, error) {
	return r.queryOne(ctx, "SELECT "+invitationColumns+" FROM kb_invitations WHERE id = ?", id)
}

// ByTokenDigest returns the invitation behind one link digest, or nil.
func (r *InvitationRepository) ByTokenDigest(ctx context.Context, digest string) (*Invitation, error) {
	return r.queryOne(ctx, "SELECT "+invitationColumns+" FROM kb_invitations WHERE token_digest = ?", digest)
}

func (r *InvitationRepository) queryOne(ctx context.Context, query string, args ...any) (*Invitation, error) {
	row := r.db.QueryRowContext(ctx, query, args...)
	invitation, err := scanInvitation(row)
	if err != nil {
		return nil, err
	}
	return invitation, nil
}

// ListByInvitee returns the invitations addressed to one account, pending first.
func (r *InvitationRepository) ListByInvitee(ctx context.Context, inviteeUserID string, limit int) ([]Invitation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+invitationColumns+" FROM kb_invitations WHERE invitee_user_id = ? "+
			"ORDER BY CASE WHEN status = 'pending' THEN 0 ELSE 1 END, created_at DESC LIMIT ?",
		inviteeUserID, limit)
	if err != nil {
		return nil, newStateError("邀请读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanInvitations(rows)
}

// ListByNamespace returns the invitations of one knowledge base, newest first.
func (r *InvitationRepository) ListByNamespace(ctx context.Context, namespace string, limit int) ([]Invitation, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+invitationColumns+" FROM kb_invitations WHERE namespace = ? "+
			"ORDER BY CASE WHEN status = 'pending' THEN 0 ELSE 1 END, created_at DESC LIMIT ?",
		namespace, limit)
	if err != nil {
		return nil, newStateError("邀请读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanInvitations(rows)
}

// SetStatus applies one status transition. acceptedAt may be empty.
func (r *InvitationRepository) SetStatus(ctx context.Context, id, status, acceptedAt string) (bool, error) {
	var accepted any
	if acceptedAt != "" {
		accepted = acceptedAt
	}
	result, err := r.db.ExecContext(ctx,
		"UPDATE kb_invitations SET status = ?, accepted_at = COALESCE(?, accepted_at), updated_at = ? WHERE id = ?",
		status, accepted, nowISO(), id)
	if err != nil {
		return false, newStateError("邀请无法更新")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, newStateError("邀请无法更新")
	}
	return changed > 0, nil
}

// RevokePending marks every pending invitation of one invitee in one knowledge
// base as revoked, so re-inviting a user invalidates older links.
func (r *InvitationRepository) RevokePending(ctx context.Context, namespace, inviteeUserID string) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		"UPDATE kb_invitations SET status = ?, updated_at = ? WHERE namespace = ? AND invitee_user_id = ? AND status = ?",
		InvitationRevoked, nowISO(), namespace, inviteeUserID, InvitationPending)
	if err != nil {
		return 0, newStateError("邀请无法更新")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, newStateError("邀请无法更新")
	}
	return changed, nil
}

// DeleteByUser removes invitations issued by or addressed to one account.
func (r *InvitationRepository) DeleteByUser(ctx context.Context, userID string) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		"DELETE FROM kb_invitations WHERE inviter_user_id = ? OR invitee_user_id = ?", userID, userID)
	if err != nil {
		return 0, newStateError("邀请无法删除")
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, newStateError("邀请无法删除")
	}
	return removed, nil
}

// scanInvitation reads the shared column list from one row.
func scanInvitation(row interface{ Scan(dest ...any) error }) (*Invitation, error) {
	var (
		invitation Invitation
		documents  string
		acceptedAt sql.NullString
	)
	if err := row.Scan(&invitation.ID, &invitation.Namespace, &invitation.InviterUserID,
		&invitation.InviteeUserID, &invitation.Permission, &invitation.Scope, &documents,
		&invitation.TokenDigest, &invitation.Status, &invitation.CreatedAt, &invitation.ExpiresAt,
		&acceptedAt, &invitation.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, newStateError("邀请读取失败")
	}
	names, err := decodeStringList(documents)
	if err != nil {
		return nil, newStateError("邀请读取失败")
	}
	invitation.DocumentNames = names
	if acceptedAt.Valid {
		value := acceptedAt.String
		invitation.AcceptedAt = &value
	}
	return &invitation, nil
}

func scanInvitations(rows *sql.Rows) ([]Invitation, error) {
	invitations := make([]Invitation, 0, 8)
	for rows.Next() {
		invitation, err := scanInvitation(rows)
		if err != nil {
			return nil, err
		}
		if invitation != nil {
			invitations = append(invitations, *invitation)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("邀请读取失败")
	}
	return invitations, nil
}

// ErrInvitationNotFound marks a missing invitation row.
var ErrInvitationNotFound = errors.New("invitation not found")
