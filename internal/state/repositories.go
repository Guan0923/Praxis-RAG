package state

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
)

// APIKey is the public projection of one stored service credential. It is the
// dict returned by ApiKeyRepository._public in Python; the plaintext secret is
// only ever exposed by Create.
type APIKey struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Namespaces []string `json:"namespaces"`
	Enabled    bool     `json:"enabled"`
	CreatedAt  string   `json:"created_at"`
	Exportable bool     `json:"exportable"`
	// OwnerUserID is the Console account that issued the key. It is empty for
	// credentials created before ownership existed (and for keys issued through
	// the trusted local management surface), which stay administrator-only.
	OwnerUserID string `json:"owner_user_id,omitempty"`
	// OwnerUsername is a display-only join of users.username; it is never used
	// for authorization.
	OwnerUsername string `json:"owner_username,omitempty"`
}

// CreatedAPIKey is the one-time response of ApiKeyRepository.Create: the public
// metadata plus the plaintext secret ("key").
type CreatedAPIKey struct {
	APIKey
	Key string `json:"key"`
}

// APIKeyImport is the row accepted by APIKeyRepository.ImportRow, mirroring the
// keyword arguments of the Python method. ProtectedKey is the stored (possibly
// legacy) protected value; the repository re-protects it with the current key.
type APIKeyImport struct {
	ID           string
	Name         string
	Prefix       string
	Digest       string
	Namespaces   []string
	Enabled      bool
	CreatedAt    string
	ProtectedKey *string
}

// APIKeyRepository is the SQL implementation of ApiKeyRepository.
type APIKeyRepository struct {
	db            *sql.DB
	encryptionKey string
	cacheSync     *CacheSynchronizer
}

func newAPIKeyRepository(db *sql.DB, encryptionKey string, cacheSync *CacheSynchronizer) *APIKeyRepository {
	return &APIKeyRepository{db: db, encryptionKey: encryptionKey, cacheSync: cacheSync}
}

const apiKeySelectColumns = "id, name, prefix, namespaces, enabled, created_at, protected_key"

// apiKeyOwnerSelectColumns projects one key together with its owning account.
// The base column list above is kept for the read-model cache loader, which
// serialises the whole api_keys table without joins.
const apiKeyOwnerSelectColumns = "k.id, k.name, k.prefix, k.namespaces, k.enabled, " +
	"k.created_at, k.protected_key, k.owner_user_id, u.username"

// apiKeyOwnerFrom is the FROM clause shared by every owner-aware read.
const apiKeyOwnerFrom = " FROM api_keys k LEFT JOIN users u ON u.id = k.owner_user_id"

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAPIKey(scanner rowScanner) (APIKey, error) {
	var key APIKey
	var namespaces string
	var protected sql.NullString
	if err := scanner.Scan(&key.ID, &key.Name, &key.Prefix, &namespaces, &key.Enabled, &key.CreatedAt, &protected); err != nil {
		return APIKey{}, err
	}
	list, err := decodeStringList(namespaces)
	if err != nil {
		return APIKey{}, err
	}
	key.Namespaces = list
	key.Exportable = protected.Valid && protected.String != ""
	return key, nil
}

// scanAPIKeyWithOwner scans apiKeyOwnerSelectColumns (key columns + owner id +
// owner username).
func scanAPIKeyWithOwner(scanner rowScanner) (APIKey, error) {
	var key APIKey
	var namespaces string
	var protected sql.NullString
	var ownerUserID sql.NullString
	var ownerUsername sql.NullString
	if err := scanner.Scan(&key.ID, &key.Name, &key.Prefix, &namespaces, &key.Enabled,
		&key.CreatedAt, &protected, &ownerUserID, &ownerUsername); err != nil {
		return APIKey{}, err
	}
	list, err := decodeStringList(namespaces)
	if err != nil {
		return APIKey{}, err
	}
	key.Namespaces = list
	if ownerUserID.Valid {
		key.OwnerUserID = ownerUserID.String
	}
	if ownerUsername.Valid {
		key.OwnerUsername = ownerUsername.String
	}
	key.Exportable = protected.Valid && protected.String != ""
	return key, nil
}

// List returns every key newest-first, including legacy unowned credentials.
// Secrets are never included.
func (r *APIKeyRepository) List(ctx context.Context) ([]APIKey, error) {
	return r.list(ctx, "")
}

// ListByOwner returns the keys issued by one Console account, newest-first. An
// empty owner id returns no keys (List is the explicit all-owners read).
func (r *APIKeyRepository) ListByOwner(ctx context.Context, ownerUserID string) ([]APIKey, error) {
	if ownerUserID == "" {
		return []APIKey{}, nil
	}
	return r.list(ctx, ownerUserID)
}

func (r *APIKeyRepository) list(ctx context.Context, ownerUserID string) ([]APIKey, error) {
	statement := "SELECT " + apiKeyOwnerSelectColumns + apiKeyOwnerFrom
	arguments := make([]any, 0, 1)
	if ownerUserID != "" {
		statement += " WHERE k.owner_user_id = ?"
		arguments = append(arguments, ownerUserID)
	}
	statement += " ORDER BY k.created_at DESC, k.id DESC"
	rows, err := r.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	keys := make([]APIKey, 0)
	for rows.Next() {
		key, err := scanAPIKeyWithOwner(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Get returns one key with its owner projection, or nil when it does not exist.
func (r *APIKeyRepository) Get(ctx context.Context, keyID string) (*APIKey, error) {
	key, err := scanAPIKeyWithOwner(r.db.QueryRowContext(ctx,
		"SELECT "+apiKeyOwnerSelectColumns+apiKeyOwnerFrom+" WHERE k.id = ?", keyID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &key, nil
}

// Create generates a "prk_" secret, stores its digest and protected form, and
// returns the plaintext exactly once. The legacy/management surface creates an
// unowned credential, which only administrators can see and manage.
func (r *APIKeyRepository) Create(ctx context.Context, name string, namespaces []string) (CreatedAPIKey, error) {
	return r.create(ctx, name, namespaces, "")
}

// CreateForOwner generates a credential owned by one Console account. An empty
// ownerUserID is the same as Create.
func (r *APIKeyRepository) CreateForOwner(ctx context.Context, name string, namespaces []string, ownerUserID string) (CreatedAPIKey, error) {
	return r.create(ctx, name, namespaces, ownerUserID)
}

func (r *APIKeyRepository) create(ctx context.Context, name string, namespaces []string, ownerUserID string) (CreatedAPIKey, error) {
	if namespaces == nil {
		namespaces = []string{}
	}
	token, err := tokenURLSafe(32)
	if err != nil {
		return CreatedAPIKey{}, err
	}
	secret := "prk_" + token
	keyID, err := randomHex(12)
	if err != nil {
		return CreatedAPIKey{}, err
	}
	encodedNamespaces, err := marshalStringList(namespaces)
	if err != nil {
		return CreatedAPIKey{}, err
	}
	protected, err := Protect(secret, r.encryptionKey)
	if err != nil {
		return CreatedAPIKey{}, err
	}
	timestamp := nowISO()
	values := APIKey{
		ID:          keyID,
		Name:        name,
		Prefix:      secret[:12],
		Namespaces:  namespaces,
		Enabled:     true,
		CreatedAt:   timestamp,
		Exportable:  protected != "",
		OwnerUserID: ownerUserID,
	}
	var ownerValue any
	if ownerUserID != "" {
		ownerValue = ownerUserID
	}
	database, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return CreatedAPIKey{}, newStateError("API Key 无法保存")
	}
	defer func() { _ = database.Rollback() }()
	if _, err := touch(ctx, database, apiKeyKeys()...); err != nil {
		return CreatedAPIKey{}, newStateError("API Key 无法保存")
	}
	_, err = database.ExecContext(ctx,
		"INSERT INTO api_keys (id, name, prefix, digest, namespaces, enabled, created_at, updated_at, protected_key, owner_user_id) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		keyID, name, values.Prefix, sha256Hex(secret), encodedNamespaces, true, timestamp, timestamp, protected, ownerValue)
	if err != nil {
		if isUniqueViolation(err) {
			return CreatedAPIKey{}, newStateConflict("API Key 已存在")
		}
		return CreatedAPIKey{}, newStateError("API Key 无法保存")
	}
	if err := database.Commit(); err != nil {
		return CreatedAPIKey{}, newStateError("API Key 无法保存")
	}
	return CreatedAPIKey{APIKey: values, Key: secret}, nil
}

// Update renames/scopes/enables one key; nil means the key does not exist.
func (r *APIKeyRepository) Update(ctx context.Context, keyID, name string, namespaces []string, enabled bool) (*APIKey, error) {
	encodedNamespaces, err := marshalStringList(namespaces)
	if err != nil {
		return nil, err
	}
	database, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, newStateError("API Key 无法更新")
	}
	defer func() { _ = database.Rollback() }()
	if _, err := touch(ctx, database, apiKeyKeys()...); err != nil {
		return nil, newStateError("API Key 无法更新")
	}
	result, err := database.ExecContext(ctx,
		"UPDATE api_keys SET name = ?, namespaces = ?, enabled = ?, updated_at = ? WHERE id = ?",
		name, encodedNamespaces, enabled, nowISO(), keyID)
	if err != nil {
		return nil, newStateError("API Key 无法更新")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, newStateError("API Key 无法更新")
	}
	var updated *APIKey
	if changed > 0 {
		key, scanErr := scanAPIKeyWithOwner(database.QueryRowContext(ctx,
			"SELECT "+apiKeyOwnerSelectColumns+apiKeyOwnerFrom+" WHERE k.id = ?", keyID))
		if scanErr != nil {
			return nil, newStateError("API Key 无法更新")
		}
		updated = &key
	}
	if err := database.Commit(); err != nil {
		return nil, newStateError("API Key 无法更新")
	}
	return updated, nil
}

// Delete removes one key and reports whether a row changed.
func (r *APIKeyRepository) Delete(ctx context.Context, keyID string) (bool, error) {
	database, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = database.Rollback() }()
	if _, err := touch(ctx, database, apiKeyKeys()...); err != nil {
		return false, err
	}
	result, err := database.ExecContext(ctx, "DELETE FROM api_keys WHERE id = ?", keyID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := database.Commit(); err != nil {
		return false, err
	}
	return changed > 0, nil
}

// DeleteMany removes a batch of credentials in one transaction: one SELECT
// classifies the requested ids, one multi-row DELETE removes them, and one
// read-model revision bump covers the whole batch. A non-empty ownerUserID
// applies Delete's ownership rule to every id, and ids owned by another account
// are reported as missing (the 404 semantics of the single-key endpoint).
func (r *APIKeyRepository) DeleteMany(ctx context.Context, keyIDs []string, ownerUserID string) ([]string, []string, error) {
	requested := make([]string, 0, len(keyIDs))
	seen := make(map[string]struct{}, len(keyIDs))
	for _, keyID := range keyIDs {
		if keyID == "" {
			continue
		}
		if _, duplicate := seen[keyID]; duplicate {
			continue
		}
		seen[keyID] = struct{}{}
		requested = append(requested, keyID)
	}
	deleted := make([]string, 0, len(requested))
	missing := make([]string, 0, len(requested))
	if len(requested) == 0 {
		return deleted, missing, nil
	}
	selectPlaceholders := make([]string, len(requested))
	selectArgs := make([]any, 0, len(requested)+1)
	for index, keyID := range requested {
		selectPlaceholders[index] = "?"
		selectArgs = append(selectArgs, keyID)
	}
	selectSQL := "SELECT id FROM api_keys WHERE id IN (" + strings.Join(selectPlaceholders, ",") + ")"
	if ownerUserID != "" {
		selectSQL += " AND owner_user_id = ?"
		selectArgs = append(selectArgs, ownerUserID)
	}
	database, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = database.Rollback() }()
	rows, err := database.QueryContext(ctx, selectSQL, selectArgs...)
	if err != nil {
		return nil, nil, err
	}
	found := make(map[string]struct{}, len(requested))
	for rows.Next() {
		var keyID string
		if scanErr := rows.Scan(&keyID); scanErr != nil {
			_ = rows.Close()
			return nil, nil, scanErr
		}
		found[keyID] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	existing := make([]string, 0, len(found))
	for _, keyID := range requested {
		if _, ok := found[keyID]; ok {
			existing = append(existing, keyID)
			continue
		}
		missing = append(missing, keyID)
	}
	if len(existing) > 0 {
		deletePlaceholders := make([]string, len(existing))
		deleteArgs := make([]any, 0, len(existing))
		for index, keyID := range existing {
			deletePlaceholders[index] = "?"
			deleteArgs = append(deleteArgs, keyID)
		}
		result, err := database.ExecContext(ctx,
			"DELETE FROM api_keys WHERE id IN ("+strings.Join(deletePlaceholders, ",")+")", deleteArgs...)
		if err != nil {
			return nil, nil, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, nil, err
		}
		if int(changed) != len(existing) {
			return nil, nil, newStateError("API Key 批量删除冲突，请重试")
		}
		if _, err := touch(ctx, database, apiKeyKeys()...); err != nil {
			return nil, nil, err
		}
	}
	if err := database.Commit(); err != nil {
		return nil, nil, err
	}
	deleted = append(deleted, existing...)
	return deleted, missing, nil
}

// NamespacesForKey resolves the granted namespaces of a presented secret.
func (r *APIKeyRepository) NamespacesForKey(ctx context.Context, secret string) ([]string, error) {
	var namespaces string
	var enabled bool
	err := r.db.QueryRowContext(ctx,
		"SELECT namespaces, enabled FROM api_keys WHERE digest = ?", sha256Hex(secret)).
		Scan(&namespaces, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, newLookupError("访问 API Key 在当前服务中不存在或已删除")
	}
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, newPermissionError("访问 API Key 已停用")
	}
	return decodeStringList(namespaces)
}

// SecretForNamespace returns the stored plaintext for an admin export request.
func (r *APIKeyRepository) SecretForNamespace(ctx context.Context, keyID, namespace string) (string, error) {
	var namespaces string
	var enabled bool
	var protected sql.NullString
	err := r.db.QueryRowContext(ctx,
		"SELECT namespaces, enabled, protected_key FROM api_keys WHERE id = ?", keyID).
		Scan(&namespaces, &enabled, &protected)
	if errors.Is(err, sql.ErrNoRows) {
		return "", newLookupError("API Key 不存在")
	}
	if err != nil {
		return "", err
	}
	grants, err := decodeStringList(namespaces)
	if err != nil {
		return "", err
	}
	if !enabled || !containsString(grants, namespace) {
		return "", newPermissionError("API Key 已停用或未获授权访问此 Namespace")
	}
	if !protected.Valid || protected.String == "" {
		return "", newValueError("该密钥仅保存摘要，无法显示原值；请在 API Key 页面重新创建")
	}
	return Unprotect(protected.String, r.encryptionKey)
}

// ImportRow inserts or updates one key during a legacy migration, re-protecting
// the stored credential with the current encryption key.
func (r *APIKeyRepository) ImportRow(ctx context.Context, row APIKeyImport) error {
	encodedNamespaces, err := marshalStringList(row.Namespaces)
	if err != nil {
		return err
	}
	var protected sql.NullString
	if row.ProtectedKey != nil && *row.ProtectedKey != "" {
		plaintext, err := Unprotect(*row.ProtectedKey, r.encryptionKey)
		if err != nil {
			return err
		}
		reprotected, err := Protect(plaintext, r.encryptionKey)
		if err != nil {
			return err
		}
		protected = sql.NullString{String: reprotected, Valid: true}
	}
	updatedAt := nowISO()
	database, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = database.Rollback() }()
	if _, err := touch(ctx, database, apiKeyKeys()...); err != nil {
		return err
	}
	var existing string
	err = database.QueryRowContext(ctx, "SELECT id FROM api_keys WHERE id = ?", row.ID).Scan(&existing)
	switch {
	case err == nil:
		if _, err := database.ExecContext(ctx,
			"UPDATE api_keys SET name = ?, prefix = ?, digest = ?, namespaces = ?, enabled = ?, "+
				"created_at = ?, updated_at = ?, protected_key = ? WHERE id = ?",
			row.Name, row.Prefix, row.Digest, encodedNamespaces, row.Enabled,
			row.CreatedAt, updatedAt, protected, row.ID); err != nil {
			return err
		}
	case errors.Is(err, sql.ErrNoRows):
		if _, err := database.ExecContext(ctx,
			"INSERT INTO api_keys (id, name, prefix, digest, namespaces, enabled, created_at, updated_at, protected_key) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			row.ID, row.Name, row.Prefix, row.Digest, encodedNamespaces, row.Enabled,
			row.CreatedAt, updatedAt, protected); err != nil {
			return err
		}
	default:
		return err
	}
	if err := database.Commit(); err != nil {
		return err
	}
	return nil
}

// KnowledgeBaseSnapshot is the settings/version pair returned by
// KnowledgeBaseRepository.Snapshot (Python's (settings, version) tuple).
type KnowledgeBaseSnapshot struct {
	Settings map[string]any
	Version  int
}

// KnowledgeBaseRepository is the SQL implementation of
// KnowledgeBaseRepository.
type KnowledgeBaseRepository struct {
	db        *sql.DB
	cacheSync *CacheSynchronizer
}

func newKnowledgeBaseRepository(db *sql.DB, cacheSync *CacheSynchronizer) *KnowledgeBaseRepository {
	return &KnowledgeBaseRepository{db: db, cacheSync: cacheSync}
}

// List returns every knowledge-base settings object ordered by namespace.
func (r *KnowledgeBaseRepository) List(ctx context.Context) ([]map[string]any, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT settings FROM knowledge_bases ORDER BY namespace")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		settings, err := decodeObject(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, settings)
	}
	return result, rows.Err()
}

// Get returns one settings object, or nil when the namespace is unknown.
func (r *KnowledgeBaseRepository) Get(ctx context.Context, namespace string) (map[string]any, error) {
	var raw string
	err := r.db.QueryRowContext(ctx,
		"SELECT settings FROM knowledge_bases WHERE namespace = ?", namespace).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeObject(raw)
}

// Create inserts a new knowledge base at version 1 without an owner.
func (r *KnowledgeBaseRepository) Create(ctx context.Context, settings map[string]any) error {
	return r.create(ctx, settings, "")
}

// CreateWithOwner inserts a new knowledge base at version 1 owned by the given
// Console account. An empty ownerUserID is the same as Create.
func (r *KnowledgeBaseRepository) CreateWithOwner(ctx context.Context, settings map[string]any, ownerUserID string) error {
	return r.create(ctx, settings, ownerUserID)
}

func (r *KnowledgeBaseRepository) create(ctx context.Context, settings map[string]any, ownerUserID string) error {
	namespace, err := requireSetting(settings, "namespace")
	if err != nil {
		return err
	}
	collection, err := requireSetting(settings, "collection")
	if err != nil {
		return err
	}
	encoded, err := marshalJSON(settings)
	if err != nil {
		return err
	}
	timestamp := nowISO()
	var owner any
	if ownerUserID != "" {
		owner = ownerUserID
	}
	database, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return newStateError("知识库配置无法保存")
	}
	defer func() { _ = database.Rollback() }()
	if _, err := touch(ctx, database, knowledgeKeys(namespace)...); err != nil {
		return newStateError("知识库配置无法保存")
	}
	_, err = database.ExecContext(ctx,
		"INSERT INTO knowledge_bases (namespace, collection, settings, version, created_at, updated_at, owner_user_id) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)",
		namespace, collection, encoded, 1, timestamp, timestamp, owner)
	if err != nil {
		if isUniqueViolation(err) {
			return newStateConflict("该知识库已存在")
		}
		return newStateError("知识库配置无法保存")
	}
	if err := database.Commit(); err != nil {
		return newStateError("知识库配置无法保存")
	}
	return nil
}

// Namespaces lists every configured knowledge-base namespace ordered by name.
func (r *KnowledgeBaseRepository) Namespaces(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT namespace FROM knowledge_bases ORDER BY namespace")
	if err != nil {
		return nil, newStateError("知识库列表读取失败")
	}
	defer func() { _ = rows.Close() }()
	namespaces := make([]string, 0, 8)
	for rows.Next() {
		var namespace string
		if err := rows.Scan(&namespace); err != nil {
			return nil, newStateError("知识库列表读取失败")
		}
		namespaces = append(namespaces, namespace)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("知识库列表读取失败")
	}
	return namespaces, nil
}

// OwnerOf returns the owner account id of one knowledge base. A missing row or a
// legacy row created before ownership existed returns "" (unowned).
func (r *KnowledgeBaseRepository) OwnerOf(ctx context.Context, namespace string) (string, error) {
	var owner sql.NullString
	err := r.db.QueryRowContext(ctx,
		"SELECT owner_user_id FROM knowledge_bases WHERE namespace = ?", namespace).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", newStateError("知识库归属读取失败")
	}
	if !owner.Valid {
		return "", nil
	}
	return owner.String, nil
}

// Exists reports whether the namespace has a knowledge-base row.
func (r *KnowledgeBaseRepository) Exists(ctx context.Context, namespace string) (bool, error) {
	var found int
	err := r.db.QueryRowContext(ctx,
		"SELECT 1 FROM knowledge_bases WHERE namespace = ? LIMIT 1", namespace).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, newStateError("知识库归属读取失败")
	}
	return true, nil
}

// OwnedNamespaces lists the knowledge bases owned by one account.
func (r *KnowledgeBaseRepository) OwnedNamespaces(ctx context.Context, ownerUserID string) ([]string, error) {
	if ownerUserID == "" {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT namespace FROM knowledge_bases WHERE owner_user_id = ? ORDER BY namespace", ownerUserID)
	if err != nil {
		return nil, newStateError("知识库归属读取失败")
	}
	defer func() { _ = rows.Close() }()
	namespaces := make([]string, 0, 4)
	for rows.Next() {
		var namespace string
		if err := rows.Scan(&namespace); err != nil {
			return nil, newStateError("知识库归属读取失败")
		}
		namespaces = append(namespaces, namespace)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("知识库归属读取失败")
	}
	return namespaces, nil
}

// SetOwner assigns or clears the owner of one knowledge base and reports whether
// the row existed.
func (r *KnowledgeBaseRepository) SetOwner(ctx context.Context, namespace, ownerUserID string) (bool, error) {
	var owner any
	if ownerUserID != "" {
		owner = ownerUserID
	}
	result, err := r.db.ExecContext(ctx,
		"UPDATE knowledge_bases SET owner_user_id = ?, updated_at = ? WHERE namespace = ?",
		owner, nowISO(), namespace)
	if err != nil {
		return false, newStateError("知识库归属无法保存")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, newStateError("知识库归属无法保存")
	}
	return changed > 0, nil
}

// Update rewrites a knowledge base and increments its version.
//
// TODO(G2): port publish/confirm_publication, which fence the switch under the
// namespace lease held by the deferred UploadJournal.
func (r *KnowledgeBaseRepository) Update(ctx context.Context, settings map[string]any) error {
	namespace, err := requireSetting(settings, "namespace")
	if err != nil {
		return err
	}
	collection, err := requireSetting(settings, "collection")
	if err != nil {
		return err
	}
	encoded, err := marshalJSON(settings)
	if err != nil {
		return err
	}
	database, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = database.Rollback() }()
	if _, err := touch(ctx, database, knowledgeKeys(namespace)...); err != nil {
		return err
	}
	result, err := database.ExecContext(ctx,
		"UPDATE knowledge_bases SET settings = ?, collection = ?, version = version + 1, updated_at = ? "+
			"WHERE namespace = ?",
		encoded, collection, nowISO(), namespace)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return newLookupError("知识库不存在")
	}
	if err := database.Commit(); err != nil {
		return err
	}
	return nil
}

// Delete removes one knowledge base and reports whether a row changed. Sharing
// state of the namespace (permissions and invitations) is dropped in the same
// transaction so a future knowledge base with the same name starts private.
func (r *KnowledgeBaseRepository) Delete(ctx context.Context, namespace string) (bool, error) {
	database, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = database.Rollback() }()
	if _, err := touch(ctx, database, knowledgeKeys(namespace)...); err != nil {
		return false, err
	}
	if _, err := database.ExecContext(ctx,
		"DELETE FROM kb_permissions WHERE namespace = ?", namespace); err != nil {
		return false, err
	}
	if _, err := database.ExecContext(ctx,
		"DELETE FROM kb_invitations WHERE namespace = ?", namespace); err != nil {
		return false, err
	}
	result, err := database.ExecContext(ctx,
		"DELETE FROM knowledge_bases WHERE namespace = ?", namespace)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := database.Commit(); err != nil {
		return false, err
	}
	return changed > 0, nil
}

// Version returns the current configuration version; found is false when the
// namespace does not exist.
func (r *KnowledgeBaseRepository) Version(ctx context.Context, namespace string) (int, bool, error) {
	var version int64
	err := r.db.QueryRowContext(ctx,
		"SELECT version FROM knowledge_bases WHERE namespace = ?", namespace).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return int(version), true, nil
}

// Snapshot returns settings and version atomically; nil when absent.
func (r *KnowledgeBaseRepository) Snapshot(ctx context.Context, namespace string) (*KnowledgeBaseSnapshot, error) {
	var raw string
	var version int64
	err := r.db.QueryRowContext(ctx,
		"SELECT settings, version FROM knowledge_bases WHERE namespace = ?", namespace).
		Scan(&raw, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	settings, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	return &KnowledgeBaseSnapshot{Settings: settings, Version: int(version)}, nil
}

// DocumentRecord is one document row joined with its head version, matching
// repositories._document_record. OperationID is omitted when no head exists.
type DocumentRecord struct {
	OperationID        string `json:"operation_id,omitempty"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	ModifiedAt         string `json:"modified_at"`
	IndexedAt          string `json:"indexed_at"`
	DocumentID         string `json:"document_id"`
	ChunkCount         int    `json:"chunk_count"`
	Collection         string `json:"collection"`
	Diagnostics        any    `json:"diagnostics"`
	SkippedObjectCount int    `json:"skipped_object_count"`
	SHA256             string `json:"sha256"`
	Chunking           any    `json:"chunking"`
}

// DocumentRepository is the SQL implementation of DocumentRepository.
type DocumentRepository struct {
	db        *sql.DB
	cacheSync *CacheSynchronizer
}

func newDocumentRepository(db *sql.DB, cacheSync *CacheSynchronizer) *DocumentRepository {
	return &DocumentRepository{db: db, cacheSync: cacheSync}
}

// FlushViews is retained as a no-op compatibility seam for callers that used to
// refresh the cached document display views after committing UpsertTx/DeleteTx.
// The revisions are already bumped inside those transactions (touch) and the
// Debezium cache consumer rebuilds the views, so writes no longer refresh the
// cache directly.
func (r *DocumentRepository) FlushViews(context.Context, string) {}

const (
	documentSelectColumns = "d.name, d.document_id, d.size, d.sha256, d.chunking, d.collection, " +
		"d.chunk_count, d.modified_at, d.indexed_at, d.diagnostics, d.skipped_object_count, h.version"
	documentSelectFrom = " FROM documents d LEFT JOIN document_heads h " +
		"ON d.namespace = h.namespace AND d.document_id = h.document_id"
	documentUpdateStatement = "UPDATE documents SET document_id = ?, size = ?, sha256 = ?, chunking = ?, " +
		"collection = ?, chunk_count = ?, modified_at = ?, indexed_at = ?, diagnostics = ?, " +
		"skipped_object_count = ?, del_flag = 0 WHERE namespace = ? AND name = ?"
	documentInsertStatement = "INSERT INTO documents (namespace, name, document_id, size, sha256, chunking, " +
		"collection, chunk_count, modified_at, indexed_at, diagnostics, skipped_object_count, del_flag) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)"
)

func scanDocument(scanner rowScanner) (DocumentRecord, error) {
	var record DocumentRecord
	var chunking string
	var diagnostics string
	var activeVersion sql.NullString
	if err := scanner.Scan(&record.Name, &record.DocumentID, &record.Size, &record.SHA256, &chunking,
		&record.Collection, &record.ChunkCount, &record.ModifiedAt, &record.IndexedAt, &diagnostics,
		&record.SkippedObjectCount, &activeVersion); err != nil {
		return DocumentRecord{}, err
	}
	if activeVersion.Valid && activeVersion.String != "" {
		record.OperationID = activeVersion.String
	}
	parsedChunking, err := decodeObject(chunking)
	if err != nil {
		return DocumentRecord{}, err
	}
	record.Chunking = parsedChunking
	parsedDiagnostics, err := decodeJSON(diagnostics, []any{})
	if err != nil {
		return DocumentRecord{}, err
	}
	record.Diagnostics = parsedDiagnostics
	return record, nil
}

// List returns the visible documents of one namespace sorted by casefolded name.
func (r *DocumentRepository) List(ctx context.Context, namespace string) ([]DocumentRecord, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+documentSelectColumns+documentSelectFrom+" WHERE d.del_flag = 0 AND d.namespace = ?", namespace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	records := make([]DocumentRecord, 0)
	for rows.Next() {
		record, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Python sorts with str.casefold(); strings.ToLower matches for the file
	// names the service accepts and keeps the stable ordering of equal names.
	sort.SliceStable(records, func(left, right int) bool {
		return strings.ToLower(records[left].Name) < strings.ToLower(records[right].Name)
	})
	return records, nil
}

// Get returns one visible document by exact (case-sensitive) name.
func (r *DocumentRepository) Get(ctx context.Context, namespace, name string) (*DocumentRecord, error) {
	record, err := scanDocument(r.db.QueryRowContext(ctx,
		"SELECT "+documentSelectColumns+documentSelectFrom+
			" WHERE d.del_flag = 0 AND d.namespace = ? AND d.name = ?", namespace, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// GetByID returns one visible document by document id.
func (r *DocumentRepository) GetByID(ctx context.Context, namespace, documentID string) (*DocumentRecord, error) {
	record, err := scanDocument(r.db.QueryRowContext(ctx,
		"SELECT "+documentSelectColumns+documentSelectFrom+
			" WHERE d.del_flag = 0 AND d.namespace = ? AND d.document_id = ?", namespace, documentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// Candidates returns records that may match a content hash, including legacy
// rows stored with an empty sha256.
func (r *DocumentRepository) Candidates(ctx context.Context, namespace, sha256Value string, name *string) ([]DocumentRecord, error) {
	query := "SELECT " + documentSelectColumns + documentSelectFrom +
		" WHERE d.del_flag = 0 AND d.namespace = ?"
	arguments := []any{namespace}
	if name != nil {
		query += " AND d.name = ?"
		arguments = append(arguments, *name)
	}
	if sha256Value != "" {
		query += " AND (d.sha256 = ? OR d.sha256 = '')"
		arguments = append(arguments, sha256Value)
	}
	rows, err := r.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	records := make([]DocumentRecord, 0)
	for rows.Next() {
		record, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// Namespaces counts the visible documents per namespace.
func (r *DocumentRepository) Namespaces(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT namespace, COUNT(*) FROM documents WHERE del_flag = 0 GROUP BY namespace")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	counts := make(map[string]int)
	for rows.Next() {
		var namespace string
		var count int64
		if err := rows.Scan(&namespace, &count); err != nil {
			return nil, err
		}
		counts[namespace] = int(count)
	}
	return counts, rows.Err()
}

// Upsert writes one document in its own transaction.
func (r *DocumentRepository) Upsert(ctx context.Context, namespace string, record DocumentRecord) error {
	database, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = database.Rollback() }()
	if err := r.UpsertTx(ctx, database, namespace, record); err != nil {
		return err
	}
	if err := database.Commit(); err != nil {
		return err
	}
	return nil
}

// UpsertTx writes one document inside the caller's transaction, like the
// `conn=` keyword of DocumentRepository.upsert.
func (r *DocumentRepository) UpsertTx(ctx context.Context, tx *sql.Tx, namespace string, record DocumentRecord) error {
	if _, err := touch(ctx, tx, documentKeys(namespace)...); err != nil {
		return err
	}
	chunking := record.Chunking
	if chunking == nil {
		chunking = map[string]any{}
	}
	diagnostics := record.Diagnostics
	if diagnostics == nil {
		diagnostics = []any{}
	}
	encodedChunking, err := marshalJSON(chunking)
	if err != nil {
		return err
	}
	encodedDiagnostics, err := marshalJSON(diagnostics)
	if err != nil {
		return err
	}
	modifiedAt := record.ModifiedAt
	if modifiedAt == "" {
		modifiedAt = nowISO()
	}
	indexedAt := record.IndexedAt
	if indexedAt == "" {
		indexedAt = nowISO()
	}
	updateArguments := []any{
		record.DocumentID, record.Size, record.SHA256, encodedChunking, record.Collection,
		record.ChunkCount, modifiedAt, indexedAt, encodedDiagnostics, record.SkippedObjectCount,
		namespace, record.Name,
	}
	result, err := tx.ExecContext(ctx, documentUpdateStatement, updateArguments...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed > 0 {
		return nil
	}
	insertArguments := []any{
		namespace, record.Name, record.DocumentID, record.Size, record.SHA256, encodedChunking,
		record.Collection, record.ChunkCount, modifiedAt, indexedAt, encodedDiagnostics,
		record.SkippedObjectCount,
	}
	if _, err := tx.ExecContext(ctx, documentInsertStatement, insertArguments...); err != nil {
		if !isUniqueViolation(err) {
			return err
		}
		// Another instance inserted this document after our UPDATE.
		_, retryErr := tx.ExecContext(ctx, documentUpdateStatement, updateArguments...)
		return retryErr
	}
	return nil
}

// Delete soft-deletes one document in its own transaction.
func (r *DocumentRepository) Delete(ctx context.Context, namespace, name string) (bool, error) {
	database, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = database.Rollback() }()
	changed, err := r.DeleteTx(ctx, database, namespace, name)
	if err != nil {
		return false, err
	}
	if err := database.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

// DeleteTx soft-deletes one document inside the caller's transaction.
func (r *DocumentRepository) DeleteTx(ctx context.Context, tx *sql.Tx, namespace, name string) (bool, error) {
	if _, err := touch(ctx, tx, documentKeys(namespace)...); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx,
		"UPDATE documents SET del_flag = 1 WHERE namespace = ? AND name = ? AND del_flag = 0", namespace, name)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed > 0, nil
}

// VisibleVersions is the authority for new searches: preserve heads, exclude
// soft-deleted ids with an empty-version tombstone.
func (r *DocumentRepository) VisibleVersions(ctx context.Context, namespace string) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT d.document_id, d.del_flag, h.version FROM documents d LEFT JOIN document_heads h "+
			"ON d.namespace = h.namespace AND d.document_id = h.document_id WHERE d.namespace = ?", namespace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make(map[string]string)
	for rows.Next() {
		var documentID string
		var deleted bool
		var version sql.NullString
		if err := rows.Scan(&documentID, &deleted, &version); err != nil {
			return nil, err
		}
		if deleted {
			result[documentID] = ""
		} else if version.Valid {
			result[documentID] = version.String
		}
	}
	return result, rows.Err()
}

// ImportRecord is the migration entry point for one document row.
func (r *DocumentRepository) ImportRecord(ctx context.Context, namespace string, record DocumentRecord) error {
	return r.Upsert(ctx, namespace, record)
}

// CatalogTransform is the read-modify-write callback of CatalogRepository.Mutate.
// current is nil for a namespace without a stored catalog. The returned data is
// persisted only when changed is true.
type CatalogTransform func(current *string) (data string, result any, changed bool, err error)

// CatalogRepository is the SQL implementation of CatalogRepository.
type CatalogRepository struct {
	db        *sql.DB
	cacheSync *CacheSynchronizer
}

func newCatalogRepository(db *sql.DB, cacheSync *CacheSynchronizer) *CatalogRepository {
	return &CatalogRepository{db: db, cacheSync: cacheSync}
}

// Load returns the stored catalog JSON; found is false for a missing namespace.
func (r *CatalogRepository) Load(ctx context.Context, namespace string) (string, bool, error) {
	var data string
	err := r.db.QueryRowContext(ctx,
		"SELECT data FROM mcp_catalogs WHERE namespace = ?", namespace).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return data, true, nil
}

// Mutate applies a read-modify-write transform under a row lock and returns its
// result. A transform error rolls back and is returned unchanged; SQL failures
// become StateError("MCP 配置无法保存").
func (r *CatalogRepository) Mutate(ctx context.Context, namespace string, transform CatalogTransform) (any, error) {
	database, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, newStateError("MCP 配置无法保存")
	}
	defer func() { _ = database.Rollback() }()
	// Serialize first creation as well as later updates across nodes.
	if _, err := touch(ctx, database, catalogKeys(namespace)...); err != nil {
		return nil, newStateError("MCP 配置无法保存")
	}
	query := "SELECT data FROM mcp_catalogs WHERE namespace = ? FOR UPDATE"
	var current string
	var currentPointer *string
	err = database.QueryRowContext(ctx, query, namespace).Scan(&current)
	switch {
	case err == nil:
		currentPointer = &current
	case errors.Is(err, sql.ErrNoRows):
		currentPointer = nil
	default:
		return nil, newStateError("MCP 配置无法保存")
	}
	newData, result, changed, err := transform(currentPointer)
	if err != nil {
		return nil, err
	}
	if changed {
		if currentPointer == nil {
			if _, err := database.ExecContext(ctx,
				"INSERT INTO mcp_catalogs (namespace, data, version, updated_at) VALUES (?, ?, ?, ?)",
				namespace, newData, 1, nowISO()); err != nil {
				return nil, newStateError("MCP 配置无法保存")
			}
		} else if _, err := database.ExecContext(ctx,
			"UPDATE mcp_catalogs SET data = ?, version = version + 1, updated_at = ? WHERE namespace = ?",
			newData, nowISO(), namespace); err != nil {
			return nil, newStateError("MCP 配置无法保存")
		}
	}
	if err := database.Commit(); err != nil {
		return nil, newStateError("MCP 配置无法保存")
	}
	return result, nil
}
