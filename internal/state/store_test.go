package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

func TestDescribePingAndClose(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	description := stores.Describe()
	if description["backend"] != "mysql" {
		t.Fatalf("backend = %v", description["backend"])
	}
	if description["database"] != stores.Settings.Database {
		t.Fatalf("database = %v, want %q", description["database"], stores.Settings.Database)
	}
	if description["host"] != stores.Settings.Host {
		t.Fatalf("host = %v, want %q", description["host"], stores.Settings.Host)
	}
	if description["encrypted"] != true {
		t.Fatalf("encrypted = %v", description["encrypted"])
	}
	if err := stores.Ping(ctx); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	if err := stores.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := stores.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if err := stores.Ping(ctx); err == nil {
		t.Fatal("Ping() after Close() succeeded")
	}
}

func TestAutoSchemaCanBeDisabled(t *testing.T) {
	isolateStateEnv(t)
	useTestEncryptionKey(t)
	seed := newTestStores(t)
	dropStateTables(t, seed)
	ctx := context.Background()
	t.Setenv("RAG_STATE_AUTO_SCHEMA", "0")
	stores, err := Create("")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	exists, err := stores.hasTable(ctx, "api_keys")
	if err != nil {
		t.Fatalf("hasTable() error = %v", err)
	}
	if exists {
		t.Fatal("RAG_STATE_AUTO_SCHEMA=0 still created the schema")
	}
	if err := stores.Ping(ctx); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	if err := stores.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	t.Setenv("RAG_STATE_AUTO_SCHEMA", "1")
	reopened, err := Create("")
	if err != nil {
		t.Fatalf("Create() with auto schema error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	exists, err = reopened.hasTable(ctx, "api_keys")
	if err != nil {
		t.Fatalf("hasTable() error = %v", err)
	}
	if !exists {
		t.Fatal("reopening with RAG_STATE_AUTO_SCHEMA=1 did not create the schema")
	}
}

func TestSchemaCreationCanBeForcedOff(t *testing.T) {
	isolateStateEnv(t)
	useTestEncryptionKey(t)
	seed := newTestStores(t)
	dropStateTables(t, seed)
	settings, err := SettingsFromEnv("")
	if err != nil {
		t.Fatalf("SettingsFromEnv() error = %v", err)
	}
	if !settings.AutoSchema {
		t.Fatal("default settings should auto-create the schema")
	}
	stores, err := Create("", WithSettings(settings), WithSchemaCreation(false))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer func() { _ = stores.Close() }()
	exists, err := stores.hasTable(context.Background(), "documents")
	if err != nil {
		t.Fatalf("hasTable() error = %v", err)
	}
	if exists {
		t.Fatal("WithSchemaCreation(false) still created the schema")
	}
}

func TestReopenKeepsPersistedState(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	settings := map[string]any{"namespace": "alpha", "collection": "c1", "value": float64(1)}
	if err := stores.KnowledgeBases.Create(ctx, settings); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	document := DocumentRecord{Name: "a.txt", DocumentID: "doc-a", Collection: "c1", SHA256: "0"}
	if err := stores.Documents.Upsert(ctx, "alpha", document); err != nil {
		t.Fatalf("Documents.Upsert() error = %v", err)
	}
	created, err := stores.APIKeys.Create(ctx, "app", []string{"alpha"})
	if err != nil {
		t.Fatalf("APIKeys.Create() error = %v", err)
	}

	reopened := reopenStores(t, stores)
	settingsAfter, err := reopened.KnowledgeBases.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("KnowledgeBases.Get() error = %v", err)
	}
	if settingsAfter["collection"] != "c1" {
		t.Fatalf("collection = %v", settingsAfter["collection"])
	}
	version, found, err := reopened.KnowledgeBases.Version(ctx, "alpha")
	if err != nil || !found || version != 1 {
		t.Fatalf("Version() = %d, %v, %v", version, found, err)
	}
	documentAfter, err := reopened.Documents.Get(ctx, "alpha", "a.txt")
	if err != nil || documentAfter == nil || documentAfter.DocumentID != "doc-a" {
		t.Fatalf("Documents.Get() = %+v, %v", documentAfter, err)
	}
	secret, err := reopened.APIKeys.SecretForNamespace(ctx, created.ID, "alpha")
	if err != nil {
		t.Fatalf("SecretForNamespace() error = %v", err)
	}
	if secret != created.Key {
		t.Fatalf("secret after restart = %q, want the created key", secret)
	}
}

func TestInitializeFailureIsWrapped(t *testing.T) {
	isolateStateEnv(t)
	settings := StateSettings{
		URL:        "mysql+pymysql://rag:pw@127.0.0.1:1/praxis_test?charset=utf8mb4",
		AutoSchema: true,
	}
	stores, err := New(settings, true)
	if stores != nil {
		_ = stores.Close()
	}
	if err == nil {
		t.Fatal("New() against an unreachable server succeeded")
	}
	stateError := mustBeError[*StateError](t, err)
	if !strings.HasPrefix(stateError.Message, "状态数据库初始化失败：") {
		t.Fatalf("message = %q", stateError.Message)
	}
}

func TestMigrationsAddMissingLegacyColumns(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	// Simulate an older database: the soft-delete tombstone, the Debezium
	// scheduling generation, the cache-refresh watermark columns and the cutover
	// bookkeeping table are all absent.
	for _, statement := range []string{
		"ALTER TABLE documents DROP COLUMN del_flag",
		"ALTER TABLE document_jobs DROP COLUMN schedule_version",
		"ALTER TABLE cache_revisions DROP COLUMN refreshed_revision",
		"ALTER TABLE cache_revisions DROP COLUMN attempts",
		"ALTER TABLE cache_revisions DROP COLUMN last_error",
		"ALTER TABLE cache_revisions DROP COLUMN updated_at",
		"DROP TABLE cdc_cutover_state",
	} {
		if _, err := stores.DB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s error = %v", statement, err)
		}
	}
	if columnExists(t, stores, "documents", "del_flag") {
		t.Fatal("del_flag still exists after the simulated legacy ALTER")
	}

	reopened := reopenStores(t, stores)
	for _, wanted := range []struct{ table, column string }{
		{"documents", "del_flag"},
		{"document_jobs", "schedule_version"},
		{"cache_revisions", "refreshed_revision"},
		{"cache_revisions", "attempts"},
		{"cache_revisions", "last_error"},
		{"cache_revisions", "updated_at"},
	} {
		if !columnExists(t, reopened, wanted.table, wanted.column) {
			t.Fatalf("reopen did not add %s.%s", wanted.table, wanted.column)
		}
	}
	// search_tasks is a retired historical table: the scheduler migration only
	// backfills document_jobs, so the dropped generated column stays dropped.
	if columnExists(t, reopened, "search_tasks", "schedule_version") {
		t.Fatal("search_tasks.schedule_version must not be migrated back")
	}
	if exists, err := reopened.hasTable(ctx, "cdc_cutover_state"); err != nil || !exists {
		t.Fatalf("reopen did not recreate cdc_cutover_state: %v, %v", exists, err)
	}
	// The additive column defaults keep old rows schedulable and dirty.
	if _, err := reopened.DB.ExecContext(ctx,
		"INSERT INTO cache_revisions (kind, scope, revision) VALUES (?, ?, ?)", "documents", "legacy", 4); err != nil {
		t.Fatalf("insert legacy cache_revisions row error = %v", err)
	}
	var refreshed, attempts int64
	var lastError sql.NullString
	var updatedAt float64
	if err := reopened.DB.QueryRowContext(ctx,
		"SELECT refreshed_revision, attempts, last_error, updated_at FROM cache_revisions "+
			"WHERE kind = ? AND scope = ?", "documents", "legacy").
		Scan(&refreshed, &attempts, &lastError, &updatedAt); err != nil {
		t.Fatalf("read legacy cache_revisions row error = %v", err)
	}
	if refreshed != 0 || attempts != 0 || lastError.Valid || updatedAt != 0 {
		t.Fatalf("legacy cache_revisions row = refreshed %d, attempts %d, last_error %v, updated_at %v",
			refreshed, attempts, lastError, updatedAt)
	}
}

func TestListJSONNeverLeaksSecrets(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	created, err := stores.APIKeys.Create(ctx, "app", []string{"alpha"})
	if err != nil {
		t.Fatalf("APIKeys.Create() error = %v", err)
	}
	keys, err := stores.APIKeys.List(ctx)
	if err != nil {
		t.Fatalf("APIKeys.List() error = %v", err)
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), created.Key) {
		t.Fatalf("list JSON leaked the secret: %s", encoded)
	}
	description := stores.Describe()
	if _, containsSecret := description["key"]; containsSecret {
		t.Fatal("describe payload exposes a key")
	}
}

func TestOpenDatabaseUsesMySQL(t *testing.T) {
	stores := newTestStores(t)
	description := stores.Describe()
	if description["backend"] != "mysql" {
		t.Fatalf("backend = %v, want mysql", description["backend"])
	}
	if err := stores.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
}
