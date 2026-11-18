package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/logging"
)

// ViewLoader loads one read-model value using only the supplied querier, which
// is the transaction holding the revision latch during a cache miss/refresh.
type ViewLoader func(ctx context.Context, querier ViewQuerier, scope string) (any, error)

// viewQuerier is implemented by *sql.DB and *sql.Tx.
type viewQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ViewQuerier is the exported alias of the loader querier so directed tests in
// other packages can register their own (for example blockable) loaders.
type ViewQuerier = viewQuerier

// defaultCacheDirtyLimit bounds one dirty-key retry sweep.
const defaultCacheDirtyLimit = 100

// Backfill lock timing. The lease (5s) covers one MySQL snapshot plus its
// publish; the renewer extends it at half the TTL while the query runs, so a
// dead holder can never block a key for longer than one TTL. Waiters back off
// for cacheBackfillPoll and re-read the cache on every wakeup; their total wait
// budget equals one TTL, so after a holder exits (release or lease expiry) a
// waiter always gets its chance to take over before timing out.
const (
	defaultCacheBackfillTTL   = 5 * time.Second
	defaultCacheBackfillPoll  = 25 * time.Millisecond
	defaultCacheBackfillWait = defaultCacheBackfillTTL
)

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

	// Backfill lock timing (see the block comment above). Tests may shrink
	// these to keep waiters fast.
	backfillTTL  time.Duration
	backfillPoll time.Duration
	backfillWait time.Duration

	// clock is the test seam for the display-cache breaker cooldown; production
	// leaves it nil and uses time.Now. It must be set before the first read.
	clock func() time.Time
	// setupMu guards the lazy construction of breaker/guard so tests can
	// override Settings after New() but before the first display read.
	setupMu sync.Mutex
	breaker *cacheBreaker
	guard   *fallbackGuard

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
		stores:       stores,
		Settings:     settings,
		Backend:      DisabledStateCache{},
		loaders:      map[string]ViewLoader{},
		backfillTTL:  defaultCacheBackfillTTL,
		backfillPoll: defaultCacheBackfillPoll,
		backfillWait: defaultCacheBackfillWait,
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
//
// The display fallback is protected in two steps, in this order: concurrent
// fallbacks for the same kind+scope are merged into one query, and only the
// merged query needs one of the instance-level fallback slots (see
// cache_guard.go). While the display-cache breaker is open, the Redis read and
// the backfill are both skipped so the request does not pay a socket timeout
// on a backend known to be down; a read that already failed on an availability
// fault also skips the backfill after its fallback succeeds. A deployment with
// the cache explicitly disabled keeps the original direct-read mode.
//
// Cross-instance, the merged fallback itself runs under the per-key backfill
// lock (SET NX PX with a unique token, see getOrFillLock): only the instance
// that wins the lock executes the MySQL snapshot; the losers back off, re-read
// the cache on every wakeup and reuse whichever result appears first. A lock
// backend failure falls back to MySQL like every other Redis failure (the
// documented outage contract); only a busy lock that never clears within the
// bounded wait surfaces as a retryable error, because bypassing it would
// reproduce the very thundering herd the lock exists to prevent.
func (c *CacheSynchronizer) GetOrLoad(ctx context.Context, kind, scope string) (any, error) {
	return c.GetOrLoadGuarded(ctx, kind, scope, nil)
}

// cacheMissGate answers one question for a cache miss: may the loader consult
// MySQL? It returns false only when the scope is provably absent without the
// database (the upload bloom filter's "definitely absent" answer). Every error
// or uncertainty inside the gate must resolve to true so the original query
// path is preserved.
type cacheMissGate func(ctx context.Context) bool

// GetOrLoadGuarded is GetOrLoad with an optional miss gate: after a cache miss
// (or an unavailable cache backend) and before the MySQL fallback, the gate can
// decline the load. A declined load returns (nil, nil) without writing the
// cache and without joining the single-flight fallback group, so the answer
// stays per-request and MySQL remains the authority. The gate is consulted
// while the display-cache breaker may be open; a gate whose own backend is
// unavailable must answer true (fall back), which the upload bloom filter's
// DefinitelyAbsent already guarantees.
func (c *CacheSynchronizer) GetOrLoadGuarded(ctx context.Context, kind, scope string, missGate cacheMissGate) (any, error) {
	loader, ok := c.loaders[kind]
	if !ok {
		return nil, fmt.Errorf("Unknown state cache kind %q", kind)
	}
	if !c.Enabled() {
		if missGate != nil && !missGate(ctx) {
			return nil, nil
		}
		return loader(ctx, c.stores.DB, scope)
	}
	breaker, guard := c.displayProtections()
	readAllowed, probe := breaker.allowRead()
	backfill := true
	if readAllowed {
		hit, cached, err := c.Backend.Get(ctx, kind, scope)
		if err == nil {
			breaker.recordReadResult(nil, probe)
			if hit {
				return cached, nil
			}
		} else {
			var unavailable *CacheUnavailable
			if !errors.As(err, &unavailable) {
				// Unexpected backend error: record the read first so a
				// half-open probe cannot wedge, then keep the historical
				// contract and abort the request.
				breaker.recordReadResult(err, probe)
				return nil, err
			}
			breaker.recordReadResult(err, probe)
			if unavailable.AvailabilityFailure() {
				// The backend could not answer this read; after the fallback
				// succeeds, do not backfill the backend that just failed.
				backfill = false
			}
			// A corrupt single entry (repaired by this request's backfill) and
			// a caller cancellation do not change the backfill decision.
		}
	} else {
		// Circuit open (or half-open with another probe in flight): skip the
		// Redis read and the backfill, go straight to the gated fallback.
		backfill = false
	}
	// The miss gate runs after the cache miss and before the fallback group:
	// a scope provably absent (the upload bloom filter) neither joins the
	// single-flight fallback nor occupies one of its concurrency slots.
	if missGate != nil && !missGate(ctx) {
		return nil, nil
	}
	// The merged fallback runs under the cross-instance backfill lock: the SET
	// NX winner executes the MySQL snapshot, losers re-read the cache while
	// they wait. A failed lock backend falls back to a direct query (the
	// documented outage contract); a permanently busy lock is a bounded,
	// retryable wait. Only the leader (the caller whose run function executed)
	// runs the lock protocol; waiters reuse its result directly.
	value, err, _ := guard.execute(ctx, callKey{kind: kind, scope: scope}, func(runCtx context.Context) (any, error) {
		return c.getOrFillLock(runCtx, kind, scope, loader, backfill)
	})
	if err != nil {
		return nil, err
	}
	return value, nil
}

// getOrFillLock serializes one key's MySQL backfill across instances using the
// per-key backfill lock. It returns the loaded value (published by this holder
// or read by a waiter after another holder published).
//
// backfill=false (breaker open or the read already failed on an availability
// fault) skips both the lock and the publish and just reads MySQL: the backend
// is known-down, so there is nothing worth guarding.
func (c *CacheSynchronizer) getOrFillLock(ctx context.Context, kind, scope string, loader ViewLoader, backfill bool) (any, error) {
	if !backfill {
		_, value, err := c.snapshot(ctx, kind, scope, loader)
		if err != nil {
			return nil, err
		}
		return value, nil
	}
	deadline := time.Now().Add(c.backfillWait)
	token := newBackfillToken()
	for {
		held, err := c.Backend.AcquireBackfillLock(ctx, kind, scope, token, c.backfillTTL)
		if err != nil {
			// The lock backend is unavailable. The documented outage
			// contract still applies: fall back to MySQL directly instead of
			// failing the request, and skip the publish (the dirty-key sweep
			// retries it later).
			_, value, loadErr := c.snapshot(ctx, kind, scope, loader)
			if loadErr != nil {
				return nil, loadErr
			}
			return value, nil
		}
		if held {
			return c.fillUnderLock(ctx, kind, scope, token, loader)
		}
		// Someone else is backfilling this key: brief backoff, then read the
		// cache again (a hit returns without touching MySQL) and contend once
		// more while the miss persists.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, newCacheRetryable(cacheWaitRetryableMessage)
		}
		pause := c.backfillPoll
		if remaining < pause {
			pause = remaining
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, newCacheRetryable(cacheWaitRetryableMessage)
		case <-timer.C:
		}
		hit, cached, err := c.Backend.Get(ctx, kind, scope)
		if err != nil {
			// A waiter whose re-read fails does not fall back to MySQL: the
			// leader fill is already in flight and one direct query would
			// defeat the lock. The bounded wait keeps this retryable.
			return nil, newCacheRetryable(cacheWaitRetryableMessage)
		}
		if hit {
			return cached, nil
		}
	}
}

// fillUnderLock is the holder path: re-read the cache, then fill from MySQL and
// publish. The lock is released in every branch before returning.
func (c *CacheSynchronizer) fillUnderLock(ctx context.Context, kind, scope, token string, loader ViewLoader) (any, error) {
	// Double-checked locking: another holder may have published between this
	// caller's miss and its lock acquisition.
	if hit, cached, err := c.Backend.Get(ctx, kind, scope); err == nil && hit {
		c.releaseFill(ctx, kind, scope, token)
		return cached, nil
	}
	revision, value, err := c.fillLocked(ctx, &cacheFillGuard{kind: kind, scope: scope, token: token, ttl: c.backfillTTL}, loader)
	if err != nil {
		c.releaseFill(ctx, kind, scope, token)
		var retryable *CacheRetryable
		if errors.As(err, &retryable) || isCacheUnavailable(err) {
			// The lease was lost or the backend became unverifiable mid-query:
			// fall back to a direct snapshot read so the request still answers
			// (the failed fill was not published; the dirty-key sweep retries
			// the publish).
			_, direct, loadErr := c.snapshot(ctx, kind, scope, loader)
			if loadErr != nil {
				return nil, loadErr
			}
			return direct, nil
		}
		return nil, err
	}
	_, putErr := c.Backend.Put(ctx, kind, scope, revision, value, value == nil)
	c.releaseFill(ctx, kind, scope, token)
	if putErr != nil {
		// A tolerated publish outage: the value was still read from MySQL, so
		// the read succeeds while the payload stays absent for the next reader
		// (the dirty-key sweep retries the publish). Other errors surface.
		if !isCacheUnavailable(putErr) {
			return nil, putErr
		}
	}
	return value, nil
}

// cacheFillGuard tracks one acquisition of the per-key backfill lock while its
// MySQL snapshot runs.
type cacheFillGuard struct {
	kind  string
	scope string
	token string
	ttl   time.Duration

	lost atomic.Bool
}

// fillLocked runs the revision-latched MySQL snapshot for one guarded fill. A
// renewer goroutine extends the lease while the query is in flight; when the
// lease is lost or its ownership becomes unverifiable the query is cancelled
// and marked lost, and this function returns a retryable error so a stale
// holder can never publish its result.
func (c *CacheSynchronizer) fillLocked(ctx context.Context, guard *cacheFillGuard, loader ViewLoader) (int64, any, error) {
	queryCtx, cancelQuery := context.WithCancel(ctx)
	defer cancelQuery()
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		guard.renewLoop(c, queryCtx, cancelQuery)
	}()
	revision, value, err := c.snapshot(queryCtx, guard.kind, guard.scope, loader)
	cancelQuery()
	<-renewDone
	if err != nil || guard.lost.Load() {
		if guard.lost.Load() {
			// The snapshot may have committed in MySQL, but this holder no
			// longer provably owns the key: the value must not be published.
			return 0, nil, newCacheRetryable(cacheLeaseRetryableMessage)
		}
		return 0, nil, err
	}
	return revision, value, nil
}

// renewLoop keeps the backfill lease alive for the duration of the MySQL
// query: it renews at half the TTL and stops (cancelling the query) as soon as
// a renewal reports the lease lost or fails to confirm ownership.
func (g *cacheFillGuard) renewLoop(sync *CacheSynchronizer, queryCtx context.Context, cancelQuery context.CancelFunc) {
	interval := g.ttl / 2
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-queryCtx.Done():
			return
		case <-ticker.C:
		}
		// Renew with its own deadline, detached from the query's context so a
		// slow query cannot shorten the renewal window.
		renewCtx, cancel := context.WithTimeout(context.WithoutCancel(queryCtx), g.ttl)
		held, err := sync.Backend.RenewBackfillLock(renewCtx, g.kind, g.scope, g.token, g.ttl)
		cancel()
		if err != nil || !held {
			// Lost, or ownership is unverifiable (backend outage): stop the
			// in-flight query and block this holder from publishing.
			g.lost.Store(true)
			cancelQuery()
			return
		}
	}
}

// releaseFill drops the backfill lock for one token. It is idempotent, never
// fails the caller, and detaches from the request's cancellation so a timed-out
// caller still frees the lock instead of leaving it to expire.
func (c *CacheSynchronizer) releaseFill(ctx context.Context, kind, scope, token string) {
	if err := c.Backend.ReleaseBackfillLock(context.WithoutCancel(ctx), kind, scope, token); err != nil {
		logging.FromContext(ctx).Warn("State cache backfill lock release failed",
			logging.ErrorAttrs(err)...)
	}
}

// newBackfillToken mints the unique lock token for one fill attempt. Tokens
// are random and single-use: renew and release verify them, so a late
// previous holder can never manage the next holder's lock.
func newBackfillToken() string {
	token, err := randomHex(16)
	if err != nil {
		// crypto/rand only fails in broken environments; the timestamp keeps
		// the token unique within this process.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return token
}

// isCacheUnavailable reports whether err is the sanitized backend-unavailable
// failure category.
func isCacheUnavailable(err error) bool {
	var unavailable *CacheUnavailable
	return errors.As(err, &unavailable)
}

// displayProtections lazily builds the display-cache breaker and fallback guard
// from the current settings. Building on first use lets tests (and operators
// wiring Stores by hand) override Settings after construction but before the
// first display read.
func (c *CacheSynchronizer) displayProtections() (*cacheBreaker, *fallbackGuard) {
	c.setupMu.Lock()
	defer c.setupMu.Unlock()
	if c.breaker == nil {
		cooldown := time.Duration(c.Settings.BreakerCooldownSeconds) * time.Second
		c.breaker = newCacheBreaker(c.Settings.BreakerThreshold, cooldown, c.clock)
	}
	if c.guard == nil {
		c.guard = newFallbackGuard(c.Settings.FallbackConcurrency)
	}
	return c.breaker, c.guard
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
// backend through the same per-key backfill lock as GetOrLoad, so a background
// refresh and a concurrent read miss serialize on one lock instead of racing.
//
// The snapshot's revision is the target: markRefreshed only advances the
// refreshed watermark to it, so a writer committing a newer revision mid-refresh
// leaves the key dirty for the next sweep. When the lock is busy and the target
// revision has not been published yet, the pending-refresh state is kept
// (false, nil) and the periodic RefreshDirty sweep compensates; a lost or
// unverifiable lease does the same. Publish failures keep recording
// attempts/last_error so the sweep retries them.
func (c *CacheSynchronizer) RefreshKey(ctx context.Context, kind, scope string) (bool, error) {
	loader, ok := c.loaders[kind]
	if !ok {
		return false, nil
	}
	if !c.Enabled() {
		// No cache backend: no lock to share, keep the dirty state draining.
		revision, _, err := c.snapshot(ctx, kind, scope, loader)
		if err != nil {
			return false, err
		}
		if err := c.markRefreshed(ctx, kind, scope, revision); err != nil {
			return false, err
		}
		return true, nil
	}
	token := newBackfillToken()
	held, err := c.Backend.AcquireBackfillLock(ctx, kind, scope, token, c.backfillTTL)
	if err != nil {
		// Ownership unverifiable while the cache is enabled: record the
		// failure like a publish failure and keep the key dirty for the sweep.
		c.recordRefreshFailure(ctx, kind, scope, err)
		return false, nil
	}
	if !held {
		// Another holder (reader miss or refresh) is filling this key. Keep
		// the pending-refresh state: refreshed_revision still lags revision,
		// so RefreshDirty compensates later.
		return false, nil
	}
	revision, value, err := c.fillLocked(ctx, &cacheFillGuard{kind: kind, scope: scope, token: token, ttl: c.backfillTTL}, loader)
	if err != nil {
		c.releaseFill(ctx, kind, scope, token)
		var retryable *CacheRetryable
		if errors.As(err, &retryable) || isCacheUnavailable(err) {
			// The lease was lost or ownership could not be confirmed: nothing
			// was published and the dirty row keeps the retry state.
			return false, nil
		}
		return false, err
	}
	_, putErr := c.Backend.Put(ctx, kind, scope, revision, value, value == nil)
	c.releaseFill(ctx, kind, scope, token)
	if putErr != nil {
		c.recordRefreshFailure(ctx, kind, scope, putErr)
		return false, nil
	}
	if err := c.markRefreshed(ctx, kind, scope, revision); err != nil {
		return false, err
	}
	return true, nil
}

func activeCacheKindFilter() (string, []any) {
	kinds := knownCacheKinds()
	placeholders := make([]string, len(kinds))
	arguments := make([]any, 0, len(kinds))
	for index, kind := range kinds {
		placeholders[index] = "?"
		arguments = append(arguments, kind)
	}
	return "kind IN (" + strings.Join(placeholders, ", ") + ")", arguments
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
	filter, arguments := activeCacheKindFilter()
	arguments = append(arguments, limit)
	query := "SELECT kind, scope FROM cache_revisions " +
		"WHERE (refreshed_revision < revision OR last_error IS NOT NULL) " +
		"AND " + filter + " " +
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
