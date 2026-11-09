package state

import (
	"context"
	"strings"
	"testing"
)

// TestMySQLStoreRoundTrip exercises the MySQL dialect through the same public
// behavior as the SQLite tests. It is skipped unless RAG_TEST_MYSQL_URL is set,
// matching the Python opt-in fixtures.
func TestMySQLStoreRoundTrip(t *testing.T) {
	stores := openMySQLTestStores(t)
	ctx := context.Background()

	if description := stores.Describe(); description["backend"] != "mysql" {
		t.Fatalf("describe = %+v", description)
	}
	for _, name := range expectedTableNames() {
		exists, err := stores.hasTable(ctx, name)
		if err != nil {
			t.Fatalf("hasTable(%q) error = %v", name, err)
		}
		if !exists {
			t.Fatalf("MySQL table %q was not created", name)
		}
	}
	// Reopening over the existing schema must be a no-op, like create_all.
	second, err := New(stores.Settings, true)
	if err != nil {
		t.Fatalf("second New() error = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	suffix, err := randomHex(6)
	if err != nil {
		t.Fatalf("randomHex() error = %v", err)
	}
	namespace := "go-test-" + suffix
	createdKeyID := ""
	t.Cleanup(func() {
		cleanupMySQLRows(t, stores, namespace, createdKeyID)
	})

	created, err := stores.APIKeys.Create(ctx, "go-test", []string{namespace, namespace + "-b"})
	if err != nil {
		t.Fatalf("APIKeys.Create() error = %v", err)
	}
	createdKeyID = created.ID
	namespaces, err := stores.APIKeys.NamespacesForKey(ctx, created.Key)
	if err != nil || len(namespaces) != 2 {
		t.Fatalf("NamespacesForKey() = %v, %v", namespaces, err)
	}
	secret, err := stores.APIKeys.SecretForNamespace(ctx, created.ID, namespace)
	if err != nil || secret != created.Key {
		t.Fatalf("SecretForNamespace() = %q, %v", secret, err)
	}

	if err := stores.KnowledgeBases.Create(ctx, map[string]any{
		"namespace": namespace, "collection": namespace + "-c", "value": "中文",
	}); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	version, found, err := stores.KnowledgeBases.Version(ctx, namespace)
	if err != nil || !found || version != 1 {
		t.Fatalf("Version() = %d, %v, %v", version, found, err)
	}
	if err := stores.KnowledgeBases.Update(ctx, map[string]any{
		"namespace": namespace, "collection": namespace + "-c2", "value": "updated",
	}); err != nil {
		t.Fatalf("KnowledgeBases.Update() error = %v", err)
	}
	settings, err := stores.KnowledgeBases.Get(ctx, namespace)
	if err != nil || settings["value"] != "updated" {
		t.Fatalf("KnowledgeBases.Get() = %+v, %v", settings, err)
	}

	document := DocumentRecord{
		Name: "mysql.txt", DocumentID: "doc-" + suffix, Size: 7, SHA256: "sha-" + suffix,
		Collection: namespace + "-c2", ChunkCount: 1,
		Chunking: map[string]any{"strategy": "character"}, Diagnostics: []any{},
	}
	if err := stores.Documents.Upsert(ctx, namespace, document); err != nil {
		t.Fatalf("Documents.Upsert() error = %v", err)
	}
	record, err := stores.Documents.Get(ctx, namespace, "mysql.txt")
	if err != nil || record == nil || record.DocumentID != document.DocumentID {
		t.Fatalf("Documents.Get() = %+v, %v", record, err)
	}
	if record.Chunking.(map[string]any)["strategy"] != "character" {
		t.Fatalf("chunking was not preserved: %#v", record.Chunking)
	}
	candidates, err := stores.Documents.Candidates(ctx, namespace, document.SHA256, nil)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("Candidates() = %+v, %v", candidates, err)
	}

	catalogData := `{"tools":{"search":{"enabled":false}},"prompts":{}}`
	if _, err := stores.Catalogs.Mutate(ctx, namespace, func(current *string) (string, any, bool, error) {
		return catalogData, true, true, nil
	}); err != nil {
		t.Fatalf("Catalogs.Mutate() error = %v", err)
	}
	loaded, found, err := stores.Catalogs.Load(ctx, namespace)
	if err != nil || !found || loaded != catalogData {
		t.Fatalf("Catalogs.Load() = %q, %v, %v", loaded, found, err)
	}

	deleted, err := stores.Documents.Delete(ctx, namespace, "mysql.txt")
	if err != nil || !deleted {
		t.Fatalf("Documents.Delete() = %v, %v", deleted, err)
	}
	if deleted, err := stores.KnowledgeBases.Delete(ctx, namespace); err != nil || !deleted {
		t.Fatalf("KnowledgeBases.Delete() = %v, %v", deleted, err)
	}
	if deleted, err := stores.APIKeys.Delete(ctx, created.ID); err != nil || !deleted {
		t.Fatalf("APIKeys.Delete() = %v, %v", deleted, err)
	}
}

// cleanupMySQLRows removes the rows a MySQL test case created; the shared test
// database is never dropped.
// TestMySQLG7StateMachines gates the document-job and search-task state
// machines on RAG_TEST_MYSQL_URL.
func TestMySQLG7StateMachines(t *testing.T) {
	stores := openMySQLTestStores(t)
	ctx := context.Background()
	suffix, err := randomHex(6)
	if err != nil {
		t.Fatalf("randomHex() error = %v", err)
	}
	namespace := "go-g7-" + suffix
	if err := stores.KnowledgeBases.Create(ctx, map[string]any{
		"namespace": namespace, "collection": namespace + "-c",
	}); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	documentPublisher := &fakeDocumentPublisher{}
	documentDispatcher, err := NewDocumentJobDispatcher(stores, documentPublisher)
	if err != nil {
		t.Fatalf("NewDocumentJobDispatcher() error = %v", err)
	}
	defer func() { _ = documentDispatcher.Close() }()
	admission, err := documentDispatcher.Submit(ctx, namespace, "mysql.txt", []byte("mysql"), nil)
	if err != nil {
		t.Fatalf("document Submit() error = %v", err)
	}
	if len(documentPublisher.messages()) != 1 {
		t.Fatalf("document notifications = %+v", documentPublisher.messages())
	}
	notification := documentNotification(t, stores, admission.ArtifactID)
	task, err := stores.DocumentJobs.Claim(ctx, notification)
	if err != nil || task == nil || task.Attempts != 1 {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	if err := stores.DocumentJobs.Fail(ctx, task, false, stringPtr("test")); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	searchPublisher := &fakeSearchPublisher{}
	searchDispatcher := NewSearchTaskDispatcher(stores, searchPublisher)
	envelope, err := searchDispatcher.Submit(ctx, namespace, map[string]any{"query": "mysql"})
	if err != nil {
		t.Fatalf("search Submit() error = %v", err)
	}
	if len(searchPublisher.messages()) != 1 || searchPublisher.messages()[0].ResultID != envelope.ResultID {
		t.Fatalf("search notifications = %+v", searchPublisher.messages())
	}
	if _, err := stores.SearchTasks.Claim(ctx, envelope); err != nil {
		t.Fatalf("search Claim() error = %v", err)
	}
	t.Cleanup(func() {
		jc := context.Background()
		for _, statement := range []string{
			"DELETE FROM document_job_outbox WHERE namespace = ?",
			"DELETE FROM document_jobs WHERE namespace = ?",
			"DELETE FROM search_task_outbox WHERE result_id = ?",
			"DELETE FROM search_tasks WHERE namespace = ?",
		} {
			arguments := []any{namespace}
			if strings.Contains(statement, "result_id = ?") {
				arguments = []any{envelope.ResultID}
			}
			if _, err := stores.DB.ExecContext(jc, statement, arguments...); err != nil {
				t.Logf("cleanup %q error = %v", statement, err)
			}
		}
	})
}

func cleanupMySQLRows(t *testing.T, stores *Stores, namespace, keyID string) {
	t.Helper()
	ctx := context.Background()
	statements := []struct {
		query string
		args  []any
	}{
		{"DELETE FROM api_keys WHERE id = ?", []any{keyID}},
		{"DELETE FROM documents WHERE namespace = ?", []any{namespace}},
		{"DELETE FROM knowledge_bases WHERE namespace = ?", []any{namespace}},
		{"DELETE FROM mcp_catalogs WHERE namespace = ?", []any{namespace}},
		{"DELETE FROM cache_outbox WHERE scope = ?", []any{namespace}},
		{"DELETE FROM cache_revisions WHERE scope = ?", []any{namespace}},
	}
	for _, statement := range statements {
		if keyID == "" && statement.query == "DELETE FROM api_keys WHERE id = ?" {
			continue
		}
		if _, err := stores.DB.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Logf("cleanup %q error = %v", statement.query, err)
		}
	}
}

// TestMySQLUploadJournalRoundTrip gates the durable upload transaction on
// RAG_TEST_MYSQL_URL, mirroring tests/test_mysql_upload_transactions.py.
func TestMySQLUploadJournalRoundTrip(t *testing.T) {
	stores := openMySQLTestStores(t)
	ctx := context.Background()
	journal, err := NewUploadJournal(stores, WithLeaseSeconds(30), WithAutoHeartbeat(false), WithCleanupDelaySeconds(0))
	if err != nil {
		t.Fatalf("NewUploadJournal() error = %v", err)
	}
	defer journal.Close()
	suffix, err := randomHex(6)
	if err != nil {
		t.Fatalf("randomHex() error = %v", err)
	}
	namespace := "go-upload-" + suffix
	idempotencyKey := "key-" + suffix
	t.Cleanup(func() {
		jc := context.Background()
		for _, statement := range []string{
			"DELETE FROM upload_objects WHERE operation_id IN (SELECT id FROM upload_operations WHERE namespace = ?)",
			"DELETE FROM upload_steps WHERE operation_id IN (SELECT id FROM upload_operations WHERE namespace = ?)",
			"DELETE FROM upload_operations WHERE namespace = ?",
			"DELETE FROM document_heads WHERE namespace = ?",
		} {
			if _, err := stores.DB.ExecContext(jc, statement, namespace); err != nil {
				t.Logf("cleanup %q error = %v", statement, err)
			}
		}
	})
	operation, err := journal.Begin(ctx, namespace, "file.txt", "doc", "collection", BeginOptions{
		IdempotencyKey: &idempotencyKey,
	})
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if err := journal.PutObject(ctx, operation, SourceBucket, "file.txt", []byte("mysql bytes"), nil); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if _, err := journal.Commit(ctx, operation, map[string]any{"ok": true}, transaction); err != nil {
		_ = transaction.Rollback()
		t.Fatalf("Commit() error = %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("Commit tx error = %v", err)
	}
	head, err := journal.Head(ctx, namespace, "doc")
	if err != nil || head == nil || *head != operation.ID {
		t.Fatalf("Head() = %v, %v", head, err)
	}
	replay, err := journal.Begin(ctx, namespace, "file.txt", "doc", "collection", BeginOptions{
		IdempotencyKey: &idempotencyKey,
	})
	if err != nil || replay.ID != operation.ID || replay.Status != UploadCommitted {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	if content, found, err := journal.GetObject(ctx, operation.ID, SourceBucket, "file.txt"); err != nil || !found || string(content) != "mysql bytes" {
		t.Fatalf("GetObject() = %q, %v, %v", content, found, err)
	}
}

// TestMySQLDocumentTaskLookupSQL executes the document-task correlation queries
// on MySQL. It exists because these LIKE ... ESCAPE queries used a backslash
// literal that MySQL rejects (error 1064) while SQLite accepts it; the live
// Debezium run surfaced the incompatibility, unit SQLite tests could not.
func TestMySQLDocumentTaskLookupSQL(t *testing.T) {
	stores := openMySQLTestStores(t)
	ctx := context.Background()
	suffix, err := randomHex(6)
	if err != nil {
		t.Fatalf("randomHex() error = %v", err)
	}
	task := &DocumentJobTask{DocumentJob: DocumentJob{
		ArtifactID:           "00000000-0000-4000-8000-000000000000",
		Namespace:            "go-task-lookup-" + suffix,
		Name:                 "lookup.md",
		SHA256:               "sha",
		Size:                 3,
		ConfigurationVersion: 1,
	}}
	operation, err := stores.DocumentJobs.CommittedOperation(ctx, task)
	if err != nil {
		t.Fatalf("CommittedOperation() SQL error = %v", err)
	}
	if operation != nil {
		t.Fatalf("CommittedOperation() = %+v, want nil", operation)
	}
	busy, err := stores.DocumentJobs.ExecutionBusy(ctx, task)
	if err != nil {
		t.Fatalf("ExecutionBusy() SQL error = %v", err)
	}
	if busy {
		t.Fatal("ExecutionBusy() = true, want false")
	}
}
