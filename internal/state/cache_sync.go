package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
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

// CacheSynchronizer is the port of rag_mcp.state.cache_sync.CacheSynchronizer:
// MySQL owns revisions/outbox, Redis failures never roll back a committed write,
// and only event IDs cross the queue.
type CacheSynchronizer struct {
	stores   *Stores
	Settings CacheSettings
	// Backend is the read-through snapshot cache (Redis in production).
	Backend StateCache
	// Transport is the durable retry publisher (RocketMQ in production).
	Transport EventPublisher

	loaders map[string]ViewLoader

	mutex        sync.Mutex
	queueRetryAt time.Time

	workerMu sync.Mutex
	stop     chan struct{}
	worker   *sync.WaitGroup
	closed   bool
}

// NewDefaultCacheSynchronizer builds the synchronizer from RAG_REDIS_* and
// RAG_ROCKETMQ_* environment settings. The Redis client and RocketMQ producer
// are not ported yet (deferred to the async phase); configured endpoints log a
// warning and fall back to authoritative reads while the durable outbox keeps
// pending events.
func NewDefaultCacheSynchronizer(stores *Stores) (*CacheSynchronizer, error) {
	settings, err := CacheSettingsFromEnv()
	if err != nil {
		return nil, err
	}
	synchronizer := &CacheSynchronizer{
		stores:   stores,
		Settings: settings,
		Backend:  DisabledStateCache{},
		loaders:  map[string]ViewLoader{},
	}
	if settings.backendEnabled() {
		log.Printf("RAG_REDIS_URL/RAG_REDIS_ENABLED is set but the Go Redis state-cache client is not ported yet; " +
			"reads use the authoritative database (deferred to the async phase)")
	}
	if RocketMQSettingsFromEnv().Endpoint != "" {
		log.Printf("RAG_ROCKETMQ_ENDPOINT is set but the Go RocketMQ transport is not ported yet; " +
			"cache refresh events stay in the durable outbox (deferred to the async phase)")
	}
	return synchronizer, nil
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
	insert := "INSERT OR IGNORE INTO cache_revisions (kind, scope, revision) VALUES (?, ?, 0)"
	if c.stores.Dialect() == DialectMySQL {
		insert = "INSERT INTO cache_revisions (kind, scope, revision) VALUES (?, ?, 0) " +
			"ON DUPLICATE KEY UPDATE revision = revision"
	}
	if _, err := transaction.ExecContext(ctx, insert, kind, scope); err != nil {
		return 0, nil, err
	}
	query := "SELECT revision FROM cache_revisions WHERE kind = ? AND scope = ?"
	if c.stores.Dialect() == DialectMySQL {
		query += " FOR UPDATE"
	}
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

// RefreshEvent loads the current committed snapshot for one outbox event and
// marks every not-newer pending event DONE.
func (c *CacheSynchronizer) RefreshEvent(ctx context.Context, eventID string) (bool, error) {
	var kind, scope, status string
	err := c.stores.DB.QueryRowContext(ctx,
		"SELECT kind, scope, status FROM cache_outbox WHERE id = ?", eventID).Scan(&kind, &scope, &status)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && status == "DONE") {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	loader, ok := c.loaders[kind]
	if !ok || !c.Enabled() {
		return false, nil
	}
	revision, value, err := c.snapshot(ctx, kind, scope, loader)
	if err != nil {
		var unavailable *CacheUnavailable
		if errors.As(err, &unavailable) {
			c.recordAttempt(ctx, eventID, err)
			return false, nil
		}
		return false, err
	}
	if _, err := c.Backend.Put(ctx, kind, scope, revision, value, value == nil); err != nil {
		var unavailable *CacheUnavailable
		if errors.As(err, &unavailable) {
			c.recordAttempt(ctx, eventID, err)
			return false, nil
		}
		return false, err
	}
	if _, err := c.stores.DB.ExecContext(ctx,
		"UPDATE cache_outbox SET status = 'DONE', completed_at = ?, last_error = NULL "+
			"WHERE kind = ? AND scope = ? AND revision <= ?",
		unixSeconds(), kind, scope, revision); err != nil {
		return false, err
	}
	return true, nil
}

func (c *CacheSynchronizer) recordAttempt(ctx context.Context, eventID string, cause error) {
	if _, err := c.stores.DB.ExecContext(ctx,
		"UPDATE cache_outbox SET attempts = attempts + 1, last_error = ? WHERE id = ? AND status != 'DONE'",
		errorTypeName(cause), eventID); err != nil {
		log.Printf("Cache refresh attempt record failed error_type=%s", errorTypeName(err))
	}
}

// FlushPending is the best-effort synchronous refresh after a committed write.
// Failure persists for the relay worker.
func (c *CacheSynchronizer) FlushPending(ctx context.Context, keys []CacheKey) error {
	return c.flushPending(ctx, 50, keys)
}

func (c *CacheSynchronizer) flushPending(ctx context.Context, limit int, keys []CacheKey) error {
	if !c.Enabled() || len(c.loaders) == 0 {
		return nil
	}
	ownsMutex := false
	if keys == nil {
		if !c.mutex.TryLock() {
			return nil
		}
		ownsMutex = true
	}
	if ownsMutex {
		defer c.mutex.Unlock()
	}
	ids, err := c.pendingEventIDs(ctx, limit, keys)
	if err != nil {
		log.Printf("Cache dispatch unavailable error_type=%s", errorTypeName(err))
		return nil
	}
	for _, eventID := range ids {
		refreshed, err := c.RefreshEvent(ctx, eventID)
		if err != nil {
			log.Printf("Cache refresh pending event_id=%s error_type=%s", eventID, errorTypeName(err))
			continue
		}
		if !refreshed {
			c.enqueue(ctx, eventID)
		}
	}
	return nil
}

func (c *CacheSynchronizer) pendingEventIDs(ctx context.Context, limit int, keys []CacheKey) ([]string, error) {
	where := "status = 'PENDING' AND retry_at <= ?"
	arguments := []any{unixSeconds()}
	if len(keys) > 0 {
		parts := make([]string, len(keys))
		for index, key := range keys {
			parts[index] = "(kind = ? AND scope = ?)"
			arguments = append(arguments, key.Kind, key.Scope)
		}
		where += " AND (" + strings.Join(parts, " OR ") + ")"
	}
	query := "SELECT kind, scope, MAX(revision) FROM cache_outbox WHERE " + where +
		" GROUP BY kind, scope LIMIT ?"
	arguments = append(arguments, limit)
	rows, err := c.stores.DB.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type pendingGroup struct {
		kind, scope string
		revision    int64
	}
	groups := make([]pendingGroup, 0)
	for rows.Next() {
		var group pendingGroup
		if err := rows.Scan(&group.kind, &group.scope, &group.revision); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(groups))
	for _, group := range groups {
		var eventID string
		err := c.stores.DB.QueryRowContext(ctx,
			"SELECT id FROM cache_outbox WHERE kind = ? AND scope = ? AND revision = ? AND status = 'PENDING' LIMIT 1",
			group.kind, group.scope, group.revision).Scan(&eventID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		ids = append(ids, eventID)
	}
	return ids, nil
}

// enqueue publishes one event ID and records the durable retry state.
func (c *CacheSynchronizer) enqueue(ctx context.Context, eventID string) {
	if c.Transport == nil || c.queueRetryAt.After(time.Now()) {
		return
	}
	if err := c.Transport.Send(eventID); err != nil {
		c.queueRetryAt = time.Now().Add(5 * time.Second)
		if _, updateErr := c.stores.DB.ExecContext(ctx,
			"UPDATE cache_outbox SET retry_at = ?, last_error = ? WHERE id = ? AND status != 'DONE'",
			unixSeconds()+5, errorTypeName(err), eventID); updateErr != nil {
			log.Printf("Cache queue retry record failed error_type=%s", errorTypeName(updateErr))
		}
		log.Printf("Cache queue publish pending event_id=%s error_type=%s", eventID, errorTypeName(err))
		return
	}
	if _, err := c.stores.DB.ExecContext(ctx,
		"UPDATE cache_outbox SET status = 'QUEUED', retry_at = ? WHERE id = ? AND status != 'DONE'",
		unixSeconds()+60, eventID); err != nil {
		log.Printf("Cache queue state record failed error_type=%s", errorTypeName(err))
	}
}

// RelayOnce re-publishes missing/expired deliveries, including events stranded
// in a dead-letter queue.
func (c *CacheSynchronizer) RelayOnce(ctx context.Context) error {
	if err := c.flushPending(ctx, 50, nil); err != nil {
		return err
	}
	if c.Transport == nil || !c.Enabled() {
		return nil
	}
	rows, err := c.stores.DB.QueryContext(ctx,
		"SELECT id FROM cache_outbox WHERE status != 'DONE' AND retry_at <= ? ORDER BY created_at LIMIT 50",
		unixSeconds())
	if err != nil {
		return err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var eventID string
		if err := rows.Scan(&eventID); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, eventID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, eventID := range ids {
		c.enqueue(ctx, eventID)
	}
	return nil
}

// Start begins the background relay/consume loop.
func (c *CacheSynchronizer) Start() {
	if !c.Enabled() {
		return
	}
	c.workerMu.Lock()
	if c.worker != nil {
		c.workerMu.Unlock()
		return
	}
	stop := make(chan struct{})
	c.stop = stop
	group := &sync.WaitGroup{}
	group.Add(1)
	c.worker = group
	c.workerMu.Unlock()
	go func() {
		defer group.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ctx := context.Background()
				if err := c.RelayOnce(ctx); err != nil {
					log.Printf("Cache worker will retry error_type=%s", errorTypeName(err))
				}
				if c.Transport != nil {
					c.Transport.ConsumeOnce(func(eventID string) bool {
						refreshed, err := c.RefreshEvent(ctx, eventID)
						if err != nil {
							log.Printf("Cache refresh failed error_type=%s", errorTypeName(err))
							return false
						}
						return refreshed
					})
				}
			}
		}
	}()
}

// Stop ends the background worker and closes the queue transport, mirroring
// CacheSynchronizer.stop.
func (c *CacheSynchronizer) Stop() {
	c.workerMu.Lock()
	stop := c.stop
	group := c.worker
	c.stop = nil
	c.worker = nil
	c.workerMu.Unlock()
	if stop != nil {
		close(stop)
	}
	if group != nil {
		done := make(chan struct{})
		go func() {
			group.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
	}
	if c.Transport != nil {
		if err := c.Transport.Close(); err != nil {
			log.Printf("Cache queue close failed error_type=%s", errorTypeName(err))
		}
	}
}

// Close stops the worker and releases the transport and cache clients.
func (c *CacheSynchronizer) Close() error {
	c.workerMu.Lock()
	if c.closed {
		c.workerMu.Unlock()
		return nil
	}
	c.closed = true
	c.workerMu.Unlock()
	c.Stop()
	var firstErr error
	if c.Transport != nil {
		if err := c.Transport.Close(); err != nil {
			firstErr = err
		}
		c.Transport = nil
	}
	if c.Backend != nil {
		if err := c.Backend.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.Backend = DisabledStateCache{}
	}
	return firstErr
}
