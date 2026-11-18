package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// memoryCache is the test StateCache: it mirrors the Python MemoryCache fixture
// (JSON-detached values, revision watermarks, outage simulation) plus the
// backfill lock semantics of the production backend.
type memoryCache struct {
	mu            sync.Mutex
	enabled       bool
	down          bool
	closed        bool
	lockedDown    bool
	entries       map[CacheKey]any
	watermarks    map[CacheKey]int64
	backfillLocks map[CacheKey]string
}

func newMemoryCache() *memoryCache {
	return &memoryCache{
		enabled:       true,
		entries:       map[CacheKey]any{},
		watermarks:    map[CacheKey]int64{},
		backfillLocks: map[CacheKey]string{},
	}
}

func (m *memoryCache) Enabled() bool { return m.enabled }

func (m *memoryCache) Get(_ context.Context, kind, scope string) (bool, any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return false, nil, newCacheUnavailable("unreachable")
	}
	key := CacheKey{Kind: kind, Scope: scope}
	value, found := m.entries[key]
	if !found {
		return false, nil, nil
	}
	detached, err := detachCacheValue(value)
	if err != nil {
		return false, nil, newCacheUnavailable("unreachable")
	}
	return true, detached, nil
}

func (m *memoryCache) Put(_ context.Context, kind, scope string, revision int64, value any, _ bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return false, newCacheUnavailable("unreachable")
	}
	key := CacheKey{Kind: kind, Scope: scope}
	if watermark, ok := m.watermarks[key]; ok && revision < watermark {
		return false, nil
	}
	m.watermarks[key] = revision
	detached, err := detachCacheValue(value)
	if err != nil {
		return false, newCacheUnavailable("unreachable")
	}
	m.entries[key] = detached
	return true, nil
}

func (m *memoryCache) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// The backfill lock methods mirror the production Redis backend: a busy lock
// is (false, nil) and an outage is CacheUnavailable.
func (m *memoryCache) AcquireBackfillLock(_ context.Context, kind, scope, token string, _ time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return false, newCacheUnavailable("unreachable")
	}
	if m.lockedDown {
		return false, newCacheUnavailable("locked down")
	}
	key := CacheKey{Kind: kind, Scope: scope}
	if holder, ok := m.backfillLocks[key]; ok && holder != token {
		return false, nil
	}
	m.backfillLocks[key] = token
	return true, nil
}

func (m *memoryCache) RenewBackfillLock(_ context.Context, kind, scope, token string, _ time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down || m.lockedDown {
		return false, newCacheUnavailable("unreachable")
	}
	holder, ok := m.backfillLocks[CacheKey{Kind: kind, Scope: scope}]
	return ok && holder == token, nil
}

func (m *memoryCache) ReleaseBackfillLock(_ context.Context, kind, scope, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if holder, ok := m.backfillLocks[CacheKey{Kind: kind, Scope: scope}]; ok && holder == token {
		delete(m.backfillLocks, CacheKey{Kind: kind, Scope: scope})
	}
	return nil
}

func detachCacheValue(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var detached any
	if err := json.Unmarshal(encoded, &detached); err != nil {
		return nil, err
	}
	return detached, nil
}

func newCacheTestStores(t *testing.T) (*Stores, *memoryCache) {
	t.Helper()
	stores := newTestStores(t)
	cache := newMemoryCache()
	stores.CacheSync.Backend = cache
	return stores, cache
}

// newFastCacheTestStores shortens the backfill lock timing so waiter tests
// stay fast: a small TTL/poll/wait still exercises the same protocol.
func newFastCacheTestStores(t *testing.T) (*Stores, *memoryCache) {
	t.Helper()
	stores, cache := newCacheTestStores(t)
	stores.CacheSync.backfillTTL = 150 * time.Millisecond
	stores.CacheSync.backfillPoll = 5 * time.Millisecond
	stores.CacheSync.backfillWait = 150 * time.Millisecond
	return stores, cache
}

// snapshotQueries records how often the documents loader touches MySQL: a
// counting querier wrapper for the backfill-once proof.
type countingQuerier struct {
	viewQuerier
	queries func(query string)
}

func (q countingQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if q.queries != nil {
		q.queries(query)
	}
	return q.viewQuerier.QueryContext(ctx, query, args...)
}

func cacheTestKnowledgeBase(collection string) map[string]any {
	return map[string]any{
		"namespace": "ns", "collection": collection, "embedding_model": "model", "reranker_model": "",
		"embedding": map[string]any{"base_url": "http://provider", "protected_key": "private-file:NEVER_CACHE"},
		"reranker":  map[string]any{},
		"strategy":  "character", "chunk_size": 100, "chunk_overlap": 10,
	}
}

// cacheTestKnowledgeBaseFor builds the same fixture for an explicit namespace.
func cacheTestKnowledgeBaseFor(namespace string) map[string]any {
	settings := cacheTestKnowledgeBase("c")
	settings["namespace"] = namespace
	return settings
}

func TestCacheHitReadsNoSQLAndConfigurationIsRedacted(t *testing.T) {
	stores, cache := newCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	redacted, err := stores.Views.KnowledgeBase(ctx, "ns")
	if err != nil {
		t.Fatalf("KnowledgeBase() error = %v", err)
	}
	embedding := redacted["embedding"].(map[string]any)
	if embedding["base_url"] != "http://provider" || embedding["api_key_set"] != true {
		t.Fatalf("embedding view = %+v", embedding)
	}
	if _, leaked := embedding["protected_key"]; leaked {
		t.Fatal("cached public settings expose protected_key")
	}
	items, err := stores.Views.KnowledgeBases(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("KnowledgeBases() = %v, %v", items, err)
	}
	documents, err := stores.Views.Documents(ctx, "ns")
	if err != nil || len(documents) != 0 {
		t.Fatalf("Documents() = %v, %v", documents, err)
	}
	// Bypass the repositories (and their revision touch) to prove the next read
	// is served by the backend without touching MySQL.
	if _, err := stores.DB.ExecContext(ctx, "DELETE FROM knowledge_bases"); err != nil {
		t.Fatalf("raw delete error = %v", err)
	}
	if _, err := stores.Views.KnowledgeBase(ctx, "ns"); err != nil {
		t.Fatalf("cached KnowledgeBase() error = %v", err)
	}
	cache.mu.Lock()
	payload := fmt.Sprintf("%v", cache.entries)
	cache.mu.Unlock()
	if strings.Contains(payload, "NEVER_CACHE") || strings.Contains(payload, "protected_key") {
		t.Fatalf("cache payload leaked a credential: %s", payload)
	}
}

func TestRevalidatePublishesLatestRevisionAndWatermarkRejectsStale(t *testing.T) {
	stores, cache := newCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if dirty, err := stores.CacheSync.DirtyKeys(ctx, 0); err != nil || len(dirty) != 3 {
		t.Fatalf("DirtyKeys() before refresh = %v, %v; want the three knowledge keys", dirty, err)
	}
	if err := stores.CacheSync.Revalidate(ctx, knowledgeKeys("ns")); err != nil {
		t.Fatalf("Revalidate() error = %v", err)
	}
	row := readCacheRevision(t, stores, "kb_public", "ns")
	if row.RefreshedRevision != row.Revision || row.LastError.Valid {
		t.Fatalf("after refresh refreshed/last_error = %d/%v (revision %d), want clean",
			row.RefreshedRevision, row.LastError, row.Revision)
	}
	if dirty, err := stores.CacheSync.DirtyKeys(ctx, 0); err != nil || len(dirty) != 0 {
		t.Fatalf("DirtyKeys() after refresh = %v, %v; want none", dirty, err)
	}
	oldRevision := row.Revision
	oldValue, err := stores.Views.KnowledgeBase(ctx, "ns")
	if err != nil || oldValue["collection"] != "c" {
		t.Fatalf("KnowledgeBase() = %+v, %v", oldValue, err)
	}
	if err := stores.KnowledgeBases.Update(ctx, cacheTestKnowledgeBase("updated")); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if dirty, err := stores.CacheSync.DirtyKeys(ctx, 0); err != nil || len(dirty) != 3 {
		t.Fatalf("DirtyKeys() after write = %v, %v; want the three knowledge keys", dirty, err)
	}
	// The CDC consumer refreshes the dirty keys; the previous revision is then
	// rejected by the backend watermark.
	if err := stores.CacheSync.RefreshDirty(ctx, 0); err != nil {
		t.Fatalf("RefreshDirty() error = %v", err)
	}
	updated, err := stores.Views.KnowledgeBase(ctx, "ns")
	if err != nil || updated["collection"] != "updated" {
		t.Fatalf("KnowledgeBase() after refresh = %+v, %v", updated, err)
	}
	if accepted, err := cache.Put(ctx, "kb_public", "ns", oldRevision, oldValue, false); err != nil || accepted {
		t.Fatalf("stale Put() = %v, %v; want false", accepted, err)
	}
	if value, err := stores.Views.KnowledgeBase(ctx, "ns"); err != nil || value["collection"] != "updated" {
		t.Fatalf("stale fill resurrected an old view: %+v, %v", value, err)
	}
	// Deleting the row is rebuilt as a cached deletion at the next revision.
	if _, err := stores.KnowledgeBases.Delete(ctx, "ns"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := stores.CacheSync.RefreshDirty(ctx, 0); err != nil {
		t.Fatalf("RefreshDirty() after delete error = %v", err)
	}
	deleted, err := stores.Views.KnowledgeBase(ctx, "ns")
	if err != nil || deleted != nil {
		t.Fatalf("KnowledgeBase() after delete = %+v, %v", deleted, err)
	}
}

func TestRefreshKeyRecordsRedisFailureAndDirtySweepConverges(t *testing.T) {
	stores, cache := newCacheTestStores(t)
	ctx := context.Background()
	cache.mu.Lock()
	cache.down = true
	cache.mu.Unlock()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// A Redis outage is not a caller error: the dirty state stays in MySQL.
	if err := stores.CacheSync.Revalidate(ctx, knowledgeKeys("ns")); err != nil {
		t.Fatalf("Revalidate() error = %v; a backend outage must not fail the call", err)
	}
	row := readCacheRevision(t, stores, "kb_public", "ns")
	if row.RefreshedRevision != 0 {
		t.Fatalf("refreshed_revision = %d after a failed publish, want 0", row.RefreshedRevision)
	}
	if row.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", row.Attempts)
	}
	if !row.LastError.Valid || row.LastError.String != "CacheUnavailable" {
		t.Fatalf("last_error = %v, want the sanitized CacheUnavailable category", row.LastError)
	}
	dirty, err := stores.CacheSync.DirtyKeys(ctx, 0)
	if err != nil || len(dirty) != 3 {
		t.Fatalf("DirtyKeys() = %v, %v; want the three failed keys", dirty, err)
	}

	cache.mu.Lock()
	cache.down = false
	cache.mu.Unlock()
	if err := stores.CacheSync.RefreshDirty(ctx, 0); err != nil {
		t.Fatalf("RefreshDirty() error = %v", err)
	}
	row = readCacheRevision(t, stores, "kb_public", "ns")
	if row.RefreshedRevision != row.Revision || row.LastError.Valid {
		t.Fatalf("after retry refreshed/last_error = %d/%v (revision %d), want clean",
			row.RefreshedRevision, row.LastError, row.Revision)
	}
	if dirty, err := stores.CacheSync.DirtyKeys(ctx, 0); err != nil || len(dirty) != 0 {
		t.Fatalf("DirtyKeys() after retry = %v, %v; want none", dirty, err)
	}
	value, err := stores.Views.KnowledgeBase(ctx, "ns")
	if err != nil || value["collection"] != "c" {
		t.Fatalf("KnowledgeBase() after recovery = %+v, %v", value, err)
	}
}

func TestInvalidateAndRefreshKeyEdges(t *testing.T) {
	stores, cache := newCacheTestStores(t)
	ctx := context.Background()
	if count, err := stores.CacheSync.Invalidate(ctx, nil); err != nil || count != 0 {
		t.Fatalf("Invalidate(nil) = %d, %v; want 0, nil", count, err)
	}
	keys := []CacheKey{
		{Kind: "kb_public", Scope: "ns"},
		{Kind: "documents", Scope: "ns"},
		{Kind: "kb_public", Scope: "ns"},
	}
	if count, err := stores.CacheSync.Invalidate(ctx, keys); err != nil || count != 2 {
		t.Fatalf("Invalidate() = %d, %v; want 2 deduplicated keys", count, err)
	}
	if value := readCacheRevision(t, stores, "kb_public", "ns").Revision; value != 1 {
		t.Fatalf("kb_public revision = %d, want 1", value)
	}
	if count, err := stores.CacheSync.Invalidate(ctx, keys); err != nil || count != 2 {
		t.Fatalf("second Invalidate() = %d, %v", count, err)
	}
	if value := readCacheRevision(t, stores, "kb_public", "ns").Revision; value != 2 {
		t.Fatalf("kb_public revision = %d, want 2", value)
	}
	if _, err := stores.CacheSync.Invalidate(ctx, []CacheKey{{Kind: "document_status", Scope: ""}}); err == nil {
		t.Fatal("Invalidate() accepted a retired cache kind")
	}
	// Kinds without a registered loader are a no-op, never an error.
	if refreshed, err := stores.CacheSync.RefreshKey(ctx, "document_status", ""); err != nil || refreshed {
		t.Fatalf("RefreshKey(retired) = %v, %v; want false, nil", refreshed, err)
	}
	// Revalidate de-duplicates (one bump) and publishes each key once.
	if err := stores.CacheSync.Revalidate(ctx, []CacheKey{{Kind: "kb_list", Scope: ""}, {Kind: "kb_list", Scope: ""}}); err != nil {
		t.Fatalf("Revalidate() error = %v", err)
	}
	if value := readCacheRevision(t, stores, "kb_list", "").Revision; value != 1 {
		t.Fatalf("kb_list revision = %d, want 1 (deduplicated)", value)
	}
	if hit, _, err := cache.Get(ctx, "kb_list", ""); err != nil || !hit {
		t.Fatalf("kb_list cache hit = %v, %v; want a published empty list", hit, err)
	}
}

func TestDirtyKeyBatchLimitsAndOrdering(t *testing.T) {
	stores, _ := newCacheTestStores(t)
	ctx := context.Background()
	if count, err := stores.CacheSync.Invalidate(ctx, knowledgeKeys("alpha")); err != nil || count != 3 {
		t.Fatalf("Invalidate() = %d, %v", count, err)
	}
	dirty, err := stores.CacheSync.DirtyKeys(ctx, 2)
	if err != nil || len(dirty) != 2 {
		t.Fatalf("DirtyKeys(2) = %v, %v; want 2 keys", dirty, err)
	}
	if dirty[0] != (CacheKey{Kind: "documents", Scope: "alpha"}) ||
		dirty[1] != (CacheKey{Kind: "kb_list", Scope: ""}) {
		t.Fatalf("DirtyKeys(2) order = %v", dirty)
	}
	if err := stores.CacheSync.RefreshDirty(ctx, 2); err != nil {
		t.Fatalf("RefreshDirty(2) error = %v", err)
	}
	remaining, err := stores.CacheSync.DirtyKeys(ctx, 0)
	if err != nil || len(remaining) != 1 || remaining[0] != (CacheKey{Kind: "kb_public", Scope: "alpha"}) {
		t.Fatalf("remaining dirty keys = %v, %v", remaining, err)
	}
}

func TestDirtySweepIgnoresRetiredKindRows(t *testing.T) {
	stores, _ := newCacheTestStores(t)
	ctx := context.Background()
	// A row left behind by a retired kind has no loader; it must not occupy the
	// dirty batch (its name sorts before the live kinds).
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO cache_revisions (kind, scope, revision, updated_at) VALUES (?, ?, ?, ?)",
		"document_status", "legacy", 5, 1.0); err != nil {
		t.Fatalf("legacy row insert error = %v", err)
	}
	if count, err := stores.CacheSync.Invalidate(ctx, []CacheKey{{Kind: "kb_public", Scope: "alpha"}}); err != nil || count != 1 {
		t.Fatalf("Invalidate() = %d, %v", count, err)
	}
	dirty, err := stores.CacheSync.DirtyKeys(ctx, 1)
	if err != nil || len(dirty) != 1 || dirty[0] != (CacheKey{Kind: "kb_public", Scope: "alpha"}) {
		t.Fatalf("DirtyKeys(1) = %v, %v; want the live key", dirty, err)
	}
	if err := stores.CacheSync.RefreshDirty(ctx, 0); err != nil {
		t.Fatalf("RefreshDirty() error = %v", err)
	}
	if dirty, err := stores.CacheSync.DirtyKeys(ctx, 0); err != nil || len(dirty) != 0 {
		t.Fatalf("DirtyKeys() after sweep = %v, %v; want none", dirty, err)
	}
	if value := readCacheRevision(t, stores, "document_status", "legacy").Revision; value != 5 {
		t.Fatalf("retired row revision = %d, want it untouched", value)
	}
}

func TestDirtyKeysIncludesFailureEvenWhenRevisionIsCurrent(t *testing.T) {
	stores, _ := newCacheTestStores(t)
	ctx := context.Background()
	if count, err := stores.CacheSync.Invalidate(ctx, []CacheKey{{Kind: "kb_public", Scope: "alpha"}}); err != nil || count != 1 {
		t.Fatalf("Invalidate() = %d, %v", count, err)
	}
	if refreshed, err := stores.CacheSync.RefreshKey(ctx, "kb_public", "alpha"); err != nil || !refreshed {
		t.Fatalf("RefreshKey() = %v, %v", refreshed, err)
	}
	// A recorded failure must keep the key dirty even when the published
	// revision already equals the committed revision.
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE cache_revisions SET attempts = 3, last_error = 'CacheUnavailable' "+
			"WHERE kind = ? AND scope = ?", "kb_public", "alpha"); err != nil {
		t.Fatalf("simulated failure update error = %v", err)
	}
	row := readCacheRevision(t, stores, "kb_public", "alpha")
	if row.RefreshedRevision != row.Revision || !row.LastError.Valid {
		t.Fatalf("simulated row = %+v, want a current watermark plus last_error", row)
	}
	dirty, err := stores.CacheSync.DirtyKeys(ctx, 0)
	if err != nil || len(dirty) != 1 || dirty[0] != (CacheKey{Kind: "kb_public", Scope: "alpha"}) {
		t.Fatalf("DirtyKeys() = %v, %v; want the failed key", dirty, err)
	}
	if err := stores.CacheSync.RefreshDirty(ctx, 0); err != nil {
		t.Fatalf("RefreshDirty() error = %v", err)
	}
	row = readCacheRevision(t, stores, "kb_public", "alpha")
	if row.LastError.Valid {
		t.Fatalf("last_error = %v after retry, want NULL", row.LastError)
	}
	if dirty, err := stores.CacheSync.DirtyKeys(ctx, 0); err != nil || len(dirty) != 0 {
		t.Fatalf("DirtyKeys() after retry = %v, %v; want none", dirty, err)
	}
}

func TestMarkAllDirtyCoversEveryKnownScope(t *testing.T) {
	stores, _ := newCacheTestStores(t)
	ctx := context.Background()
	for _, namespace := range []string{"alpha", "beta"} {
		if err := stores.KnowledgeBases.Create(ctx, map[string]any{"namespace": namespace, "collection": "c"}); err != nil {
			t.Fatalf("Create(%q) error = %v", namespace, err)
		}
	}
	if err := stores.Documents.Upsert(ctx, "alpha", DocumentRecord{Name: "a.txt", DocumentID: "doc-a", Collection: "c"}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO mcp_catalogs (namespace, data, version, updated_at) VALUES (?, ?, ?, ?)",
			[]any{"alpha", `{"servers":[]}`, 1, nowISO()}},
		{"INSERT INTO document_heads (namespace, document_id, version, updated_at) VALUES (?, ?, ?, ?)",
			[]any{"beta", "doc-b", "v1", 1.0}},
		{"INSERT INTO document_leases (namespace, resource, owner, operation_id, expires_at) VALUES (?, ?, ?, NULL, ?)",
			[]any{"gamma", "resource:one", "owner", unixSeconds() + 60}},
		{"INSERT INTO upload_operations (id, namespace, name, document_id, collection, owner, status, " +
			"cleanup_pending, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			[]any{"op-1", "delta", "file.txt", "doc-d", "c", "owner", "RUNNING", false, 1.0, 2.0}},
	} {
		if _, err := stores.DB.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed %q error = %v", statement.query, err)
		}
	}
	expected := []CacheKey{
		{Kind: "kb_list", Scope: ""},
		{Kind: "kb_public", Scope: "alpha"},
		{Kind: "documents", Scope: "alpha"},
		{Kind: "catalog", Scope: "alpha"},
		{Kind: "kb_public", Scope: "beta"},
		{Kind: "documents", Scope: "beta"},
		{Kind: "heads", Scope: "beta"},
		{Kind: "leases", Scope: "gamma"},
		{Kind: "upload_list", Scope: "delta"},
		{Kind: "upload_status", Scope: "op-1"},
	}
	before := make(map[CacheKey]int64, len(expected))
	for _, key := range expected {
		before[key] = cacheRevisionOrZero(t, stores, key.Kind, key.Scope)
	}
	coveredKinds := make(map[string]struct{}, len(expected))
	for _, key := range expected {
		coveredKinds[key.Kind] = struct{}{}
	}
	for kind := range cacheKinds {
		if _, covered := coveredKinds[kind]; !covered {
			t.Fatalf("cacheMarkAllDirty expected keys miss the registered kind %q", kind)
		}
	}
	count, err := stores.CacheSync.MarkAllDirty(ctx)
	if err != nil {
		t.Fatalf("MarkAllDirty() error = %v", err)
	}
	if count != len(expected) {
		t.Fatalf("MarkAllDirty() count = %d, want %d", count, len(expected))
	}
	for _, key := range expected {
		row := readCacheRevision(t, stores, key.Kind, key.Scope)
		if row.Revision != before[key]+1 {
			t.Fatalf("%s/%s revision = %d, want %d", key.Kind, key.Scope, row.Revision, before[key]+1)
		}
	}
	dirty, err := stores.CacheSync.DirtyKeys(ctx, 0)
	if err != nil || len(dirty) != len(expected) {
		t.Fatalf("DirtyKeys() after baseline = %d keys, %v; want %d", len(dirty), err, len(expected))
	}
	// Idempotent: a second baseline pass bumps the same rows again.
	if count, err := stores.CacheSync.MarkAllDirty(ctx); err != nil || count != len(expected) {
		t.Fatalf("second MarkAllDirty() = %d, %v", count, err)
	}
	if value := readCacheRevision(t, stores, "kb_list", "").Revision; value != before[expected[0]]+2 {
		t.Fatalf("kb_list revision after second baseline = %d, want %d", value, before[expected[0]]+2)
	}
	// Every derived view can be rebuilt through the registered loaders.
	if err := stores.CacheSync.RefreshDirty(ctx, 0); err != nil {
		t.Fatalf("RefreshDirty() error = %v", err)
	}
	if dirty, err := stores.CacheSync.DirtyKeys(ctx, 0); err != nil || len(dirty) != 0 {
		t.Fatalf("DirtyKeys() after full rebuild = %v, %v; want none", dirty, err)
	}
}

func TestMarkAllDirtyOnEmptyDatabase(t *testing.T) {
	stores, _ := newCacheTestStores(t)
	ctx := context.Background()
	count, err := stores.CacheSync.MarkAllDirty(ctx)
	if err != nil || count != 1 {
		t.Fatalf("MarkAllDirty() on an empty database = %d, %v; want the global knowledge-base list", count, err)
	}
	row := readCacheRevision(t, stores, "kb_list", "")
	if row.Revision != 1 || row.RefreshedRevision != 0 {
		t.Fatalf("kb_list revision/refreshed = %d/%d, want 1/0", row.Revision, row.RefreshedRevision)
	}
}

func TestDisabledBackendStillDrainsDirtyState(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if stores.CacheSync.Enabled() {
		t.Fatal("default synchronizer should start with the cache disabled")
	}
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := stores.CacheSync.Revalidate(ctx, knowledgeKeys("ns")); err != nil {
		t.Fatalf("Revalidate() error = %v", err)
	}
	if dirty, err := stores.CacheSync.DirtyKeys(ctx, 0); err != nil || len(dirty) != 0 {
		t.Fatalf("DirtyKeys() with a disabled backend = %v, %v; want none", dirty, err)
	}
	value, err := stores.Views.KnowledgeBase(ctx, "ns")
	if err != nil || value["collection"] != "c" {
		t.Fatalf("KnowledgeBase() = %+v, %v", value, err)
	}
}

func TestCloseClosesBackendAndIsIdempotent(t *testing.T) {
	stores, cache := newCacheTestStores(t)
	if err := stores.CacheSync.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	cache.mu.Lock()
	closed := cache.closed
	cache.mu.Unlock()
	if !closed {
		t.Fatal("Close() did not close the cache backend")
	}
	if err := stores.CacheSync.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	// Start/Stop are compatibility no-ops now that the relay poll is gone.
	stores.CacheSync.Start()
	stores.CacheSync.Stop()
}

func TestBusinessTransactionRollbackAlsoRollsBackRevisions(t *testing.T) {
	stores, _ := newCacheTestStores(t)
	ctx := context.Background()
	before := cacheRevisionCount(t, stores)
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := stores.Documents.UpsertTx(ctx, transaction, "ns", DocumentRecord{
		Name: "a", DocumentID: "doc", Collection: "c",
	}); err != nil {
		t.Fatalf("UpsertTx() error = %v", err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if after := cacheRevisionCount(t, stores); after != before {
		t.Fatalf("rollback leaked cache_revisions rows: %d -> %d", before, after)
	}
	if documents, err := stores.Documents.List(ctx, "ns"); err != nil || len(documents) != 0 {
		t.Fatalf("Documents.List() = %v, %v", documents, err)
	}
}

// --- Randomized TTL ---------------------------------------------------------

func TestRandomTTLSecondsRange(t *testing.T) {
	seen := make(map[int]bool, 13)
	for range 500 {
		drawn := RandomTTLSeconds(60)
		if drawn < 48 || drawn > 60 {
			t.Fatalf("RandomTTLSeconds(60) = %d, want within [48, 60]", drawn)
		}
		seen[drawn] = true
	}
	if len(seen) < 10 {
		t.Fatalf("RandomTTLSeconds(60) drew only %d distinct values, want a real distribution", len(seen))
	}
	if drawn := RandomTTLSeconds(0); drawn != 0 {
		t.Fatalf("RandomTTLSeconds(0) = %d, want 0", drawn)
	}
}

// --- Per-key backfill lock read path -----------------------------------------

// TestGetOrLoadBackfillsOnceForConcurrentMisses covers the thundering-herd
// row: two "instances" miss the same expired documents list at the same time,
// but only one backfill executes the list query; the waiter re-reads the cache
// and returns the holder's published value.
func TestGetOrLoadBackfillsOnceForConcurrentMisses(t *testing.T) {
	stores, cache := newFastCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var queries int
	var mu sync.Mutex
	loader := stores.CacheSync.loaders["documents"]
	stores.CacheSync.loaders["documents"] = func(loaderCtx context.Context, querier viewQuerier, scope string) (any, error) {
		mu.Lock()
		queries++
		mu.Unlock()
		return loader(loaderCtx, countingQuerier{viewQuerier: querier}, scope)
	}

	var wg sync.WaitGroup
	results := make([]any, 2)
	errs := make([]error, 2)
	for index := range 2 {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			results[slot], errs[slot] = stores.CacheSync.GetOrLoad(ctx, "documents", "ns")
		}(index)
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("GetOrLoad(%d) error = %v", index, err)
		}
	}
	if queries != 1 {
		t.Fatalf("backfill list queries = %d, want exactly 1", queries)
	}
	// Both callers return the same JSON-detached shape the backend hands back
	// (a []any of records after the round trip).
	if fmt.Sprint(results[0]) != fmt.Sprint(results[1]) {
		t.Fatalf("results differ between caller slots: %#v vs %#v", results[0], results[1])
	}
	if list, ok := results[0].([]any); !ok || len(list) != 0 {
		t.Fatalf("first result = %#v, want the shared empty list", results[0])
	}
	if hit, _, err := cache.Get(ctx, "documents", "ns"); err != nil || !hit {
		t.Fatalf("cache hit after backfill = (%v, %v), want a hit", hit, err)
	}
}

// uniqueCacheNamespace builds a per-test namespace so repeated runs of the
// backfill-lock tests never collide inside one shared schema sweep.
func uniqueCacheNamespace(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1_000_000_000)
}

// TestGetOrLoadParallelScopesBackfillIndependently covers the granularity
// row: one lock per kind+scope, different knowledge bases backfill in
// parallel without waiting for each other.
func TestGetOrLoadParallelScopesBackfillIndependently(t *testing.T) {
	stores := newTestStores(t)
	cache := newMemoryCache()
	stores.CacheSync.Backend = cache
	stores.CacheSync.backfillTTL = 150 * time.Millisecond
	stores.CacheSync.backfillPoll = 5 * time.Millisecond
	stores.CacheSync.backfillWait = 150 * time.Millisecond
	ctx := context.Background()
	base := uniqueCacheNamespace(t, "parallel")
	scopes := []string{base + "-a", base + "-b"}
	for _, namespace := range scopes {
		if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBaseFor(namespace)); err != nil {
			t.Fatalf("Create(%s) error = %v", namespace, err)
		}
	}

	// One barrier per scope: each fill blocks until both scopes have entered
	// the loader, which only completes if the two fills run concurrently.
	entered := make(chan string, 2)
	release := make(chan struct{})
	loader := stores.CacheSync.loaders["documents"]
	stores.CacheSync.loaders["documents"] = func(loaderCtx context.Context, querier viewQuerier, scope string) (any, error) {
		entered <- scope
		select {
		case <-release:
		case <-loaderCtx.Done():
			return nil, loaderCtx.Err()
		}
		return loader(loaderCtx, querier, scope)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for index, namespace := range scopes {
		wg.Add(1)
		go func(slot int, scope string) {
			defer wg.Done()
			_, errs[slot] = stores.CacheSync.GetOrLoad(ctx, "documents", scope)
		}(index, namespace)
	}
	seen := make(map[string]bool, 2)
	for range 2 {
		select {
		case scope := <-entered:
			seen[scope] = true
		case <-time.After(5 * time.Second):
			close(release)
			wg.Wait()
			t.Fatalf("scopes serialized: only %v started while both fills should run concurrently", seen)
		}
	}
	close(release)
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("GetOrLoad(%d) error = %v", index, err)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("expected both scopes to fill concurrently, started = %v", seen)
	}
}

// TestGetOrLoadWaiterTakesOverAfterHolderExits covers the takeover row: the
// first holder fills and releases; a waiter that missed both the payload and
// the lock window acquires the freed lock and performs its own backfill.
func TestGetOrLoadWaiterTakesOverAfterHolderExits(t *testing.T) {
	stores, cache := newFastCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var fills int
	var mu sync.Mutex
	loader := stores.CacheSync.loaders["documents"]
	stores.CacheSync.loaders["documents"] = func(loaderCtx context.Context, querier viewQuerier, scope string) (any, error) {
		mu.Lock()
		fills++
		mu.Unlock()
		if fills == 1 {
			// Hold the first fill long enough that the second reader waits
			// on the lock, then publish and release.
			time.Sleep(60 * time.Millisecond)
		}
		return loader(loaderCtx, querier, scope)
	}

	done := make(chan error, 2)
	go func() {
		_, err := stores.CacheSync.GetOrLoad(ctx, "documents", "ns")
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	go func() {
		_, err := stores.CacheSync.GetOrLoad(ctx, "documents", "ns")
		done <- err
	}()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("GetOrLoad error = %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("GetOrLoad did not finish after the holder released")
		}
	}
	if fills < 1 {
		t.Fatalf("backfill fills = %d, want at least 1", fills)
	}
	if hit, _, err := cache.Get(ctx, "documents", "ns"); err != nil || !hit {
		t.Fatalf("cache hit after takeover = (%v, %v), want a hit", hit, err)
	}
}

// TestGetOrLoadWaiterTimesOutWithoutBypassingLock covers the wait-timeout row:
// a holder that never releases makes the waiter exhaust its bounded wait and
// return a retryable error — it must never fall back to reading MySQL
// directly.
func TestGetOrLoadWaiterTimesOutWithoutBypassingLock(t *testing.T) {
	stores, _ := newFastCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Hold the lock forever and drop every Get: the waiter can neither see a
	// payload nor take the lock.
	stolen := newBackfillToken()
	stores.CacheSync.loaders["documents"] = func(context.Context, viewQuerier, string) (any, error) {
		time.Sleep(2 * time.Second)
		return nil, nil
	}
	stores.CacheSync.Backend = heldOnlyCache{owner: stolen}

	start := time.Now()
	_, err := stores.CacheSync.GetOrLoad(ctx, "documents", "ns")
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("GetOrLoad returned after %v, want it to wait before timing out", elapsed)
	}
	if err == nil {
		t.Fatal("GetOrLoad must fail with a retryable error when the wait budget is exhausted")
	}
	var retryable *CacheRetryable
	if !errors.As(err, &retryable) {
		t.Fatalf("GetOrLoad error = %v (%T), want *CacheRetryable", err, err)
	}
}

// heldOnlyCache simulates a backend whose backfill lock is permanently held by
// a foreign owner while every Get misses.
type heldOnlyCache struct {
	owner string
}

func (h heldOnlyCache) Enabled() bool { return true }

func (h heldOnlyCache) Get(context.Context, string, string) (bool, any, error) {
	return false, nil, nil
}

func (h heldOnlyCache) Put(context.Context, string, string, int64, any, bool) (bool, error) {
	return false, nil
}

func (h heldOnlyCache) AcquireBackfillLock(_ context.Context, _, _, token string, _ time.Duration) (bool, error) {
	return token == h.owner, nil
}

func (h heldOnlyCache) RenewBackfillLock(_ context.Context, _, _, token string, _ time.Duration) (bool, error) {
	return token == h.owner, nil
}

func (h heldOnlyCache) ReleaseBackfillLock(context.Context, string, string, string) error {
	return nil
}

func (h heldOnlyCache) Close() error { return nil }

// TestGetOrLoadRedisOutageIsRetryableNotFallback pins the replaced failure
// semantics: with the cache enabled, an outage during the miss path (lock
// acquire) must return a retryable error instead of silently reading MySQL.
func TestGetOrLoadRedisOutageIsRetryableNotFallback(t *testing.T) {
	stores, cache := newFastCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	cache.mu.Lock()
	cache.down = true
	cache.mu.Unlock()

	var fallback bool
	loader := stores.CacheSync.loaders["documents"]
	stores.CacheSync.loaders["documents"] = func(loaderCtx context.Context, querier viewQuerier, scope string) (any, error) {
		fallback = true
		return loader(loaderCtx, querier, scope)
	}

	_, err := stores.CacheSync.GetOrLoad(ctx, "documents", "ns")
	if err == nil {
		t.Fatal("GetOrLoad must surface the outage as a retryable error")
	}
	var retryable *CacheRetryable
	if !errors.As(err, &retryable) {
		t.Fatalf("GetOrLoad error = %v (%T), want *CacheRetryable", err, err)
	}
	if fallback {
		t.Fatal("GetOrLoad fell back to MySQL during a Redis outage; the lock ownership could not be confirmed")
	}
}

// --- Shared lock with the background refresh ----------------------------------

// TestRefreshKeySharesTheBackfillLockWithGetOrLoad covers the shared-lock row:
// while a read miss holds the key, the background refresh observes a busy lock
// and keeps the pending-refresh state; after the reader publishes and
// releases, the next refresh (RefreshDirty) completes the target revision.
func TestRefreshKeySharesTheBackfillLockWithGetOrLoad(t *testing.T) {
	stores, cache := newFastCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := stores.Documents.Upsert(ctx, "ns", DocumentRecord{Name: "a.txt", Collection: "c"}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	inFill := make(chan struct{}, 4)
	release := make(chan struct{})
	loader := stores.CacheSync.loaders["documents"]
	stores.CacheSync.loaders["documents"] = func(loaderCtx context.Context, querier viewQuerier, scope string) (any, error) {
		select {
		case inFill <- struct{}{}:
		case <-loaderCtx.Done():
			return nil, loaderCtx.Err()
		}
		select {
		case <-release:
		case <-loaderCtx.Done():
			return nil, loaderCtx.Err()
		}
		return loader(loaderCtx, querier, scope)
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := stores.CacheSync.GetOrLoad(ctx, "documents", "ns")
		readDone <- err
	}()
	select {
	case <-inFill:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader never reached the MySQL snapshot")
	}

	// The background refresh contends on the same lock and must not fill.
	refreshed, err := stores.CacheSync.RefreshKey(ctx, "documents", "ns")
	if err != nil {
		t.Fatalf("RefreshKey() while the lock is held = %v, want nil", err)
	}
	if refreshed {
		t.Fatal("RefreshKey must report (false, nil) while another holder fills the key")
	}
	select {
	case <-inFill:
		t.Fatal("RefreshKey filled the key while the reader held the lock")
	default:
	}

	// The pending refresh stays dirty: refreshed_revision lags revision.
	row := readCacheRevision(t, stores, "documents", "ns")
	if row.RefreshedRevision >= row.Revision {
		t.Fatalf("busy-lock refresh must keep the key dirty: refreshed=%d revision=%d",
			row.RefreshedRevision, row.Revision)
	}

	close(release)
	if err := <-readDone; err != nil {
		t.Fatalf("reader GetOrLoad error = %v", err)
	}

	// The compensation sweep publishes the target revision and clears the
	// dirty state.
	if err := stores.CacheSync.RefreshDirty(ctx, 0); err != nil {
		t.Fatalf("RefreshDirty() error = %v", err)
	}
	row = readCacheRevision(t, stores, "documents", "ns")
	if row.RefreshedRevision != row.Revision || row.LastError.Valid {
		t.Fatalf("after compensation refreshed/last_error = %d/%v (revision %d), want clean",
			row.RefreshedRevision, row.LastError, row.Revision)
	}
	if dirty, err := stores.CacheSync.DirtyKeys(ctx, 0); err != nil || len(dirty) != 0 {
		t.Fatalf("DirtyKeys() after compensation = %v, %v, want none", dirty, err)
	}
	if hit, _, err := cache.Get(ctx, "documents", "ns"); err != nil || !hit {
		t.Fatalf("cache hit after refresh = (%v, %v), want a hit", hit, err)
	}
}

// TestGetOrLoadLostLeaseBlocksStaleRefill covers the fencing row: a holder
// whose lease expires mid-query (renewal fails) must have its query cancelled
// and must not publish its snapshot; the next reader re-runs the backfill and
// returns a live value.
func TestGetOrLoadLostLeaseBlocksStaleRefill(t *testing.T) {
	stores, cache := newFastCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// The lease is owned by a foreign token, so the holder's first renewal
	// (half the TTL) reports the lease lost, cancels the query and blocks the
	// publish. The loader waits longer than the TTL to prove the cancel.
	cancels := make(chan struct{}, 1)
	loader := stores.CacheSync.loaders["documents"]
	stores.CacheSync.loaders["documents"] = func(loaderCtx context.Context, querier viewQuerier, scope string) (any, error) {
		select {
		case <-loaderCtx.Done():
			select {
			case cancels <- struct{}{}:
			default:
			}
			return nil, loaderCtx.Err()
		case <-time.After(2 * time.Second):
			// Without a cancel this outlives the test; the renewal must stop it.
		}
		return loader(loaderCtx, querier, scope)
	}
	stolen := newBackfillToken()
	stores.CacheSync.Backend = leaseStealingCache{inner: cache, owner: stolen}

	_, err := stores.CacheSync.GetOrLoad(ctx, "documents", "ns")
	if err == nil {
		t.Fatal("the lease-lost read must fail retryably instead of publishing a stale snapshot")
	}
	var retryable *CacheRetryable
	if !errors.As(err, &retryable) {
		t.Fatalf("GetOrLoad error = %v (%T), want *CacheRetryable", err, err)
	}
	select {
	case <-cancels:
	case <-time.After(2 * time.Second):
		t.Fatal("the in-flight query was not cancelled after the lease was lost")
	}
	if _, cached, err := cache.Get(ctx, "documents", "ns"); err != nil || cached != nil {
		t.Fatalf("the lease-lost snapshot was published: hit=%v value=%v err=%v", true, cached, err)
	}

	// The next reader takes over and succeeds.
	stores.CacheSync.Backend = cache
	stores.CacheSync.loaders["documents"] = loader
	if _, err := stores.CacheSync.GetOrLoad(ctx, "documents", "ns"); err != nil {
		t.Fatalf("second GetOrLoad error = %v", err)
	}
}

// leaseStealingCache wraps a real backend but answers every renewal as though a
// foreign owner had taken the lock, simulating an expired-and-reacquired lease.
type leaseStealingCache struct {
	inner StateCache
	owner string
}

func (s leaseStealingCache) Enabled() bool { return true }

func (s leaseStealingCache) Get(ctx context.Context, kind, scope string) (bool, any, error) {
	return s.inner.Get(ctx, kind, scope)
}

func (s leaseStealingCache) Put(ctx context.Context, kind, scope string, revision int64, value any, deleted bool) (bool, error) {
	return s.inner.Put(ctx, kind, scope, revision, value, deleted)
}

func (s leaseStealingCache) AcquireBackfillLock(ctx context.Context, kind, scope, token string, ttl time.Duration) (bool, error) {
	return s.inner.AcquireBackfillLock(ctx, kind, scope, token, ttl)
}

func (s leaseStealingCache) RenewBackfillLock(_ context.Context, kind, scope, token string, _ time.Duration) (bool, error) {
	// Only the foreign owner renews successfully.
	return token == s.owner, nil
}

func (s leaseStealingCache) ReleaseBackfillLock(ctx context.Context, kind, scope, token string) error {
	return s.inner.ReleaseBackfillLock(ctx, kind, scope, token)
}

func (s leaseStealingCache) Close() error { return nil }

// TestRefreshKeyRecordsLockOutageAsDirty covers the refresh-path outage row:
// an unverifiable lock records the sanitized failure category and keeps the
// key dirty instead of pretending refreshed.
func TestRefreshKeyRecordsLockOutageAsDirty(t *testing.T) {
	stores, cache := newFastCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	cache.mu.Lock()
	cache.down = true
	cache.mu.Unlock()

	if refreshed, err := stores.CacheSync.RefreshKey(ctx, "kb_public", "ns"); err != nil || refreshed {
		t.Fatalf("RefreshKey() during a lock outage = (%v, %v), want (false, nil)", refreshed, err)
	}
	row := readCacheRevision(t, stores, "kb_public", "ns")
	if !row.LastError.Valid || row.LastError.String != "CacheUnavailable" {
		t.Fatalf("lock outage must record the sanitized failure, got last_error = %v", row.LastError)
	}
	if row.RefreshedRevision != 0 {
		t.Fatalf("refreshed_revision = %d after a lock outage, want 0", row.RefreshedRevision)
	}
	if dirty, err := stores.CacheSync.DirtyKeys(ctx, 0); err != nil || len(dirty) == 0 {
		t.Fatalf("DirtyKeys() after lock outage = %v, %v, want the key pending compensation", dirty, err)
	}
}
