package cdcacceptance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/cacheconsumer"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// cacheTestDocumentsSQL inserts a documents row directly into MySQL, bypassing
// the repository layer, to simulate data changed outside the application (the
// scenario the Debezium cache connector exists for).
func insertDocumentSQL(t *testing.T, stores *state.Stores, namespace, name, documentID, sha256 string) {
	t.Helper()
	execSQL(t, stores,
		"INSERT INTO documents (namespace, name, document_id, size, sha256, chunking, collection, "+
			"chunk_count, modified_at, indexed_at, diagnostics, skipped_object_count, del_flag) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)",
		namespace, name, documentID, 3, sha256, `{"strategy":"direct"}`, "acceptance-collection",
		1, "", "", "[]", 0)
}

// updateDocumentSQL changes one documents row directly in MySQL.
func updateDocumentSQL(t *testing.T, stores *state.Stores, namespace, name, sha256 string) {
	t.Helper()
	execSQL(t, stores, "UPDATE documents SET sha256 = ? WHERE namespace = ? AND name = ?",
		sha256, namespace, name)
}

// deleteDocumentSQL removes one documents row directly in MySQL.
func deleteDocumentSQL(t *testing.T, stores *state.Stores, namespace, name string) {
	t.Helper()
	execSQL(t, stores, "DELETE FROM documents WHERE namespace = ? AND name = ?", namespace, name)
}

// documentLocators builds the captured documents connector columns.
func documentLocators(namespace, name, documentID string) map[string]any {
	return map[string]any{
		"namespace":   namespace,
		"name":        name,
		"document_id": documentID,
		"del_flag":    false,
	}
}

// requireDocumentNames asserts the cached documents view for one namespace.
func requireDocumentNames(t *testing.T, backend *fakeStateCache, namespace string, want ...string) {
	t.Helper()
	names := documentResultNames(backend.value("documents", namespace))
	if len(names) != len(want) {
		t.Fatalf("cached documents(%s) = %v, want %v", namespace, names, want)
	}
	for index, name := range want {
		if names[index] != name {
			t.Fatalf("cached documents(%s) = %v, want %v", namespace, names, want)
		}
	}
}

// cachedDocumentSHA finds one cached documents view record by name.
func cachedDocumentSHA(backend *fakeStateCache, namespace, name string) (string, bool) {
	items, ok := backend.value("documents", namespace).([]any)
	if !ok {
		return "", false
	}
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok || record["name"] != name {
			continue
		}
		sha, _ := record["sha256"].(string)
		return sha, true
	}
	return "", false
}

// TestCacheChainKeysForChangeMapping pins the table→cache-key mapping of the
// cache consumer independently from the synchronizer.
func TestCacheChainKeysForChangeMapping(t *testing.T) {
	namespace := "acc-cache-map"
	keys := cacheconsumer.KeysForChange(cacheconsumer.Change{
		Table: "documents", Op: "u", After: map[string]any{"namespace": namespace},
	})
	if !containsCacheKey(keys, "kb_list", "") || !containsCacheKey(keys, "documents", namespace) {
		t.Fatalf("documents mapping = %+v", keys)
	}
	keys = cacheconsumer.KeysForChange(cacheconsumer.Change{
		Table: "knowledge_bases", Op: "r", After: map[string]any{"namespace": namespace},
	})
	for _, wanted := range []struct{ kind, scope string }{
		{"kb_list", ""}, {"kb_public", namespace}, {"documents", namespace},
	} {
		if !containsCacheKey(keys, wanted.kind, wanted.scope) {
			t.Fatalf("knowledge_bases mapping %+v is missing %s/%s", keys, wanted.kind, wanted.scope)
		}
	}
	keys = cacheconsumer.KeysForChange(cacheconsumer.Change{
		Table: "documents", Op: "d", Before: map[string]any{"namespace": namespace},
	})
	if !containsCacheKey(keys, "documents", namespace) {
		t.Fatalf("delete mapping = %+v", keys)
	}
	if keys := cacheconsumer.KeysForChange(cacheconsumer.Change{
		Table: "unrelated_table", Op: "u", After: map[string]any{"namespace": namespace},
	}); len(keys) != 0 {
		t.Fatalf("unknown table mapping = %+v, want none", keys)
	}
	if keys := cacheconsumer.KeysForChange(cacheconsumer.Change{
		Table: "documents", Op: "u", After: map[string]any{},
	}); len(keys) != 0 {
		t.Fatalf("identifier-less change mapping = %+v, want none", keys)
	}
}

// TestCacheChainDirectSQLConverges covers the manual-SQL acceptance row: after a
// direct INSERT/UPDATE/DELETE/rebuild of documents and knowledge_bases, the
// Debezium cache event rebuilds the affected read models with the new values.
func TestCacheChainDirectSQLConverges(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()
	backend := newFakeStateCache()
	stores.CacheSync.Backend = backend
	processor := newCacheProcessor(t, stores)
	namespace := uniqueNamespace(t, "acc-cache")
	createKnowledgeBase(t, stores, namespace, "acceptance-collection")
	// Converge the initial revisions so the assertions start from a clean cache.
	if err := stores.CacheSync.RefreshDirty(ctx, 1000); err != nil {
		t.Fatalf("RefreshDirty(baseline) error = %v", err)
	}
	requireDocumentNames(t, backend, namespace)

	// A row inserted directly into MySQL, announced by the connector.
	insertDocumentSQL(t, stores, namespace, "a.txt", "doc-a", "sha-old")
	if acked := deliver(t, processor.MessageHandler(), "praxis_cache_change",
		rawCacheEvent(t, "documents", "c", nil, documentLocators(namespace, "a.txt", "doc-a"))); !acked {
		t.Fatal("direct insert change was not acknowledged")
	}
	requireDocumentNames(t, backend, namespace, "a.txt")
	if item := knowledgeBaseListItem(backend.value("kb_list", ""), namespace); item == nil ||
		item["document_count"] != float64(1) {
		t.Fatalf("kb_list(%s) = %+v, want document_count 1", namespace, item)
	}
	if sha, _ := cachedDocumentSHA(backend, namespace, "a.txt"); sha != "sha-old" {
		t.Fatalf("cached sha = %q, want sha-old", sha)
	}

	// A direct UPDATE rebuilds the document view.
	updateDocumentSQL(t, stores, namespace, "a.txt", "sha-new")
	if acked := deliver(t, processor.MessageHandler(), "praxis_cache_change",
		rawCacheEvent(t, "documents", "u", nil, documentLocators(namespace, "a.txt", "doc-a"))); !acked {
		t.Fatal("direct update change was not acknowledged")
	}
	if sha, _ := cachedDocumentSHA(backend, namespace, "a.txt"); sha != "sha-new" {
		t.Fatalf("cached sha after update = %q, want sha-new", sha)
	}

	// A direct DELETE removes it from the view, and a same-name rebuild with new
	// content converges to the new row (no stale cached value).
	deleteDocumentSQL(t, stores, namespace, "a.txt")
	if acked := deliver(t, processor.MessageHandler(), "praxis_cache_change",
		rawCacheEvent(t, "documents", "d", documentLocators(namespace, "a.txt", "doc-a"), nil)); !acked {
		t.Fatal("delete change was not acknowledged")
	}
	requireDocumentNames(t, backend, namespace)
	insertDocumentSQL(t, stores, namespace, "a.txt", "doc-b", "sha-rebuilt")
	if acked := deliver(t, processor.MessageHandler(), "praxis_cache_change",
		rawCacheEvent(t, "documents", "c", nil, documentLocators(namespace, "a.txt", "doc-b"))); !acked {
		t.Fatal("rebuild change was not acknowledged")
	}
	requireDocumentNames(t, backend, namespace, "a.txt")
	if sha, _ := cachedDocumentSHA(backend, namespace, "a.txt"); sha != "sha-rebuilt" {
		t.Fatalf("cached sha after rebuild = %q, want sha-rebuilt", sha)
	}

	// A direct knowledge-base update invalidates the list, the public settings
	// and the documents view (which embeds the collection).
	execSQL(t, stores,
		"UPDATE knowledge_bases SET settings = ?, version = version + 1 WHERE namespace = ?",
		`{"collection":"updated-collection"}`, namespace)
	if acked := deliver(t, processor.MessageHandler(), "praxis_cache_change",
		rawCacheEvent(t, "knowledge_bases", "u", nil,
			map[string]any{"namespace": namespace, "version": 2})); !acked {
		t.Fatal("knowledge-base change was not acknowledged")
	}
	publicValue, ok := backend.value("kb_public", namespace).(map[string]any)
	if !ok || publicValue["collection"] != "updated-collection" {
		t.Fatalf("cached kb_public(%s) = %+v", namespace, backend.value("kb_public", namespace))
	}
	item := knowledgeBaseListItem(backend.value("kb_list", ""), namespace)
	if item == nil || item["collection"] != "updated-collection" {
		t.Fatalf("cached kb_list(%s) = %+v", namespace, item)
	}
	items, ok := backend.value("documents", namespace).([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("cached documents(%s) = %+v", namespace, backend.value("documents", namespace))
	}
	if record := items[0].(map[string]any); record["collection"] != "updated-collection" {
		t.Fatalf("cached document collection = %v, want updated-collection", record["collection"])
	}

	// The cache backend still has JSON-shaped values only (a serializing
	// backend cannot have stored live Go pointers).
	encoded, err := json.Marshal(backend.value("documents", namespace))
	if err != nil || len(encoded) == 0 {
		t.Fatalf("cached documents view is not JSON marshable: %v", err)
	}
}

// TestCacheChainRedisFailureRecordsErrorAndRecovers covers the compensation row:
// a failed snapshot publish is recorded on cache_revisions (attempts/last_error)
// and the periodic RefreshDirty sweep converges once the backend recovers. The
// backend watermark rejects a stale revision write.
func TestCacheChainRedisFailureRecordsErrorAndRecovers(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()
	backend := newFakeStateCache()
	stores.CacheSync.Backend = backend
	processor := newCacheProcessor(t, stores)
	namespace := uniqueNamespace(t, "acc-cache-fail")
	createKnowledgeBase(t, stores, namespace, "acceptance-collection")
	if err := stores.CacheSync.RefreshDirty(ctx, 1000); err != nil {
		t.Fatalf("RefreshDirty(baseline) error = %v", err)
	}
	insertDocumentSQL(t, stores, namespace, "fail.txt", "doc-fail", "sha-fail")

	// The next snapshot write fails (Redis down); the message is still ACKed
	// because the dirty revision is durable in MySQL.
	backend.failNextPuts(1)
	if acked := deliver(t, processor.MessageHandler(), "praxis_cache_change",
		rawCacheEvent(t, "documents", "c", nil, documentLocators(namespace, "fail.txt", "doc-fail"))); !acked {
		t.Fatal("cache message with a Redis write failure was not acknowledged")
	}
	var revision, refreshed, attempts int64
	var lastError string
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT revision, refreshed_revision, attempts, COALESCE(last_error, '') "+
			"FROM cache_revisions WHERE kind = ? AND scope = ?", "kb_list", "").
		Scan(&revision, &refreshed, &attempts, &lastError); err != nil {
		t.Fatalf("read kb_list revision error = %v", err)
	}
	if refreshed >= revision || attempts < 1 || lastError == "" {
		t.Fatalf("failed refresh row = revision %d refreshed %d attempts %d last_error %q",
			revision, refreshed, attempts, lastError)
	}
	dirty, err := stores.CacheSync.DirtyKeys(ctx, 100)
	if err != nil || !containsCacheKey(dirty, "kb_list", "") {
		t.Fatalf("DirtyKeys() = %+v, %v; want the failed kb_list key", dirty, err)
	}

	// Recover: the next compensation pass publishes the missing view.
	if err := stores.CacheSync.RefreshDirty(ctx, 1000); err != nil {
		t.Fatalf("RefreshDirty(recovery) error = %v", err)
	}
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT revision, refreshed_revision, attempts, COALESCE(last_error, '') "+
			"FROM cache_revisions WHERE kind = ? AND scope = ?", "kb_list", "").
		Scan(&revision, &refreshed, &attempts, &lastError); err != nil {
		t.Fatalf("re-read kb_list revision error = %v", err)
	}
	if refreshed != revision || lastError != "" {
		t.Fatalf("recovered row = revision %d refreshed %d last_error %q", revision, refreshed, lastError)
	}
	if item := knowledgeBaseListItem(backend.value("kb_list", ""), namespace); item == nil ||
		item["document_count"] != float64(1) {
		t.Fatalf("recovered kb_list(%s) = %+v", namespace, item)
	}

	// The backend watermark rejects a stale published revision.
	published, found := backend.revision("documents", namespace)
	if !found || published <= 0 {
		t.Fatalf("documents(%s) published revision = %d, found = %v", namespace, published, found)
	}
	if ok, err := backend.Put(ctx, "documents", namespace, published-1, []any{
		map[string]any{"name": "stale.txt"},
	}, false); err != nil || ok {
		t.Fatalf("stale Put() = %v, %v; want rejected by the watermark", ok, err)
	}
	requireDocumentNames(t, backend, namespace, "fail.txt")
}
