package state

import (
	"context"
	"testing"
)

// TestKnowledgeBaseVersioningAndDuplicates ports
// tests/test_state_store.py::test_knowledge_base_versioning_and_duplicates.
func TestKnowledgeBaseVersioningAndDuplicates(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	if err := stores.KnowledgeBases.Create(ctx, map[string]any{
		"namespace": "alpha", "collection": "c1", "value": 1,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	version, found, err := stores.KnowledgeBases.Version(ctx, "alpha")
	if err != nil || !found || version != 1 {
		t.Fatalf("Version() = %d, %v, %v; want 1, true, nil", version, found, err)
	}
	if err := stores.KnowledgeBases.Update(ctx, map[string]any{
		"namespace": "alpha", "collection": "c2", "value": 2,
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	version, found, err = stores.KnowledgeBases.Version(ctx, "alpha")
	if err != nil || !found || version != 2 {
		t.Fatalf("Version() after update = %d, %v, %v; want 2", version, found, err)
	}
	settings, err := stores.KnowledgeBases.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if settings["collection"] != "c2" || settings["value"] != float64(2) {
		t.Fatalf("settings = %+v", settings)
	}

	err = stores.KnowledgeBases.Create(ctx, map[string]any{"namespace": "alpha", "collection": "c3"})
	if conflict := mustBeError[*StateConflict](t, err); conflict.Message != "该知识库已存在" {
		t.Fatalf("conflict message = %q", conflict.Message)
	}
	err = stores.KnowledgeBases.Update(ctx, map[string]any{"namespace": "missing", "collection": "c4"})
	if lookup := mustBeError[*LookupError](t, err); lookup.Message != "知识库不存在" {
		t.Fatalf("lookup message = %q", lookup.Message)
	}

	deleted, err := stores.KnowledgeBases.Delete(ctx, "alpha")
	if err != nil || !deleted {
		t.Fatalf("Delete() = %v, %v; want true", deleted, err)
	}
	settings, err = stores.KnowledgeBases.Get(ctx, "alpha")
	if err != nil || settings != nil {
		t.Fatalf("Get() after delete = %+v, %v", settings, err)
	}
	version, found, err = stores.KnowledgeBases.Version(ctx, "alpha")
	if err != nil || found || version != 0 {
		t.Fatalf("Version() after delete = %d, %v, %v", version, found, err)
	}
	deleted, err = stores.KnowledgeBases.Delete(ctx, "alpha")
	if err != nil || deleted {
		t.Fatalf("second Delete() = %v, %v; want false", deleted, err)
	}
}

func TestKnowledgeBaseListSortedByNamespace(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	for _, namespace := range []string{"beta", "alpha", "gamma"} {
		if err := stores.KnowledgeBases.Create(ctx, map[string]any{
			"namespace": namespace, "collection": namespace + "-c",
		}); err != nil {
			t.Fatalf("Create(%q) error = %v", namespace, err)
		}
	}
	list, err := stores.KnowledgeBases.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List() returned %d entries", len(list))
	}
	for index, namespace := range []string{"alpha", "beta", "gamma"} {
		if list[index]["namespace"] != namespace {
			t.Fatalf("list[%d].namespace = %v, want %q", index, list[index]["namespace"], namespace)
		}
	}
}

func TestKnowledgeBaseListEmpty(t *testing.T) {
	stores := newTestStores(t)
	empty, err := stores.KnowledgeBases.List(context.Background())
	if err != nil {
		t.Fatalf("empty List() error = %v", err)
	}
	if len(empty) != 0 || empty == nil {
		t.Fatalf("empty List() = %#v, want an empty slice", empty)
	}
}

func TestKnowledgeBaseSnapshot(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, map[string]any{"namespace": "alpha", "collection": "c1"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	snapshot, err := stores.KnowledgeBases.Snapshot(ctx, "alpha")
	if err != nil || snapshot == nil || snapshot.Version != 1 || snapshot.Settings["collection"] != "c1" {
		t.Fatalf("Snapshot() = %+v, %v", snapshot, err)
	}
	if err := stores.KnowledgeBases.Update(ctx, map[string]any{"namespace": "alpha", "collection": "c2"}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	snapshot, err = stores.KnowledgeBases.Snapshot(ctx, "alpha")
	if err != nil || snapshot == nil || snapshot.Version != 2 {
		t.Fatalf("Snapshot() after update = %+v, %v", snapshot, err)
	}
	missing, err := stores.KnowledgeBases.Snapshot(ctx, "missing")
	if err != nil || missing != nil {
		t.Fatalf("Snapshot(missing) = %+v, %v", missing, err)
	}
}

func TestKnowledgeBaseFailedUpdateRollsBackRevisions(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	err := stores.KnowledgeBases.Update(ctx, map[string]any{"namespace": "missing", "collection": "c"})
	mustBeError[*LookupError](t, err)
	if revisions := revisionCount(t, stores); revisions != 0 {
		t.Fatalf("failed update left %d cache revisions, want 0", revisions)
	}
	if events := outboxCount(t, stores); events != 0 {
		t.Fatalf("failed update left %d outbox events, want 0", events)
	}
}

// TestKnowledgeBaseReadModelPrimitives ports the read-model expectations of
// tests/test_knowledge_base_settings.py that live in the state layer: the list
// reads the current repository state and a namespace may still own documents
// after its creation record is deleted.
func TestKnowledgeBaseReadModelPrimitives(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, map[string]any{"namespace": "praxis", "collection": "c1"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := stores.Documents.Upsert(ctx, "praxis", DocumentRecord{
		Name: "existing.txt", DocumentID: "doc-existing", Collection: "c1",
	}); err != nil {
		t.Fatalf("Documents.Upsert() error = %v", err)
	}
	if _, err := stores.KnowledgeBases.Delete(ctx, "praxis"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	settings, err := stores.KnowledgeBases.Get(ctx, "praxis")
	if err != nil || settings != nil {
		t.Fatalf("Get() after delete = %+v, %v", settings, err)
	}
	knowledgeBases, err := stores.KnowledgeBases.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(knowledgeBases) != 0 {
		t.Fatalf("List() = %v, want no creation records", knowledgeBases)
	}
	namespaces, err := stores.Documents.Namespaces(ctx)
	if err != nil {
		t.Fatalf("Namespaces() error = %v", err)
	}
	if namespaces["praxis"] != 1 {
		t.Fatalf("document namespaces = %v, want praxis:1", namespaces)
	}
	documents, err := stores.Documents.List(ctx, "praxis")
	if err != nil || len(documents) != 1 {
		t.Fatalf("Documents.List() = %+v, %v", documents, err)
	}
}

func TestKnowledgeBaseCreateRequiresNamespaceAndCollection(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	for _, settings := range []map[string]any{
		{"collection": "c1"},
		{"namespace": "alpha"},
		{"namespace": 42, "collection": "c1"},
	} {
		if err := stores.KnowledgeBases.Create(ctx, settings); err == nil {
			t.Fatalf("Create(%v) succeeded, want a validation error", settings)
		}
	}
}

func TestStoredInvalidJSONBecomesStateError(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO knowledge_bases (namespace, collection, settings, version, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?)",
		"broken", "c", "not-json", 1, nowISO(), nowISO()); err != nil {
		t.Fatalf("insert error = %v", err)
	}
	_, err := stores.KnowledgeBases.Get(ctx, "broken")
	stateError := mustBeError[*StateError](t, err)
	if stateError.Message != "Stored JSON state is invalid" {
		t.Fatalf("message = %q", stateError.Message)
	}
	_, err = stores.KnowledgeBases.List(ctx)
	mustBeError[*StateError](t, err)
}
