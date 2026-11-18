package state

import (
	"context"
	"database/sql"
	"errors"
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
type Permission struct {
	ID            string
	Namespace     string
	GranteeUserID string
	DocumentName  string
	Permission    string
	GrantedBy     string
	CreatedAt     string
	UpdatedAt     string
}

// PermissionRepository persists knowledge-base sharing grants.
type PermissionRepository struct{ db *sql.DB }

func newPermissionRepository(db *sql.DB) *PermissionRepository {
	return &PermissionRepository{db: db}
}

// permissionColumns is the shared SELECT list.
const permissionColumns = "id, namespace, grantee_user_id, document_name, permission, granted_by, created_at, updated_at"

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
// duplicating rows. An empty ID is generated.
func (r *PermissionRepository) Upsert(ctx context.Context, permission Permission) error {
	return upsertPermissionRow(ctx, r.db, permission, nowISO())
}

// upsertPermissionRow writes one grant through the pool or an open
// transaction (both satisfy sqlQueryer). An empty ID is generated; the
// (namespace, grantee, document_name) triple is unique, so re-issuing a
// permission overwrites the level instead of duplicating rows.
func upsertPermissionRow(ctx context.Context, db sqlQueryer, permission Permission, timestamp string) error {
	id := permission.ID
	if id == "" {
		generated, err := randomHex(16)
		if err != nil {
			return newStateError("知识库权限无法保存")
		}
		id = generated
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO kb_permissions ("+permissionColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE permission = VALUES(permission), granted_by = VALUES(granted_by), "+
			"updated_at = VALUES(updated_at)",
		id, permission.Namespace, permission.GranteeUserID, permission.DocumentName,
		permission.Permission, permission.GrantedBy, timestamp, timestamp); err != nil {
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
		if err := upsertPermissionRow(ctx, transaction, permission, timestamp); err != nil {
			return err
		}
	}
	if err := transaction.Commit(); err != nil {
		return newStateError("知识库权限无法保存")
	}
	return nil
}

// Delete removes every grant of one account inside one knowledge base and
// reports how many rows were removed.
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
			&permission.DocumentName, &permission.Permission, &permission.GrantedBy,
			&permission.CreatedAt, &permission.UpdatedAt); err != nil {
			return nil, newStateError("知识库权限读取失败")
		}
		permissions = append(permissions, permission)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("知识库权限读取失败")
	}
	return permissions, nil
}

// ErrPermissionNotFound marks a missing grant row.
var ErrPermissionNotFound = errors.New("permission not found")
