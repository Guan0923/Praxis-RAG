package state

import (
	"context"
	"database/sql"
)

// MaxAvatarBytes is the hard limit for one profile image (2 MiB). The HTTP layer
// enforces it while streaming the multipart body; the repository enforces it
// again so a direct caller cannot store an unbounded blob.
const MaxAvatarBytes = 2 << 20

// UserAvatar is one stored profile image. The bytes are only loaded by Get; the
// users projection carries just the updated_at timestamp.
type UserAvatar struct {
	UserID      string
	ContentType string
	Size        int64
	SHA256      string
	Content     []byte
	UpdatedAt   string
}

// UserAvatarRepository persists the per-account profile image.
type UserAvatarRepository struct{ db *sql.DB }

func newUserAvatarRepository(db *sql.DB) *UserAvatarRepository {
	return &UserAvatarRepository{db: db}
}

// Set stores (or replaces) the avatar of one account.
func (r *UserAvatarRepository) Set(ctx context.Context, userID, contentType string, data []byte) error {
	if len(data) == 0 || len(data) > MaxAvatarBytes {
		return newValueError("头像需为 PNG/JPEG/WEBP/GIF 图片且不超过 2MB")
	}
	if _, err := r.db.ExecContext(ctx,
		"INSERT INTO user_avatars (user_id, content_type, size, sha256, content, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE content_type = VALUES(content_type), size = VALUES(size), "+
			"sha256 = VALUES(sha256), content = VALUES(content), updated_at = VALUES(updated_at)",
		userID, contentType, int64(len(data)), sha256Sum(data), data, nowISO()); err != nil {
		return newStateError("头像无法保存")
	}
	return nil
}

// Get returns the avatar of one account, or nil when it has none.
func (r *UserAvatarRepository) Get(ctx context.Context, userID string) (*UserAvatar, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT user_id, content_type, size, sha256, content, updated_at "+
			"FROM user_avatars WHERE user_id = ?", userID)
	var avatar UserAvatar
	if err := row.Scan(&avatar.UserID, &avatar.ContentType, &avatar.Size, &avatar.SHA256,
		&avatar.Content, &avatar.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, newStateError("头像读取失败")
	}
	return &avatar, nil
}

// Delete removes the avatar of one account and reports whether one existed.
func (r *UserAvatarRepository) Delete(ctx context.Context, userID string) (bool, error) {
	result, err := r.db.ExecContext(ctx, "DELETE FROM user_avatars WHERE user_id = ?", userID)
	if err != nil {
		return false, newStateError("头像无法删除")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, newStateError("头像无法删除")
	}
	return affected > 0, nil
}

// EmailCode is one issued verification code. Only the SHA-256 digest of the
// 6-digit code is stored, so a database leak cannot be replayed.
type EmailCode struct {
	ID         string
	Email      string
	Purpose    string
	CodeDigest string
	Attempts   int
	Consumed   bool
	ExpiresAt  string
	CreatedAt  string
}

// EmailVerificationRepository persists the registration and e-mail-change
// verification codes.
type EmailVerificationRepository struct{ db *sql.DB }

func newEmailVerificationRepository(db *sql.DB) *EmailVerificationRepository {
	return &EmailVerificationRepository{db: db}
}

// Create issues one code, invalidating any previously unused code for the same
// address and purpose in the same transaction.
func (r *EmailVerificationRepository) Create(ctx context.Context, email, purpose, digest, expiresAt string) error {
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return newStateError("验证码无法保存")
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx,
		"UPDATE email_verification_codes SET consumed = ? WHERE email = ? AND purpose = ? AND consumed = ?",
		true, email, purpose, false); err != nil {
		return newStateError("验证码无法保存")
	}
	id, err := randomHex(16)
	if err != nil {
		return newStateError("验证码无法保存")
	}
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO email_verification_codes "+
			"(id, email, purpose, code_digest, attempts, consumed, expires_at, created_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		id, email, purpose, digest, 0, false, expiresAt, nowISO()); err != nil {
		return newStateError("验证码无法保存")
	}
	if err := transaction.Commit(); err != nil {
		return newStateError("验证码无法保存")
	}
	return nil
}

// LatestActive returns the newest unused code for an address and purpose that
// has not expired before now, or nil when none is usable.
func (r *EmailVerificationRepository) LatestActive(ctx context.Context, email, purpose, now string) (*EmailCode, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT id, email, purpose, code_digest, attempts, consumed, expires_at, created_at "+
			"FROM email_verification_codes "+
			"WHERE email = ? AND purpose = ? AND consumed = ? AND expires_at > ? "+
			"ORDER BY created_at DESC, id DESC LIMIT 1",
		email, purpose, false, now)
	var code EmailCode
	if err := row.Scan(&code.ID, &code.Email, &code.Purpose, &code.CodeDigest, &code.Attempts,
		&code.Consumed, &code.ExpiresAt, &code.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, newStateError("验证码读取失败")
	}
	return &code, nil
}

// Consume marks one code unusable after a successful verification.
func (r *EmailVerificationRepository) Consume(ctx context.Context, id string) error {
	if _, err := r.db.ExecContext(ctx,
		"UPDATE email_verification_codes SET consumed = ? WHERE id = ?", true, id); err != nil {
		return newStateError("验证码无法更新")
	}
	return nil
}

// IncrementAttempts records one wrong guess so brute forcing is bounded.
func (r *EmailVerificationRepository) IncrementAttempts(ctx context.Context, id string) error {
	if _, err := r.db.ExecContext(ctx,
		"UPDATE email_verification_codes SET attempts = attempts + 1 WHERE id = ?", id); err != nil {
		return newStateError("验证码无法更新")
	}
	return nil
}

// DeleteExpired removes every code that expired before the supplied timestamp.
func (r *EmailVerificationRepository) DeleteExpired(ctx context.Context, before string) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		"DELETE FROM email_verification_codes WHERE expires_at < ?", before)
	if err != nil {
		return 0, newStateError("验证码无法清理")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, newStateError("验证码无法清理")
	}
	return affected, nil
}

// AppSettingsRepository persists the Console-editable key/value settings.
type AppSettingsRepository struct{ db *sql.DB }

func newAppSettingsRepository(db *sql.DB) *AppSettingsRepository {
	return &AppSettingsRepository{db: db}
}

// Get returns one stored setting value. found=false means the key was never
// written (or was removed).
func (r *AppSettingsRepository) Get(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := r.db.QueryRowContext(ctx,
		"SELECT setting_value FROM app_settings WHERE setting_key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, newStateError("系统设置读取失败")
	}
	return value, true, nil
}

// Put writes (or replaces) one setting value.
func (r *AppSettingsRepository) Put(ctx context.Context, key, value string) error {
	if _, err := r.db.ExecContext(ctx,
		"INSERT INTO app_settings (setting_key, setting_value, updated_at) VALUES (?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE setting_value = VALUES(setting_value), updated_at = VALUES(updated_at)",
		key, value, nowISO()); err != nil {
		return newStateError("系统设置无法保存")
	}
	return nil
}
