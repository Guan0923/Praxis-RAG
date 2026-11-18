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
