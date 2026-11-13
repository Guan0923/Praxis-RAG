package state

import (
	"context"
	"testing"
)

// TestDocumentLifecycleRoundTrip ports the documents half of
// tests/test_state_store.py::test_documents_and_blobs_round_trip.
func TestDocumentLifecycleRoundTrip(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	document := DocumentRecord{
		Name:               "a.txt",
		DocumentID:         "doc-a",
		Size:               5,
		SHA256:             "0000000000000000000000000000000000000000000000000000000000000000",
		ChunkCount:         1,
		Collection:         "c",
		Chunking:           map[string]any{"strategy": "character"},
		Diagnostics:        []any{map[string]any{"kind": "note"}},
		SkippedObjectCount: 2,
	}
	if err := stores.Documents.Upsert(ctx, "alpha", document); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	documents, err := stores.Documents.List(ctx, "alpha")
	if err != nil || len(documents) != 1 || documents[0].Name != "a.txt" {
		t.Fatalf("List() = %+v, %v", documents, err)
	}
	record := documents[0]
	if record.Size != 5 || record.DocumentID != "doc-a" || record.ChunkCount != 1 ||
		record.Collection != "c" || record.SkippedObjectCount != 2 {
		t.Fatalf("unexpected record: %+v", record)
	}
	chunking, ok := record.Chunking.(map[string]any)
	if !ok || chunking["strategy"] != "character" {
		t.Fatalf("chunking = %#v", record.Chunking)
	}
	diagnostics, ok := record.Diagnostics.([]any)
	if !ok || len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v", record.Diagnostics)
	}
	if record.ModifiedAt == "" || record.IndexedAt == "" {
		t.Fatalf("timestamps were not defaulted: %+v", record)
	}

	byName, err := stores.Documents.Get(ctx, "alpha", "a.txt")
	if err != nil || byName == nil || byName.DocumentID != "doc-a" {
		t.Fatalf("Get() = %+v, %v", byName, err)
	}
	byID, err := stores.Documents.GetByID(ctx, "alpha", "doc-a")
	if err != nil || byID == nil || byID.Name != "a.txt" {
		t.Fatalf("GetByID() = %+v, %v", byID, err)
	}

	candidates, err := stores.Documents.Candidates(ctx, "alpha", document.SHA256, nil)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("Candidates(sha) = %+v, %v", candidates, err)
	}
	name := "a.txt"
	candidates, err = stores.Documents.Candidates(ctx, "alpha", document.SHA256, &name)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("Candidates(sha, name) = %+v, %v", candidates, err)
	}
	other := "other.txt"
	candidates, err = stores.Documents.Candidates(ctx, "alpha", document.SHA256, &other)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("Candidates(sha, other name) = %+v, %v", candidates, err)
	}

	namespaces, err := stores.Documents.Namespaces(ctx)
	if err != nil || namespaces["alpha"] != 1 {
		t.Fatalf("Namespaces() = %v, %v", namespaces, err)
	}

	deleted, err := stores.Documents.Delete(ctx, "alpha", "a.txt")
	if err != nil || !deleted {
		t.Fatalf("Delete() = %v, %v; want true", deleted, err)
	}
	documents, err = stores.Documents.List(ctx, "alpha")
	if err != nil || len(documents) != 0 {
		t.Fatalf("List() after delete = %+v, %v", documents, err)
	}
	if record, err := stores.Documents.Get(ctx, "alpha", "a.txt"); err != nil || record != nil {
		t.Fatalf("Get() after delete = %+v, %v", record, err)
	}
	deleted, err = stores.Documents.Delete(ctx, "alpha", "a.txt")
	if err != nil || deleted {
		t.Fatalf("second Delete() = %v, %v; want false", deleted, err)
	}

	// Upserting a deleted document resurrects it (del_flag is reset to 0).
	if err := stores.Documents.Upsert(ctx, "alpha", document); err != nil {
		t.Fatalf("resurrect Upsert() error = %v", err)
	}
	if record, err := stores.Documents.Get(ctx, "alpha", "a.txt"); err != nil || record == nil {
		t.Fatalf("Get() after resurrect = %+v, %v", record, err)
	}
}

// TestDocumentNamesAndNamespacesAreCaseSensitive ports
// tests/test_state_store.py::test_document_names_and_namespaces_are_case_sensitive.
func TestDocumentNamesAndNamespacesAreCaseSensitive(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Documents.Upsert(ctx, "Alpha", DocumentRecord{Name: "Doc.txt", DocumentID: "d1", Collection: "c"}); err != nil {
		t.Fatalf("Upsert(Alpha) error = %v", err)
	}
	if err := stores.Documents.Upsert(ctx, "alpha", DocumentRecord{Name: "Doc.txt", DocumentID: "d2", Collection: "c"}); err != nil {
		t.Fatalf("Upsert(alpha) error = %v", err)
	}
	upper, err := stores.Documents.Get(ctx, "Alpha", "Doc.txt")
	if err != nil || upper == nil || upper.DocumentID != "d1" {
		t.Fatalf("Get(Alpha, Doc.txt) = %+v, %v", upper, err)
	}
	lower, err := stores.Documents.Get(ctx, "alpha", "Doc.txt")
	if err != nil || lower == nil || lower.DocumentID != "d2" {
		t.Fatalf("Get(alpha, Doc.txt) = %+v, %v", lower, err)
	}
	if record, err := stores.Documents.Get(ctx, "Alpha", "doc.txt"); err != nil || record != nil {
		t.Fatalf("Get(Alpha, doc.txt) = %+v, %v; names are not case-sensitive", record, err)
	}
}

func TestDocumentDefaults(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Documents.Upsert(ctx, "alpha", DocumentRecord{
		Name: "plain.txt", DocumentID: "doc-plain", Collection: "c",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	record, err := stores.Documents.Get(ctx, "alpha", "plain.txt")
	if err != nil || record == nil {
		t.Fatalf("Get() = %+v, %v", record, err)
	}
	if record.Size != 0 || record.SHA256 != "" || record.ChunkCount != 0 || record.SkippedObjectCount != 0 {
		t.Fatalf("unexpected defaults: %+v", record)
	}
	chunking, ok := record.Chunking.(map[string]any)
	if !ok || len(chunking) != 0 {
		t.Fatalf("chunking default = %#v, want {}", record.Chunking)
	}
	diagnostics, ok := record.Diagnostics.([]any)
	if !ok || len(diagnostics) != 0 {
		t.Fatalf("diagnostics default = %#v, want []", record.Diagnostics)
	}
	if record.ModifiedAt == "" || record.IndexedAt == "" {
		t.Fatalf("timestamps were not defaulted: %+v", record)
	}
}

func TestCandidatesIncludeLegacyRowsWithoutSHA(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Documents.Upsert(ctx, "alpha", DocumentRecord{
		Name: "legacy.txt", DocumentID: "legacy", Collection: "c",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	candidates, err := stores.Documents.Candidates(ctx, "alpha", "abc123", nil)
	if err != nil || len(candidates) != 1 || candidates[0].Name != "legacy.txt" {
		t.Fatalf("Candidates() = %+v, %v", candidates, err)
	}
}

func TestDocumentListSortsCaseInsensitively(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	for _, name := range []string{"Beta.txt", "alpha.txt", "Gamma.txt"} {
		if err := stores.Documents.Upsert(ctx, "alpha", DocumentRecord{
			Name: name, DocumentID: "doc-" + name, Collection: "c",
		}); err != nil {
			t.Fatalf("Upsert(%q) error = %v", name, err)
		}
	}
	documents, err := stores.Documents.List(ctx, "alpha")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	expected := []string{"alpha.txt", "Beta.txt", "Gamma.txt"}
	if len(documents) != len(expected) {
		t.Fatalf("List() returned %d documents", len(documents))
	}
	for index, name := range expected {
		if documents[index].Name != name {
			t.Fatalf("documents[%d] = %q, want %q", index, documents[index].Name, name)
		}
	}
}

func TestDocumentUpsertTransactionRollsBack(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := stores.Documents.UpsertTx(ctx, transaction, "alpha", DocumentRecord{
		Name: "tx.txt", DocumentID: "doc-tx", Collection: "c",
	}); err != nil {
		t.Fatalf("UpsertTx() error = %v", err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if record, err := stores.Documents.Get(ctx, "alpha", "tx.txt"); err != nil || record != nil {
		t.Fatalf("Get() after rollback = %+v, %v", record, err)
	}
	if revisions := revisionCount(t, stores); revisions != 0 {
		t.Fatalf("rollback left %d cache revisions", revisions)
	}
}

func TestDocumentDeleteTransactionRollsBack(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Documents.Upsert(ctx, "alpha", DocumentRecord{
		Name: "keep.txt", DocumentID: "doc-keep", Collection: "c",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	deleted, err := stores.Documents.DeleteTx(ctx, transaction, "alpha", "keep.txt")
	if err != nil || !deleted {
		t.Fatalf("DeleteTx() = %v, %v", deleted, err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if record, err := stores.Documents.Get(ctx, "alpha", "keep.txt"); err != nil || record == nil {
		t.Fatalf("Get() after rollback = %+v, %v", record, err)
	}
}

func TestVisibleVersionsAndOperationID(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Documents.Upsert(ctx, "alpha", DocumentRecord{
		Name: "a.txt", DocumentID: "doc-a", Collection: "c",
	}); err != nil {
		t.Fatalf("Upsert(a) error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO document_heads (namespace, document_id, version, updated_at) VALUES (?, ?, ?, ?)",
		"alpha", "doc-a", "op-1", 1.0); err != nil {
		t.Fatalf("document_heads insert error = %v", err)
	}
	record, err := stores.Documents.Get(ctx, "alpha", "a.txt")
	if err != nil || record == nil || record.OperationID != "op-1" {
		t.Fatalf("Get() = %+v, %v; want operation_id op-1", record, err)
	}
	if err := stores.Documents.Upsert(ctx, "alpha", DocumentRecord{
		Name: "b.txt", DocumentID: "doc-b", Collection: "c",
	}); err != nil {
		t.Fatalf("Upsert(b) error = %v", err)
	}
	if _, err := stores.Documents.Delete(ctx, "alpha", "b.txt"); err != nil {
		t.Fatalf("Delete(b) error = %v", err)
	}
	versions, err := stores.Documents.VisibleVersions(ctx, "alpha")
	if err != nil {
		t.Fatalf("VisibleVersions() error = %v", err)
	}
	if versions["doc-a"] != "op-1" {
		t.Fatalf("visible version for doc-a = %q, want op-1", versions["doc-a"])
	}
	if version, exists := versions["doc-b"]; !exists || version != "" {
		t.Fatalf("visible version for doc-b = %q (exists=%v), want the empty tombstone", version, exists)
	}
}

func TestDocumentImportRecord(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Documents.ImportRecord(ctx, "alpha", DocumentRecord{
		Name: "imported.txt", DocumentID: "doc-imported", Collection: "c",
	}); err != nil {
		t.Fatalf("ImportRecord() error = %v", err)
	}
	record, err := stores.Documents.Get(ctx, "alpha", "imported.txt")
	if err != nil || record == nil {
		t.Fatalf("Get() = %+v, %v", record, err)
	}
}
