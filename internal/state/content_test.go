package state

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// countTable returns the number of rows in one table.
func countTable(t *testing.T, stores *Stores, table string) int {
	t.Helper()
	var count int
	if err := stores.DB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return count
}

// deleteOperationObjects removes one operation's staged objects through the real
// deletion path so payload retirement is exercised as in production.
func deleteOperationObjects(t *testing.T, stores *Stores, operationID string) {
	t.Helper()
	ctx := context.Background()
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := deleteUploadObjectsTx(ctx, transaction, operationID); err != nil {
		_ = transaction.Rollback()
		t.Fatalf("deleteUploadObjectsTx() error = %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("commit error = %v", err)
	}
}

// retiredAt returns the retired_at value of one payload.
func retiredAt(t *testing.T, stores *Stores, hash string) bool {
	t.Helper()
	var retired bool
	if err := stores.DB.QueryRowContext(context.Background(),
		"SELECT retired_at IS NOT NULL FROM content_objects WHERE hash = ?", hash).
		Scan(&retired); err != nil {
		t.Fatalf("reading retired_at: %v", err)
	}
	return retired
}

// TestContentObjectsDedupAcrossNamespaces verifies that equal bytes are stored
// once and that content rows survive until their retention window expires after
// the last reference disappears.
func TestContentObjectsDedupAcrossNamespaces(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	payload := []byte("the same payload uploaded by two tenants")
	payloadHash := sha256Sum(payload)

	first := beginTestUpload(t, journal, "dedupe-one", "a.txt", "doc-a", "collection", BeginOptions{})
	if err := journal.PutObject(ctx, first, SourceBucket, "a.txt", payload, nil); err != nil {
		t.Fatalf("PutObject(first) error = %v", err)
	}
	second := beginTestUpload(t, journal, "dedupe-two", "b.txt", "doc-b", "collection", BeginOptions{})
	if err := journal.PutObject(ctx, second, SourceBucket, "b.txt", payload, nil); err != nil {
		t.Fatalf("PutObject(second) error = %v", err)
	}
	if err := stores.Blobs.Put(ctx, "dedupe-three", SourceBucket, "c.txt", payload, nil); err != nil {
		t.Fatalf("Blobs.Put() error = %v", err)
	}
	if count := countTable(t, stores, "content_objects"); count != 1 {
		t.Fatalf("content_objects rows = %d, want 1 (payload must be stored once)", count)
	}
	for _, item := range []struct {
		version string
		bucket  string
		key     string
	}{
		{first.ID, SourceBucket, "a.txt"},
		{second.ID, SourceBucket, "b.txt"},
	} {
		content, found, err := journal.GetObject(ctx, item.version, item.bucket, item.key)
		if err != nil || !found || string(content) != string(payload) {
			t.Fatalf("GetObject(%s) = %q, %v, %v", item.key, content, found, err)
		}
	}
	if content, found, err := stores.Blobs.Get(ctx, "dedupe-three", SourceBucket, "c.txt"); err != nil || !found || string(content) != string(payload) {
		t.Fatalf("Blobs.Get() = %q, %v, %v", content, found, err)
	}

	// One removed reference does not retire a payload others still point at.
	deleteOperationObjects(t, stores, first.ID)
	if retiredAt(t, stores, payloadHash) {
		t.Fatal("payload retired while two references still exist")
	}
	if moved, err := stores.PurgeUnreferencedContent(ctx, 0, 100); err != nil || moved != 0 {
		t.Fatalf("PurgeUnreferencedContent() = %d, %v; want 0 while referenced", moved, err)
	}

	// Removing the remaining references retires it, but the retention window
	// keeps the bytes around.
	deleteOperationObjects(t, stores, second.ID)
	if err := stores.Blobs.DeleteKey(ctx, "dedupe-three", SourceBucket, "c.txt"); err != nil {
		t.Fatalf("Blobs.DeleteKey() error = %v", err)
	}
	if !retiredAt(t, stores, payloadHash) {
		t.Fatal("payload was not retired after its last reference disappeared")
	}
	if moved, err := stores.PurgeUnreferencedContent(ctx, ContentObjectRetentionSeconds, 100); err != nil || moved != 0 {
		t.Fatalf("PurgeUnreferencedContent() = %d, %v; want 0 inside the retention window", moved, err)
	}
	if count := countTable(t, stores, "content_objects"); count != 1 {
		t.Fatalf("content_objects rows inside retention = %d, want 1", count)
	}
	if moved, err := stores.PurgeUnreferencedContent(ctx, 0, 100); err != nil || moved != 1 {
		t.Fatalf("PurgeUnreferencedContent() = %d, %v; want 1 removed after retention", moved, err)
	}
	if count := countTable(t, stores, "content_objects"); count != 0 {
		t.Fatalf("content_objects rows after GC = %d, want 0", count)
	}
}

// TestContentRetirementRearmsOnNewReference verifies that reusing a retired
// payload clears retired_at so the GC does not delete a live object.
func TestContentRetirementRearmsOnNewReference(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	payload := []byte("payload that is referenced again")
	payloadHash := sha256Sum(payload)

	first := beginTestUpload(t, journal, "rearm-one", "a.txt", "doc-a", "collection", BeginOptions{})
	if err := journal.PutObject(ctx, first, SourceBucket, "a.txt", payload, nil); err != nil {
		t.Fatalf("PutObject(first) error = %v", err)
	}
	deleteOperationObjects(t, stores, first.ID)
	if !retiredAt(t, stores, payloadHash) {
		t.Fatal("payload was not retired")
	}

	second := beginTestUpload(t, journal, "rearm-two", "b.txt", "doc-b", "collection", BeginOptions{})
	if err := journal.PutObject(ctx, second, SourceBucket, "b.txt", payload, nil); err != nil {
		t.Fatalf("PutObject(second) error = %v", err)
	}
	if retiredAt(t, stores, payloadHash) {
		t.Fatal("a new reference did not re-arm the retired payload")
	}
	if moved, err := stores.PurgeUnreferencedContent(ctx, 0, 100); err != nil || moved != 0 {
		t.Fatalf("PurgeUnreferencedContent() = %d, %v; want 0 for a re-armed payload", moved, err)
	}
	if content, found, err := journal.GetObject(ctx, second.ID, SourceBucket, "b.txt"); err != nil || !found || string(content) != string(payload) {
		t.Fatalf("GetObject(second) = %q, %v, %v", content, found, err)
	}
}

// TestContentObjectsGCKeepsReferencedRows verifies that the GC never removes a
// payload another reference row still points at.
func TestContentObjectsGCKeepsReferencedRows(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	payload := []byte("payload shared by two uploads")
	payloadHash := sha256Sum(payload)

	first := beginTestUpload(t, journal, "gc-one", "a.txt", "doc-a", "collection", BeginOptions{})
	if err := journal.PutObject(ctx, first, SourceBucket, "a.txt", payload, nil); err != nil {
		t.Fatalf("PutObject(first) error = %v", err)
	}
	second := beginTestUpload(t, journal, "gc-two", "b.txt", "doc-b", "collection", BeginOptions{})
	if err := journal.PutObject(ctx, second, SourceBucket, "b.txt", payload, nil); err != nil {
		t.Fatalf("PutObject(second) error = %v", err)
	}
	deleteOperationObjects(t, stores, first.ID)
	if retiredAt(t, stores, payloadHash) {
		t.Fatal("payload retired although the second reference still exists")
	}
	if moved, err := stores.PurgeUnreferencedContent(ctx, 0, 100); err != nil || moved != 0 {
		t.Fatalf("PurgeUnreferencedContent() = %d, %v; want 0 removed while referenced", moved, err)
	}
	if count := countTable(t, stores, "content_objects"); count != 1 {
		t.Fatalf("content_objects rows = %d, want 1", count)
	}
	if content, found, err := journal.GetObject(ctx, second.ID, SourceBucket, "b.txt"); err != nil || !found || string(content) != string(payload) {
		t.Fatalf("GetObject(second) = %q, %v, %v", content, found, err)
	}
	deleteOperationObjects(t, stores, second.ID)
	if moved, err := stores.PurgeUnreferencedContent(ctx, 0, 100); err != nil || moved != 1 {
		t.Fatalf("PurgeUnreferencedContent() = %d, %v; want 1 removed after the last reference", moved, err)
	}
}

// TestContentBackfillMovesLegacyRows verifies the bounded migration that moves
// inline bytes of pre-content_objects rows into the shared store.
func TestContentBackfillMovesLegacyRows(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	legacy := []byte("legacy inline bytes")
	legacyHash := sha256Sum(legacy)
	operationID := "0123456789abcdef0123456789abcdef"
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO upload_objects (operation_id, bucket, `key`, content, metadata, created_at) VALUES (?, ?, ?, ?, NULL, ?)",
		operationID, SourceBucket, "legacy.txt", legacy, unixSeconds()); err != nil {
		t.Fatalf("inserting legacy upload object: %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO blobs (namespace, bucket, `key`, document_id, content, size, sha256, created_at) VALUES (?, ?, ?, NULL, ?, ?, ?, ?)",
		"legacy-ns", SourceBucket, "legacy-blob.txt", legacy, int64(len(legacy)), legacyHash, nowISO()); err != nil {
		t.Fatalf("inserting legacy blob: %v", err)
	}
	if moved, err := stores.BackfillContentObjects(ctx, 100); err != nil || moved != 2 {
		t.Fatalf("BackfillContentObjects() = %d, %v; want 2 moved", moved, err)
	}
	if count := countTable(t, stores, "content_objects"); count != 1 {
		t.Fatalf("content_objects rows = %d, want 1 (legacy payloads share one row)", count)
	}
	var hash string
	var contentLength int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT content_hash, OCTET_LENGTH(content) FROM upload_objects WHERE operation_id = ?", operationID).
		Scan(&hash, &contentLength); err != nil {
		t.Fatalf("reading migrated upload object: %v", err)
	}
	if hash != legacyHash || contentLength != 0 {
		t.Fatalf("upload object after backfill = hash %q, inline bytes %d", hash, contentLength)
	}
	journal := newTestJournal(t, stores)
	content, found, err := journal.GetObject(ctx, operationID, SourceBucket, "legacy.txt")
	if err != nil || !found || string(content) != string(legacy) {
		t.Fatalf("GetObject(legacy) = %q, %v, %v", content, found, err)
	}
	blob, found, err := stores.Blobs.Get(ctx, "legacy-ns", SourceBucket, "legacy-blob.txt")
	if err != nil || !found || string(blob) != string(legacy) {
		t.Fatalf("Blobs.Get(legacy) = %q, %v, %v", blob, found, err)
	}
	// The backfill is idempotent.
	if moved, err := stores.BackfillContentObjects(ctx, 100); err != nil || moved != 0 {
		t.Fatalf("second BackfillContentObjects() = %d, %v; want 0", moved, err)
	}
}

// TestKnowledgeBasePurgeBatchDeletesNamespaceRows verifies the single-statement
// batched purge: every namespace-scoped row is removed for both namespaces while
// payloads shared with a surviving namespace are kept.
func TestKnowledgeBasePurgeBatchDeletesNamespaceRows(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	payload := []byte("payload shared by alpha and beta")

	documentIDForName := func(name string) string { return sha256Sum([]byte(name))[:32] }
	insertKnowledgeBase := func(namespace, collection string) {
		if _, err := stores.DB.ExecContext(ctx,
			"INSERT INTO knowledge_bases (namespace, collection, settings, version, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
			namespace, collection, `{}`, 1, nowISO(), nowISO()); err != nil {
			t.Fatalf("inserting knowledge base %s: %v", namespace, err)
		}
	}
	insertDocument := func(namespace, name string) {
		if _, err := stores.DB.ExecContext(ctx,
			"INSERT INTO documents (namespace, name, document_id, size, sha256, chunking, collection, chunk_count, "+
				"modified_at, indexed_at, diagnostics, skipped_object_count, del_flag) VALUES (?, ?, ?, 0, '', '{}', 'c', 0, '', '', '{}', 0, 0)",
			namespace, name, documentIDForName(name)); err != nil {
			t.Fatalf("inserting document %s/%s: %v", namespace, name, err)
		}
	}

	insertKnowledgeBase("alpha", "collection-alpha")
	insertKnowledgeBase("beta", "collection-beta")
	insertKnowledgeBase("gamma", "collection-gamma")
	insertDocument("alpha", "a.txt")
	insertDocument("alpha", "a2.txt")
	insertDocument("beta", "b.txt")
	insertDocument("gamma", "g.txt")

	alphaOperation := beginTestUpload(t, journal, "alpha", "a.txt", documentIDForName("a.txt"), "collection-alpha", BeginOptions{})
	if err := journal.PutObject(ctx, alphaOperation, SourceBucket, "a.txt", payload, nil); err != nil {
		t.Fatalf("PutObject(alpha) error = %v", err)
	}
	betaOperation := beginTestUpload(t, journal, "beta", "b.txt", documentIDForName("b.txt"), "collection-beta", BeginOptions{})
	if err := journal.PutObject(ctx, betaOperation, SourceBucket, "b.txt", payload, nil); err != nil {
		t.Fatalf("PutObject(beta) error = %v", err)
	}
	// The purge refuses namespaces with a live upload lease; expire the leases
	// these test operations hold so the batch purge proceeds.
	expireUploadLeases(t, stores, alphaOperation.ID)
	expireUploadLeases(t, stores, betaOperation.ID)
	if err := stores.Blobs.Put(ctx, "alpha", PDFAssetBucket, documentIDForName("a.txt")+"/document.md", payload, nil); err != nil {
		t.Fatalf("Blobs.Put(alpha) error = %v", err)
	}
	if err := stores.Blobs.Put(ctx, "gamma", SourceBucket, "g.txt", payload, nil); err != nil {
		t.Fatalf("Blobs.Put(gamma) error = %v", err)
	}
	// A payload only alpha references must be retired (not deleted) by the purge.
	alphaOnly := []byte("payload referenced only by alpha")
	alphaOnlyHash := sha256Sum(alphaOnly)
	if err := stores.Blobs.Put(ctx, "alpha", SourceBucket, "only-alpha.bin", alphaOnly, nil); err != nil {
		t.Fatalf("Blobs.Put(alpha only) error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO kb_permissions (id, namespace, grantee_user_id, document_name, permission, granted_by, created_at, updated_at) "+
			"VALUES (?, ?, ?, '', 'read', ?, ?, ?)",
		"perm-alpha", "alpha", "user-1", "admin-1", nowISO(), nowISO()); err != nil {
		t.Fatalf("inserting permission: %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO mcp_catalogs (namespace, data, version, updated_at) VALUES (?, ?, ?, ?)",
		"alpha", `{}`, 1, nowISO()); err != nil {
		t.Fatalf("inserting mcp catalog: %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO document_heads (namespace, document_id, version, updated_at) VALUES (?, ?, ?, ?)",
		"alpha", documentIDForName("a.txt"), alphaOperation.ID, unixSeconds()); err != nil {
		t.Fatalf("inserting document head: %v", err)
	}

	report, err := stores.KnowledgeBases.Purge(ctx, []string{"alpha", "beta", "missing"})
	if err != nil {
		t.Fatalf("Purge() error = %v", err)
	}
	if !report.Present["alpha"] || !report.Present["beta"] || report.Present["missing"] {
		t.Fatalf("Purge() present = %v", report.Present)
	}
	if report.DocumentsDeleted != 3 || report.DocumentsByNamespace["alpha"] != 2 || report.DocumentsByNamespace["beta"] != 1 {
		t.Fatalf("Purge() documents = %d, by namespace %v", report.DocumentsDeleted, report.DocumentsByNamespace)
	}
	if report.ConfigurationsDeleted != 2 {
		t.Fatalf("Purge() configurations = %d, want 2", report.ConfigurationsDeleted)
	}
	if len(report.Collections) != 2 || report.Collections["alpha"] != "collection-alpha" {
		t.Fatalf("Purge() collections = %v", report.Collections)
	}
	for _, namespace := range []string{"alpha", "beta"} {
		for _, table := range []string{"knowledge_bases", "document_heads", "kb_permissions", "mcp_catalogs", "blobs"} {
			var count int
			if err := stores.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE namespace = ?", namespace).Scan(&count); err != nil {
				t.Fatalf("counting %s: %v", table, err)
			}
			if count != 0 {
				t.Fatalf("%s still has %d rows for %s", table, count, namespace)
			}
		}
		var uploadCount int
		if err := stores.DB.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM upload_operations WHERE namespace = ?", namespace).Scan(&uploadCount); err != nil {
			t.Fatalf("counting uploads: %v", err)
		}
		if uploadCount != 0 {
			t.Fatalf("upload_operations still has %d rows for %s", uploadCount, namespace)
		}
		var deleted int
		if err := stores.DB.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM documents WHERE namespace = ? AND del_flag = 1", namespace).Scan(&deleted); err != nil {
			t.Fatalf("counting deleted documents: %v", err)
		}
		if deleted == 0 {
			t.Fatalf("documents for %s were not soft-deleted", namespace)
		}
	}
	if count := countTable(t, stores, "content_objects"); count != 2 {
		t.Fatalf("content_objects rows = %d, want 2 (shared + alpha-only payload)", count)
	}
	if retiredAt(t, stores, sha256Sum(payload)) {
		t.Fatal("shared payload retired although gamma still references it")
	}
	if !retiredAt(t, stores, alphaOnlyHash) {
		t.Fatal("alpha-only payload was not retired by the purge")
	}
	if moved, err := stores.PurgeUnreferencedContent(ctx, ContentObjectRetentionSeconds, 100); err != nil || moved != 0 {
		t.Fatalf("PurgeUnreferencedContent() = %d, %v; want 0 inside the retention window", moved, err)
	}
	if moved, err := stores.PurgeUnreferencedContent(ctx, 0, 100); err != nil || moved != 1 {
		t.Fatalf("PurgeUnreferencedContent() = %d, %v; want the alpha-only payload removed", moved, err)
	}
	if count := countTable(t, stores, "content_objects"); count != 1 {
		t.Fatalf("content_objects rows after GC = %d, want 1 (shared payload)", count)
	}
	if count := countTable(t, stores, "knowledge_bases"); count != 1 {
		t.Fatalf("knowledge_bases rows = %d, want 1 (gamma untouched)", count)
	}
	if revision(t, stores, "kb_list", "") < 1 {
		t.Fatal("batch purge did not bump the knowledge-base list revision")
	}
}

// TestKnowledgeBasePurgeSkipsBusyNamespace verifies that an active upload lease
// blocks a purge instead of deleting the namespace underneath the upload.
func TestKnowledgeBasePurgeSkipsBusyNamespace(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO knowledge_bases (namespace, collection, settings, version, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		"busy", "collection-busy", `{}`, 1, nowISO(), nowISO()); err != nil {
		t.Fatalf("inserting knowledge base: %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO document_leases (namespace, resource, owner, operation_id, expires_at) VALUES (?, ?, ?, NULL, ?)",
		"busy", UploadNamespaceResource, "owner", unixSeconds()+60); err != nil {
		t.Fatalf("inserting lease: %v", err)
	}
	report, err := stores.KnowledgeBases.Purge(ctx, []string{"busy"})
	if err != nil {
		t.Fatalf("Purge() error = %v", err)
	}
	if !report.Busy["busy"] {
		t.Fatalf("Purge() busy = %v, want busy", report.Busy)
	}
	if report.Present["busy"] || report.ConfigurationsDeleted != 0 {
		t.Fatalf("Purge() touched a busy namespace: %+v", report)
	}
	if count := countTable(t, stores, "knowledge_bases"); count != 1 {
		t.Fatalf("knowledge_bases rows = %d, want 1", count)
	}
}

// TestKnowledgeBasePurgeBatchScalesWithDocuments documents that a purge of one
// namespace costs one transaction no matter how many documents it owns: 300
// documents, operations and references are removed in one batched purge.
func TestKnowledgeBasePurgeBatchScalesWithDocuments(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	const documents = 300
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO knowledge_bases (namespace, collection, settings, version, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		"scale", "collection-scale", `{}`, 1, nowISO(), nowISO()); err != nil {
		t.Fatalf("inserting knowledge base: %v", err)
	}
	payload := []byte("one payload shared by every scaled document")
	if err := stores.Blobs.Put(ctx, "scale", SourceBucket, "shared.bin", payload, nil); err != nil {
		t.Fatalf("Blobs.Put() error = %v", err)
	}
	contentHash := sha256Sum(payload)
	for index := 0; index < documents; index++ {
		operationID := fmt.Sprintf("%032d", index)
		if _, err := stores.DB.ExecContext(ctx,
			"INSERT INTO upload_operations (id, namespace, name, document_id, collection, owner, status, cleanup_pending, created_at, updated_at) "+
				"VALUES (?, 'scale', ?, ?, 'collection-scale', 'owner', 'COMMITTED', 0, ?, ?)",
			operationID, fmt.Sprintf("doc-%d.txt", index), operationID, unixSeconds(), unixSeconds()); err != nil {
			t.Fatalf("inserting upload operation %d: %v", index, err)
		}
		if _, err := stores.DB.ExecContext(ctx,
			"INSERT INTO upload_objects (operation_id, bucket, `key`, content, content_hash, metadata, created_at) "+
				"VALUES (?, ?, ?, ?, ?, NULL, ?)",
			operationID, SourceBucket, fmt.Sprintf("doc-%d.txt", index), []byte{}, contentHash, unixSeconds()); err != nil {
			t.Fatalf("inserting upload object %d: %v", index, err)
		}
		if _, err := stores.DB.ExecContext(ctx,
			"INSERT INTO documents (namespace, name, document_id, size, sha256, chunking, collection, chunk_count, "+
				"modified_at, indexed_at, diagnostics, skipped_object_count, del_flag) "+
				"VALUES ('scale', ?, ?, ?, ?, '{}', 'collection-scale', 1, '', '', '{}', 0, 0)",
			fmt.Sprintf("doc-%d.txt", index), operationID, int64(len(payload)), contentHash); err != nil {
			t.Fatalf("inserting document %d: %v", index, err)
		}
	}
	started := time.Now()
	report, err := stores.KnowledgeBases.Purge(ctx, []string{"scale"})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Purge() error = %v", err)
	}
	if report.DocumentsDeleted != documents {
		t.Fatalf("Purge() documents = %d, want %d", report.DocumentsDeleted, documents)
	}
	if count := countTable(t, stores, "upload_operations"); count != 0 {
		t.Fatalf("upload_operations rows = %d, want 0", count)
	}
	// The payload is retired by the purge, not deleted: the retention window
	// keeps it recoverable, and the GC removes it once the window has passed.
	if count := countTable(t, stores, "content_objects"); count != 1 {
		t.Fatalf("content_objects rows = %d, want 1 (retired, not deleted)", count)
	}
	if !retiredAt(t, stores, sha256Sum([]byte("one payload shared by every scaled document"))) {
		t.Fatal("purged payload was not retired")
	}
	if moved, err := stores.PurgeUnreferencedContent(ctx, ContentObjectRetentionSeconds, 100); err != nil || moved != 0 {
		t.Fatalf("PurgeUnreferencedContent() = %d, %v; want 0 inside the retention window", moved, err)
	}
	if moved, err := stores.PurgeUnreferencedContent(ctx, 0, 100); err != nil || moved != 1 {
		t.Fatalf("PurgeUnreferencedContent() = %d, %v; want 1 removed after retention", moved, err)
	}
	if count := countTable(t, stores, "content_objects"); count != 0 {
		t.Fatalf("content_objects rows after GC = %d, want 0", count)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("batched purge of %d documents took %s; it must stay one transaction", documents, elapsed)
	}
	t.Logf("batched purge removed %d documents in %s", documents, elapsed)
}
