package state

import (
	"context"
	"database/sql"
)

// Knowledge-base permission levels. A write grant always implies read.
const (
	PermissionRead  = "read"
	PermissionWrite = "write"
)

// IsPermission reports whether value is a supported permission level.
func IsPermission(value string) bool {
	return value == PermissionRead || value == PermissionWrite
}

// Permission is one knowledge-base grant row. An empty DocumentName means the
// whole knowledge base; otherwise it names exactly one document.
//
// AllowMask/DenyMask carry the operation bits (read=1, create=2, update=4,
// delete=8). Permission keeps the conservative legacy projection for old
// readers: "write" when read+update+delete are all allowed, "read" when the
// read bit is allowed, "" otherwise.
type Permission struct {
	ID            string
	Namespace     string
	GranteeUserID string
	DocumentName  string
	Permission    string
	AllowMask     uint16
	DenyMask      uint16
	GrantedBy     string
	CreatedAt     string
	UpdatedAt     string
}

// LegacyPermissionFor renders the conservative historical permission level of
// one allow mask. It is stored in the permission column so pre-mask readers
// never see more access than the mask grants.
func LegacyPermissionFor(allow uint16) string {
	if allow&(1|4|8) == (1 | 4 | 8) {
		return PermissionWrite
	}
	if allow&1 != 0 {
		return PermissionRead
	}
	return ""
}

// DeriveAllowMask converts a legacy permission level into its allow mask:
// read -> 1, a whole-library write -> 15, a file-scoped write -> 13. Rows
// created before masks existed are backfilled with the same rule.
func DeriveAllowMask(permission, documentName string) uint16 {
	switch permission {
	case PermissionWrite:
		if documentName == "" {
			return 15
		}
		return 13
	case PermissionRead:
		return 1
	default:
		return 0
	}
}

// PermissionRepository persists knowledge-base sharing grants.
type PermissionRepository struct{ db *sql.DB }

func newPermissionRepository(db *sql.DB) *PermissionRepository {
	return &PermissionRepository{db: db}
}

// permissionColumns is the shared SELECT list.
const permissionColumns = "id, namespace, grantee_user_id, document_name, permission, " +
	"COALESCE(allow_mask, 0), COALESCE(deny_mask, 0), granted_by, created_at, updated_at"

// ListByNamespace returns every grant of one knowledge base ordered by user and
// document.
func (r *PermissionRepository) ListByNamespace(ctx context.Context, namespace string) ([]Permission, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+permissionColumns+" FROM kb_permissions WHERE namespace = ? "+
			"ORDER BY grantee_user_id, document_name", namespace)
	if err != nil {
		return nil, newStateError("知识库权限读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanPermissions(rows)
}

// ListByGrantee returns every grant held by one account across all knowledge
// bases.
func (r *PermissionRepository) ListByGrantee(ctx context.Context, granteeUserID string) ([]Permission, error) {
	if granteeUserID == "" {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+permissionColumns+" FROM kb_permissions WHERE grantee_user_id = ? "+
			"ORDER BY namespace, document_name", granteeUserID)
	if err != nil {
		return nil, newStateError("知识库权限读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanPermissions(rows)
}

// ListFor returns the grants of one account inside one knowledge base.
func (r *PermissionRepository) ListFor(ctx context.Context, namespace, granteeUserID string) ([]Permission, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+permissionColumns+" FROM kb_permissions WHERE namespace = ? AND grantee_user_id = ? "+
			"ORDER BY document_name", namespace, granteeUserID)
	if err != nil {
		return nil, newStateError("知识库权限读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanPermissions(rows)
}

// Upsert inserts or updates one grant. The (namespace, grantee, document_name)
// triple is unique, so re-issuing a permission overwrites the level instead of
// duplicating rows. An empty ID is generated. When AllowMask is unset it is
// derived from the legacy Permission level, so historical callers and tests
// keep working.
func (r *PermissionRepository) Upsert(ctx context.Context, permission Permission) error {
	permission = permission.withDerivedMasks()
	return upsertPermissionRow(ctx, r.db, permission, nowISO(), false)
}

// UpsertAllow writes the allow side of one rule only: re-issuing the same
// scope overwrites the previous allow while any stored deny stays untouched.
// Invitation acceptance uses it so an accepted grant never lifts a manager's
// explicit deny.
func (r *PermissionRepository) UpsertAllow(ctx context.Context, permission Permission) error {
	permission = permission.withDerivedMasks()
	return upsertPermissionRow(ctx, r.db, permission, nowISO(), true)
}

// withDerivedMasks fills AllowMask from the legacy permission level when the
// caller did not set it, and refreshes the legacy projection column.
func (p Permission) withDerivedMasks() Permission {
	if p.AllowMask == 0 && (p.Permission == PermissionRead || p.Permission == PermissionWrite) {
		p.AllowMask = DeriveAllowMask(p.Permission, p.DocumentName)
	}
	if p.Permission != PermissionRead && p.Permission != PermissionWrite {
		p.Permission = LegacyPermissionFor(p.AllowMask)
	}
	return p
}

// upsertPermissionRow writes one grant through the pool or an open
// transaction (both satisfy sqlQueryer). An empty ID is generated; the
// (namespace, grantee, document_name) triple is unique, so re-issuing a
// permission overwrites the level instead of duplicating rows. When
// preserveDeny is set the stored deny_mask survives (the allow-side overwrite
// used by invitation acceptance).
func upsertPermissionRow(ctx context.Context, db sqlQueryer, permission Permission, timestamp string, preserveDeny bool) error {
	id := permission.ID
	if id == "" {
		generated, err := randomHex(16)
		if err != nil {
			return newStateError("知识库权限无法保存")
		}
		id = generated
	}
	statement := "INSERT INTO kb_permissions (id, namespace, grantee_user_id, document_name, permission, " +
		"allow_mask, deny_mask, granted_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) " +
		"ON DUPLICATE KEY UPDATE permission = VALUES(permission), allow_mask = VALUES(allow_mask), " +
		"granted_by = VALUES(granted_by), updated_at = VALUES(updated_at)"
	if !preserveDeny {
		statement += ", deny_mask = VALUES(deny_mask)"
	}
	if _, err := db.ExecContext(ctx, statement,
		id, permission.Namespace, permission.GranteeUserID, permission.DocumentName,
		permission.Permission, permission.AllowMask, permission.DenyMask,
		permission.GrantedBy, timestamp, timestamp); err != nil {
		return newStateError("知识库权限无法保存")
	}
	return nil
}

// UpsertMany writes a batch of grants in one transaction. A document-set grant
// is represented by one row per document name (or a single "" row for the whole
// knowledge base).
func (r *PermissionRepository) UpsertMany(ctx context.Context, permissions []Permission) error {
	if len(permissions) == 0 {
		return nil
	}
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return newStateError("知识库权限无法保存")
	}
	defer func() { _ = transaction.Rollback() }()
	timestamp := nowISO()
	for _, permission := range permissions {
		if err := upsertPermissionRow(ctx, transaction, permission.withDerivedMasks(), timestamp, false); err != nil {
			return err
		}
	}
	if err := transaction.Commit(); err != nil {
		return newStateError("知识库权限无法保存")
	}
	return nil
}

// ClearAllow zeroes the allow mask of one rule while keeping its deny (the
// explicit "revoke allow" operation), and reports the number of rules that
// still carry a deny afterwards. A rule left with neither mask is deleted.
func (r *PermissionRepository) ClearAllow(ctx context.Context, namespace, granteeUserID, documentName string) (int64, error) {
	return r.clearMask(ctx, namespace, granteeUserID, documentName, "allow_mask")
}

// ClearDeny zeroes the deny mask of one rule while keeping its allow (the
// explicit "clear deny" operation), and reports whether a rule existed.
func (r *PermissionRepository) ClearDeny(ctx context.Context, namespace, granteeUserID, documentName string) (bool, error) {
	changed, err := r.clearMask(ctx, namespace, granteeUserID, documentName, "deny_mask")
	if err != nil {
		return false, err
	}
	return changed > 0, nil
}

// clearMask zeroes one mask column on a single (namespace, grantee, document)
// rule and removes the row when neither mask remains. It returns the number
// of rows the update touched (0 means the rule does not exist).
func (r *PermissionRepository) clearMask(ctx context.Context, namespace, granteeUserID, documentName, column string) (int64, error) {
	if column != "allow_mask" && column != "deny_mask" {
		return 0, newStateError("知识库权限无法删除")
	}
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, newStateError("知识库权限无法删除")
	}
	defer func() { _ = transaction.Rollback() }()
	result, err := transaction.ExecContext(ctx,
		"UPDATE kb_permissions SET "+column+" = 0, permission = '', updated_at = ? "+
			"WHERE namespace = ? AND grantee_user_id = ? AND document_name = ?",
		nowISO(), namespace, granteeUserID, documentName)
	if err != nil {
		return 0, newStateError("知识库权限无法删除")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, newStateError("知识库权限无法删除")
	}
	if changed > 0 {
		// Rules without either mask are meaningless; dropping them keeps the
		// table aligned with what the Console lists.
		if _, err := transaction.ExecContext(ctx,
			"DELETE FROM kb_permissions WHERE namespace = ? AND grantee_user_id = ? AND document_name = ? "+
				"AND COALESCE(allow_mask, 0) = 0 AND COALESCE(deny_mask, 0) = 0",
			namespace, granteeUserID, documentName); err != nil {
			return 0, newStateError("知识库权限无法删除")
		}
	}
	if err := transaction.Commit(); err != nil {
		return 0, newStateError("知识库权限无法删除")
	}
	return changed, nil
}

// ClearAllAllow zeroes the allow mask of every rule one account holds inside
// one knowledge base (the legacy revoke endpoint: it removes the allows but
// never the denies) and reports how many rules carried any mask before.
func (r *PermissionRepository) ClearAllAllow(ctx context.Context, namespace, granteeUserID string) (int64, error) {
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, newStateError("知识库权限无法删除")
	}
	defer func() { _ = transaction.Rollback() }()
	result, err := transaction.ExecContext(ctx,
		"UPDATE kb_permissions SET allow_mask = 0, permission = '', updated_at = ? "+
			"WHERE namespace = ? AND grantee_user_id = ? AND COALESCE(allow_mask, 0) <> 0",
		nowISO(), namespace, granteeUserID)
	if err != nil {
		return 0, newStateError("知识库权限无法删除")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, newStateError("知识库权限无法删除")
	}
	if _, err := transaction.ExecContext(ctx,
		"DELETE FROM kb_permissions WHERE namespace = ? AND grantee_user_id = ? "+
			"AND COALESCE(allow_mask, 0) = 0 AND COALESCE(deny_mask, 0) = 0",
		namespace, granteeUserID); err != nil {
		return 0, newStateError("知识库权限无法删除")
	}
	if err := transaction.Commit(); err != nil {
		return 0, newStateError("知识库权限无法删除")
	}
	return changed, nil
}

// Delete removes every grant of one account inside one knowledge base and
// reports how many rows were removed (account deletion and knowledge-base
// purges; the revoke surface uses ClearAllAllow instead).
func (r *PermissionRepository) Delete(ctx context.Context, namespace, granteeUserID string) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		"DELETE FROM kb_permissions WHERE namespace = ? AND grantee_user_id = ?", namespace, granteeUserID)
	if err != nil {
		return 0, newStateError("知识库权限无法删除")
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, newStateError("知识库权限无法删除")
	}
	return removed, nil
}

// DeleteDocument removes one document-scoped grant and reports whether a row
// changed.
func (r *PermissionRepository) DeleteDocument(ctx context.Context, namespace, granteeUserID, documentName string) (bool, error) {
	result, err := r.db.ExecContext(ctx,
		"DELETE FROM kb_permissions WHERE namespace = ? AND grantee_user_id = ? AND document_name = ?",
		namespace, granteeUserID, documentName)
	if err != nil {
		return false, newStateError("知识库权限无法删除")
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, newStateError("知识库权限无法删除")
	}
	return removed > 0, nil
}

// DeleteByGrantee removes every grant held by one account (account deletion).
func (r *PermissionRepository) DeleteByGrantee(ctx context.Context, granteeUserID string) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		"DELETE FROM kb_permissions WHERE grantee_user_id = ?", granteeUserID)
	if err != nil {
		return 0, newStateError("知识库权限无法删除")
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, newStateError("知识库权限无法删除")
	}
	return removed, nil
}

// scanPermissions reads the shared column list.
func scanPermissions(rows *sql.Rows) ([]Permission, error) {
	permissions := make([]Permission, 0, 8)
	for rows.Next() {
		var permission Permission
		if err := rows.Scan(&permission.ID, &permission.Namespace, &permission.GranteeUserID,
			&permission.DocumentName, &permission.Permission, &permission.AllowMask, &permission.DenyMask,
			&permission.GrantedBy, &permission.CreatedAt, &permission.UpdatedAt); err != nil {
			return nil, newStateError("知识库权限读取失败")
		}
		permissions = append(permissions, permission)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("知识库权限读取失败")
	}
	return permissions, nil
}
