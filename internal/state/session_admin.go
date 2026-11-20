package state

import (
	"context"
	"database/sql"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/audit"
)

type SessionAccount struct {
	ID       string  `json:"id"`
	Username string  `json:"username"`
	Role     string  `json:"role"`
	Enabled  bool    `json:"enabled"`
	Sessions int     `json:"sessions"`
	Presence string  `json:"presence"`
	LastSeen *string `json:"last_seen_at"`
}

// ListSessionAccounts includes accounts that have never logged in. Expired or
// revoked sessions contribute historical last activity but never presence.
func (r *UserSessionRepository) ListSessionAccounts(ctx context.Context, now time.Time) ([]SessionAccount, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT u.id,u.username,u.role,u.enabled,
 COUNT(CASE WHEN u.enabled AND NOT s.revoked AND s.expires_at > ? THEN 1 END),
 MAX(CASE WHEN u.enabled AND NOT s.revoked AND s.expires_at > ? THEN s.last_seen_at END),
 MAX(s.last_seen_at)
 FROM users u LEFT JOIN user_sessions s ON s.user_id=u.id
 GROUP BY u.id,u.username,u.role,u.enabled ORDER BY u.username,u.id`, now.UTC().Format(isoTimestampFormat), now.UTC().Format(isoTimestampFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cutoff := now.UTC().Add(-5 * time.Minute).Format(isoTimestampFormat)
	accounts := []SessionAccount{}
	for rows.Next() {
		var user SessionAccount
		var lastActive sql.NullString
		if err := rows.Scan(&user.ID, &user.Username, &user.Role, &user.Enabled, &user.Sessions, &lastActive, &user.LastSeen); err != nil {
			return nil, err
		}
		switch {
		case lastActive.Valid && lastActive.String >= cutoff:
			user.Presence = "online"
		case user.Sessions > 0:
			user.Presence = "idle"
		default:
			user.Presence = "offline"
		}
		accounts = append(accounts, user)
	}
	return accounts, rows.Err()
}

// ForceLogout commits the audit trail together with the session revocations.
func (r *UserSessionRepository) ForceLogout(ctx context.Context, id string) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var username string
	if err := tx.QueryRowContext(ctx, "SELECT username FROM users WHERE id=? FOR UPDATE", id).Scan(&username); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, "UPDATE user_sessions SET revoked=1 WHERE user_id=? AND revoked=0 AND expires_at>?", id, now.Format(isoTimestampFormat))
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count > 0 {
		entry := audit.NewEntry(ctx, now, "REST", "force_logout", "", audit.Target{Name: username + "（" + id + "）", Outcome: "success"})
		if err := (audit.Store{DB: tx}).Append(ctx, []audit.Entry{entry}); err != nil {
			return 0, err
		}
	}
	return count, tx.Commit()
}
