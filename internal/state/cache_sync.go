package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Guan0923/Praxis-RAG/internal/logging"
)

// ViewLoader loads one read-model value using only the supplied querier, which
// is the transaction holding the revision latch during a cache miss/refresh.
type ViewLoader func(ctx context.Context, querier viewQuerier, scope string) (any, error)

// viewQuerier is implemented by *sql.DB and *sql.Tx.
type viewQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// defaultCacheDirtyLimit bounds one dirty-key retry sweep.
const defaultCacheDirtyLimit = 100

// CacheSynchronizer owns the MySQL revision water line and the Redis
// read-model snapshot cache. Writers bump cache_revisions inside their business
// transaction (touch); the Debezium cache consumer derives the affected keys,
// rebuilds each view in a revision-latched transaction and publishes it to the
// backend with the new revision. Redis failures are recorded on the revision
// row (attempts/last_error) and retried by RefreshDirty, so they never roll
// back or fail the MySQL fact.
type CacheSynchronizer struct {
	stores   *Stores
	Settings CacheSettings
	// Backend is the read-through snapshot cache (Redis in production).
	Backend StateCache

	loaders map[string]ViewLoader

	closeMu sync.Mutex
	closed  bool
}

// NewDefaultCacheSynchronizer builds the synchronizer from RAG_REDIS_*
// settings. It starts with the cache disabled; the process that owns a Redis
// client attaches it through Stores.CacheSync.Backend (see redisstate.Attach).
func NewDefaultCacheSynchronizer(stores *Stores) (*CacheSynchronizer, error) {
	settings, err := CacheSettingsFromEnv()
	if err != nil {
		return nil, err
	}
	return &CacheSynchronizer{
		stores:   stores,
		Settings: settings,
		Backend:  DisabledStateCache{},
		loaders:  map[string]ViewLoader{},
	}, nil
}

// Register installs the loader for one cache kind.
func (c *CacheSynchronizer) Register(kind string, loader ViewLoader) {
	if c.loaders == nil {
		c.loaders = map[string]ViewLoader{}
	}
	c.loaders[kind] = loader
}

// Enabled reports whether a cache backend is active.
func (c *CacheSynchronizer) Enabled() bool {
	return c.Backend != nil && c.Backend.Enabled()
}

// Loader returns the registered loader for one kind.
func (c *CacheSynchronizer) Loader(kind string) (ViewLoader, bool) {
	loader, ok := c.loaders[kind]
	return loader, ok
}

// GetOrLoad implements the read-through cache-miss path: a hit returns directly;
// a miss/unavailable cache takes a revision-latched consistent snapshot.
func (c *CacheSynchronizer) GetOrLoad(ctx context.Context, kind, scope string) (any, error) {
	loader, ok := c.loaders[kind]
	if !ok {
		return nil, fmt.Errorf("Unknown state cache kind %q", kind)
	}
	if !c.Enabled() {
		return loader(ctx, c.stores.DB, scope)
	}
	hit, cached, err := c.Backend.Get(ctx, kind, scope)
	if err == nil && hit {
		return cached, nil
	}
	if err != nil {
		var unavailable *CacheUnavailable
		if !errors.As(err, &unavailable) {
			return nil, err
		}
	}
	revision, value, err := c.snapshot(ctx, kind, scope, loader)
	if err != nil {
		return nil, err
	}
	if _, err := c.Backend.Put(ctx, kind, scope, revision, value, value == nil); err != nil {
		var unavailable *CacheUnavailable
		if !errors.As(err, &unavailable) {
			return nil, err
		}
	}
	return value, nil
}

// snapshot locks the revision before the first consistent read. Writers acquire
// this same latch before changing the underlying read model.
func (c *CacheSynchronizer) snapshot(ctx context.Context, kind, scope string, loader ViewLoader) (int64, any, error) {
	transaction, err := c.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = transaction.Rollback() }()
	insert := "INSERT INTO cache_revisions (kind, scope, revision) VALUES (?, ?, 0) " +
		"ON DUPLICATE KEY UPDATE revision = revision"
	if _, err := transaction.ExecContext(ctx, insert, kind, scope); err != nil {
		return 0, nil, err
	}
	query := "SELECT revision FROM cache_revisions WHERE kind = ? AND scope = ? FOR UPDATE"
	var revision int64
	if err := transaction.QueryRowContext(ctx, query, kind, scope).Scan(&revision); err != nil {
		return 0, nil, err
	}
	value, err := loader(ctx, transaction, scope)
	if err != nil {
		return 0, nil, err
	}
	if err := transaction.Commit(); err != nil {
		return 0, nil, err
	}
	return revision, value, nil
}

// Invalidate bumps every given read-model revision in one transaction without
// rebuilding the views. It is the entry point of the CDC cache consumer: the
// committed revisions are the dirty state that RefreshKey/RefreshDirty drain.
func (c *CacheSynchronizer) Invalidate(ctx context.Context, keys []CacheKey) (int, error) {
	pending, err := sortedCacheKeys(keys)
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}
	transaction, err := c.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = transaction.Rollback() }()
	updatedAt := unixSeconds()
	for _, key := range pending {
		if err := bumpRevisionTx(ctx, transaction, key, updatedAt); err != nil {
			return 0, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return len(pending), nil
}

// Revalidate invalidates the keys and immediately rebuilds each one. A Redis
// failure is recorded for the dirty-key retry and does not fail the call; only
// database errors are returned.
func (c *CacheSynchronizer) Revalidate(ctx context.Context, keys []CacheKey) error {
	if _, err := c.Invalidate(ctx, keys); err != nil {
		return err
	}
	refreshed := make(map[CacheKey]struct{}, len(keys))
	for _, key := range keys {
		if _, duplicate := refreshed[key]; duplicate {
			continue
		}
		refreshed[key] = struct{}{}
		if _, err := c.RefreshKey(ctx, key.Kind, key.Scope); err != nil {
			return err
		}
	}
	return nil
}

// RefreshKey rebuilds one view at its current revision and publishes it to the
// backend. The loader runs in the same revision-latched transaction as the
// read-path snapshot, so the value and the watermark always agree. Success
// records refreshed_revision and clears last_error; an unavailable backend
// records attempts+1/last_error and returns false, nil so the caller ACKs and a
// periodic sweep retries later.
func (c *CacheSynchronizer) RefreshKey(ctx context.Context, kind, scope string) (bool, error) {
	loader, ok := c.loaders[kind]
	if !ok {
		return false, nil
	}
	revision, value, err := c.snapshot(ctx, kind, scope, loader)
	if err != nil {
		return false, err
	}
	backend := c.Backend
	if backend == nil || !backend.Enabled() {
		backend = DisabledStateCache{}
	}
	if _, err := backend.Put(ctx, kind, scope, revision, value, value == nil); err != nil {
		c.recordRefreshFailure(ctx, kind, scope, err)
		return false, nil
	}
	if err := c.markRefreshed(ctx, kind, scope, revision); err != nil {
		return false, err
	}
	return true, nil
}

// DirtyKeys returns up to limit keys whose published revision lags the committed
// revision or whose last refresh failed. Rows are ordered deterministically so
// concurrent sweeps make progress on the same head of the backlog. Revision rows
// for retired kinds are ignored: they have no loader and would otherwise
// starve the sweep.
func (c *CacheSynchronizer) DirtyKeys(ctx context.Context, limit int) ([]CacheKey, error) {
	if limit <= 0 {
		limit = defaultCacheDirtyLimit
	}
	kinds := knownCacheKinds()
	placeholders := make([]string, len(kinds))
	arguments := make([]any, 0, len(kinds)+1)
	for index, kind := range kinds {
		placeholders[index] = "?"
		arguments = append(arguments, kind)
	}
	arguments = append(arguments, limit)
	query := "SELECT kind, scope FROM cache_revisions " +
		"WHERE (refreshed_revision < revision OR last_error IS NOT NULL) " +
		"AND kind IN (" + strings.Join(placeholders, ", ") + ") " +
		"ORDER BY kind, scope LIMIT ?"
	rows, err := c.stores.DB.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	keys := make([]CacheKey, 0, limit)
	for rows.Next() {
		var key CacheKey
		if err := rows.Scan(&key.Kind, &key.Scope); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

// RefreshDirty drains one batch of dirty keys: it rebuilds every view whose
// published revision lags or whose last publish failed. Redis failures stay
// recorded and are retried by the next sweep; only database errors return.
func (c *CacheSynchronizer) RefreshDirty(ctx context.Context, limit int) error {
	keys, err := c.DirtyKeys(ctx, limit)
	if err != nil {
		return err
	}
	var firstErr error
	for _, key := range keys {
		if _, err := c.RefreshKey(ctx, key.Kind, key.Scope); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// MarkAllDirty is the cutover baseline: it bumps the revision of every known
// kind/scope pair derived from the live business tables (fixed globals plus each
// namespace/operation id), forcing the cache consumer to rebuild the whole
// read model. It is safe on an empty database and idempotent (a second call
// bumps the revisions again). It needs neither registered loaders nor Redis: it
// only reads MySQL and writes cache_revisions.
func (c *CacheSynchronizer) MarkAllDirty(ctx context.Context) (int, error) {
	keys := []CacheKey{
		{Kind: "kb_list", Scope: ""},
		{Kind: "api_keys", Scope: ""},
	}
	namespaceMappings := []struct {
		query string
		kind  string
	}{
		{"SELECT DISTINCT namespace FROM knowledge_bases", "kb_public"},
		{"SELECT DISTINCT namespace FROM knowledge_bases", "documents"},
		{"SELECT DISTINCT namespace FROM mcp_catalogs", "catalog"},
		{"SELECT DISTINCT namespace FROM document_heads", "heads"},
		{"SELECT DISTINCT namespace FROM document_leases", "leases"},
		{"SELECT DISTINCT namespace FROM upload_operations", "upload_list"},
	}
	for _, mapping := range namespaceMappings {
		scopes, err := c.distinctScopes(ctx, mapping.query)
		if err != nil {
			return 0, err
		}
		for _, scope := range scopes {
			keys = append(keys, CacheKey{Kind: mapping.kind, Scope: scope})
		}
	}
	operationIDs, err := c.distinctScopes(ctx, "SELECT DISTINCT id FROM upload_operations")
	if err != nil {
		return 0, err
	}
	for _, operationID := range operationIDs {
		keys = append(keys, CacheKey{Kind: "upload_status", Scope: operationID})
	}
	return c.Invalidate(ctx, keys)
}

// distinctScopes runs one fixed, compile-time constant projection query.
func (c *CacheSynchronizer) distinctScopes(ctx context.Context, query string) ([]string, error) {
	rows, err := c.stores.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	scopes := make([]string, 0)
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return scopes, nil
}

// markRefreshed records a successful publish without ever moving the published
// revision backwards (a concurrent, newer refresh may have won the race).
func (c *CacheSynchronizer) markRefreshed(ctx context.Context, kind, scope string, revision int64) error {
	_, err := c.stores.DB.ExecContext(ctx,
		"UPDATE cache_revisions SET refreshed_revision = GREATEST(refreshed_revision, ?), last_error = NULL "+
			"WHERE kind = ? AND scope = ?", revision, kind, scope)
	return err
}

// recordRefreshFailure bumps the retry counter and stores a sanitized failure
// category (never the raw backend error, which could echo credentials). The
// dirty row stays eligible for RefreshDirty until a publish succeeds.
func (c *CacheSynchronizer) recordRefreshFailure(ctx context.Context, kind, scope string, cause error) {
	_, err := c.stores.DB.ExecContext(ctx,
		"UPDATE cache_revisions SET attempts = attempts + 1, last_error = ? WHERE kind = ? AND scope = ?",
		cacheFailureCategory(cause), kind, scope)
	if err != nil {
		logging.FromContext(ctx).Warn("Cache refresh failure record failed", logging.ErrorAttrs(err)...)
	}
}

// cacheFailureCategory is the sanitized last_error value (column is VARCHAR(128)).
func cacheFailureCategory(cause error) string {
	category := errorTypeName(cause)
	if category == "" {
		category = "Error"
	}
	if len(category) > 128 {
		category = category[:128]
	}
	return category
}

// Start is retained for process-wiring compatibility. The legacy 2s
// relay/consume poll over cache_outbox was removed; refreshes are driven by the
// Debezium cache consumer and the RefreshDirty sweep it owns.
func (c *CacheSynchronizer) Start() {}

// Stop is retained for process-wiring compatibility; Close releases the
// backend client.
func (c *CacheSynchronizer) Stop() {}

// Close releases the cache backend. It is idempotent.
func (c *CacheSynchronizer) Close() error {
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return nil
	}
	c.closed = true
	backend := c.Backend
	c.Backend = DisabledStateCache{}
	c.closeMu.Unlock()
	if backend == nil {
		return nil
	}
	return backend.Close()
}
