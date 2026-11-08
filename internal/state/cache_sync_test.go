package state

import (
	"context"
	"encoding/json"
	"errors"
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

// memoryPublisher is the test EventPublisher.
type memoryPublisher struct {
	mu       sync.Mutex
	down     bool
	closed   bool
	messages []string
}

func (m *memoryPublisher) Send(eventID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return errors.New("queue unavailable")
	}
	m.messages = append(m.messages, eventID)
	return nil
}

func (m *memoryPublisher) ConsumeOnce(callback func(eventID string) bool) {
	m.mu.Lock()
	pending := m.messages
	m.messages = nil
	m.mu.Unlock()
	for _, eventID := range pending {
		if !callback(eventID) {
			m.mu.Lock()
			m.messages = append(m.messages, eventID)
			m.mu.Unlock()
		}
	}
}

func (m *memoryPublisher) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *memoryPublisher) snapshotMessages() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.messages...)
}

func newCacheTestStores(t *testing.T) (*Stores, *memoryCache, *memoryPublisher) {
	t.Helper()
	stores := newTestStores(t)
	cache := newMemoryCache()
	transport := &memoryPublisher{}
	stores.CacheSync.Backend = cache
	stores.CacheSync.Transport = transport
	return stores, cache, transport
}

func cacheTestKnowledgeBase(collection string) map[string]any {
	return map[string]any{
		"namespace": "ns", "collection": collection, "embedding_model": "model", "reranker_model": "",
		"embedding": map[string]any{"base_url": "http://provider", "protected_key": "private-file:NEVER_CACHE"},
		"reranker":  map[string]any{},
		"strategy":  "character", "chunk_size": 100, "chunk_overlap": 10,
	}
}

type outboxRow struct {
	ID, Kind, Scope, Status string
	Revision                int64
	Attempts                int
	RetryAt                 float64
	LastError               *string
}

func outboxRows(t *testing.T, stores *Stores) []outboxRow {
	t.Helper()
	rows, err := stores.DB.QueryContext(context.Background(),
		"SELECT id, kind, scope, status, revision, attempts, retry_at, last_error FROM cache_outbox ORDER BY created_at, id")
	if err != nil {
		t.Fatalf("outbox query error = %v", err)
	}
	defer closeRows(t, rows)
	records := make([]outboxRow, 0)
	for rows.Next() {
		var record outboxRow
		var lastError *string
		if err := rows.Scan(&record.ID, &record.Kind, &record.Scope, &record.Status, &record.Revision,
			&record.Attempts, &record.RetryAt, &lastError); err != nil {
			t.Fatalf("outbox scan error = %v", err)
		}
		record.LastError = lastError
		records = append(records, record)
	}
	return records
}

func statuses(rows []outboxRow) []string {
	values := make([]string, len(rows))
	for index, row := range rows {
		values[index] = row.Status
	}
	return values
}

func TestCacheHitReadsNoSQLAndConfigurationIsRedacted(t *testing.T) {
	stores, cache, _ := newCacheTestStores(t)
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
	// Bypass the repositories (and their touch) to prove the next read is a hit.
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
	for _, row := range outboxRows(t, stores) {
		if row.Status != "DONE" {
			t.Fatalf("outbox row %+v was not refreshed", row)
		}
	}
}

func TestCacheWriteRefreshesValuesAndDeleteIsVersioned(t *testing.T) {
	stores, cache, _ := newCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	cache.mu.Lock()
	firstRevision := cache.watermarks[CacheKey{Kind: "kb_public", Scope: "ns"}]
	cache.mu.Unlock()
	oldValue := cacheTestKnowledgeBase("c")
	if err := stores.KnowledgeBases.Update(ctx, cacheTestKnowledgeBase("updated")); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	updated, err := stores.Views.KnowledgeBase(ctx, "ns")
	if err != nil || updated["collection"] != "updated" {
		t.Fatalf("KnowledgeBase() after update = %+v, %v", updated, err)
	}
	if _, err := stores.KnowledgeBases.Delete(ctx, "ns"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	deleted, err := stores.Views.KnowledgeBase(ctx, "ns")
	if err != nil || deleted != nil {
		t.Fatalf("KnowledgeBase() after delete = %+v, %v", deleted, err)
	}
	stale, err := cache.Put(ctx, "kb_public", "ns", firstRevision, oldValue, false)
	if err != nil || stale {
		t.Fatalf("stale Put() = %v, %v; want false", stale, err)
	}
	if value, err := stores.Views.KnowledgeBase(ctx, "ns"); err != nil || value != nil {
		t.Fatalf("stale fill resurrected a deleted view: %+v, %v", value, err)
	}
	for _, row := range outboxRows(t, stores) {
		if row.Status != "DONE" {
			t.Fatalf("outbox row %+v was not marked DONE", row)
		}
	}
}

func TestBusinessTransactionRollbackAlsoRollsBackOutbox(t *testing.T) {
	stores, _, _ := newCacheTestStores(t)
	ctx := context.Background()
	before := outboxRows(t, stores)
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
	if after := outboxRows(t, stores); len(after) != len(before) {
		t.Fatalf("rollback leaked outbox rows: %d -> %d", len(before), len(after))
	}
	if documents, err := stores.Documents.List(ctx, "ns"); err != nil || len(documents) != 0 {
		t.Fatalf("Documents.List() = %v, %v", documents, err)
	}
}

func TestCacheFailureQueuesOnlyEventIDsThenConsumerRefreshesLatest(t *testing.T) {
	stores, cache, transport := newCacheTestStores(t)
	ctx := context.Background()
	cache.mu.Lock()
	cache.down = true
	cache.mu.Unlock()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	rows := outboxRows(t, stores)
	if statuses(rows) == nil || len(rows) == 0 {
		t.Fatal("expected durable outbox rows")
	}
	for _, row := range rows {
		if row.Status != "QUEUED" {
			t.Fatalf("row status = %q, want QUEUED", row.Status)
		}
	}
	messages := transport.snapshotMessages()
	if len(messages) == 0 {
		t.Fatal("queue did not receive retry events")
	}
	for _, message := range messages {
		if len(message) != 32 || !ValidateCacheEventID(message) {
			t.Fatalf("queue message %q is not a 32-char event id", message)
		}
	}
	if err := stores.KnowledgeBases.Update(ctx, cacheTestKnowledgeBase("latest")); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	cache.mu.Lock()
	cache.down = false
	cache.mu.Unlock()
	replayed := make([]string, 0, len(transport.snapshotMessages())*2)
	replayed = append(replayed, transport.snapshotMessages()...)
	replayed = append(replayed, replayed...)
	for index := len(replayed) - 1; index >= 0; index-- {
		refreshed, err := stores.CacheSync.RefreshEvent(ctx, replayed[index])
		if err != nil {
			t.Fatalf("RefreshEvent() error = %v", err)
		}
		if !refreshed {
			t.Fatalf("RefreshEvent(%s) returned false", replayed[index])
		}
	}
	value, err := stores.Views.KnowledgeBase(ctx, "ns")
	if err != nil || value["collection"] != "latest" {
		t.Fatalf("KnowledgeBase() = %+v, %v; want the latest committed snapshot", value, err)
	}
	for _, row := range outboxRows(t, stores) {
		if row.Status != "DONE" {
			t.Fatalf("row %+v was not marked DONE", row)
		}
	}
}

func TestBothCacheAndQueueFailuresLeaveDurableRetry(t *testing.T) {
	stores, cache, transport := newCacheTestStores(t)
	ctx := context.Background()
	cache.mu.Lock()
	cache.down = true
	cache.mu.Unlock()
	transport.down = true
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	for _, row := range outboxRows(t, stores) {
		if row.Status != "PENDING" {
			t.Fatalf("row status = %q, want PENDING", row.Status)
		}
	}
	settings := stores.Settings
	if err := stores.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	restarted, err := New(settings, true)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = restarted.Close() }()
	restartedCache := newMemoryCache()
	restarted.CacheSync.Backend = restartedCache
	restarted.CacheSync.Transport = &memoryPublisher{}
	if _, err := restarted.DB.ExecContext(ctx, "UPDATE cache_outbox SET retry_at = 0"); err != nil {
		t.Fatalf("retry_at reset error = %v", err)
	}
	if err := restarted.CacheSync.FlushPending(ctx, nil); err != nil {
		t.Fatalf("FlushPending() error = %v", err)
	}
	value, err := restarted.Views.KnowledgeBase(ctx, "ns")
	if err != nil || value["collection"] != "c" {
		t.Fatalf("KnowledgeBase() after restart = %+v, %v", value, err)
	}
	for _, row := range outboxRows(t, restarted) {
		if row.Status != "DONE" {
			t.Fatalf("row %+v was not marked DONE after restart", row)
		}
	}
}

func TestStaleMissCannotOverwriteNewerWrite(t *testing.T) {
	stores, cache, _ := newCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	cache.mu.Lock()
	oldRevision := cache.watermarks[CacheKey{Kind: "kb_public", Scope: "ns"}]
	cache.mu.Unlock()
	oldValue, err := stores.Views.KnowledgeBase(ctx, "ns")
	if err != nil {
		t.Fatalf("KnowledgeBase() error = %v", err)
	}
	if err := stores.KnowledgeBases.Update(ctx, cacheTestKnowledgeBase("new")); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if accepted, err := cache.Put(ctx, "kb_public", "ns", oldRevision, oldValue, false); err != nil || accepted {
		t.Fatalf("stale Put() = %v, %v; want false", accepted, err)
	}
	value, err := stores.Views.KnowledgeBase(ctx, "ns")
	if err != nil || value["collection"] != "new" {
		t.Fatalf("KnowledgeBase() = %+v, %v; want new", value, err)
	}
}

func TestUploadPublicationRollbackNeverRefreshesUncommittedList(t *testing.T) {
	stores, _, _ := newCacheTestStores(t)
	ctx := context.Background()
	if documents, err := stores.Views.Documents(ctx, "ns"); err != nil || len(documents) != 0 {
		t.Fatalf("Documents() = %v, %v", documents, err)
	}
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
	if err := stores.CacheSync.FlushPending(ctx, nil); err != nil {
		t.Fatalf("FlushPending() error = %v", err)
	}
	if documents, err := stores.Views.Documents(ctx, "ns"); err != nil || len(documents) != 0 {
		t.Fatalf("Documents() after rollback = %v, %v", documents, err)
	}
	if value := revision(t, stores, "documents", "ns"); value != 0 {
		t.Fatalf("documents revision = %d, want 0", value)
	}
}

func TestLifespanStopClosesQueueClients(t *testing.T) {
	stores, _, transport := newCacheTestStores(t)
	stores.CacheSync.Start()
	stores.CacheSync.Stop()
	transport.mu.Lock()
	closed := transport.closed
	transport.mu.Unlock()
	if !closed {
		t.Fatal("Stop() did not close the queue transport")
	}
	stores.CacheSync.workerMu.Lock()
	worker := stores.CacheSync.worker
	stores.CacheSync.workerMu.Unlock()
	if worker != nil {
		t.Fatal("Stop() left a background worker registered")
	}
}

func TestSynchronousWriteRefreshNotSkippedWhileRelayIsActive(t *testing.T) {
	stores, _, _ := newCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, cacheTestKnowledgeBase("c")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	stores.CacheSync.mutex.Lock()
	defer stores.CacheSync.mutex.Unlock()
	if err := stores.KnowledgeBases.Update(ctx, cacheTestKnowledgeBase("synchronously-updated")); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	value, err := stores.Views.KnowledgeBase(ctx, "ns")
	if err != nil || value["collection"] != "synchronously-updated" {
		t.Fatalf("KnowledgeBase() = %+v, %v", value, err)
	}
}
