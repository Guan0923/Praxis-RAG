package state

import (
	"context"
	"database/sql"
	"errors"
)

// ModelProfile is one saved Console model configuration (Embedding, Reranker,
// VLM or JEV). Only the protected form of the API key is stored; decrypting it
// stays in the kbconfig layer like knowledge-base provider credentials.
type ModelProfile struct {
	ID      string
	Kind    string
	Name    string
	BaseURL string
	Model   string
	// OwnerUserID is the Console account that owns this profile. Every account
	// manages its own list; an empty value marks a legacy row that predates
	// ownership (the startup migration assigns those to the first administrator).
	OwnerUserID string
	// OwnerUsername is the read-only label joined from users for the
	// administrator's view. It is empty when the owner no longer exists.
	OwnerUsername string
	ProtectedKey  string
	// VectorSize is the probed dense dimension, recorded for embedding profiles
	// only and NULL for the other kinds.
	VectorSize *int
	CreatedAt  string
	UpdatedAt  string
}

// ModelProfileRepository persists the saved model configuration list.
type ModelProfileRepository struct{ db *sql.DB }

func newModelProfileRepository(db *sql.DB) *ModelProfileRepository {
	return &ModelProfileRepository{db: db}
}

// modelProfileInsertColumns is the writable column list (owner_username is a
// join-only label, never stored).
const modelProfileInsertColumns = "id, kind, name, base_url, model, owner_user_id, protected_key, vector_size, created_at, updated_at"

// modelProfileSelect reads one row plus the read-only owner label. The LEFT JOIN
// keeps a profile whose owner account was deleted visible to administrators.
const modelProfileSelect = "SELECT p.id, p.kind, p.name, p.base_url, p.model, p.owner_user_id, " +
	"COALESCE(u.username, ''), p.protected_key, p.vector_size, p.created_at, p.updated_at " +
	"FROM model_profiles p LEFT JOIN users u ON u.id = p.owner_user_id"

// List returns every profile ordered by kind then display name. It feeds the
// administrator's read-only view; per-account lists use ListByOwner.
func (r *ModelProfileRepository) List(ctx context.Context) ([]ModelProfile, error) {
	rows, err := r.db.QueryContext(ctx, modelProfileSelect+" ORDER BY p.kind, p.name")
	if err != nil {
		return nil, newStateError("模型配置读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanModelProfiles(rows)
}

// ListByOwner returns one account's profiles ordered by kind then display name.
func (r *ModelProfileRepository) ListByOwner(ctx context.Context, ownerUserID string) ([]ModelProfile, error) {
	rows, err := r.db.QueryContext(ctx,
		modelProfileSelect+" WHERE p.owner_user_id = ? ORDER BY p.kind, p.name",
		ownerUserID)
	if err != nil {
		return nil, newStateError("模型配置读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanModelProfiles(rows)
}

// ListByOwnerAndKind returns one account's profiles of one kind.
func (r *ModelProfileRepository) ListByOwnerAndKind(ctx context.Context, ownerUserID, kind string) ([]ModelProfile, error) {
	rows, err := r.db.QueryContext(ctx,
		modelProfileSelect+" WHERE p.owner_user_id = ? AND p.kind = ? ORDER BY p.name",
		ownerUserID, kind)
	if err != nil {
		return nil, newStateError("模型配置读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanModelProfiles(rows)
}

// ListByKind returns the profiles of one kind for every account. It keeps the
// administrative view complete and is never used to resolve a knowledge base's
// provider.
func (r *ModelProfileRepository) ListByKind(ctx context.Context, kind string) ([]ModelProfile, error) {
	rows, err := r.db.QueryContext(ctx,
		modelProfileSelect+" WHERE p.kind = ? ORDER BY p.name", kind)
	if err != nil {
		return nil, newStateError("模型配置读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanModelProfiles(rows)
}

// Get returns one profile, or nil when it does not exist.
func (r *ModelProfileRepository) Get(ctx context.Context, id string) (*ModelProfile, error) {
	profile, err := scanModelProfile(r.db.QueryRowContext(ctx, modelProfileSelect+" WHERE p.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, newStateError("模型配置读取失败")
	}
	return profile, nil
}

// Create inserts one profile, generating the id and timestamps when they are
// empty. A duplicate (owner, kind, name) triple is reported as StateConflict, so
// two accounts can keep a profile with the same display name.
func (r *ModelProfileRepository) Create(ctx context.Context, profile ModelProfile) (ModelProfile, error) {
	if profile.ID == "" {
		generated, err := randomHex(16)
		if err != nil {
			return ModelProfile{}, newStateError("模型配置无法保存")
		}
		profile.ID = generated
	}
	timestamp := nowISO()
	if profile.CreatedAt == "" {
		profile.CreatedAt = timestamp
	}
	if profile.UpdatedAt == "" {
		profile.UpdatedAt = timestamp
	}
	if _, err := r.db.ExecContext(ctx,
		"INSERT INTO model_profiles ("+modelProfileInsertColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		profile.ID, profile.Kind, profile.Name, profile.BaseURL, profile.Model,
		nullString(profile.OwnerUserID), nullString(profile.ProtectedKey),
		nullInteger(profile.VectorSize), profile.CreatedAt, profile.UpdatedAt); err != nil {
		if isUniqueViolation(err) {
			return ModelProfile{}, newStateConflict("模型配置已存在")
		}
		return ModelProfile{}, newStateError("模型配置无法保存")
	}
	return profile, nil
}

// Update rewrites one profile by id. The kind and the owner are immutable and
// always kept from the stored row; a missing row is reported as LookupError.
func (r *ModelProfileRepository) Update(ctx context.Context, profile ModelProfile) (ModelProfile, error) {
	if profile.UpdatedAt == "" {
		profile.UpdatedAt = nowISO()
	}
	if _, err := r.db.ExecContext(ctx,
		"UPDATE model_profiles SET name = ?, base_url = ?, model = ?, protected_key = ?, "+
			"vector_size = ?, updated_at = ? WHERE id = ?",
		profile.Name, profile.BaseURL, profile.Model, nullString(profile.ProtectedKey),
		nullInteger(profile.VectorSize), profile.UpdatedAt, profile.ID); err != nil {
		// A duplicate (owner, kind, name) collision is a user-visible conflict,
		// exactly like Create.
		if isUniqueViolation(err) {
			return ModelProfile{}, newStateConflict("模型配置已存在")
		}
		return ModelProfile{}, newStateError("模型配置无法更新")
	}
	updated, err := r.Get(ctx, profile.ID)
	if err != nil {
		return ModelProfile{}, err
	}
	if updated == nil {
		return ModelProfile{}, newLookupError("该模型配置不存在")
	}
	return *updated, nil
}

// Delete removes one profile and reports whether a row changed.
func (r *ModelProfileRepository) Delete(ctx context.Context, id string) (bool, error) {
	result, err := r.db.ExecContext(ctx, "DELETE FROM model_profiles WHERE id = ?", id)
	if err != nil {
		return false, newStateError("模型配置无法删除")
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, newStateError("模型配置无法删除")
	}
	return removed > 0, nil
}

// scanModelProfiles reads the shared column list.
func scanModelProfiles(rows *sql.Rows) ([]ModelProfile, error) {
	profiles := make([]ModelProfile, 0, 8)
	for rows.Next() {
		profile, err := scanModelProfile(rows)
		if err != nil {
			return nil, newStateError("模型配置读取失败")
		}
		profiles = append(profiles, *profile)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("模型配置读取失败")
	}
	return profiles, nil
}

// scanModelProfile scans one row, mapping the nullable columns.
func scanModelProfile(scanner rowScanner) (*ModelProfile, error) {
	var profile ModelProfile
	var owner sql.NullString
	var protected sql.NullString
	var vectorSize sql.NullInt64
	if err := scanner.Scan(&profile.ID, &profile.Kind, &profile.Name, &profile.BaseURL,
		&profile.Model, &owner, &profile.OwnerUsername, &protected, &vectorSize,
		&profile.CreatedAt, &profile.UpdatedAt); err != nil {
		return nil, err
	}
	profile.OwnerUserID = owner.String
	profile.ProtectedKey = protected.String
	if vectorSize.Valid {
		value := int(vectorSize.Int64)
		profile.VectorSize = &value
	}
	return &profile, nil
}

// nullString maps an empty string to SQL NULL, the storage shape of an empty
// protected key.
func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// nullInteger maps a nil dimension to SQL NULL.
func nullInteger(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
