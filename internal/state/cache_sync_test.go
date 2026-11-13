package state

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// memoryCache is the test StateCache: it mirrors the Python MemoryCache fixture
// (JSON-detached values, revision watermarks, outage simulation).
type memoryCache struct {
	mu         sync.Mutex
	enabled    bool
	down       bool
	closed     bool
	entries    map[CacheKey]any
	watermarks map[CacheKey]int64
}

func newMemoryCache() *memoryCache {
	return &memoryCache{
		enabled:    true,
		entries:    map[CacheKey]any{},
		watermarks: map[CacheKey]int64{},
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

func cacheTestKnowledgeBase(collection string) map[string]any {
	return map[string]any{
		"namespace": "ns", "collection": collection, "embedding_model": "model", "reranker_model": "",
		"embedding": map[string]any{"base_url": "http://provider", "protected_key": "private-file:NEVER_CACHE"},
		"reranker":  map[string]any{},
		"strategy":  "character", "chunk_size": 100, "chunk_overlap": 10,
	}
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
	if err := stores.CacheSync.Revalidate(ctx, []CacheKey{{Kind: "api_keys", Scope: ""}, {Kind: "api_keys", Scope: ""}}); err != nil {
		t.Fatalf("Revalidate() error = %v", err)
	}
	if value := readCacheRevision(t, stores, "api_keys", "").Revision; value != 1 {
		t.Fatalf("api_keys revision = %d, want 1 (deduplicated)", value)
	}
	if hit, _, err := cache.Get(ctx, "api_keys", ""); err != nil || !hit {
		t.Fatalf("api_keys cache hit = %v, %v; want a published empty list", hit, err)
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
		{Kind: "api_keys", Scope: ""},
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
	if err != nil || count != 2 {
		t.Fatalf("MarkAllDirty() on an empty database = %d, %v; want the two global keys", count, err)
	}
	for _, kind := range []string{"kb_list", "api_keys"} {
		row := readCacheRevision(t, stores, kind, "")
		if row.Revision != 1 || row.RefreshedRevision != 0 {
			t.Fatalf("%s revision/refreshed = %d/%d, want 1/0", kind, row.Revision, row.RefreshedRevision)
		}
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
