// Package statetest opens the disposable MySQL state database used by the Go
// test suite. It replaces the per-package SQLite temp files: every store the
// tests open lives in the schema named by RAG_TEST_MYSQL_URL, whose name must
// contain "test" so a production database can never be truncated.
//
// go test runs each package in its own process, so the state tables are shared
// by concurrent package binaries. A MySQL advisory lock (GET_LOCK) keeps one
// process at a time inside the schema, and the schema is truncated at the first
// store opened by each test, mirroring the Python conftest fixture.
package statetest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestEncryptionKey is the fixed key every test store uses, matching the Python
// fixtures.
var TestEncryptionKey = "base64:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32))

// lockName serializes the test binaries that share one MySQL schema.
const lockName = "praxis_rag_go_test_schema"

var (
	lockMu   sync.Mutex
	lockConn *sql.Conn
	lockDB   *sql.DB
	lockHeld bool
)

// Connection returns the disposable MySQL URL and the fixed encryption key the
// suite must use. The suite fails instead of skipping: MySQL is the only state
// backend.
func Connection(tb testing.TB) (string, string) {
	tb.Helper()
	raw := strings.TrimSpace(os.Getenv("RAG_TEST_MYSQL_URL"))
	if raw == "" {
		tb.Fatalf("RAG_TEST_MYSQL_URL is not set: the Go test suite runs against MySQL. " +
			"Create a disposable schema (its name must contain \"test\") and export " +
			"RAG_TEST_MYSQL_URL=mysql://user:password@127.0.0.1:3306/praxis_unit_test")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		tb.Fatalf("RAG_TEST_MYSQL_URL is not a valid URL: %v", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "mysql", "mysql+pymysql", "mariadb":
	default:
		tb.Fatalf("RAG_TEST_MYSQL_URL must be a MySQL URL, got scheme %q", parsed.Scheme)
	}
	database := strings.TrimPrefix(parsed.Path, "/")
	if !strings.Contains(strings.ToLower(database), "test") {
		tb.Fatalf("refusing to run against %q: the RAG_TEST_MYSQL_URL database name must contain \"test\"", database)
	}
	return raw, TestEncryptionKey
}

// Prepare takes the cross-process advisory lock for the duration of the test
// (released by its cleanup) and truncates every table of the test schema, so
// each test starts from a known state while concurrent package binaries take
// turns instead of holding the schema for a whole run.
func Prepare(tb testing.TB, database *sql.DB) {
	tb.Helper()
	lockMu.Lock()
	if !lockHeld {
		connection, handle, err := connectLock(tb)
		if err != nil {
			lockMu.Unlock()
			tb.Fatalf("acquiring the MySQL test lock: %v", err)
		}
		lockConn, lockDB, lockHeld = connection, handle, true
		tb.Cleanup(releaseLock)
	}
	lockMu.Unlock()
	if err := truncateSchema(database); err != nil {
		tb.Fatalf("truncating the MySQL test schema: %v", err)
	}
}

// releaseLock drops the advisory lock connection so the next package binary can
// enter the schema.
func releaseLock() {
	lockMu.Lock()
	defer lockMu.Unlock()
	if !lockHeld {
		return
	}
	_ = lockConn.Close()
	_ = lockDB.Close()
	lockConn, lockDB, lockHeld = nil, nil, false
}

// connectLock opens a dedicated single-connection pool and waits for the
// advisory lock on it.
func connectLock(tb testing.TB) (*sql.Conn, *sql.DB, error) {
	raw, _ := Connection(tb)
	handle, err := sql.Open("mysql", mysqlDSN(tb, raw))
	if err != nil {
		return nil, nil, err
	}
	handle.SetMaxOpenConns(1)
	connection, err := handle.Conn(context.Background())
	if err != nil {
		return nil, nil, err
	}
	var acquired sql.NullInt64
	if err := connection.QueryRowContext(context.Background(),
		"SELECT GET_LOCK(?, 600)", lockName).Scan(&acquired); err != nil {
		return nil, nil, err
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return nil, nil, fmt.Errorf("timed out waiting for the MySQL test lock %q", lockName)
	}
	return connection, handle, nil
}


// truncateSchema empties every base table of the current schema, like the
// autouse Python fixture. Tables are discovered from information_schema so the
// helper does not need the state package (which would import it back).
func truncateSchema(database *sql.DB) error {
	ctx := context.Background()
	rows, err := database.QueryContext(ctx,
		"SELECT table_name FROM information_schema.tables "+
			"WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'")
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(tables) == 0 {
		return nil
	}
	if _, err := database.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		return err
	}
	for _, name := range tables {
		// DELETE is far cheaper than TRUNCATE, which is DDL and takes a metadata
		// lock per table; auto-increment counters do not matter to the tests.
		if _, err := database.ExecContext(ctx, "DELETE FROM `"+name+"`"); err != nil {
			return fmt.Errorf("truncate %s: %w", name, err)
		}
	}
	if _, err := database.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 1"); err != nil {
		return err
	}
	return nil
}

// mysqlDSN converts a SQLAlchemy-style MySQL URL into a go-sql-driver DSN.
func mysqlDSN(tb testing.TB, raw string) string {
	tb.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		tb.Fatalf("RAG_TEST_MYSQL_URL is not a valid URL: %v", err)
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		port = "3306"
	}
	user := ""
	password := ""
	if parsed.User != nil {
		user = parsed.User.Username()
		password, _ = parsed.User.Password()
	}
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=false",
		user, password, net.JoinHostPort(host, port), strings.TrimPrefix(parsed.Path, "/"))
}
