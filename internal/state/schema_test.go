package state

import (
	"context"
	"strings"
	"testing"
)

// expectedSchema is the literal port of src/rag_mcp/state/schema.py: the table
// order and every column name must stay identical for existing databases. The
// users/user_sessions (Console login) and model_profiles (Console saved model
// configurations) tables are Go-only additions and must stay after the ported
// tables so shared databases keep the Python order.
var expectedSchema = []struct {
	name    string
	columns []string
}{
	{"api_keys", []string{"id", "name", "prefix", "digest", "namespaces", "enabled", "created_at", "updated_at", "protected_key", "owner_user_id"}},
	{"knowledge_bases", []string{"namespace", "collection", "settings", "version", "created_at", "updated_at", "owner_user_id"}},
	{"documents", []string{"namespace", "name", "document_id", "size", "sha256", "chunking", "collection", "chunk_count", "modified_at", "indexed_at", "diagnostics", "skipped_object_count", "del_flag"}},
	{"blobs", []string{"namespace", "bucket", "key", "document_id", "content", "size", "sha256", "created_at"}},
	{"mcp_catalogs", []string{"namespace", "data", "version", "updated_at"}},
	{"upload_operations", []string{"id", "namespace", "name", "document_id", "collection", "idempotency_key", "fingerprint", "owner", "old_version", "status", "result", "error", "cleanup_pending", "workspace", "workspace_node", "created_at", "updated_at"}},
	{"upload_steps", []string{"operation_id", "name", "status", "details", "error", "started_at", "finished_at"}},
	{"document_heads", []string{"namespace", "document_id", "version", "updated_at"}},
	{"document_leases", []string{"namespace", "resource", "owner", "operation_id", "expires_at"}},
	{"upload_objects", []string{"operation_id", "bucket", "key", "content", "metadata", "created_at"}},
	{"cache_revisions", []string{"kind", "scope", "revision"}},
	{"cache_outbox", []string{"id", "kind", "scope", "revision", "status", "attempts", "retry_at", "created_at", "completed_at", "last_error"}},
	{"search_result_snapshots", []string{"id", "namespace", "content", "created_at", "expires_at"}},
	{"artifact_snapshots", []string{"id", "namespace", "identity", "document_id", "version", "kind", "name", "mime_type", "content", "created_at", "expires_at"}},
	{"document_tasks", []string{"artifact_id", "namespace", "name", "name_key", "content", "sha256", "idempotency_key", "configuration_version", "status", "stage", "attempts", "owner", "lease_until", "operation_id", "error", "created_at", "updated_at"}},
	{"document_task_outbox", []string{"artifact_id", "pending", "generation", "retry_at", "owner", "lease_until", "attempts", "last_error"}},
	{"document_jobs", []string{"artifact_id", "namespace", "name", "name_key", "content", "size", "sha256", "configuration_version", "idempotency_key", "status", "stage", "attempts", "owner", "execution_id", "lease_until", "operation_id", "retry_at", "error", "created_at", "updated_at"}},
	{"document_job_outbox", []string{"id", "artifact_id", "namespace", "name", "status", "attempts", "retry_at", "created_at", "published_at", "last_error"}},
	{"search_tasks", []string{"result_id", "namespace", "arguments", "status", "stage", "attempts", "owner", "lease_until", "retry_at", "deadline", "created_at", "updated_at"}},
	{"search_task_outbox", []string{"result_id", "pending", "generation", "retry_at"}},
	{"users", []string{"id", "username", "password_hash", "role", "enabled", "created_at", "updated_at", "last_login_at"}},
	{"user_sessions", []string{"id", "user_id", "token_digest", "csrf_token", "expires_at", "created_at", "last_seen_at", "revoked"}},
	{"kb_permissions", []string{"id", "namespace", "grantee_user_id", "document_name", "permission", "granted_by", "created_at", "updated_at"}},
	{"kb_invitations", []string{"id", "namespace", "inviter_user_id", "invitee_user_id", "permission", "scope", "document_names", "token_digest", "status", "created_at", "expires_at", "accepted_at", "updated_at"}},
	{"model_profiles", []string{"id", "kind", "name", "base_url", "model", "protected_key", "vector_size", "created_at", "updated_at"}},
}

var expectedPrimaryKeys = map[string][]string{
	"api_keys":                {"id"},
	"knowledge_bases":         {"namespace"},
	"documents":               {"namespace", "name"},
	"blobs":                   {"namespace", "bucket", "key"},
	"mcp_catalogs":            {"namespace"},
	"upload_operations":       {"id"},
	"upload_steps":            {"operation_id", "name"},
	"document_heads":          {"namespace", "document_id"},
	"document_leases":         {"namespace", "resource"},
	"upload_objects":          {"operation_id", "bucket", "key"},
	"cache_revisions":         {"kind", "scope"},
	"cache_outbox":            {"id"},
	"search_result_snapshots": {"id"},
	"artifact_snapshots":      {"id"},
	"document_tasks":          {"artifact_id"},
	"document_task_outbox":    {"artifact_id"},
	"document_jobs":           {"artifact_id"},
	"document_job_outbox":     {"id"},
	"search_tasks":            {"result_id"},
	"search_task_outbox":      {"result_id"},
	"users":                   {"id"},
	"user_sessions":           {"id"},
	"kb_permissions":          {"id"},
	"kb_invitations":          {"id"},
	"model_profiles":          {"id"},
}

var expectedUniques = map[string]string{
	"api_keys":           "uq_api_keys_digest",
	"documents":          "uq_documents_namespace_id",
	"upload_operations":  "uq_upload_idempotency",
	"artifact_snapshots": "uq_artifact_identity",
	"document_tasks":     "uq_document_task_idempotency",
	"document_jobs":      "uq_document_job_idempotency",
	"users":              "uq_users_username",
	"user_sessions":      "uq_user_sessions_token_digest",
	"kb_permissions":     "uq_kb_permissions_scope",
	"kb_invitations":     "uq_kb_invitations_token_digest",
	"model_profiles":     "uq_model_profiles_kind_name",
}

var expectedIndexes = map[string][]string{
	"api_keys":                {"ix_api_keys_owner"},
	"knowledge_bases":         {"ix_knowledge_bases_collection"},
	"documents":               {"ix_documents_namespace_sha256"},
	"blobs":                   {"ix_blobs_document"},
	"upload_operations":       {"ix_upload_unfinished"},
	"cache_outbox":            {"ix_cache_outbox_pending"},
	"search_result_snapshots": {"ix_search_result_expiry", "ix_search_result_namespace"},
	"artifact_snapshots":      {"ix_artifact_expiry", "ix_artifact_namespace"},
	"document_tasks":          {"ix_document_tasks_name", "ix_document_tasks_recovery"},
	"document_task_outbox":    {"ix_document_task_dispatch"},
	"document_jobs":           {"ix_document_job_name", "ix_document_job_recovery"},
	"document_job_outbox":     {"ix_document_job_outbox_pending", "ix_document_job_outbox_artifact", "ix_document_job_outbox_age"},
	"search_tasks":            {"ix_search_tasks_deadline", "ix_search_tasks_recovery", "ix_search_tasks_namespace"},
	"search_task_outbox":      {"ix_search_task_dispatch"},
	"user_sessions":           {"ix_user_sessions_user_id"},
	"kb_permissions":          {"ix_kb_permissions_grantee"},
	"kb_invitations":          {"ix_kb_invitations_invitee", "ix_kb_invitations_namespace"},
	"model_profiles":          {"ix_model_profiles_kind"},
}

// expectedTableNames returns the schema table names in Python declaration order.
func expectedTableNames() []string {
	names := make([]string, len(expectedSchema))
	for index, item := range expectedSchema {
		names[index] = item.name
	}
	return names
}

func TestSchemaMatchesPythonDefinition(t *testing.T) {
	tables := SchemaTables()
	if len(tables) != len(expectedSchema) {
		t.Fatalf("schema has %d tables, want %d", len(tables), len(expectedSchema))
	}
	for index, expected := range expectedSchema {
		table := tables[index]
		if table.Name != expected.name {
			t.Fatalf("table[%d] = %q, want %q", index, table.Name, expected.name)
		}
		actual := table.ColumnNames()
		if len(actual) != len(expected.columns) {
			t.Fatalf("%s has %d columns, want %d (%v)", table.Name, len(actual), len(expected.columns), actual)
		}
		for position, column := range expected.columns {
			if actual[position] != column {
				t.Fatalf("%s column[%d] = %q, want %q", table.Name, position, actual[position], column)
			}
		}
		wantPK := expectedPrimaryKeys[table.Name]
		actualPK := table.PrimaryKey()
		if len(actualPK) != len(wantPK) {
			t.Fatalf("%s primary key = %v, want %v", table.Name, actualPK, wantPK)
		}
		for position, column := range wantPK {
			if actualPK[position] != column {
				t.Fatalf("%s primary key[%d] = %q, want %q", table.Name, position, actualPK[position], column)
			}
		}
		wantUnique, hasUnique := expectedUniques[table.Name]
		if !hasUnique && len(table.Uniques) != 0 {
			t.Fatalf("%s unexpectedly has unique constraints %v", table.Name, table.Uniques)
		}
		if hasUnique {
			if len(table.Uniques) != 1 || table.Uniques[0].Name != wantUnique {
				t.Fatalf("%s unique constraints = %v, want %q", table.Name, table.Uniques, wantUnique)
			}
		}
		wantIndexes := expectedIndexes[table.Name]
		if len(table.Indexes) != len(wantIndexes) {
			t.Fatalf("%s indexes = %v, want %v", table.Name, table.Indexes, wantIndexes)
		}
		for position, name := range wantIndexes {
			if table.Indexes[position].Name != name {
				t.Fatalf("%s index[%d] = %q, want %q", table.Name, position, table.Indexes[position].Name, name)
			}
		}
	}
	if _, found := TableByName("documents"); !found {
		t.Fatal("TableByName(documents) did not find the table")
	}
}

func TestMySQLSchemaCompilesWithLongColumns(t *testing.T) {
	var builder strings.Builder
	for _, table := range SchemaTables() {
		builder.WriteString(table.DDL())
		builder.WriteString("\n")
		for _, statement := range table.IndexDDL() {
			builder.WriteString(statement)
			builder.WriteString("\n")
		}
	}
	ddl := builder.String()
	for _, want := range []string{
		"LONGBLOB",
		"LONGTEXT",
		"utf8mb4_bin",
		"ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin",
		"`key` VARCHAR(512) COLLATE utf8mb4_bin NOT NULL",
		"`del_flag` BOOL NOT NULL DEFAULT 0",
		"CONSTRAINT `uq_documents_namespace_id` UNIQUE (`namespace`, `document_id`)",
	} {
		if !strings.Contains(ddl, want) {
			t.Fatalf("MySQL DDL does not contain %q", want)
		}
	}
}

func TestBucketConstants(t *testing.T) {
	if SourceBucket != "source" || PDFAssetBucket != "pdf-assets" || WordAssetBucket != "word-assets" {
		t.Fatalf("unexpected bucket constants: %q %q %q", SourceBucket, PDFAssetBucket, WordAssetBucket)
	}
}

func TestCreatedSchemaContainsEveryTableAndIndex(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	for _, name := range expectedTableNames() {
		exists, err := stores.hasTable(ctx, name)
		if err != nil {
			t.Fatalf("hasTable(%q) error = %v", name, err)
		}
		if !exists {
			t.Fatalf("table %q was not created", name)
		}
	}

	rows, err := stores.DB.QueryContext(ctx,
		"SELECT DISTINCT index_name FROM information_schema.statistics WHERE table_schema = DATABASE()")
	if err != nil {
		t.Fatalf("information_schema index query error = %v", err)
	}
	defer closeRows(t, rows)
	created := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index name error = %v", err)
		}
		created[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err() = %v", err)
	}
	for _, names := range expectedIndexes {
		for _, name := range names {
			if !created[name] {
				t.Fatalf("index %q was not created (created: %v)", name, created)
			}
		}
	}
}
