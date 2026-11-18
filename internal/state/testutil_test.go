package state

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/statetest"
)

// stateEnvKeys are the environment variables the state layer reads.
var stateEnvKeys = []string{
	"RAG_STATE_URL",
	"RAG_MYSQL_HOST",
	"RAG_MYSQL_PORT",
	"RAG_MYSQL_USER",
	"RAG_MYSQL_PASSWORD",
	"RAG_MYSQL_DATABASE",
	"RAG_MYSQL_SSL_CA",
	"RAG_STATE_ENCRYPTION_KEY",
	"RAG_STATE_POOL_SIZE",
	"RAG_STATE_MAX_OVERFLOW",
	"RAG_STATE_POOL_RECYCLE",
	"RAG_STATE_POOL_TIMEOUT",
	"RAG_STATE_ECHO",
	"RAG_STATE_AUTO_SCHEMA",
	"RAG_REDIS_URL",
	"RAG_REDIS_PREFIX",
	"RAG_REDIS_TTL_SECONDS",
	"RAG_REDIS_TIMEOUT_SECONDS",
	"RAG_REDIS_ENABLED",
	"RAG_REDIS_BREAKER_THRESHOLD",
	"RAG_REDIS_BREAKER_COOLDOWN_SECONDS",
	"RAG_CACHE_FALLBACK_CONCURRENCY",
	"RAG_DOCUMENT_LOCK_MODE",
	"RAG_REDIS_LOCK_URL",
	"RAG_REDIS_LOCK_PREFIX",
	"RAG_REDIS_LOCK_TTL_SECONDS",
	"RAG_REDIS_LOCK_TIMEOUT_SECONDS",
	"RAG_REDIS_LOCK_WAIT_SECONDS",
	"RAG_KAFKA_BOOTSTRAP_SERVERS",
	"RAG_KAFKA_CLIENT_ID",
	"RAG_KAFKA_SECURITY_PROTOCOL",
	"RAG_KAFKA_SASL_MECHANISM",
	"RAG_KAFKA_SASL_USERNAME",
	"RAG_KAFKA_SASL_PASSWORD",
	"RAG_KAFKA_TLS_CA",
	"RAG_KAFKA_TLS_INSECURE",
	"RAG_KAFKA_PUBLISH_TIMEOUT_SECONDS",
	"RAG_CACHE_TOPIC",
	"RAG_CACHE_GROUP",
	"RAG_DOCUMENT_DLQ_TOPIC",
	"RAG_RESULT_TTL_SECONDS",
	"RAG_ARTIFACT_MAX_BYTES",
	"RAG_RESULT_MAX_ARTIFACTS",
	"RAG_UPLOAD_LEASE_SECONDS",
	"RAG_UPLOAD_CLEANUP_DELAY_SECONDS",
	"RAG_DOCUMENT_TOPIC",
	"RAG_DOCUMENT_GROUP",
	"RAG_DOCUMENT_JOB_LEASE_SECONDS",
	"RAG_DOCUMENT_JOB_HEARTBEAT_SECONDS",
	"RAG_DOCUMENT_JOB_SWEEP_SECONDS",
	"RAG_DOCUMENT_OUTBOX_RETENTION_SECONDS",
	"RAG_DOCUMENT_JOB_RETENTION_SECONDS",
	"RAG_SEARCH_TIMEOUT_SECONDS",
}

// testEncryptionKey is the same 32-byte "k" key the Python fixtures use.
var testEncryptionKey = "base64:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32))

// isolateStateEnv clears every state-related environment variable for the test
// and restores the previous values afterwards.
func isolateStateEnv(t *testing.T) {
	t.Helper()
	type saved struct {
		value   string
		present bool
	}
	snapshot := make(map[string]saved, len(stateEnvKeys))
	for _, name := range stateEnvKeys {
		value, present := os.LookupEnv(name)
		snapshot[name] = saved{value: value, present: present}
		_ = os.Unsetenv(name)
	}
	t.Cleanup(func() {
		for name, item := range snapshot {
			if item.present {
				_ = os.Setenv(name, item.value)
			} else {
				_ = os.Unsetenv(name)
			}
		}
	})
}

func useTestEncryptionKey(t *testing.T) {
	t.Helper()
	t.Setenv(EncryptionKeyEnv, testEncryptionKey)
}

// newTestStores opens the shared MySQL test schema with portable encryption
// enabled, truncating it first. It replaces the SQLite temp-file fixture.
func newTestStores(t *testing.T) *Stores {
	t.Helper()
	isolateStateEnv(t)
	useTestEncryptionKey(t)
	url, key := statetest.Connection(t)
	t.Setenv("RAG_STATE_URL", url)
	stores, err := New(StateSettings{
		URL:           url,
		EncryptionKey: key,
		AutoSchema:    true,
		PoolSize:      10,
		MaxOverflow:   20,
		PoolRecycle:   1800,
		PoolTimeout:   30,
	}, true)
	if err != nil {
		t.Fatalf("New(MySQL) error = %v", err)
	}
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	return stores
}

// reopenStores opens a second store over the same database file.
func reopenStores(t *testing.T, stores *Stores) *Stores {
	t.Helper()
	settings := stores.Settings
	if err := stores.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened, err := New(settings, true)
	if err != nil {
		t.Fatalf("New(reopen) error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

// mustBeError is a generic errors.As assertion helper.
func mustBeError[T error](t *testing.T, err error) T {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error of type %T, got nil", *new(T))
	}
	var target T
	if !errors.As(err, &target) {
		t.Fatalf("error %v (%T) is not a %T", err, err, target)
	}
	return target
}

// revision returns the current read-model revision for a (kind, scope) pair.
func revision(t *testing.T, stores *Stores, kind, scope string) int64 {
	t.Helper()
	var value int64
	err := stores.DB.QueryRowContext(context.Background(),
		"SELECT revision FROM cache_revisions WHERE kind = ? AND scope = ?", kind, scope).Scan(&value)
	if err != nil {
		t.Fatalf("revision(%q, %q) error = %v", kind, scope, err)
	}
	return value
}

// revisionCount returns the number of tracked read-model revisions.
func revisionCount(t *testing.T, stores *Stores) int {
	t.Helper()
	var count int
	if err := stores.DB.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM cache_revisions").Scan(&count); err != nil {
		t.Fatalf("revisionCount error = %v", err)
	}
	return count
}

// columnExists reports whether the table currently has the column.
func columnExists(t *testing.T, stores *Stores, table, column string) bool {
	t.Helper()
	columns, err := stores.tableColumns(context.Background(), table)
	if err != nil {
		t.Fatalf("tableColumns(%q) error = %v", table, err)
	}
	return containsString(columns, column)
}

// mysqlTestURL returns the opt-in MySQL test database URL.
func mysqlTestURL(t *testing.T) string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("RAG_TEST_MYSQL_URL"))
	if raw == "" {
		t.Skip("RAG_TEST_MYSQL_URL is not set")
	}
	return raw
}

// openMySQLTestStores opens the shared MySQL test database.
func openMySQLTestStores(t *testing.T) *Stores {
	t.Helper()
	return newTestStores(t)
}

// closeRows is a small helper used by tests that scan raw rows.
func closeRows(t *testing.T, rows *sql.Rows) {
	t.Helper()
	if err := rows.Close(); err != nil {
		t.Fatalf("rows.Close() error = %v", err)
	}
}

// dropStateTables removes every base table of the test schema so a test can
// verify that the store creates them again.
func dropStateTables(t *testing.T, stores *Stores) {
	t.Helper()
	ctx := context.Background()
	rows, err := stores.DB.QueryContext(ctx,
		"SELECT table_name FROM information_schema.tables "+
			"WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'")
	if err != nil {
		t.Fatalf("listing state tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			t.Fatalf("scanning state table name: %v", err)
		}
		tables = append(tables, name)
	}
	closeRows(t, rows)
	if _, err := stores.DB.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		t.Fatalf("disabling foreign key checks: %v", err)
	}
	for _, name := range tables {
		if _, err := stores.DB.ExecContext(ctx, "DROP TABLE `"+name+"`"); err != nil {
			t.Fatalf("dropping %s: %v", name, err)
		}
	}
	if _, err := stores.DB.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 1"); err != nil {
		t.Fatalf("enabling foreign key checks: %v", err)
	}
}
