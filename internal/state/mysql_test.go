package state

import (
	"context"
	"testing"
)

// TestMySQLStoreRoundTrip exercises the MySQL dialect through the same public
// behavior as the SQLite tests. It is skipped unless RAG_TEST_MYSQL_URL is set,
// matching the Python opt-in fixtures.
func TestMySQLStoreRoundTrip(t *testing.T) {
	stores := openMySQLTestStores(t)
	ctx := context.Background()

	if stores.Dialect() != DialectMySQL || stores.Dialect().String() != "mysql" {
		t.Fatalf("dialect = %v, want mysql", stores.Dialect())
	}
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
