package state

import (
	"context"
	"sort"
	"strings"
	"testing"
)

func TestAPIKeyCreateEmitsRevisionAndOutboxEvent(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if _, err := stores.APIKeys.Create(ctx, "app", []string{"alpha"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if value := revision(t, stores, "api_keys", ""); value != 1 {
		t.Fatalf("api_keys revision = %d, want 1", value)
	}
	var (
		kind, scope, status string
		eventRevision       int64
		attempts            int
		retryAt, createdAt  float64
		completedAt         any
		lastError           any
	)
	err := stores.DB.QueryRowContext(ctx,
		"SELECT kind, scope, revision, status, attempts, retry_at, created_at, completed_at, last_error FROM cache_outbox").
		Scan(&kind, &scope, &eventRevision, &status, &attempts, &retryAt, &createdAt, &completedAt, &lastError)
	if err != nil {
		t.Fatalf("cache_outbox query error = %v", err)
	}
	if kind != "api_keys" || scope != "" || eventRevision != 1 || status != "PENDING" || attempts != 0 || retryAt != 0 {
		t.Fatalf("unexpected outbox event: kind=%q scope=%q revision=%d status=%q attempts=%d retry_at=%v",
			kind, scope, eventRevision, status, attempts, retryAt)
	}
	if createdAt <= 0 {
		t.Fatalf("created_at = %v, want a positive epoch timestamp", createdAt)
	}
	if completedAt != nil || lastError != nil {
		t.Fatalf("completed_at/last_error = %v/%v, want NULL", completedAt, lastError)
	}
	var eventID string
	if err := stores.DB.QueryRowContext(ctx, "SELECT id FROM cache_outbox").Scan(&eventID); err != nil {
		t.Fatalf("outbox id query error = %v", err)
	}
	if len(eventID) != 32 || strings.ToLower(eventID) != eventID {
		t.Fatalf("event id = %q, want 32 lowercase hex characters", eventID)
	}
}

func TestKnowledgeAndDocumentTouchesBumpEveryScope(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, map[string]any{"namespace": "alpha", "collection": "c1"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// knowledge_keys = kb_list "", kb_public alpha, documents alpha
	for _, expected := range []struct {
		kind, scope string
	}{
		{"kb_list", ""}, {"kb_public", "alpha"}, {"documents", "alpha"},
	} {
		if value := revision(t, stores, expected.kind, expected.scope); value != 1 {
			t.Fatalf("%s/%s revision = %d, want 1", expected.kind, expected.scope, value)
		}
	}
	if events := outboxCount(t, stores); events != 3 {
		t.Fatalf("outbox events = %d, want 3", events)
	}

	if err := stores.Documents.Upsert(ctx, "alpha", DocumentRecord{Name: "a.txt", DocumentID: "doc-a", Collection: "c1"}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	// document_keys = kb_list "", documents alpha
	if value := revision(t, stores, "kb_list", ""); value != 2 {
		t.Fatalf("kb_list revision = %d, want 2", value)
	}
	if value := revision(t, stores, "documents", "alpha"); value != 2 {
		t.Fatalf("documents revision = %d, want 2", value)
	}
	if value := revision(t, stores, "kb_public", "alpha"); value != 1 {
		t.Fatalf("kb_public revision = %d, want 1", value)
	}
	if events := outboxCount(t, stores); events != 5 {
		t.Fatalf("outbox events = %d, want 5", events)
	}

	if _, err := stores.Documents.Delete(ctx, "alpha", "a.txt"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if value := revision(t, stores, "kb_list", ""); value != 3 {
		t.Fatalf("kb_list revision after delete = %d, want 3", value)
	}
	if value := revision(t, stores, "documents", "alpha"); value != 3 {
		t.Fatalf("documents revision after delete = %d, want 3", value)
	}
}

func TestTouchSortsAndDeduplicatesKeys(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	defer func() { _ = transaction.Rollback() }()
	keys := []CacheKey{
		{Kind: "kb_public", Scope: "alpha"},
		{Kind: "documents", Scope: "alpha"},
		{Kind: "documents", Scope: "alpha"},
		{Kind: "kb_list", Scope: ""},
	}
	if _, err := touch(ctx, transaction, keys...); err != nil {
		t.Fatalf("touch() error = %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	for _, expected := range []struct {
		kind, scope string
	}{
		{"kb_public", "alpha"}, {"documents", "alpha"}, {"kb_list", ""},
	} {
		if value := revision(t, stores, expected.kind, expected.scope); value != 1 {
			t.Fatalf("%s/%s revision = %d, want 1", expected.kind, expected.scope, value)
		}
	}
	// Python iterates sorted(set(keys)); the outbox insertion order therefore
	// follows (kind, scope) ordering: documents, kb_list, kb_public. MySQL has no
	// rowid, so only the event set is asserted here.
	rows, err := stores.DB.QueryContext(ctx, "SELECT kind FROM cache_outbox")
	if err != nil {
		t.Fatalf("outbox query error = %v", err)
	}
	defer closeRows(t, rows)
	ordered := make([]string, 0, 3)
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan error = %v", err)
		}
		ordered = append(ordered, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err() = %v", err)
	}
	expectedOrder := []string{"documents", "kb_list", "kb_public"}
	if len(ordered) != len(expectedOrder) {
		t.Fatalf("outbox kinds = %v", ordered)
	}
	sort.Strings(ordered)
	for index, kind := range expectedOrder {
		if ordered[index] != kind {
			t.Fatalf("outbox kinds = %v, want %v", ordered, expectedOrder)
		}
	}
	if events := outboxCount(t, stores); events != 3 {
		t.Fatalf("deduplication failed: %d outbox events", events)
	}
}

func TestTouchRejectsUnknownCacheKind(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	defer func() { _ = transaction.Rollback() }()
	_, err = touch(ctx, transaction, CacheKey{Kind: "unknown", Scope: ""})
	if err == nil || err.Error() != "Unknown state cache kind" {
		t.Fatalf("touch() error = %v, want %q", err, "Unknown state cache kind")
	}
}

func TestFailedMutationsRollBackRevisions(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, map[string]any{"namespace": "alpha", "collection": "c"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	revisionsBefore := revisionCount(t, stores)
	eventsBefore := outboxCount(t, stores)

	mustBeError[*StateConflict](t, stores.KnowledgeBases.Create(ctx,
		map[string]any{"namespace": "alpha", "collection": "c2"}))
	mustBeError[*LookupError](t, stores.KnowledgeBases.Update(ctx,
		map[string]any{"namespace": "missing", "collection": "c"}))

	if revisions := revisionCount(t, stores); revisions != revisionsBefore {
		t.Fatalf("failed mutations changed revisions: %d -> %d", revisionsBefore, revisions)
	}
	if events := outboxCount(t, stores); events != eventsBefore {
		t.Fatalf("failed mutations added outbox events: %d -> %d", eventsBefore, events)
	}
}
