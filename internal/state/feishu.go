package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// FeishuConnection is one Console account's stored Feishu authorization.
//
// AccessToken and RefreshToken are plaintext in memory only; the repository
// encrypts them with the shared state key before writing and decrypts them on
// read, using the same enc:v1: format as API keys and model profiles.
type FeishuConnection struct {
	UserID           string
	OpenID           string
	UnionID          string
	Name             string
	AvatarURL        string
	AccessToken      string
	RefreshToken     string
	AccessExpiresAt  int64
	RefreshExpiresAt int64
	Scope            string
	CreatedAt        string
	UpdatedAt        string
}

// FeishuOAuthState is one short-lived OAuth state row. Only the SHA-256 digest
// of the browser state value is stored, so a database read cannot be replayed.
type FeishuOAuthState struct {
	StateDigest string
	UserID      string
	RedirectURI string
	ExpiresAt   int64
	Consumed    bool
	CreatedAt   int64
}

// FeishuConnectionRepository stores per-account Feishu tokens.
type FeishuConnectionRepository struct {
	stores        *Stores
	encryptionKey string
}

// NewFeishuConnectionRepository builds the repository.
func NewFeishuConnectionRepository(stores *Stores, encryptionKey string) *FeishuConnectionRepository {
	return &FeishuConnectionRepository{stores: stores, encryptionKey: encryptionKey}
}

// Get reads one account's connection, decrypting both tokens. A missing row
// returns (nil, nil).
func (r *FeishuConnectionRepository) Get(ctx context.Context, userID string) (*FeishuConnection, error) {
	var (
		connection                               FeishuConnection
		unionID, avatarURL, refreshToken         sql.NullString
		accessToken, scope, createdAt, updatedAt string
		accessExpiresAt, refreshExpiresAt        int64
	)
	err := r.stores.DB.QueryRowContext(ctx,
		"SELECT user_id, open_id, union_id, name, avatar_url, access_token, refresh_token, "+
			"access_expires_at, refresh_expires_at, scope, created_at, updated_at "+
			"FROM feishu_connections WHERE user_id = ?", userID).
		Scan(&connection.UserID, &connection.OpenID, &unionID, &connection.Name, &avatarURL,
			&accessToken, &refreshToken, &accessExpiresAt, &refreshExpiresAt, &scope,
			&createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	plainAccess, err := Unprotect(accessToken, r.encryptionKey)
	if err != nil {
		return nil, err
	}
	plainRefresh, err := Unprotect(refreshToken.String, r.encryptionKey)
	if err != nil {
		return nil, err
	}
	connection.UnionID = unionID.String
	connection.AvatarURL = avatarURL.String
	connection.AccessToken = plainAccess
	connection.RefreshToken = plainRefresh
	connection.AccessExpiresAt = accessExpiresAt
	connection.RefreshExpiresAt = refreshExpiresAt
	connection.Scope = scope
	connection.CreatedAt = createdAt
	connection.UpdatedAt = updatedAt
	return &connection, nil
}

// Upsert writes one account's connection, replacing the previous tokens. The
// original created_at is preserved.
func (r *FeishuConnectionRepository) Upsert(ctx context.Context, connection *FeishuConnection) error {
	if connection == nil {
		return newValueError("Feishu connection is required")
	}
	protectedAccess, err := Protect(connection.AccessToken, r.encryptionKey)
	if err != nil {
		return err
	}
	protectedRefresh, err := Protect(connection.RefreshToken, r.encryptionKey)
	if err != nil {
		return err
	}
	now := nowISO()
	if connection.CreatedAt == "" {
		connection.CreatedAt = now
	}
	connection.UpdatedAt = now
	_, err = r.stores.DB.ExecContext(ctx,
		"INSERT INTO feishu_connections (user_id, open_id, union_id, name, avatar_url, "+
			"access_token, refresh_token, access_expires_at, refresh_expires_at, scope, "+
			"created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE open_id = VALUES(open_id), union_id = VALUES(union_id), "+
			"name = VALUES(name), avatar_url = VALUES(avatar_url), "+
			"access_token = VALUES(access_token), refresh_token = VALUES(refresh_token), "+
			"access_expires_at = VALUES(access_expires_at), "+
			"refresh_expires_at = VALUES(refresh_expires_at), scope = VALUES(scope), "+
			"updated_at = VALUES(updated_at)",
		connection.UserID, connection.OpenID, nullableString(connection.UnionID), connection.Name,
		nullableString(connection.AvatarURL), protectedAccess, protectedRefresh,
		connection.AccessExpiresAt, connection.RefreshExpiresAt, connection.Scope,
		connection.CreatedAt, connection.UpdatedAt)
	return err
}

// Delete removes one account's stored authorization.
func (r *FeishuConnectionRepository) Delete(ctx context.Context, userID string) (bool, error) {
	result, err := r.stores.DB.ExecContext(ctx,
		"DELETE FROM feishu_connections WHERE user_id = ?", userID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// FeishuOAuthStateRepository stores short-lived OAuth state digests.
type FeishuOAuthStateRepository struct {
	stores *Stores
}

// NewFeishuOAuthStateRepository builds the repository.
func NewFeishuOAuthStateRepository(stores *Stores) *FeishuOAuthStateRepository {
	return &FeishuOAuthStateRepository{stores: stores}
}

// Create stores one state digest and prunes stale rows.
func (r *FeishuOAuthStateRepository) Create(ctx context.Context, oauthState *FeishuOAuthState) error {
	if oauthState == nil || oauthState.StateDigest == "" || oauthState.UserID == "" {
		return newValueError("Feishu OAuth state is required")
	}
	now := time.Now().Unix()
	// Keep the table bounded without a background job: consumed rows older than
	// one hour and expired rows are no longer useful.
	_, _ = r.stores.DB.ExecContext(ctx,
		"DELETE FROM feishu_oauth_states WHERE expires_at <= ? OR (consumed = 1 AND created_at <= ?)",
		now, now-3600)
	_, err := r.stores.DB.ExecContext(ctx,
		"INSERT INTO feishu_oauth_states (state_digest, user_id, redirect_uri, expires_at, consumed, created_at) "+
			"VALUES (?, ?, ?, ?, 0, ?)",
		oauthState.StateDigest, oauthState.UserID, oauthState.RedirectURI,
		oauthState.ExpiresAt, oauthState.CreatedAt)
	return err
}

// Consume atomically marks one state value used and returns it. A missing,
// expired, consumed or other-user state returns (nil, nil).
func (r *FeishuOAuthStateRepository) Consume(ctx context.Context, digest, userID string, now int64) (*FeishuOAuthState, error) {
	result, err := r.stores.DB.ExecContext(ctx,
		"UPDATE feishu_oauth_states SET consumed = 1 "+
			"WHERE state_digest = ? AND user_id = ? AND consumed = 0 AND expires_at > ?",
		digest, userID, now)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, nil
	}
	var oauthState FeishuOAuthState
	err = r.stores.DB.QueryRowContext(ctx,
		"SELECT state_digest, user_id, redirect_uri, expires_at, consumed, created_at "+
			"FROM feishu_oauth_states WHERE state_digest = ?", digest).
		Scan(&oauthState.StateDigest, &oauthState.UserID, &oauthState.RedirectURI,
			&oauthState.ExpiresAt, &oauthState.Consumed, &oauthState.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &oauthState, nil
}

// nullableString maps "" to SQL NULL for nullable columns.
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
