package state

// MySQL round-trip tests for the SAG event-entity index (retrieval mode
// sag). They follow the same RAG_TEST_MYSQL_URL opt-in as every other MySQL
// test in this package.
import (
	"context"
	"testing"
)

func TestSagRepositoryRoundTrip(t *testing.T) {
	stores := openMySQLTestStores(t)
	ctx := context.Background()

	suffix, err := randomHex(6)
	if err != nil {
		t.Fatalf("randomHex() error = %v", err)
	}
	namespace := "go-test-" + suffix
	documentID := "doc-" + suffix
	t.Cleanup(func() {
		_ = stores.SagIndex.DeleteDocument(ctx, namespace, documentID)
		_ = cleanupMySQLNamespace(t, stores, namespace)
	})

	// Two chunks sharing one entity ("atlas") plus one private entity each.
	extractions := []SagExtraction{
		{
			ChunkID:  "chunk-1",
			Event:    "Project Atlas started in 2024.",
			Entities: []string{"Atlas", "2024"},
		},
		{
			ChunkID:  "chunk-2",
			Event:    "Atlas released version 2.",
			Entities: []string{"atlas", "Version 2"},
		},
	}
	if err := stores.SagIndex.ReplaceDocument(ctx, namespace, documentID, "op-1", extractions); err != nil {
		t.Fatalf("ReplaceDocument() error = %v", err)
	}

	// Replacing the same document must be idempotent: the previous events
	// are deleted, not duplicated.
	if err := stores.SagIndex.ReplaceDocument(ctx, namespace, documentID, "op-2", extractions); err != nil {
		t.Fatalf("ReplaceDocument(republish) error = %v", err)
	}

	events, err := stores.SagIndex.EventsByChunkIDs(ctx, namespace, []string{"chunk-1", "chunk-2"})
	if err != nil {
		t.Fatalf("EventsByChunkIDs() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("EventsByChunkIDs() = %d events, want 2 (republish must not duplicate)", len(events))
	}
	if events["chunk-1"].Event != "Project Atlas started in 2024." {
		t.Fatalf("chunk-1 event = %q", events["chunk-1"].Event)
	}

	// Entity resolution: "Atlas" and "atlas" must join on one normalized row.
	seedEventIDs := make([]string, 0, len(events))
	for _, event := range events {
		seedEventIDs = append(seedEventIDs, event.ID)
	}
	entities, err := stores.SagIndex.EntitiesByEvents(ctx, namespace, seedEventIDs)
	if err != nil {
		t.Fatalf("EntitiesByEvents() error = %v", err)
	}
	normalized := map[string]bool{}
	for _, entity := range entities {
		normalized[entity.NormalizedKey] = true
	}
	if !normalized["atlas"] {
		t.Fatalf("normalized keys = %v, want 'atlas' (casefold join)", normalized)
	}

	// The dynamic hyperedge: expanding the "atlas" entity returns both
	// chunks' events.
	var atlasID string
	for _, entity := range entities {
		if entity.NormalizedKey == "atlas" {
			atlasID = entity.ID
			break
		}
	}
	if atlasID == "" {
		t.Fatalf("atlas entity id not resolved: %+v", entities)
	}
	expanded, err := stores.SagIndex.EventsByEntities(ctx, namespace, []string{atlasID}, 10)
	if err != nil {
		t.Fatalf("EventsByEntities() error = %v", err)
	}
	if len(expanded) != 2 {
		t.Fatalf("EventsByEntities(atlas) = %d events, want 2 (both chunks share it)", len(expanded))
	}

	// Deleting the document removes events and associations; entities remain
	// (shared rows) but their events are gone.
	if err := stores.SagIndex.DeleteDocument(ctx, namespace, documentID); err != nil {
		t.Fatalf("DeleteDocument() error = %v", err)
	}
	events, err = stores.SagIndex.EventsByChunkIDs(ctx, namespace, []string{"chunk-1"})
	if err != nil {
		t.Fatalf("EventsByChunkIDs() after delete error = %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("EventsByChunkIDs() after delete = %v, want none", events)
	}
	expanded, err = stores.SagIndex.EventsByEntities(ctx, namespace, []string{atlasID}, 10)
	if err != nil {
		t.Fatalf("EventsByEntities() after delete error = %v", err)
	}
	if len(expanded) != 0 {
		t.Fatalf("EventsByEntities() after delete = %d, want no dangling hyperedge", len(expanded))
	}
}

// cleanupMySQLNamespace removes stray rows of one test namespace. SagIndex
// rows are cleaned by the caller; this handles documents/kb rows that the
// test may have written.
func cleanupMySQLNamespace(t *testing.T, stores *Stores, namespace string) error {
	t.Helper()
	ctx := context.Background()
	if _, err := stores.DB.ExecContext(ctx,
		"DELETE FROM documents WHERE namespace = ?", namespace); err != nil {
		return err
	}
	if _, err := stores.DB.ExecContext(ctx,
		"DELETE FROM sag_entities WHERE namespace = ?", namespace); err != nil {
		return err
	}
	return nil
}

// seedSagDocument writes the document and head rows BackfillDocument fences
// on: an active document whose head carries the given version.
func seedSagDocument(t *testing.T, stores *Stores, namespace, documentID, version string) {
	t.Helper()
	ctx := context.Background()
	if err := stores.Documents.Upsert(ctx, namespace, DocumentRecord{
		Name: documentID + ".md", DocumentID: documentID, Collection: "rag-docs", ChunkCount: 2,
	}); err != nil {
		t.Fatalf("Documents.Upsert() error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO document_heads (namespace, document_id, version, updated_at) VALUES (?, ?, ?, 0)",
		namespace, documentID, version); err != nil {
		t.Fatalf("insert document_heads error = %v", err)
	}
}

// TestSagRepositoryBackfillDocumentPublishesOnlyUnmanagedSnapshots covers the
// CLI backfill write path: with a current, unmanaged snapshot it publishes the
// extractions, a stale version silently skips, and once the worker owns the
// document's sag_extraction_jobs the backfill skips instead of racing it.
func TestSagRepositoryBackfillDocumentPublishesOnlyUnmanagedSnapshots(t *testing.T) {
	stores := openMySQLTestStores(t)
	ctx := context.Background()
	suffix, err := randomHex(6)
	if err != nil {
		t.Fatalf("randomHex() error = %v", err)
	}
	namespace := "go-test-" + suffix
	documentID := "doc-" + suffix
	const version = "op-backfill"
	t.Cleanup(func() {
		_ = stores.SagIndex.DeleteDocument(ctx, namespace, documentID)
		_ = cleanupMySQLNamespace(t, stores, namespace)
	})
	seedSagDocument(t, stores, namespace, documentID, version)

	extractions := []SagExtraction{{
		ChunkID: "chunk-backfill", Event: "Atlas backfilled from the CLI.",
		Entities: []string{"Atlas"},
	}}
	if err := stores.SagIndex.BackfillDocument(ctx, namespace, documentID, version, extractions); err != nil {
		t.Fatalf("BackfillDocument() error = %v", err)
	}
	events, err := stores.SagIndex.EventsByChunkIDs(ctx, namespace, []string{"chunk-backfill"})
	if err != nil {
		t.Fatalf("EventsByChunkIDs() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("EventsByChunkIDs() = %d events, want the backfilled one", len(events))
	}

	// A stale version skips silently: the snapshot moved on after the CLI run.
	if err := stores.SagIndex.BackfillDocument(ctx, namespace, documentID, "op-stale", []SagExtraction{{
		ChunkID: "chunk-stale", Event: "must not appear", Entities: []string{"Atlas"},
	}}); err != nil {
		t.Fatalf("BackfillDocument(stale) error = %v", err)
	}
	if stale, err := stores.SagIndex.EventsByChunkIDs(ctx, namespace, []string{"chunk-stale"}); err != nil || len(stale) != 0 {
		t.Fatalf("stale backfill wrote events: %v, %v", stale, err)
	}

	// A document managed by the worker's sag_extraction_jobs must not be
	// touched by the CLI backfill.
	if _, err := stores.ModelProfiles.Create(ctx, ModelProfile{
		Kind: "extraction", Name: "sag", BaseURL: "http://model.local/v1", Model: "extractor",
	}); err != nil {
		t.Fatalf("ModelProfiles.Create() error = %v", err)
	}
	tx, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := stores.SagIndex.QueueExtraction(ctx, tx, namespace, documentID, version, []string{"chunk-managed"}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("QueueExtraction() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := stores.SagIndex.BackfillDocument(ctx, namespace, documentID, version, []SagExtraction{{
		ChunkID: "chunk-managed", Event: "must not appear either", Entities: []string{"Atlas"},
	}}); err != nil {
		t.Fatalf("BackfillDocument(managed) error = %v", err)
	}
	if managed, err := stores.SagIndex.EventsByChunkIDs(ctx, namespace, []string{"chunk-managed"}); err != nil || len(managed) != 0 {
		t.Fatalf("managed backfill wrote events: %v, %v", managed, err)
	}
}

// TestSagRepositoryReplaceDocumentEmptyExtractions pins the empty-slice
// contract documented on ReplaceDocument: republishing a document whose
// chunks produced no extraction removes its index rows so no stale
// hyperedge survives.
func TestSagRepositoryReplaceDocumentEmptyExtractions(t *testing.T) {
	stores := openMySQLTestStores(t)
	ctx := context.Background()
	suffix, err := randomHex(6)
	if err != nil {
		t.Fatalf("randomHex() error = %v", err)
	}
	namespace := "go-test-" + suffix
	documentID := "doc-" + suffix
	t.Cleanup(func() {
		_ = stores.SagIndex.DeleteDocument(ctx, namespace, documentID)
		_ = cleanupMySQLNamespace(t, stores, namespace)
	})

	extractions := []SagExtraction{{
		ChunkID: "chunk-1", Event: "Atlas started.", Entities: []string{"Atlas"},
	}}
	if err := stores.SagIndex.ReplaceDocument(ctx, namespace, documentID, "op-1", extractions); err != nil {
		t.Fatalf("ReplaceDocument() error = %v", err)
	}
	events, err := stores.SagIndex.EventsByChunkIDs(ctx, namespace, []string{"chunk-1"})
	if err != nil || len(events) != 1 {
		t.Fatalf("EventsByChunkIDs() = %v, %v, want one seeded event", events, err)
	}

	if err := stores.SagIndex.ReplaceDocument(ctx, namespace, documentID, "op-2", nil); err != nil {
		t.Fatalf("ReplaceDocument(empty) error = %v", err)
	}
	if remaining, err := stores.SagIndex.EventsByChunkIDs(ctx, namespace, []string{"chunk-1"}); err != nil || len(remaining) != 0 {
		t.Fatalf("EventsByChunkIDs() after empty replace = %v, %v, want the index rows removed", remaining, err)
	}
}
