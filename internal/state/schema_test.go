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
	{"document_leases", []string{"namespace", "resource", "owner", "operation_id", "expires_at", "fencing_epoch"}},
	{"upload_objects", []string{"operation_id", "bucket", "key", "content", "content_hash", "metadata", "created_at"}},
	{"cache_revisions", []string{"kind", "scope", "revision", "refreshed_revision", "attempts", "last_error", "updated_at"}},
	{"search_result_snapshots", []string{"id", "namespace", "content", "created_at", "expires_at"}},
	{"artifact_snapshots", []string{"id", "namespace", "identity", "document_id", "version", "kind", "name", "mime_type", "content", "created_at", "expires_at"}},
	{"document_tasks", []string{"artifact_id", "namespace", "name", "name_key", "content", "sha256", "idempotency_key", "configuration_version", "status", "stage", "attempts", "owner", "lease_until", "operation_id", "error", "created_at", "updated_at"}},
	{"document_jobs", []string{"artifact_id", "namespace", "name", "name_key", "content", "size", "sha256", "configuration_version", "idempotency_key", "status", "stage", "attempts", "owner", "execution_id", "lease_until", "operation_id", "retry_at", "error", "created_at", "updated_at", "schedule_version"}},
	{"search_tasks", []string{"result_id", "namespace", "arguments", "status", "stage", "attempts", "owner", "lease_until", "retry_at", "deadline", "created_at", "updated_at"}},
	{"users", []string{"id", "username", "password_hash", "role", "enabled", "created_at", "updated_at", "last_login_at", "email", "email_verified", "age", "gender"}},
	{"user_sessions", []string{"id", "user_id", "token_digest", "csrf_token", "expires_at", "created_at", "last_seen_at", "revoked"}},
	{"kb_permissions", []string{"id", "namespace", "grantee_user_id", "document_name", "permission", "allow_mask", "deny_mask", "granted_by", "created_at", "updated_at"}},
	{"kb_invitations", []string{"id", "namespace", "inviter_user_id", "invitee_user_id", "permission", "allow_mask", "scope", "document_names", "token_digest", "status", "created_at", "expires_at", "accepted_at", "updated_at"}},
	{"model_profiles", []string{"id", "kind", "name", "base_url", "model", "protected_key", "vector_size", "created_at", "updated_at", "owner_user_id"}},
	{"document_batches", []string{"id", "namespace", "owner", "idempotency_key", "status", "total", "accepted", "skipped", "failed", "created_at", "updated_at"}},
	{"document_batch_items", []string{"batch_id", "position", "name", "status", "reason", "artifact_id", "error", "created_at", "updated_at"}},
	{"user_avatars", []string{"user_id", "content_type", "size", "sha256", "content", "updated_at"}},
	{"email_verification_codes", []string{"id", "email", "purpose", "code_digest", "attempts", "consumed", "expires_at", "created_at"}},
	{"app_settings", []string{"setting_key", "setting_value", "updated_at"}},
	// Go-only monitoring tables (Console 监控页的活动计数与 Worker 心跳); they
	// are appended after the ported tables so migrated databases keep the order.
	{"activity_counters", []string{"metric", "window_start", "count"}},
	{"runtime_heartbeats", []string{"role", "instance", "updated_at"}},
	// Go-only CDC cutover bookkeeping table, appended last like other Go-only
	// additions.
	{"cdc_cutover_state", []string{"k", "v"}},
	// content_objects is the content-addressed byte store referenced by
	// upload_objects.content_hash and blobs.sha256.
	{"content_objects", []string{"hash", "size", "content", "created_at", "retired_at"}},
	// feishu_connections / feishu_oauth_states are Go-only additions for the
	// optional Feishu document import, appended after the ported tables.
	{"feishu_connections", []string{"user_id", "open_id", "union_id", "name", "avatar_url", "access_token", "refresh_token", "access_expires_at", "refresh_expires_at", "scope", "created_at", "updated_at"}},
	{"feishu_oauth_states", []string{"state_digest", "user_id", "redirect_uri", "expires_at", "consumed", "created_at"}},
	// Personal membership, quota ledger and Alipay order state (Go-only
	// additions appended after the ported tables).
	{"user_memberships", []string{"user_id", "plan", "source", "starts_at", "expires_at", "anchor_day", "entitlement_version", "created_at", "updated_at"}},
	{"membership_terms", []string{"id", "user_id", "order_id", "plan", "billing_period", "kind", "starts_at", "ends_at", "anchor_day", "status", "created_at", "updated_at"}},
	{"billing_orders", []string{"id", "user_id", "plan", "billing_period", "kind", "status", "amount_cents", "currency", "quote", "product_snapshot", "entitlement_version", "term_start", "term_end", "alipay_trade_no", "expires_at", "paid_at", "closed_at", "refund_request_no", "refund_amount_cents", "refund_status", "refund_reason", "manual_note", "created_at", "updated_at"}},
	{"embedding_quota_periods", []string{"id", "user_id", "source", "period_start", "period_end", "plan", "allowance_chars", "used_chars", "reserved_chars", "created_at", "updated_at"}},
	{"embedding_usage", []string{"id", "operation_id", "user_id", "quota_period_id", "namespace", "document_name", "document_version", "model", "purpose", "reserved_chars", "used_chars", "status", "expires_at", "created_at", "updated_at"}},
	{"billing_events", []string{"id", "order_id", "event_key", "event_type", "alipay_trade_no", "amount_cents", "detail", "created_at"}},
	// lightrag_documents is the knowledge-graph mirror ledger (Go-only), the
	// register of which documents the LightRAG sidecar currently holds.
	{"lightrag_documents", []string{"namespace", "document_id", "document_name", "config_fingerprint", "chunk_count", "status", "attempts", "last_error", "indexed_at", "updated_at"}},
	// SAG event-entity index (retrieval mode sag): one event per chunk plus
	// normalized entities and their associations.
	{"sag_events", []string{"id", "namespace", "document_id", "chunk_id", "event", "version", "created_at"}},
	{"sag_entities", []string{"id", "namespace", "normalized_key", "entity_key", "display_name", "mention_count", "created_at", "updated_at"}},
	{"sag_event_entities", []string{"event_id", "entity_id", "namespace", "created_at"}},
}

var expectedPrimaryKeys = map[string][]string{
	"api_keys":                 {"id"},
	"knowledge_bases":          {"namespace"},
	"documents":                {"namespace", "name"},
	"blobs":                    {"namespace", "bucket", "key"},
	"mcp_catalogs":             {"namespace"},
	"upload_operations":        {"id"},
	"upload_steps":             {"operation_id", "name"},
	"document_heads":           {"namespace", "document_id"},
	"document_leases":          {"namespace", "resource"},
	"upload_objects":           {"operation_id", "bucket", "key"},
	"cache_revisions":          {"kind", "scope"},
	"search_result_snapshots":  {"id"},
	"artifact_snapshots":       {"id"},
	"document_tasks":           {"artifact_id"},
	"document_jobs":            {"artifact_id"},
	"search_tasks":             {"result_id"},
	"users":                    {"id"},
	"user_sessions":            {"id"},
	"kb_permissions":           {"id"},
	"kb_invitations":           {"id"},
	"model_profiles":           {"id"},
	"document_batches":         {"id"},
	"document_batch_items":     {"batch_id", "position"},
	"user_avatars":             {"user_id"},
	"email_verification_codes": {"id"},
	"app_settings":             {"setting_key"},
	"activity_counters":        {"metric", "window_start"},
	"runtime_heartbeats":       {"role", "instance"},
	"cdc_cutover_state":        {"k"},
	"content_objects":          {"hash"},
	"feishu_connections":       {"user_id"},
	"feishu_oauth_states":      {"state_digest"},
	"user_memberships":         {"user_id"},
	"membership_terms":         {"id"},
	"billing_orders":           {"id"},
	"embedding_quota_periods":  {"id"},
	"embedding_usage":          {"id"},
	"billing_events":           {"id"},
	"lightrag_documents":       {"namespace", "document_id"},
	"sag_events":               {"id"},
	"sag_entities":             {"id"},
	"sag_event_entities":       {"event_id", "entity_id"},
}

var expectedUniques = map[string][]string{
	"api_keys":                {"uq_api_keys_digest"},
	"documents":               {"uq_documents_namespace_id"},
	"upload_operations":       {"uq_upload_idempotency"},
	"artifact_snapshots":      {"uq_artifact_identity"},
	"document_tasks":          {"uq_document_task_idempotency"},
	"document_jobs":           {"uq_document_job_idempotency"},
	"users":                   {"uq_users_username", "uq_users_email"},
	"user_sessions":           {"uq_user_sessions_token_digest"},
	"kb_permissions":          {"uq_kb_permissions_scope"},
	"kb_invitations":          {"uq_kb_invitations_token_digest"},
	"model_profiles":          {"uq_model_profiles_owner_kind_name"},
	"document_batches":        {"uq_document_batch_idempotency"},
	"membership_terms":        {"uq_membership_terms_order"},
	"billing_orders":          {"uq_billing_orders_trade", "uq_billing_orders_refund"},
	"embedding_quota_periods": {"uq_embedding_period"},
	"embedding_usage":         {"uq_embedding_usage_operation"},
	"billing_events":          {"uq_billing_events_key"},
	"sag_events":              {"uq_sag_events_chunk"},
	"sag_entities":            {"uq_sag_entities_key"},
}

var expectedIndexes = map[string][]string{
	"api_keys":                 {"ix_api_keys_owner"},
	"knowledge_bases":          {"ix_knowledge_bases_collection"},
	"documents":                {"ix_documents_namespace_sha256"},
	"blobs":                    {"ix_blobs_document", "ix_blobs_sha256"},
	"upload_objects":           {"ix_upload_objects_content"},
	"content_objects":          {"ix_content_objects_retired"},
	"upload_operations":        {"ix_upload_unfinished"},
	"search_result_snapshots":  {"ix_search_result_expiry", "ix_search_result_namespace"},
	"artifact_snapshots":       {"ix_artifact_expiry", "ix_artifact_namespace"},
	"document_tasks":           {"ix_document_tasks_name", "ix_document_tasks_recovery"},
	"document_jobs":            {"ix_document_job_name", "ix_document_job_recovery"},
	"search_tasks":             {"ix_search_tasks_deadline", "ix_search_tasks_recovery", "ix_search_tasks_namespace"},
	"user_sessions":            {"ix_user_sessions_user_id"},
	"kb_permissions":           {"ix_kb_permissions_grantee"},
	"kb_invitations":           {"ix_kb_invitations_invitee", "ix_kb_invitations_namespace"},
	"model_profiles":           {"ix_model_profiles_kind", "ix_model_profiles_owner"},
	"document_batches":         {"ix_document_batch_active", "ix_document_batch_owner"},
	"document_batch_items":     {"ix_document_batch_item_artifact"},
	"email_verification_codes": {"ix_email_codes_lookup", "ix_email_codes_expiry"},
	"runtime_heartbeats":       {"ix_runtime_heartbeats_role"},
	"feishu_oauth_states":      {"ix_feishu_oauth_states_user", "ix_feishu_oauth_states_expiry"},
	"membership_terms":         {"ix_membership_terms_user"},
	"billing_orders":           {"ix_billing_orders_user", "ix_billing_orders_status"},
	"embedding_quota_periods":  {"ix_embedding_period_user"},
	"embedding_usage":          {"ix_embedding_usage_user", "ix_embedding_usage_review"},
	"billing_events":           {"ix_billing_events_order"},
	"lightrag_documents":       {"ix_lightrag_documents_status"},
	"sag_events":               {"ix_sag_events_document", "ix_sag_events_version"},
	"sag_entities":             {"ix_sag_entities_normalized"},
	"sag_event_entities":       {"ix_sag_event_entities_entity", "ix_sag_event_entities_event"},
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
		wantUniques := expectedUniques[table.Name]
		if len(wantUniques) == 0 && len(table.Uniques) != 0 {
			t.Fatalf("%s unexpectedly has unique constraints %v", table.Name, table.Uniques)
		}
		if len(table.Uniques) != len(wantUniques) {
			t.Fatalf("%s unique constraints = %v, want %v", table.Name, table.Uniques, wantUniques)
		}
		for position, name := range wantUniques {
			if table.Uniques[position].Name != name {
				t.Fatalf("%s unique[%d] = %q, want %q", table.Name, position, table.Uniques[position].Name, name)
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
	if SourceBucket != "source" || PDFAssetBucket != "pdf-assets" ||
		WordAssetBucket != "word-assets" || HTMLAssetBucket != "html-assets" {
		t.Fatalf("unexpected bucket constants: %q %q %q %q",
			SourceBucket, PDFAssetBucket, WordAssetBucket, HTMLAssetBucket)
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
