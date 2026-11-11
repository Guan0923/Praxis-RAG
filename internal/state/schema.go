// Package state is the Go port of rag_mcp.state: the authoritative shared
// state database schema, its repositories, credential protection, and the MySQL
// store assembly.
//
// MySQL is the only supported backend. The schema keeps table names, column
// names, primary keys, unique constraints, index names, MySQL table options
// (InnoDB/utf8mb4/utf8mb4_bin) and LONGTEXT/LONGBLOB types identical to the
// Python implementation so existing databases can be read in place.
package state

import (
	"fmt"
	"strings"
)

// ColumnKind is the storage class of a schema column.
type ColumnKind int

const (
	// ColumnVarchar is a bounded string; Length applies.
	ColumnVarchar ColumnKind = iota
	// ColumnText is unbounded text (LONGTEXT on MySQL when Large).
	ColumnText
	// ColumnBlob is a byte string (LONGBLOB on MySQL).
	ColumnBlob
	// ColumnBoolean is a BOOL/BOOLEAN column.
	ColumnBoolean
	// ColumnBigInt is a 64-bit signed integer.
	ColumnBigInt
	// ColumnInteger is a 32-bit signed integer.
	ColumnInteger
	// ColumnDouble is a 64-bit float.
	ColumnDouble
)

// SchemaColumn describes one table column.
type SchemaColumn struct {
	Name string
	Kind ColumnKind
	// Length is the VARCHAR length for ColumnVarchar.
	Length int
	// CaseSensitive emits an explicit utf8mb4_bin collation, mirroring
	// schema.py::_ci_string.
	CaseSensitive bool
	// Large selects LONGTEXT/LONGBLOB instead of TEXT/BLOB, mirroring
	// _long_text()/_long_blob().
	Large bool
	// AllowNull marks nullable columns (all others are NOT NULL).
	AllowNull bool
	// PrimaryKey marks primary-key columns (composite keys mark every part).
	PrimaryKey bool
	// ServerDefault is a literal DEFAULT clause, only used where schema.py sets
	// server_default (legacy "0" defaults).
	ServerDefault string
}

// Null returns the column marked nullable.
func (c SchemaColumn) Null() SchemaColumn {
	c.AllowNull = true
	return c
}

// PK returns the column marked as (part of) the primary key.
func (c SchemaColumn) PK() SchemaColumn {
	c.PrimaryKey = true
	return c
}

// Default returns the column with a literal server-side default.
func (c SchemaColumn) Default(literal string) SchemaColumn {
	c.ServerDefault = literal
	return c
}

// SchemaIndex is a named secondary index.
type SchemaIndex struct {
	Name    string
	Columns []string
}

// SchemaUnique is a named unique constraint.
type SchemaUnique struct {
	Name    string
	Columns []string
}

// SchemaTable is one table definition rendered as MySQL DDL.
type SchemaTable struct {
	Name    string
	Columns []SchemaColumn
	Uniques []SchemaUnique
	Indexes []SchemaIndex
}

// PrimaryKey returns the primary-key column names in declaration order.
func (t *SchemaTable) PrimaryKey() []string {
	columns := make([]string, 0, 2)
	for _, column := range t.Columns {
		if column.PrimaryKey {
			columns = append(columns, column.Name)
		}
	}
	return columns
}

// Column returns the column definition with the given name.
func (t *SchemaTable) Column(name string) (SchemaColumn, bool) {
	for _, column := range t.Columns {
		if column.Name == name {
			return column, true
		}
	}
	return SchemaColumn{}, false
}

// Columns returns the column names in declaration order.
func (t *SchemaTable) ColumnNames() []string {
	names := make([]string, len(t.Columns))
	for index, column := range t.Columns {
		names[index] = column.Name
	}
	return names
}

func quoteIdentifier(name string) string {
	return "`" + name + "`"
}

func (c SchemaColumn) sqlType() string {
	switch c.Kind {
	case ColumnVarchar:
		if c.CaseSensitive {
			return fmt.Sprintf("VARCHAR(%d) COLLATE utf8mb4_bin", c.Length)
		}
		return fmt.Sprintf("VARCHAR(%d)", c.Length)
	case ColumnText:
		if c.Large {
			return "LONGTEXT"
		}
		return "TEXT"
	case ColumnBlob:
		return "LONGBLOB"
	case ColumnBoolean:
		return "BOOL"
	case ColumnBigInt:
		return "BIGINT"
	case ColumnInteger:
		return "INTEGER"
	case ColumnDouble:
		return "DOUBLE"
	default:
		return "TEXT"
	}
}

func (c SchemaColumn) ddl() string {
	var builder strings.Builder
	builder.WriteString(quoteIdentifier(c.Name))
	builder.WriteString(" ")
	builder.WriteString(c.sqlType())
	if !c.AllowNull {
		builder.WriteString(" NOT NULL")
	}
	if c.ServerDefault != "" {
		builder.WriteString(" DEFAULT ")
		builder.WriteString(c.ServerDefault)
	}
	return builder.String()
}

// DDL renders the CREATE TABLE statement.
func (t *SchemaTable) DDL() string {
	parts := make([]string, 0, len(t.Columns)+len(t.Uniques)+1)
	for _, column := range t.Columns {
		parts = append(parts, "  "+column.ddl())
	}
	if primary := t.PrimaryKey(); len(primary) > 0 {
		quoted := make([]string, len(primary))
		for index, name := range primary {
			quoted[index] = quoteIdentifier(name)
		}
		parts = append(parts, "  PRIMARY KEY ("+strings.Join(quoted, ", ")+")")
	}
	for _, unique := range t.Uniques {
		quoted := make([]string, len(unique.Columns))
		for index, name := range unique.Columns {
			quoted[index] = quoteIdentifier(name)
		}
		parts = append(parts, "  CONSTRAINT "+quoteIdentifier(unique.Name)+" UNIQUE ("+strings.Join(quoted, ", ")+")")
	}
	return "CREATE TABLE " + quoteIdentifier(t.Name) + " (\n" + strings.Join(parts, ",\n") + "\n)" +
		" ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"
}

// IndexDDL renders the CREATE INDEX statements.
func (t *SchemaTable) IndexDDL() []string {
	statements := make([]string, 0, len(t.Indexes))
	for _, index := range t.Indexes {
		quoted := make([]string, len(index.Columns))
		for position, name := range index.Columns {
			quoted[position] = quoteIdentifier(name)
		}
		statements = append(statements, "CREATE INDEX "+quoteIdentifier(index.Name)+
			" ON "+quoteIdentifier(t.Name)+" ("+strings.Join(quoted, ", ")+")")
	}
	return statements
}

// Bucket names used by the blob store. They are part of the persisted data
// contract, so they live with the schema like schema.py.
const (
	SourceBucket    = "source"
	PDFAssetBucket  = "pdf-assets"
	WordAssetBucket = "word-assets"
)

func varchar(name string, length int) SchemaColumn {
	return SchemaColumn{Name: name, Kind: ColumnVarchar, Length: length}
}

// identifier mirrors _ci_string: a case-sensitive identifier column in MySQL.
func identifier(name string, length int) SchemaColumn {
	return SchemaColumn{Name: name, Kind: ColumnVarchar, Length: length, CaseSensitive: true}
}

func text(name string) SchemaColumn {
	return SchemaColumn{Name: name, Kind: ColumnText}
}

func longText(name string) SchemaColumn {
	return SchemaColumn{Name: name, Kind: ColumnText, Large: true}
}

func longBlob(name string) SchemaColumn {
	return SchemaColumn{Name: name, Kind: ColumnBlob, Large: true}
}

func boolean(name string) SchemaColumn {
	return SchemaColumn{Name: name, Kind: ColumnBoolean}
}

func bigint(name string) SchemaColumn {
	return SchemaColumn{Name: name, Kind: ColumnBigInt}
}

func integer(name string) SchemaColumn {
	return SchemaColumn{Name: name, Kind: ColumnInteger}
}

func double(name string) SchemaColumn {
	return SchemaColumn{Name: name, Kind: ColumnDouble}
}

func unique(name string, columns ...string) SchemaUnique {
	return SchemaUnique{Name: name, Columns: columns}
}

func index(name string, columns ...string) SchemaIndex {
	return SchemaIndex{Name: name, Columns: columns}
}

// schemaTables is the ordered port of schema.py::metadata. Tables are listed in
// the same order as the Python module; no table has foreign keys, so this is
// also the dependency-free creation order.
var schemaTables = []*SchemaTable{
	{
		Name: "api_keys",
		Columns: []SchemaColumn{
			identifier("id", 32).PK(),
			varchar("name", 80),
			varchar("prefix", 16),
			identifier("digest", 64),
			text("namespaces"),
			boolean("enabled"),
			varchar("created_at", 40),
			varchar("updated_at", 40),
			text("protected_key").Null(),
			// owner_user_id is the Console account that issued the key. NULL
			// means a credential created before ownership existed (or by the
			// trusted local management surface); those stay administrator-only.
			identifier("owner_user_id", 32).Null(),
		},
		Uniques: []SchemaUnique{unique("uq_api_keys_digest", "digest")},
		Indexes: []SchemaIndex{index("ix_api_keys_owner", "owner_user_id")},
	},
	{
		Name: "knowledge_bases",
		Columns: []SchemaColumn{
			identifier("namespace", 64).PK(),
			identifier("collection", 128),
			longText("settings"),
			bigint("version"),
			varchar("created_at", 40),
			varchar("updated_at", 40),
			// owner_user_id is the Console account that created the knowledge
			// base. It is appended after the ported columns so a migrated
			// database and a freshly created one have the same column order.
			// NULL means "unowned" (a row created before ownership existed).
			identifier("owner_user_id", 32).Null(),
		},
		Indexes: []SchemaIndex{index("ix_knowledge_bases_collection", "collection")},
	},
	{
		Name: "documents",
		Columns: []SchemaColumn{
			identifier("namespace", 64).PK(),
			identifier("name", 200).PK(),
			identifier("document_id", 64),
			bigint("size"),
			identifier("sha256", 64),
			text("chunking"),
			varchar("collection", 128),
			integer("chunk_count"),
			varchar("modified_at", 40),
			varchar("indexed_at", 40),
			longText("diagnostics"),
			integer("skipped_object_count"),
			boolean("del_flag").Default("0"),
		},
		Uniques: []SchemaUnique{unique("uq_documents_namespace_id", "namespace", "document_id")},
		Indexes: []SchemaIndex{index("ix_documents_namespace_sha256", "namespace", "sha256")},
	},
	{
		Name: "blobs",
		Columns: []SchemaColumn{
			identifier("namespace", 64).PK(),
			identifier("bucket", 32).PK(),
			identifier("key", 512).PK(),
			identifier("document_id", 64).Null(),
			longBlob("content"),
			bigint("size"),
			identifier("sha256", 64),
			varchar("created_at", 40),
		},
		Indexes: []SchemaIndex{index("ix_blobs_document", "namespace", "bucket", "document_id")},
	},
	{
		Name: "mcp_catalogs",
		Columns: []SchemaColumn{
			identifier("namespace", 64).PK(),
			longText("data"),
			bigint("version"),
			varchar("updated_at", 40),
		},
	},
	{
		Name: "upload_operations",
		Columns: []SchemaColumn{
			identifier("id", 32).PK(),
			identifier("namespace", 64),
			identifier("name", 200),
			identifier("document_id", 64),
			identifier("collection", 128),
			identifier("idempotency_key", 200).Null(),
			identifier("fingerprint", 128).Null(),
			identifier("owner", 32),
			identifier("old_version", 32).Null(),
			identifier("status", 32),
			longText("result").Null(),
			text("error").Null(),
			boolean("cleanup_pending"),
			text("workspace").Null(),
			identifier("workspace_node", 255).Null(),
			double("created_at"),
			double("updated_at"),
		},
		Uniques: []SchemaUnique{unique("uq_upload_idempotency", "namespace", "idempotency_key")},
		Indexes: []SchemaIndex{index("ix_upload_unfinished", "namespace", "status")},
	},
	{
		Name: "upload_steps",
		Columns: []SchemaColumn{
			identifier("operation_id", 32).PK(),
			identifier("name", 128).PK(),
			identifier("status", 32),
			longText("details"),
			text("error").Null(),
			double("started_at"),
			double("finished_at").Null(),
		},
	},
	{
		Name: "document_heads",
		Columns: []SchemaColumn{
			identifier("namespace", 64).PK(),
			identifier("document_id", 64).PK(),
			identifier("version", 32),
			double("updated_at"),
		},
	},
	{
		Name: "document_leases",
		Columns: []SchemaColumn{
			identifier("namespace", 64).PK(),
			identifier("resource", 80).PK(),
			identifier("owner", 32),
			identifier("operation_id", 32).Null(),
			double("expires_at"),
		},
	},
	{
		Name: "upload_objects",
		Columns: []SchemaColumn{
			identifier("operation_id", 32).PK(),
			identifier("bucket", 32).PK(),
			identifier("key", 512).PK(),
			longBlob("content"),
			longText("metadata").Null(),
			double("created_at"),
		},
	},
	{
		Name: "cache_revisions",
		Columns: []SchemaColumn{
			identifier("kind", 32).PK(),
			identifier("scope", 64).PK(),
			bigint("revision"),
		},
	},
	{
		Name: "cache_outbox",
		Columns: []SchemaColumn{
			identifier("id", 32).PK(),
			identifier("kind", 32),
			identifier("scope", 64),
			bigint("revision"),
			identifier("status", 24),
			integer("attempts"),
			double("retry_at"),
			double("created_at"),
			double("completed_at").Null(),
			identifier("last_error", 128).Null(),
		},
		Indexes: []SchemaIndex{index("ix_cache_outbox_pending", "status", "retry_at")},
	},
	{
		Name: "search_result_snapshots",
		Columns: []SchemaColumn{
			identifier("id", 36).PK(),
			identifier("namespace", 64),
			longText("content"),
			double("created_at"),
			double("expires_at"),
		},
		Indexes: []SchemaIndex{
			index("ix_search_result_expiry", "expires_at"),
			index("ix_search_result_namespace", "namespace"),
		},
	},
	{
		Name: "artifact_snapshots",
		Columns: []SchemaColumn{
			identifier("id", 36).PK(),
			identifier("namespace", 64),
			identifier("identity", 64),
			identifier("document_id", 64),
			identifier("version", 64).Null(),
			identifier("kind", 32),
			identifier("name", 512),
			varchar("mime_type", 128),
			longBlob("content"),
			double("created_at"),
			double("expires_at"),
		},
		Uniques: []SchemaUnique{unique("uq_artifact_identity", "namespace", "identity")},
		Indexes: []SchemaIndex{
			index("ix_artifact_expiry", "expires_at"),
			index("ix_artifact_namespace", "namespace"),
		},
	},
	// Retired tables: retained so existing databases keep their history, but
	// they are no longer read or written. The active path is document_jobs +
	// document_job_outbox (MySQL is the source of truth, Kafka carries a
	// notification only).
	{
		Name: "document_tasks",
		Columns: []SchemaColumn{
			identifier("artifact_id", 36).PK(),
			identifier("namespace", 64),
			identifier("name", 200),
			identifier("name_key", 64),
			longBlob("content"),
			identifier("sha256", 64),
			identifier("idempotency_key", 128).Null(),
			bigint("configuration_version"),
			identifier("status", 24),
			identifier("stage", 64),
			integer("attempts"),
			identifier("owner", 32).Null(),
			double("lease_until"),
			identifier("operation_id", 32).Null(),
			text("error").Null(),
			double("created_at"),
			double("updated_at"),
		},
		Uniques: []SchemaUnique{unique("uq_document_task_idempotency", "namespace", "idempotency_key")},
		Indexes: []SchemaIndex{
			index("ix_document_tasks_name", "namespace", "name_key", "status"),
			index("ix_document_tasks_recovery", "status", "lease_until"),
		},
	},
	{
		Name: "document_task_outbox",
		Columns: []SchemaColumn{
			identifier("artifact_id", 36).PK(),
			boolean("pending"),
			bigint("generation"),
			double("retry_at"),
			identifier("owner", 32).Null(),
			double("lease_until").Default("0"),
			integer("attempts").Default("0"),
			text("last_error").Null(),
		},
		Indexes: []SchemaIndex{index("ix_document_task_dispatch", "pending", "retry_at")},
	},
	{
		Name: "document_jobs",
		Columns: []SchemaColumn{
			identifier("artifact_id", 36).PK(),
			identifier("namespace", 64),
			identifier("name", 200),
			identifier("name_key", 64),
			// Cleared once the bytes are durably published to the source bucket,
			// so a finished job never keeps a second copy of the uploaded file.
			longBlob("content").Null(),
			bigint("size"),
			identifier("sha256", 64),
			bigint("configuration_version"),
			identifier("idempotency_key", 128).Null(),
			identifier("status", 24),
			identifier("stage", 64),
			integer("attempts").Default("0"),
			identifier("owner", 32).Null(),
			identifier("execution_id", 32).Null(),
			double("lease_until").Default("0"),
			identifier("operation_id", 32).Null(),
			double("retry_at").Default("0"),
			text("error").Null(),
			double("created_at"),
			double("updated_at"),
		},
		Uniques: []SchemaUnique{unique("uq_document_job_idempotency", "namespace", "idempotency_key")},
		Indexes: []SchemaIndex{
			index("ix_document_job_name", "namespace", "name_key", "status"),
			index("ix_document_job_recovery", "status", "lease_until"),
		},
	},
	{
		Name: "document_job_outbox",
		Columns: []SchemaColumn{
			identifier("id", 32).PK(),
			identifier("artifact_id", 36),
			identifier("namespace", 64),
			identifier("name", 200),
			identifier("status", 16),
			integer("attempts").Default("0"),
			double("retry_at").Default("0"),
			double("created_at"),
			double("published_at").Null(),
			identifier("last_error", 128).Null(),
		},
		Indexes: []SchemaIndex{
			index("ix_document_job_outbox_pending", "status", "retry_at"),
			index("ix_document_job_outbox_artifact", "artifact_id"),
			index("ix_document_job_outbox_age", "created_at"),
		},
	},
	{
		Name: "search_tasks",
		Columns: []SchemaColumn{
			identifier("result_id", 36).PK(),
			identifier("namespace", 64),
			longText("arguments"),
			identifier("status", 24),
			identifier("stage", 24),
			integer("attempts"),
			identifier("owner", 32).Null(),
			double("lease_until"),
			double("retry_at"),
			double("deadline"),
			double("created_at"),
			double("updated_at"),
		},
		Indexes: []SchemaIndex{
			index("ix_search_tasks_deadline", "status", "deadline"),
			index("ix_search_tasks_recovery", "status", "lease_until", "updated_at"),
			index("ix_search_tasks_namespace", "namespace"),
		},
	},
	{
		Name: "search_task_outbox",
		Columns: []SchemaColumn{
			identifier("result_id", 36).PK(),
			boolean("pending"),
			bigint("generation"),
			double("retry_at"),
		},
		Indexes: []SchemaIndex{index("ix_search_task_dispatch", "pending", "retry_at")},
	},
	// users + user_sessions back the Console login/registration flow. The
	// password hash is a self-describing PBKDF2 string; session tokens are only
	// stored as SHA-256 digests so a database leak cannot replay them.
	{
		Name: "users",
		Columns: []SchemaColumn{
			identifier("id", 32).PK(),
			identifier("username", 64),
			varchar("password_hash", 255),
			identifier("role", 16),
			boolean("enabled"),
			varchar("created_at", 40),
			varchar("updated_at", 40),
			varchar("last_login_at", 40).Null(),
		},
		Uniques: []SchemaUnique{unique("uq_users_username", "username")},
	},
	{
		Name: "user_sessions",
		Columns: []SchemaColumn{
			identifier("id", 32).PK(),
			identifier("user_id", 32),
			identifier("token_digest", 64),
			varchar("csrf_token", 64),
			varchar("expires_at", 40),
			varchar("created_at", 40),
			varchar("last_seen_at", 40),
			boolean("revoked"),
		},
		Uniques: []SchemaUnique{unique("uq_user_sessions_token_digest", "token_digest")},
		Indexes: []SchemaIndex{index("ix_user_sessions_user_id", "user_id")},
	},
	// kb_permissions and kb_invitations back the knowledge-base sharing model:
	// one permission row per (namespace, grantee, document) where an empty
	// document_name means the whole knowledge base, and one invitation row per
	// issued share link (only the token digest is stored, like user_sessions).
	{
		Name: "kb_permissions",
		Columns: []SchemaColumn{
			identifier("id", 32).PK(),
			identifier("namespace", 64),
			identifier("grantee_user_id", 32),
			identifier("document_name", 200),
			identifier("permission", 8),
			identifier("granted_by", 32),
			varchar("created_at", 40),
			varchar("updated_at", 40),
		},
		Uniques: []SchemaUnique{unique("uq_kb_permissions_scope", "namespace", "grantee_user_id", "document_name")},
		Indexes: []SchemaIndex{index("ix_kb_permissions_grantee", "grantee_user_id")},
	},
	{
		Name: "kb_invitations",
		Columns: []SchemaColumn{
			identifier("id", 32).PK(),
			identifier("namespace", 64),
			identifier("inviter_user_id", 32),
			identifier("invitee_user_id", 32),
			identifier("permission", 8),
			identifier("scope", 16),
			longText("document_names"),
			identifier("token_digest", 64),
			identifier("status", 16),
			varchar("created_at", 40),
			varchar("expires_at", 40),
			varchar("accepted_at", 40).Null(),
			varchar("updated_at", 40),
		},
		Uniques: []SchemaUnique{unique("uq_kb_invitations_token_digest", "token_digest")},
		Indexes: []SchemaIndex{
			index("ix_kb_invitations_invitee", "invitee_user_id"),
			index("ix_kb_invitations_namespace", "namespace"),
		},
	},
	// model_profiles is a Console-only registry of reusable model
	// configurations (Embedding / Reranker / VLM / OCR / JEV). A knowledge base
	// still stores its own copy of the
	// selected connection, so editing or deleting a profile never mutates an
	// existing knowledge base. The API key is protected like every other provider
	// secret; it is appended last so a migrated database keeps the same shape.
	{
		Name: "model_profiles",
		Columns: []SchemaColumn{
			identifier("id", 32).PK(),
			identifier("kind", 16),
			varchar("name", 120),
			varchar("base_url", 2048),
			varchar("model", 512),
			longText("protected_key").Null(),
			integer("vector_size").Null(),
			varchar("created_at", 40),
			varchar("updated_at", 40),
		},
		Uniques: []SchemaUnique{unique("uq_model_profiles_kind_name", "kind", "name")},
		Indexes: []SchemaIndex{index("ix_model_profiles_kind", "kind")},
	},
}

// SchemaTables returns the schema tables in creation order. The returned slice
// is a copy; table values are shared and must be treated as read-only.
func SchemaTables() []*SchemaTable {
	tables := make([]*SchemaTable, len(schemaTables))
	copy(tables, schemaTables)
	return tables
}

// TableByName returns the table definition with the given name.
func TableByName(name string) (*SchemaTable, bool) {
	for _, table := range schemaTables {
		if table.Name == name {
			return table, true
		}
	}
	return nil, false
}
