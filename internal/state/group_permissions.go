package state

import (
	"context"
	"database/sql"
	"strings"
)

// Document permission bits stored in kb_group_permissions.allow_mask. The four
// bits are the contract of the group permission merge: read=0001,
// create=0010, update=0100, delete=1000.
const (
	PermissionBitRead   = 1 // 0001
	PermissionBitCreate = 2 // 0010
	PermissionBitUpdate = 4 // 0100
	PermissionBitDelete = 8 // 1000
	// PermissionMaskAll is every supported bit at once.
	PermissionMaskAll = PermissionBitRead | PermissionBitCreate | PermissionBitUpdate | PermissionBitDelete
)

// IsValidAllowMask reports whether value is a non-empty subset of the four
// permission bits.
func IsValidAllowMask(value int) bool {
	return value >= PermissionBitRead && value <= PermissionMaskAll
}

// KBGroupPermission is one group grant row. An empty DocumentName means the
// whole knowledge base; otherwise it names exactly one document. Only allow
// bits are stored in the first version — a group never reduces a member's
// existing permissions.
type KBGroupPermission struct {
	ID           string
	Namespace    string
	GroupID      string
	DocumentName string
	AllowMask    int
	GrantedBy    string
	CreatedAt    string
	UpdatedAt    string
}

// KBGroupPermissionRepository persists the knowledge-base grants of groups.
type KBGroupPermissionRepository struct{ db *sql.DB }

func newKBGroupPermissionRepository(db *sql.DB) *KBGroupPermissionRepository {
	return &KBGroupPermissionRepository{db: db}
}

const kbGroupPermissionColumns = "id, namespace, group_id, document_name, allow_mask, granted_by, created_at, updated_at"

// Upsert inserts or updates one grant. The (namespace, group, document_name)
// triple is unique, so re-issuing a grant overwrites the mask instead of
// duplicating rows.
func (r *KBGroupPermissionRepository) Upsert(ctx context.Context, permission KBGroupPermission) error {
	id := permission.ID
	if id == "" {
		generated, err := randomHex(16)
		if err != nil {
			return newStateError("用户组授权无法保存")
		}
		id = generated
	}
	timestamp := nowISO()
	if _, err := r.db.ExecContext(ctx,
		"INSERT INTO kb_group_permissions ("+kbGroupPermissionColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE allow_mask = VALUES(allow_mask), granted_by = VALUES(granted_by), "+
			"updated_at = VALUES(updated_at)",
		id, permission.Namespace, permission.GroupID, permission.DocumentName, permission.AllowMask,
		permission.GrantedBy, timestamp, timestamp); err != nil {
		return newStateError("用户组授权无法保存")
	}
	return nil
}

// ListByNamespace returns every group grant of one knowledge base.
func (r *KBGroupPermissionRepository) ListByNamespace(ctx context.Context, namespace string) ([]KBGroupPermission, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+kbGroupPermissionColumns+" FROM kb_group_permissions WHERE namespace = ? "+
			"ORDER BY group_id, document_name", namespace)
	if err != nil {
		return nil, newStateError("用户组授权读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanKBGroupPermissions(rows)
}

// ListForGroups returns the grants of the given groups inside one knowledge
// base. The caller passes only active memberships, so a dissolved group or a
// left/removed member contributes nothing.
func (r *KBGroupPermissionRepository) ListForGroups(ctx context.Context, namespace string, groupIDs []string) ([]KBGroupPermission, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat(", ?", len(groupIDs)-1)
	arguments := make([]any, 0, len(groupIDs)+1)
	arguments = append(arguments, namespace)
	for _, id := range groupIDs {
		arguments = append(arguments, id)
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+kbGroupPermissionColumns+" FROM kb_group_permissions WHERE namespace = ? AND group_id IN (?"+placeholders+") "+
			"ORDER BY group_id, document_name", arguments...)
	if err != nil {
		return nil, newStateError("用户组授权读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanKBGroupPermissions(rows)
}

// NamespacesForGroups returns the distinct namespaces granted to the given
// groups. It backs the knowledge-base visibility merge.
func (r *KBGroupPermissionRepository) NamespacesForGroups(ctx context.Context, groupIDs []string) ([]string, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat(", ?", len(groupIDs)-1)
	arguments := make([]any, 0, len(groupIDs))
	for _, id := range groupIDs {
		arguments = append(arguments, id)
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT DISTINCT namespace FROM kb_group_permissions WHERE group_id IN (?"+placeholders+")",
		arguments...)
	if err != nil {
		return nil, newStateError("用户组授权读取失败")
	}
	defer func() { _ = rows.Close() }()
	namespaces := make([]string, 0, 8)
	for rows.Next() {
		var namespace string
		if err := rows.Scan(&namespace); err != nil {
			return nil, newStateError("用户组授权读取失败")
		}
		namespaces = append(namespaces, namespace)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("用户组授权读取失败")
	}
	return namespaces, nil
}

// Delete removes every grant of one group inside one knowledge base and
// reports how many rows were removed.
func (r *KBGroupPermissionRepository) Delete(ctx context.Context, namespace, groupID string) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		"DELETE FROM kb_group_permissions WHERE namespace = ? AND group_id = ?", namespace, groupID)
	if err != nil {
		return 0, newStateError("用户组授权无法删除")
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, newStateError("用户组授权无法删除")
	}
	return removed, nil
}

func scanKBGroupPermissions(rows *sql.Rows) ([]KBGroupPermission, error) {
	permissions := make([]KBGroupPermission, 0, 8)
	for rows.Next() {
		var permission KBGroupPermission
		if err := rows.Scan(&permission.ID, &permission.Namespace, &permission.GroupID,
			&permission.DocumentName, &permission.AllowMask, &permission.GrantedBy,
			&permission.CreatedAt, &permission.UpdatedAt); err != nil {
			return nil, newStateError("用户组授权读取失败")
		}
		permissions = append(permissions, permission)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("用户组授权读取失败")
	}
	return permissions, nil
}
