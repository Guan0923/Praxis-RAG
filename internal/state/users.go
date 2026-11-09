package state

import (
	"context"
	"database/sql"
)

// User roles. Admins manage users and service configuration; every other
// account is a normal user.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// User is one Console account. The password hash never leaves the repository
// except through ByUsername, which the auth service uses for login.
type User struct {
	ID          string
	Username    string
	Role        string
	Enabled     bool
	CreatedAt   string
	UpdatedAt   string
	LastLoginAt *string
}

// IsAdmin reports whether the account may administer users and configuration.
func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

// UserRepository persists accounts and their PBKDF2 password hashes.
type UserRepository struct{ db *sql.DB }

func newUserRepository(db *sql.DB) *UserRepository { return &UserRepository{db: db} }

// Create inserts one account. The first account in an empty table becomes the
// administrator, so a fresh deployment is bootstrappable without manual SQL.
func (r *UserRepository) Create(ctx context.Context, username, passwordHash string) (User, error) {
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, newStateError("用户无法保存")
	}
	defer func() { _ = transaction.Rollback() }()
	var count int
	// FOR UPDATE serializes concurrent registrations so exactly one first user
	// can win the admin role.
	if err := transaction.QueryRowContext(ctx, "SELECT COUNT(*) FROM users FOR UPDATE").Scan(&count); err != nil {
		return User{}, newStateError("用户无法保存")
	}
	role := RoleUser
	if count == 0 {
		role = RoleAdmin
	}
	id, err := randomHex(16)
	if err != nil {
		return User{}, newStateError("用户无法保存")
	}
	timestamp := nowISO()
	user := User{ID: id, Username: username, Role: role, Enabled: true, CreatedAt: timestamp, UpdatedAt: timestamp}
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO users (id, username, password_hash, role, enabled, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)",
		id, username, passwordHash, role, true, timestamp, timestamp); err != nil {
		if isUniqueViolation(err) {
			return User{}, newStateConflict("用户名已存在")
		}
		return User{}, newStateError("用户无法保存")
	}
	if err := transaction.Commit(); err != nil {
		return User{}, newStateError("用户无法保存")
	}
	return user, nil
}

// ByUsername returns one account and its password hash for login.
func (r *UserRepository) ByUsername(ctx context.Context, username string) (*User, string, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT id, username, password_hash, role, enabled, created_at, updated_at, last_login_at "+
			"FROM users WHERE username = ?", username)
	return scanUser(row, true)
}

// ByID returns one account, or nil when it does not exist.
func (r *UserRepository) ByID(ctx context.Context, id string) (*User, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT id, username, password_hash, role, enabled, created_at, updated_at, last_login_at "+
			"FROM users WHERE id = ?", id)
	user, _, err := scanUser(row, false)
	if err != nil || user == nil {
		return user, err
	}
	return user, nil
}

// List returns every account, oldest first.
func (r *UserRepository) List(ctx context.Context) ([]User, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT id, username, password_hash, role, enabled, created_at, updated_at, last_login_at "+
			"FROM users ORDER BY created_at, username")
	if err != nil {
		return nil, newStateError("用户列表读取失败")
	}
	defer func() { _ = rows.Close() }()
	users := make([]User, 0, 8)
	for rows.Next() {
		user, _, err := scanUser(rows, false)
		if err != nil {
			return nil, err
		}
		if user != nil {
			users = append(users, *user)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("用户列表读取失败")
	}
	return users, nil
}

// Count returns the number of accounts.
func (r *UserRepository) Count(ctx context.Context) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		return 0, newStateError("用户数量读取失败")
	}
	return count, nil
}

// CountEnabledAdmins returns the number of enabled administrator accounts. It
// guards the last-admin rules applied by the auth service.
func (r *UserRepository) CountEnabledAdmins(ctx context.Context) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM users WHERE role = ? AND enabled = ?", RoleAdmin, true).Scan(&count); err != nil {
		return 0, newStateError("用户数量读取失败")
	}
	return count, nil
}

// Update replaces the role and enabled flag of one account.
func (r *UserRepository) Update(ctx context.Context, id, role string, enabled bool) (*User, error) {
	result, err := r.db.ExecContext(ctx,
		"UPDATE users SET role = ?, enabled = ?, updated_at = ? WHERE id = ?",
		role, enabled, nowISO(), id)
	if err != nil {
		return nil, newStateError("用户无法保存")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, newStateError("用户无法保存")
	}
	if affected == 0 {
		return nil, nil
	}
	return r.ByID(ctx, id)
}

// Delete removes one account and reports whether it existed.
func (r *UserRepository) Delete(ctx context.Context, id string) (bool, error) {
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, newStateError("用户无法删除")
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, "DELETE FROM user_sessions WHERE user_id = ?", id); err != nil {
		return false, newStateError("用户无法删除")
	}
	result, err := transaction.ExecContext(ctx, "DELETE FROM users WHERE id = ?", id)
	if err != nil {
		return false, newStateError("用户无法删除")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, newStateError("用户无法删除")
	}
	if err := transaction.Commit(); err != nil {
		return false, newStateError("用户无法删除")
	}
	return affected > 0, nil
}

// TouchLogin records a successful login timestamp.
func (r *UserRepository) TouchLogin(ctx context.Context, id string) error {
	if _, err := r.db.ExecContext(ctx, "UPDATE users SET last_login_at = ?, updated_at = ? WHERE id = ?",
		nowISO(), nowISO(), id); err != nil {
		return newStateError("用户无法保存")
	}
	return nil
}

// UserSession is one issued login session. Only the token digest is stored.
type UserSession struct {
	ID        string
	UserID    string
	CSRF      string
	ExpiresAt string
	CreatedAt string
	Revoked   bool
}

// UserSessionRepository persists issued login sessions.
type UserSessionRepository struct{ db *sql.DB }

func newUserSessionRepository(db *sql.DB) *UserSessionRepository {
	return &UserSessionRepository{db: db}
}

// Create stores one session digest.
func (r *UserSessionRepository) Create(ctx context.Context, userID, tokenDigest, csrf, expiresAt string) (UserSession, error) {
	id, err := randomHex(16)
	if err != nil {
		return UserSession{}, newStateError("会话无法创建")
	}
	timestamp := nowISO()
	if _, err := r.db.ExecContext(ctx,
		"INSERT INTO user_sessions (id, user_id, token_digest, csrf_token, expires_at, created_at, last_seen_at, revoked) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		id, userID, tokenDigest, csrf, expiresAt, timestamp, timestamp, false); err != nil {
		return UserSession{}, newStateError("会话无法创建")
	}
	return UserSession{ID: id, UserID: userID, CSRF: csrf, ExpiresAt: expiresAt, CreatedAt: timestamp}, nil
}

// ByDigest returns the session for a token digest, or nil when it does not exist.
func (r *UserSessionRepository) ByDigest(ctx context.Context, tokenDigest string) (*UserSession, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT id, user_id, csrf_token, expires_at, created_at, revoked FROM user_sessions WHERE token_digest = ?",
		tokenDigest)
	var session UserSession
	if err := row.Scan(&session.ID, &session.UserID, &session.CSRF, &session.ExpiresAt,
		&session.CreatedAt, &session.Revoked); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, newStateError("会话读取失败")
	}
	return &session, nil
}

// Touch records activity on one session.
func (r *UserSessionRepository) Touch(ctx context.Context, id string) error {
	if _, err := r.db.ExecContext(ctx, "UPDATE user_sessions SET last_seen_at = ? WHERE id = ?", nowISO(), id); err != nil {
		return newStateError("会话无法保存")
	}
	return nil
}

// Revoke marks one session unusable.
func (r *UserSessionRepository) Revoke(ctx context.Context, id string) error {
	if _, err := r.db.ExecContext(ctx, "UPDATE user_sessions SET revoked = ? WHERE id = ?", true, id); err != nil {
		return newStateError("会话无法撤销")
	}
	return nil
}

// RevokeUserSessions marks every live session of one account unusable.
func (r *UserSessionRepository) RevokeUserSessions(ctx context.Context, userID string) error {
	if _, err := r.db.ExecContext(ctx,
		"UPDATE user_sessions SET revoked = ? WHERE user_id = ? AND revoked = ?", true, userID, false); err != nil {
		return newStateError("会话无法撤销")
	}
	return nil
}

// DeleteExpired removes sessions that expired before the given timestamp.
func (r *UserSessionRepository) DeleteExpired(ctx context.Context, before string) (int64, error) {
	result, err := r.db.ExecContext(ctx, "DELETE FROM user_sessions WHERE expires_at < ?", before)
	if err != nil {
		return 0, newStateError("会话无法清理")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, newStateError("会话无法清理")
	}
	return affected, nil
}

// scanUser scans the shared column list. withPassword selects whether the
// password hash is returned; it is always read so both variants share one
// scanner.
func scanUser(row interface{ Scan(dest ...any) error }, withPassword bool) (*User, string, error) {
	var (
		user         User
		passwordHash string
		lastLogin    sql.NullString
	)
	if err := row.Scan(&user.ID, &user.Username, &passwordHash, &user.Role, &user.Enabled,
		&user.CreatedAt, &user.UpdatedAt, &lastLogin); err != nil {
		if err == sql.ErrNoRows {
			return nil, "", nil
		}
		return nil, "", newStateError("用户读取失败")
	}
	if lastLogin.Valid {
		value := lastLogin.String
		user.LastLoginAt = &value
	}
	if !withPassword {
		passwordHash = ""
	}
	return &user, passwordHash, nil
}
