package state

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func activeKnowledgeBaseSettings() map[string]any {
	return map[string]any{
		"namespace": "alpha", "collection": "active-index", "embedding_model": "active-model",
		"reranker_model": "", "vector_size": 2, "tokenizer": "test-tokenizer",
		"embedding": map[string]any{"base_url": "https://provider.example", "protected_key": "secret-provider-key"},
		"reranker":  map[string]any{"base_url": "", "protected_key": ""},
		"strategy":  "character", "chunk_size": 120, "chunk_overlap": 20,
	}
}

func viewDocumentRecord(name string) DocumentRecord {
	return DocumentRecord{
		Name: name, DocumentID: "doc-" + name, Collection: "old-index", ChunkCount: 3,
		Chunking: map[string]any{"strategy": "character", "embedding_model": "old-model"},
	}
}

func cachedEntry(t *testing.T, cache *memoryCache, kind, scope string) any {
	t.Helper()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.entries[CacheKey{Kind: kind, Scope: scope}]
}

func TestPublicSettingsRedactsProviderCredentials(t *testing.T) {
	settings := activeKnowledgeBaseSettings()
	public := PublicSettings(settings)
	embedding := public["embedding"].(map[string]any)
	if embedding["base_url"] != "https://provider.example" || embedding["api_key_set"] != true {
		t.Fatalf("public embedding = %+v", embedding)
	}
	if _, leaked := embedding["protected_key"]; leaked {
		t.Fatal("public settings expose protected_key")
	}
	if public["collection"] != "active-index" || public["chunk_size"] != 120 {
		t.Fatalf("public settings lost non-credential values: %+v", public)
	}
	if _, leaked := public["reranker"].(map[string]any)["protected_key"]; leaked {
		t.Fatal("public reranker exposes protected_key")
	}
}

func TestMissingViewsAreCachedPerNamespace(t *testing.T) {
	stores, cache := newCacheTestStores(t)
	ctx := context.Background()
	if value, err := stores.Views.KnowledgeBase(ctx, "absent"); err != nil || value != nil {
		t.Fatalf("KnowledgeBase(absent) = %+v, %v", value, err)
	}
	if value, found, err := stores.Views.Catalog(ctx, "absent"); err != nil || found || value != "" {
		t.Fatalf("Catalog(absent) = %q, %v, %v", value, found, err)
	}
	if documents, err := stores.Views.Documents(ctx, "absent"); err != nil || len(documents) != 0 {
		t.Fatalf("Documents(absent) = %v, %v", documents, err)
	}
	// A raw insert bypasses the repositories (and their revision touch) so the
	// next read proves the missing value was cached.
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO knowledge_bases (namespace, collection, settings, version, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?)", "absent", "c", `{"namespace":"absent"}`, 1, nowISO(), nowISO()); err != nil {
		t.Fatalf("raw insert error = %v", err)
	}
	if value, err := stores.Views.KnowledgeBase(ctx, "absent"); err != nil || value != nil {
		t.Fatalf("KnowledgeBase(absent) after raw insert = %+v, %v", value, err)
	}
	if documents, err := stores.Views.Documents(ctx, "absent"); err != nil || len(documents) != 0 {
		t.Fatalf("Documents(absent) after raw insert = %v, %v", documents, err)
	}
	if err := stores.KnowledgeBases.Create(ctx, map[string]any{"namespace": "other", "collection": "c"}); err != nil {
		t.Fatalf("Create(other) error = %v", err)
	}
	if value, err := stores.Views.KnowledgeBase(ctx, "other"); err != nil || value["namespace"] != "other" {
		t.Fatalf("KnowledgeBase(other) = %+v, %v", value, err)
	}
	if entry := cachedEntry(t, cache, "kb_public", "absent"); entry != nil {
		t.Fatalf("missing view cache entry = %#v, want nil", entry)
	}
}

func TestKnowledgeBaseListMergesConfigurationAndDocumentCounts(t *testing.T) {
	stores, cache := newCacheTestStores(t)
	ctx := context.Background()
	for _, namespace := range []string{"zulu", "alpha", "empty"} {
		settings := activeKnowledgeBaseSettings()
		settings["namespace"] = namespace
		if err := stores.KnowledgeBases.Create(ctx, settings); err != nil {
			t.Fatalf("Create(%q) error = %v", namespace, err)
		}
	}
	for _, namespace := range []string{"alpha", "legacy"} {
		if err := stores.Documents.Upsert(ctx, namespace, viewDocumentRecord("document.txt")); err != nil {
			t.Fatalf("Upsert(%q) error = %v", namespace, err)
		}
	}
	if err := stores.Documents.Upsert(ctx, "legacy", viewDocumentRecord("second.txt")); err != nil {
		t.Fatalf("Upsert(legacy) error = %v", err)
	}
	items, err := stores.Views.KnowledgeBases(ctx)
	if err != nil {
		t.Fatalf("KnowledgeBases() error = %v", err)
	}
	type summary struct {
		namespace  string
		configured bool
		count      int
	}
	summaries := make([]summary, 0, len(items))
	for _, item := range items {
		namespace, _ := item["namespace"].(string)
		configured, _ := item["configured"].(bool)
		count, _ := item["document_count"].(float64)
		summaries = append(summaries, summary{namespace: namespace, configured: configured, count: int(count)})
	}
	expected := []summary{{"alpha", true, 1}, {"empty", true, 0}, {"legacy", false, 2}, {"zulu", true, 0}}
	if len(summaries) != len(expected) {
		t.Fatalf("KnowledgeBases() = %+v", summaries)
	}
	for index, want := range expected {
		if summaries[index] != want {
			t.Fatalf("KnowledgeBases()[%d] = %+v, want %+v", index, summaries[index], want)
		}
	}
	payload := fmt.Sprintf("%v", cachedEntry(t, cache, "kb_list", ""))
	if strings.Contains(payload, "secret-provider-key") {
		t.Fatalf("kb_list cache leaked a credential: %s", payload)
	}
}

func TestDocumentViewOverlaysActiveConfigAndPreservesRepositoryRecord(t *testing.T) {
	stores, cache := newCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, activeKnowledgeBaseSettings()); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	for _, name := range []string{"zulu.txt", "Alpha.txt"} {
		if err := stores.Documents.Upsert(ctx, "alpha", viewDocumentRecord(name)); err != nil {
			t.Fatalf("Upsert(%q) error = %v", name, err)
		}
	}
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO document_heads (namespace, document_id, version, updated_at) VALUES (?, ?, ?, ?)",
		"alpha", "doc-Alpha.txt", "published-version", 1.0); err != nil {
		t.Fatalf("document_heads insert error = %v", err)
	}
	cache.mu.Lock()
	delete(cache.entries, CacheKey{Kind: "documents", Scope: "alpha"})
	cache.mu.Unlock()
	records, err := stores.Views.Documents(ctx, "alpha")
	if err != nil {
		t.Fatalf("Documents() error = %v", err)
	}
	if len(records) != 2 || records[0]["name"] != "Alpha.txt" || records[1]["name"] != "zulu.txt" {
		t.Fatalf("Documents() order = %+v", records)
	}
	if records[0]["operation_id"] != "published-version" {
		t.Fatalf("operation_id = %v", records[0]["operation_id"])
	}
	for _, record := range records {
		if record["collection"] != "active-index" {
			t.Fatalf("collection = %v, want the active configuration", record["collection"])
		}
		chunking := record["chunking"].(map[string]any)
		if chunking["embedding_model"] != "active-model" || chunking["strategy"] != "character" {
			t.Fatalf("chunking overlay = %+v", chunking)
		}
	}
	repositoryRecords, err := stores.Documents.List(ctx, "alpha")
	if err != nil || len(repositoryRecords) == 0 || repositoryRecords[0].Collection != "old-index" {
		t.Fatalf("repository records were mutated: %+v, %v", repositoryRecords, err)
	}
	if err := stores.Documents.Upsert(ctx, "legacy", viewDocumentRecord("legacy.txt")); err != nil {
		t.Fatalf("Upsert(legacy) error = %v", err)
	}
	legacy, err := stores.Views.Documents(ctx, "legacy")
	if err != nil || len(legacy) != 1 || legacy[0]["collection"] != "old-index" {
		t.Fatalf("Documents(legacy) = %+v, %v", legacy, err)
	}
}

func TestRegisteredRefreshLoadersRedactSecrets(t *testing.T) {
	stores, cache := newCacheTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, activeKnowledgeBaseSettings()); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := stores.Documents.Upsert(ctx, "alpha", viewDocumentRecord("document.txt")); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	cache.mu.Lock()
	cache.entries = map[CacheKey]any{}
	cache.mu.Unlock()
	for kind, scope := range map[string]string{
		"kb_public": "alpha", "kb_list": "", "documents": "alpha", "catalog": "alpha",
	} {
		loader, ok := stores.CacheSync.Loader(kind)
		if !ok {
			t.Fatalf("loader %q is not registered", kind)
		}
		value, err := loader(ctx, stores.DB, scope)
		if err != nil {
			t.Fatalf("loader(%q) error = %v", kind, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("loader(%q) value is not JSON-safe: %v", kind, err)
		}
		if strings.Contains(string(encoded), "secret-provider-key") ||
			strings.Contains(string(encoded), "protected_key") {
			t.Fatalf("loader(%q) leaked a credential: %s", kind, encoded)
		}
	}
	cache.mu.Lock()
	entries := len(cache.entries)
	cache.mu.Unlock()
	if entries != 0 {
		t.Fatalf("direct loader calls populated %d cache entries", entries)
	}
}

func TestUploadReadViews(t *testing.T) {
	stores, cache := newCacheTestStores(t)
	ctx := context.Background()
	now := unixSeconds()
	ctxInsert := "INSERT INTO upload_operations (id, namespace, name, document_id, collection, " +
		"idempotency_key, fingerprint, owner, old_version, status, result, error, cleanup_pending, " +
		"workspace, workspace_node, created_at, updated_at) VALUES (?, ?, ?, ?, ?, NULL, NULL, ?, NULL, ?, NULL, NULL, 0, ?, NULL, ?, ?)"
	if _, err := stores.DB.ExecContext(ctx, ctxInsert,
		"op-1", "alpha", "file.txt", "doc", "collection", "secret-owner", "RUNNING", "sensitive-local-path", 1.0, 2.0); err != nil {
		t.Fatalf("upload_operations insert error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO upload_steps (operation_id, name, status, details, error, started_at, finished_at) VALUES (?, ?, ?, ?, NULL, ?, ?)",
		"op-1", "parse", "DONE", `{"sensitive_detail":"private-content"}`, 1.0, 2.0); err != nil {
		t.Fatalf("upload_steps insert error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO document_leases (namespace, resource, owner, operation_id, expires_at) VALUES (?, ?, ?, ?, ?)",
		"alpha", "namespace:exclusive", "secret-holder", "op-1", now+100); err != nil {
		t.Fatalf("document_leases insert error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO document_leases (namespace, resource, owner, operation_id, expires_at) VALUES (?, ?, ?, NULL, ?)",
		"alpha", "namespace:guard", "secret-holder", now+100); err != nil {
		t.Fatalf("document_leases guard insert error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO document_heads (namespace, document_id, version, updated_at) VALUES (?, ?, ?, ?)",
		"alpha", "doc", "op-1", 1.0); err != nil {
		t.Fatalf("document_heads insert error = %v", err)
	}

	detail, err := stores.Views.UploadOperation(ctx, "alpha", "op-1")
	if err != nil || detail == nil {
		t.Fatalf("UploadOperation() = %+v, %v", detail, err)
	}
	if detail["status"] != "RUNNING" || detail["holder_id"] == nil {
		t.Fatalf("unexpected operation detail: %+v", detail)
	}
	steps := detail["steps"].([]any)
	if len(steps) != 1 || steps[0].(map[string]any)["name"] != "parse" ||
		steps[0].(map[string]any)["status"] != "DONE" {
		t.Fatalf("steps = %+v", steps)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal error = %v", err)
	}
	for _, secret := range []string{"secret-owner", "sensitive-local-path", "private-content"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("upload detail leaked %q: %s", secret, encoded)
		}
	}
	if value, err := stores.Views.UploadOperation(ctx, "beta", "op-1"); err != nil || value != nil {
		t.Fatalf("UploadOperation(beta) = %+v, %v", value, err)
	}
	if operations, err := stores.Views.UploadOperations(ctx, "beta"); err != nil || len(operations) != 0 {
		t.Fatalf("UploadOperations(beta) = %v, %v", operations, err)
	}
	operations, err := stores.Views.UploadOperations(ctx, "alpha")
	if err != nil || len(operations) != 1 || operations[0]["id"] != "op-1" {
		t.Fatalf("UploadOperations(alpha) = %+v, %v", operations, err)
	}
	versions, err := stores.Views.PublishedVersions(ctx, "alpha")
	if err != nil || versions["doc"] != "op-1" {
		t.Fatalf("PublishedVersions() = %v, %v", versions, err)
	}
	leases, err := stores.Views.Leases(ctx, "alpha")
	if err != nil || len(leases) != 1 {
		t.Fatalf("Leases() = %+v, %v", leases, err)
	}
	if leases[0]["resource"] != "namespace:exclusive" || leases[0]["expired"] != false {
		t.Fatalf("lease = %+v", leases[0])
	}
	if strings.Contains(fmt.Sprintf("%v", leases[0]), "secret-holder") {
		t.Fatalf("lease view leaked the owner: %+v", leases[0])
	}

	// Deleting the underlying rows must not change already-cached display views.
	for _, statement := range []string{
		"DELETE FROM upload_steps", "DELETE FROM upload_operations",
		"DELETE FROM document_leases", "DELETE FROM document_heads",
	} {
		if _, err := stores.DB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s error = %v", statement, err)
		}
	}
	if detail, err := stores.Views.UploadOperation(ctx, "alpha", "op-1"); err != nil || detail == nil {
		t.Fatalf("cached UploadOperation() = %+v, %v", detail, err)
	}
	if operations, err := stores.Views.UploadOperations(ctx, "alpha"); err != nil || len(operations) != 1 {
		t.Fatalf("cached UploadOperations() = %v, %v", operations, err)
	}
	if leases, err := stores.Views.Leases(ctx, "alpha"); err != nil || len(leases) != 1 {
		t.Fatalf("cached Leases() = %+v, %v", leases, err)
	}
	cache.mu.Lock()
	entries := fmt.Sprintf("%v", cache.entries)
	cache.mu.Unlock()
	for _, secret := range []string{"secret-owner", "sensitive-local-path", "private-content"} {
		if strings.Contains(entries, secret) {
			t.Fatalf("cache leaked %q: %s", secret, entries)
		}
	}
}

func TestLeaseExpiryRecalculatedWithoutCacheWrite(t *testing.T) {
	stores, cache := newCacheTestStores(t)
	ctx := context.Background()
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO document_leases (namespace, resource, owner, operation_id, expires_at) VALUES (?, ?, ?, NULL, ?)",
		"alpha", "resource:one", "holder", unixSeconds()-1); err != nil {
		t.Fatalf("document_leases insert error = %v", err)
	}
	leases, err := stores.Views.Leases(ctx, "alpha")
	if err != nil || len(leases) != 1 || leases[0]["expired"] != true {
		t.Fatalf("Leases() = %+v, %v", leases, err)
	}
	// Mutating the cached payload only changes the display recalculation.
	cache.mu.Lock()
	entry := cache.entries[CacheKey{Kind: "leases", Scope: "alpha"}].([]any)
	entry[0].(map[string]any)["expires_at"] = unixSeconds() + 1000
	cache.mu.Unlock()
	leases, err = stores.Views.Leases(ctx, "alpha")
	if err != nil || len(leases) != 1 || leases[0]["expired"] != false {
		t.Fatalf("Leases() after cache mutation = %+v, %v", leases, err)
	}
	cache.mu.Lock()
	stored := cache.entries[CacheKey{Kind: "leases", Scope: "alpha"}].([]any)[0].(map[string]any)
	cache.mu.Unlock()
	if _, written := stored["expired"]; written {
		t.Fatal("Leases() wrote the computed expiry back into the cache")
	}
}
