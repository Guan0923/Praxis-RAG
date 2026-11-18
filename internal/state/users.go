package state

import (
	"context"
	"database/sql"
)

// User roles. Admins manage users and service configuration; VIP accounts may
// share the knowledge bases they own; every other account is a normal user.
const (
	RoleAdmin = "admin"
	RoleVIP   = "vip"
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
	// Email is the normalized (lower case) account address; empty when the
	// account has none. MySQL keeps the unique index NULL-tolerant, so any
	// number of accounts may omit it.
	Email         string
	EmailVerified bool
	// Age and Gender are optional profile fields; nil/empty means "not set".
	Age    *int
	Gender string
	// AvatarUpdatedAt is the avatar row timestamp (the bytes are not loaded with
	// the account); nil means the account has no avatar.
	AvatarUpdatedAt *string
}

// IsAdmin reports whether the account may administer users and configuration.
func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

// IsVIP reports whether the account holds the VIP role.
func (u User) IsVIP() bool { return u.Role == RoleVIP }

// CanShare reports whether the account may manage knowledge-base permissions
// and invitations. Only administrators and VIPs can share.
func (u User) CanShare() bool { return u.IsAdmin() || u.IsVIP() }

// IsRole reports whether value is one of the supported account roles.
func IsRole(value string) bool {
	return value == RoleAdmin || value == RoleVIP || value == RoleUser
}

// UserRepository persists accounts and their PBKDF2 password hashes.
type UserRepository struct{ db *sql.DB }

func newUserRepository(db *sql.DB) *UserRepository { return &UserRepository{db: db} }

// userSelectColumns is the projection shared by every users read. The avatar
// join only contributes updated_at: avatar bytes are always read on demand so a
// user list never carries image data.
const userSelectColumns = "u.id, u.username, u.password_hash, u.role, u.enabled, " +
	"u.created_at, u.updated_at, u.last_login_at, u.email, u.email_verified, " +
	"u.age, u.gender, a.updated_at"

// userFromClause is the FROM/JOIN clause shared by every users read.
const userFromClause = " FROM users u LEFT JOIN user_avatars a ON a.user_id = u.id"

// Create inserts one account. The first account in an empty table becomes the
// administrator, so a fresh deployment is bootstrappable without manual SQL.
func (r *UserRepository) Create(ctx context.Context, username, passwordHash string) (User, error) {
	return r.CreateWithProfile(ctx, username, passwordHash, "")
}

// CreateWithProfile inserts one account with an optional e-mail address. An
// empty email stores NULL, which keeps the unique index satisfied for accounts
// that never configured one. The first account in an empty table becomes the
// administrator, exactly like Create.
func (r *UserRepository) CreateWithProfile(ctx context.Context, username, passwordHash, email string) (User, error) {
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
	var emailValue any
	if email != "" {
		emailValue = email
		user.Email = email
	}
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO users (id, username, password_hash, role, enabled, created_at, updated_at, email) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		id, username, passwordHash, role, true, timestamp, timestamp, emailValue); err != nil {
		if isUniqueViolation(err) {
			return User{}, newStateConflict("用户名或邮箱已存在")
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
		"SELECT "+userSelectColumns+userFromClause+" WHERE u.username = ?", username)
	return scanUser(row, true)
}

// ByID returns one account, or nil when it does not exist.
func (r *UserRepository) ByID(ctx context.Context, id string) (*User, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+userSelectColumns+userFromClause+" WHERE u.id = ?", id)
	user, _, err := scanUser(row, false)
	if err != nil || user == nil {
		return user, err
	}
	return user, nil
}

// ByEmail returns the account owning an e-mail address, or nil when none does.
// The caller normalizes the address to lower case first.
func (r *UserRepository) ByEmail(ctx context.Context, email string) (*User, error) {
	if email == "" {
		return nil, nil
	}
	row := r.db.QueryRowContext(ctx,
		"SELECT "+userSelectColumns+userFromClause+" WHERE u.email = ?", email)
	user, _, err := scanUser(row, false)
	if err != nil || user == nil {
		return user, err
	}
	return user, nil
}

// List returns every account, oldest first.
func (r *UserRepository) List(ctx context.Context) ([]User, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+userSelectColumns+userFromClause+" ORDER BY u.created_at, u.username")
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

// UpdateProfile replaces the editable profile fields of one account. An empty
// username keeps the stored one; a nil age and an empty gender clear the
// optional fields. A username collision is reported as StateConflict and a
// missing account as (nil, nil).
func (r *UserRepository) UpdateProfile(ctx context.Context, id, username string, age *int, gender string) (*User, error) {
	var ageValue any
	if age != nil {
		ageValue = *age
	}
	var genderValue any
	if gender != "" {
		genderValue = gender
	}
	result, err := r.db.ExecContext(ctx,
		"UPDATE users SET username = CASE WHEN ? = '' THEN username ELSE ? END, "+
			"age = ?, gender = ?, updated_at = ? WHERE id = ?",
		username, username, ageValue, genderValue, nowISO(), id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, newStateConflict("用户名已存在")
		}
		return nil, newStateError("用户无法保存")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, newStateError("用户无法保存")
	}
	if affected == 0 {
		// updated_at always changes, so no affected row means the account is gone.
		return nil, nil
	}
	return r.ByID(ctx, id)
}

// UpdatePassword replaces the PBKDF2 hash of one account.
func (r *UserRepository) UpdatePassword(ctx context.Context, id, passwordHash string) error {
	if _, err := r.db.ExecContext(ctx,
		"UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?",
		passwordHash, nowISO(), id); err != nil {
		return newStateError("密码无法保存")
	}
	return nil
}

// UpdateEmail sets the account e-mail address and its verified flag, reporting
// whether the account existed. A collision with another account is reported as
// StateConflict.
func (r *UserRepository) UpdateEmail(ctx context.Context, id, email string, verified bool) (bool, error) {
	var emailValue any
	if email != "" {
		emailValue = email
	}
	result, err := r.db.ExecContext(ctx,
		"UPDATE users SET email = ?, email_verified = ?, updated_at = ? WHERE id = ?",
		emailValue, verified, nowISO(), id)
	if err != nil {
		if isUniqueViolation(err) {
			return false, newStateConflict("该邮箱已被其他账号使用")
		}
		return false, newStateError("邮箱无法保存")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, newStateError("邮箱无法保存")
	}
	return affected > 0, nil
}

// Delete removes one account and reports whether it existed. The transaction
// also drops the account's sessions, the API keys it issued, the knowledge-base
// permissions granted to it, and the invitations it issued or received;
// knowledge bases it owned become unowned (administrator-only) instead of being
// deleted with the account. Deleting the account must delete its API keys: an
// issued credential stays valid for tenant traffic even after the login account
// is gone.
func (r *UserRepository) Delete(ctx context.Context, id string) (bool, error) {
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, newStateError("用户无法删除")
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, "DELETE FROM user_sessions WHERE user_id = ?", id); err != nil {
		return false, newStateError("用户无法删除")
	}

	if _, err := transaction.ExecContext(ctx, "DELETE FROM api_keys WHERE owner_user_id = ?", id); err != nil {
		return false, newStateError("用户无法删除")
	}
	if _, err := transaction.ExecContext(ctx, "DELETE FROM kb_permissions WHERE grantee_user_id = ?", id); err != nil {
		return false, newStateError("用户无法删除")
	}
	if _, err := transaction.ExecContext(ctx,
		"DELETE FROM kb_invitations WHERE inviter_user_id = ? OR invitee_user_id = ?", id, id); err != nil {
		return false, newStateError("用户无法删除")
	}
	// The account's user groups are dissolved with their members, invitations
	// and knowledge-base grants, and its own memberships and invitations are
	// removed, so no residual group row keeps effect after the account is gone.
	if _, err := transaction.ExecContext(ctx,
		"DELETE FROM kb_group_permissions WHERE group_id IN (SELECT id FROM user_groups WHERE owner_user_id = ?)", id); err != nil {
		return false, newStateError("用户无法删除")
	}
	if _, err := transaction.ExecContext(ctx,
		"DELETE FROM user_group_invitations WHERE group_id IN (SELECT id FROM user_groups WHERE owner_user_id = ?)", id); err != nil {
		return false, newStateError("用户无法删除")
	}
	if _, err := transaction.ExecContext(ctx,
		"DELETE FROM user_group_members WHERE group_id IN (SELECT id FROM user_groups WHERE owner_user_id = ?)", id); err != nil {
		return false, newStateError("用户无法删除")
	}
	if _, err := transaction.ExecContext(ctx, "DELETE FROM user_groups WHERE owner_user_id = ?", id); err != nil {
		return false, newStateError("用户无法删除")
	}
	if _, err := transaction.ExecContext(ctx, "DELETE FROM user_group_members WHERE user_id = ?", id); err != nil {
		return false, newStateError("用户无法删除")
	}
	if _, err := transaction.ExecContext(ctx,
		"DELETE FROM user_group_invitations WHERE inviter_user_id = ? OR invitee_user_id = ?", id, id); err != nil {
		return false, newStateError("用户无法删除")
	}
	if _, err := transaction.ExecContext(ctx, "DELETE FROM kb_group_permissions WHERE granted_by = ?", id); err != nil {
		return false, newStateError("用户无法删除")
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE knowledge_bases SET owner_user_id = NULL WHERE owner_user_id = ?", id); err != nil {
		return false, newStateError("用户无法删除")
	}
	// The avatar and pending verification codes belong to the account and would
	// otherwise leak rows after it disappears.
	if _, err := transaction.ExecContext(ctx, "DELETE FROM user_avatars WHERE user_id = ?", id); err != nil {
		return false, newStateError("用户无法删除")
	}
	if _, err := transaction.ExecContext(ctx, "DELETE FROM email_verification_codes WHERE email = (SELECT email FROM users WHERE id = ?)", id); err != nil {
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
		user          User
		passwordHash  string
		lastLogin     sql.NullString
		email         sql.NullString
		age           sql.NullInt64
		gender        sql.NullString
		avatarUpdated sql.NullString
	)
	if err := row.Scan(&user.ID, &user.Username, &passwordHash, &user.Role, &user.Enabled,
		&user.CreatedAt, &user.UpdatedAt, &lastLogin, &email, &user.EmailVerified,
		&age, &gender, &avatarUpdated); err != nil {
		if err == sql.ErrNoRows {
			return nil, "", nil
		}
		return nil, "", newStateError("用户读取失败")
	}
	if lastLogin.Valid {
		value := lastLogin.String
		user.LastLoginAt = &value
	}
	if email.Valid {
		user.Email = email.String
	}
	if age.Valid {
		value := int(age.Int64)
		user.Age = &value
	}
	if gender.Valid {
		user.Gender = gender.String
	}
	if avatarUpdated.Valid {
		value := avatarUpdated.String
		user.AvatarUpdatedAt = &value
	}
	if !withPassword {
		passwordHash = ""
	}
	return &user, passwordHash, nil
}
