package state

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Directed GetOrLoad coverage: a scripted backend stands in for Redis (modes
// for network outage, single-entry corruption and caller cancellation), a
// blockable loader stands in for the MySQL view query, and a manual clock drives
// the breaker cooldown. These tests use the shared MySQL test schema like the
// other cache_sync tests.

// scriptedCache is the controllable StateCache fake.
type scriptedCache struct {
	mu         sync.Mutex
	down       bool // every Get fails as a network outage
	corruptAll bool // every Get fails as a corrupt entry
	cancelAll  bool // every Get fails as a caller cancellation
	entries    map[CacheKey]any
	watermarks map[CacheKey]int64
	gets       int
	puts       int
	// getBarrier releases the first getBarrierWait Get calls simultaneously,
	// so a merge test starts all its callers before any query can finish.
	getBarrierWait    int
	getBarrierArrived int
	getBarrier        chan struct{}
}

func newScriptedCache() *scriptedCache {
	return &scriptedCache{
		entries:    map[CacheKey]any{},
		watermarks: map[CacheKey]int64{},
	}
}

func (c *scriptedCache) Enabled() bool { return true }

func (c *scriptedCache) Get(ctx context.Context, kind, scope string) (bool, any, error) {
	c.mu.Lock()
	c.gets++
	if c.getBarrierWait > 0 && c.getBarrier != nil {
		c.getBarrierArrived++
		if c.getBarrierArrived == c.getBarrierWait {
			c.getBarrierWait = 0
			close(c.getBarrier)
		}
	}
	// Canceling under a barrier with the caller's own context would make every
	// Get report a cancellation; the production reader would have to reflect
	// the caller's context there, so keep the barrier options separated below.
	barrier := c.getBarrier
	c.mu.Unlock()
	if barrier != nil {
		select {
		case <-barrier:
		case <-ctx.Done():
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.down:
		return false, nil, NewCacheUnavailableNetwork("unreachable")
	case c.cancelAll:
		return false, nil, NewCacheUnavailableCanceled("caller canceled")
	case c.corruptAll:
		return false, nil, NewCacheUnavailableCorrupt("corrupt payload")
	}
	key := CacheKey{Kind: kind, Scope: scope}
	value, found := c.entries[key]
	if !found {
		return false, nil, nil
	}
	detached, err := detachCacheValue(value)
	if err != nil {
		return false, nil, NewCacheUnavailableCorrupt("detach failed")
	}
	return true, detached, nil
}

func (c *scriptedCache) Put(_ context.Context, kind, scope string, revision int64, value any, _ bool) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts++
	if c.down {
		return false, NewCacheUnavailableNetwork("unreachable")
	}
	key := CacheKey{Kind: kind, Scope: scope}
	if watermark, ok := c.watermarks[key]; ok && revision < watermark {
		return false, nil
	}
	c.watermarks[key] = revision
	detached, err := detachCacheValue(value)
	if err != nil {
		return false, NewCacheUnavailableCorrupt("detach failed")
	}
	c.entries[key] = detached
	return true, nil
}

// snapshotStored counts the entries actually stored in the backend, which is
// the visible effect of a backfill (a Put that failed with an outage is not a
// stored entry).
func (c *scriptedCache) snapshotStored() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *scriptedCache) Close() error { return nil }

func (c *scriptedCache) snapshotCounts() (gets int, puts int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets, c.puts
}

// newBackpressureTestStores wires one MySQL test store with the scripted
// backend and the given breaker/guard settings before any display read.
func newBackpressureTestStores(t *testing.T, threshold, cooldownSeconds, fallbackConcurrency int) (*Stores, *scriptedCache, *manualClock) {
	t.Helper()
	stores := newTestStores(t)
	cache := newScriptedCache()
	stores.CacheSync.Backend = cache
	clock := newManualClock()
	stores.CacheSync.clock = clock.Now
	stores.CacheSync.Settings.BreakerThreshold = threshold
	stores.CacheSync.Settings.BreakerCooldownSeconds = cooldownSeconds
	stores.CacheSync.Settings.FallbackConcurrency = fallbackConcurrency
	return stores, cache, clock
}

// blockableLoader is a view loader that signals it started, blocks until
// released and returns a scope-tagged value.
type blockableLoader struct {
	started   chan string
	release   chan struct{}
	failError error
	calls     int32
}

func newBlockableLoader() *blockableLoader {
	return &blockableLoader{started: make(chan string, 64), release: make(chan struct{})}
}

func (l *blockableLoader) load(ctx context.Context, _ ViewQuerier, scope string) (any, error) {
	atomic.AddInt32(&l.calls, 1)
	l.started <- scope
	<-l.release
	if l.failError != nil {
		return nil, l.failError
	}
	return map[string]any{"scope": scope}, nil
}

func (l *blockableLoader) callCount() int { return int(atomic.LoadInt32(&l.calls)) }

func (l *blockableLoader) open() { close(l.release) }

// awaitStarted waits for n loader invocations to begin.
func awaitStarted(t *testing.T, loader *blockableLoader, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-loader.started:
		case <-deadline:
			t.Fatalf("only %d of %d queries started", i, n)
		}
	}
}

// awaitStartedConcurrent waits until n loader invocations are running
// simultaneously, proving real overlap instead of serialized execution.
func awaitStartedConcurrent(t *testing.T, loader *blockableLoader, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	seen := 0
	for seen < n {
		select {
		case <-loader.started:
			seen++
		case <-deadline:
			t.Fatalf("only %d of %d queries started", seen, n)
		}
	}
	// The loader blocks inside the query until released, so every started call
	// is still running here.
}

func TestGetOrLoadBreakerSkipsRedisAfterThreshold(t *testing.T) {
	stores, cache, clock := newBackpressureTestStores(t, 3, 5, 4)
	loader := newBlockableLoader()
	stores.CacheSync.Register("probe", loader.load)
	loader.open()
	ctx := context.Background()

	// The backend goes down before the first request: every read is a
	// connection/timeout failure, not a miss.
	cache.mu.Lock()
	cache.down = true
	cache.mu.Unlock()

	// Three availability failures trip the breaker; every failing read falls
	// back to MySQL, so callers still get real data even while Redis is down.
	for i := 0; i < 3; i++ {
		value, err := stores.CacheSync.GetOrLoad(ctx, "probe", "alpha")
		if err != nil || value == nil {
			t.Fatalf("fallback %d = (%v, %v)", i, value, err)
		}
	}
	gets, _ := cache.snapshotCounts()
	if gets != 3 || cache.snapshotStored() != 0 {
		t.Fatalf("reads/stored before trip = %d/%d, want 3/0 (no backfill into a failed backend)", gets, cache.snapshotStored())
	}
	if calls := loader.callCount(); calls != 3 {
		t.Fatalf("loader ran %d times before the trip, want 3 (one per sequential call)", calls)
	}

	// Tripped: the next request must not touch Redis at all, and the request
	// that already failed on an availability fault must skip the backfill.
	for i := 0; i < 2; i++ {
		value, err := stores.CacheSync.GetOrLoad(ctx, "probe", "alpha")
		if err != nil || value == nil {
			t.Fatalf("open-circuit fallback %d = (%v, %v)", i, value, err)
		}
	}
	if gets, _ = cache.snapshotCounts(); gets != 3 {
		t.Fatalf("open circuit still read Redis: %d gets", gets)
	}
	if stored := cache.snapshotStored(); stored != 0 {
		t.Fatalf("open circuit still wrote Redis: %d stored", stored)
	}
	if loader.callCount() != 5 {
		t.Fatalf("loader ran %d times, want 5", loader.callCount())
	}

	// Cooldown elapsed: exactly one request probes Redis; concurrent others
	// keep the guarded fallback without touching Redis.
	clock.advance(6 * time.Second)
	probe := make(chan struct{})
	go func() {
		_, _ = stores.CacheSync.GetOrLoad(ctx, "probe", "alpha")
		close(probe)
	}()
	awaitStarted(t, loader, 1) // the probe's own fallback query (6th call)
	<-probe
	if gets, _ = cache.snapshotCounts(); gets != 4 {
		t.Fatalf("cooldown produced %d probe reads, want 1", gets-3)
	}

	// The probe still fails: the breaker cools down again. Then a later
	// cooldown's probe meets a healthy backend: it misses (an answered
	// miss proves liveness), loads from MySQL and backfills again.
	clock.advance(6 * time.Second)
	cache.mu.Lock()
	cache.down = false
	cache.mu.Unlock()
	probeDone := make(chan struct{})
	go func() {
		_, _ = stores.CacheSync.GetOrLoad(ctx, "probe", "alpha")
		close(probeDone)
	}()
	awaitStarted(t, loader, 1) // the recovering probe's query (7th call)
	<-probeDone
	if stored := cache.snapshotStored(); stored != 1 {
		t.Fatalf("recovered probe stored %d entries, want 1", stored)
	}
	if calls := loader.callCount(); calls != 7 {
		t.Fatalf("loader ran %d times total, want 7", calls)
	}

	// Circuit closed: the next read hits Redis and skips the loader.
	before := loader.callCount()
	value, err := stores.CacheSync.GetOrLoad(ctx, "probe", "alpha")
	if err != nil {
		t.Fatalf("post-recovery read error = %v", err)
	}
	if scope, _ := value.(map[string]any)["scope"].(string); scope != "alpha" {
		t.Fatalf("post-recovery value = %v", value)
	}
	if loader.callCount() != before {
		t.Fatalf("Redis hit still ran the loader (%d -> %d)", before, loader.callCount())
	}
}

func TestGetOrLoadMergesConcurrentSameKindScope(t *testing.T) {
	stores, cache, _ := newBackpressureTestStores(t, 5, 10, 4)
	loader := newBlockableLoader()
	stores.CacheSync.Register("documents", loader.load)
	ctx := context.Background()

	const callers = 6
	// Every caller's Redis read waits on one barrier, so the six requests
	// provably overlap before any of them can finish its fallback.
	cache.mu.Lock()
	cache.getBarrierWait = callers
	cache.getBarrier = make(chan struct{})
	cache.mu.Unlock()

	results := make(chan struct {
		value any
		err   error
	}, callers)
	for i := 0; i < callers; i++ {
		go func() {
			value, err := stores.CacheSync.GetOrLoad(ctx, "documents", "alpha")
			results <- struct {
				value any
				err   error
			}{value, err}
		}()
	}
	// All six miss simultaneously and the first one starts the shared MySQL
	// query, which stays executing (the loader blocks).
	awaitStartedConcurrent(t, loader, 1)
	loader.open()

	for i := 0; i < callers; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("merged caller error = %v", result.err)
		}
		scope, _ := result.value.(map[string]any)["scope"].(string)
		if scope != "alpha" {
			t.Fatalf("merged caller value = %v", result.value)
		}
	}
	if calls := loader.callCount(); calls != 1 {
		t.Fatalf("hot knowledge base ran the loader %d times, want 1", calls)
	}
	// Only the caller whose query ran backfills the shared result.
	if stored := cache.snapshotStored(); stored != 1 {
		t.Fatalf("merged query stored %d entries, want 1", stored)
	}
}

func TestGetOrLoadRejectsThirdKnowledgeBaseWhenBudgetFull(t *testing.T) {
	stores, _, _ := newBackpressureTestStores(t, 5, 10, 2)
	loader := newBlockableLoader()
	stores.CacheSync.Register("documents", loader.load)
	ctx := context.Background()

	// Two different knowledge bases occupy the whole budget.
	blocked := make(chan struct{}, 2)
	for _, scope := range []string{"kb-1", "kb-2"} {
		go func(scope string) {
			_, _ = stores.CacheSync.GetOrLoad(ctx, "documents", scope)
			blocked <- struct{}{}
		}(scope)
	}
	awaitStarted(t, loader, 2)

	// A third, different knowledge base is rejected immediately.
	rejected := make(chan error, 1)
	go func() {
		_, err := stores.CacheSync.GetOrLoad(ctx, "documents", "kb-3")
		rejected <- err
	}()
	select {
	case err := <-rejected:
		if !IsViewBackpressure(err) {
			t.Fatalf("third knowledge base error = %v, want ViewBackpressureError", err)
		}
		var backpressure *ViewBackpressureError
		if errors.As(err, &backpressure) && backpressure.Message != ViewBackpressureMessage {
			t.Fatalf("busy message = %q", backpressure.Message)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("third knowledge base queued instead of being rejected")
	}
	if calls := loader.callCount(); calls != 2 {
		t.Fatalf("rejected request still reached MySQL: %d queries", calls)
	}

	// Releasing the budget serves the next query again.
	loader.open()
	<-blocked
	<-blocked
	value, err := stores.CacheSync.GetOrLoad(ctx, "documents", "kb-3")
	if err != nil {
		t.Fatalf("post-release read error = %v", err)
	}
	if scope, _ := value.(map[string]any)["scope"].(string); scope != "kb-3" {
		t.Fatalf("post-release value = %v", value)
	}
}

func TestGetOrLoadCorruptionAndCancelDoNotTripBreaker(t *testing.T) {
	stores, cache, _ := newBackpressureTestStores(t, 2, 5, 4)
	loader := newBlockableLoader()
	stores.CacheSync.Register("documents", loader.load)
	loader.open()
	ctx := context.Background()

	// A corrupt single entry falls back to MySQL and is repaired by the
	// backfill of the very request that hit it.
	cache.mu.Lock()
	cache.corruptAll = true
	cache.mu.Unlock()
	if _, err := stores.CacheSync.GetOrLoad(ctx, "documents", "alpha"); err != nil {
		t.Fatalf("corrupt-entry fallback error = %v", err)
	}
	cache.mu.Lock()
	cache.corruptAll = false
	cache.mu.Unlock()
	// The entry was backfilled despite the corrupt read, so the next read hits.
	if _, err := stores.CacheSync.GetOrLoad(ctx, "documents", "alpha"); err != nil {
		t.Fatalf("repaired-entry read error = %v", err)
	}
	if calls := loader.callCount(); calls != 1 {
		t.Fatalf("corrupt entry ran the loader %d times, want 1", calls)
	}

	// Caller cancellations are not Redis faults either.
	cache.mu.Lock()
	cache.entries = map[CacheKey]any{} // drop the repaired entry
	cache.cancelAll = true
	cache.mu.Unlock()
	for i := 0; i < 5; i++ {
		if _, err := stores.CacheSync.GetOrLoad(ctx, "documents", "alpha"); err != nil {
			t.Fatalf("canceled-read fallback %d error = %v", i, err)
		}
	}
	cache.mu.Lock()
	cache.cancelAll = false
	cache.mu.Unlock()

	// After all that, the breaker never tripped: Redis reads still happen and
	// a real miss backfills normally.
	if gets, _ := cache.snapshotCounts(); gets != 7 {
		t.Fatalf("Redis reads = %d, want 7 (breaker never skipped reads)", gets)
	}
	if _, err := stores.CacheSync.GetOrLoad(ctx, "documents", "beta"); err != nil {
		t.Fatalf("final miss error = %v", err)
	}
	if stored := cache.snapshotStored(); stored != 2 {
		t.Fatalf("stored entries = %d, want 2", stored)
	}
}
