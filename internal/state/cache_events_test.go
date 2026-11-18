package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// cacheRevisionOrZero reads a revision that may not exist yet (0).
func cacheRevisionOrZero(t *testing.T, stores *Stores, kind, scope string) int64 {
	t.Helper()
	var value int64
	err := stores.DB.QueryRowContext(context.Background(),
		"SELECT revision FROM cache_revisions WHERE kind = ? AND scope = ?", kind, scope).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("cache_revisions(%q, %q) error = %v", kind, scope, err)
	}
	return value
}

// cacheRevisionRow is one cache_revisions row, including the dirty/refresh
// water-line columns added for the Debezium cache consumer.
type cacheRevisionRow struct {
	Revision          int64
	RefreshedRevision int64
	Attempts          int
	LastError         sql.NullString
	UpdatedAt         float64
}

// readCacheRevision reads the current dirty state of one (kind, scope).
func readCacheRevision(t *testing.T, stores *Stores, kind, scope string) cacheRevisionRow {
	t.Helper()
	var row cacheRevisionRow
	err := stores.DB.QueryRowContext(context.Background(),
		"SELECT revision, refreshed_revision, attempts, last_error, updated_at "+
			"FROM cache_revisions WHERE kind = ? AND scope = ?", kind, scope).
		Scan(&row.Revision, &row.RefreshedRevision, &row.Attempts, &row.LastError, &row.UpdatedAt)
	if err != nil {
		t.Fatalf("cache_revisions(%q, %q) error = %v", kind, scope, err)
	}
	return row
}

// cacheRevisionCount returns the number of tracked read-model revisions.
func cacheRevisionCount(t *testing.T, stores *Stores) int {
	t.Helper()
	var count int
	if err := stores.DB.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM cache_revisions").Scan(&count); err != nil {
		t.Fatalf("cache revision count error = %v", err)
	}
	return count
}

// requireNoCacheOutbox asserts the retired transactional outbox holds no rows.
// A pre-existing test schema may still carry the table until the cutover CLI
// drops it, so absence of the table itself is accepted.
func requireNoCacheOutbox(t *testing.T, stores *Stores) {
	t.Helper()
	ctx := context.Background()
	var tables int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables "+
			"WHERE table_schema = DATABASE() AND table_name = 'cache_outbox'").Scan(&tables); err != nil {
		t.Fatalf("cache_outbox existence query error = %v", err)
	}
	if tables == 0 {
		return
	}
	var rows int
	if err := stores.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM cache_outbox").Scan(&rows); err != nil {
		t.Fatalf("cache_outbox count error = %v", err)
	}
	if rows != 0 {
		t.Fatalf("cache_outbox has %d rows: the revision water line must be the only durable signal", rows)
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
		if value := readCacheRevision(t, stores, expected.kind, expected.scope).Revision; value != 1 {
			t.Fatalf("%s/%s revision = %d, want 1", expected.kind, expected.scope, value)
		}
	}

	if err := stores.Documents.Upsert(ctx, "alpha", DocumentRecord{Name: "a.txt", DocumentID: "doc-a", Collection: "c1"}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	// document_keys = kb_list "", documents alpha
	for _, expected := range []struct {
		kind, scope string
		revision    int64
	}{
		{"kb_list", "", 2}, {"documents", "alpha", 2}, {"kb_public", "alpha", 1},
	} {
		if value := readCacheRevision(t, stores, expected.kind, expected.scope).Revision; value != expected.revision {
			t.Fatalf("%s/%s revision = %d, want %d", expected.kind, expected.scope, value, expected.revision)
		}
	}

	if _, err := stores.Documents.Delete(ctx, "alpha", "a.txt"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if value := readCacheRevision(t, stores, "kb_list", "").Revision; value != 3 {
		t.Fatalf("kb_list revision after delete = %d, want 3", value)
	}
	if value := readCacheRevision(t, stores, "documents", "alpha").Revision; value != 3 {
		t.Fatalf("documents revision after delete = %d, want 3", value)
	}
	requireNoCacheOutbox(t, stores)
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
	eventIDs, err := touch(ctx, transaction, keys...)
	if err != nil {
		t.Fatalf("touch() error = %v", err)
	}
	if eventIDs != nil {
		t.Fatalf("touch() event ids = %v, want nil (the outbox is retired)", eventIDs)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	for _, expected := range []struct {
		kind, scope string
	}{
		{"kb_public", "alpha"}, {"documents", "alpha"}, {"kb_list", ""},
	} {
		row := readCacheRevision(t, stores, expected.kind, expected.scope)
		if row.Revision != 1 || row.RefreshedRevision != 0 {
			t.Fatalf("%s/%s revision/refreshed = %d/%d, want 1/0 (dirty)",
				expected.kind, expected.scope, row.Revision, row.RefreshedRevision)
		}
	}
	if rows := cacheRevisionCount(t, stores); rows != 3 {
		t.Fatalf("cache_revisions rows = %d, want 3 (deduplicated)", rows)
	}
	// A second touch bumps the same rows instead of inserting new ones.
	transaction, err = stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := touch(ctx, transaction, keys...); err != nil {
		t.Fatalf("second touch() error = %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("second Commit() error = %v", err)
	}
	if rows := cacheRevisionCount(t, stores); rows != 3 {
		t.Fatalf("cache_revisions rows after second touch = %d, want 3", rows)
	}
	if value := readCacheRevision(t, stores, "documents", "alpha").Revision; value != 2 {
		t.Fatalf("documents revision after second touch = %d, want 2", value)
	}
	requireNoCacheOutbox(t, stores)
}

func TestTouchRejectsUnknownOrRetiredCacheKind(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	defer func() { _ = transaction.Rollback() }()
	// document_status/search_status had no registered loader and are retired.
	for _, kind := range []string{"unknown", "document_status", "search_status"} {
		_, err = touch(ctx, transaction, CacheKey{Kind: kind, Scope: ""})
		if err == nil || !strings.Contains(err.Error(), "Unknown state cache kind") {
			t.Fatalf("touch(%q) error = %v, want Unknown state cache kind", kind, err)
		}
	}
}

func TestFailedMutationsRollBackRevisions(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, map[string]any{"namespace": "alpha", "collection": "c"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	revisionsBefore := cacheRevisionCount(t, stores)

	mustBeError[*StateConflict](t, stores.KnowledgeBases.Create(ctx,
		map[string]any{"namespace": "alpha", "collection": "c2"}))
	mustBeError[*LookupError](t, stores.KnowledgeBases.Update(ctx,
		map[string]any{"namespace": "missing", "collection": "c"}))

	if revisions := cacheRevisionCount(t, stores); revisions != revisionsBefore {
		t.Fatalf("failed mutations changed cache_revisions rows: %d -> %d", revisionsBefore, revisions)
	}
	if value := readCacheRevision(t, stores, "kb_list", "").Revision; value != 1 {
		t.Fatalf("kb_list revision = %d, want 1 (failed mutations must not bump)", value)
	}
}
