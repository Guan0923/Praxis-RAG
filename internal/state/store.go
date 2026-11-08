package state

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	sqlite "modernc.org/sqlite"

	"github.com/Guan0923/xinghe_rag/internal/config"
)

const sqliteDriverName = "sqlite"

// sqliteConstraintCode is SQLITE_CONSTRAINT; the extended result codes for the
// primary-key (1555) and unique (2067) failures share this low byte.
const sqliteConstraintCode = 19

// isUniqueViolation reports a constraint failure that Python surfaces as
// SQLAlchemy IntegrityError, so repositories can map it to StateConflict or
// retry an upsert exactly like the Python code.
func isUniqueViolation(err error) bool {
	var mysqlError *mysql.MySQLError
	if errors.As(err, &mysqlError) {
		switch mysqlError.Number {
		case 1022, 1048, 1062, 1452:
			return true
		}
		return false
	}
	var sqliteError *sqlite.Error
	if errors.As(err, &sqliteError) {
		return sqliteError.Code()&0xff == sqliteConstraintCode
	}
	return false
}

// Stores is one shared database handle plus every repository in a service
// process. It mirrors rag_mcp.state.store.StateStores.
//
// TODO(G2/G7) still deferred from the state-layer port:
//   - UploadJournal + UploadCoordinator (uploads.py, upload_coordinator.py):
//     upload_operations/upload_steps/upload_objects/document_heads/
//     document_leases rows, idempotency keys, workspace registration, lease
//     heartbeat/expiry, cleanup_pending, recovery claims, and the transactional
//     commit/rollback protocol. The schema and the upload read views above are
//     already in place, but nothing writes those tables yet.
//   - DocumentJobs + DocumentJobDispatcher (document_jobs.py,
//     document_queue.py) and SearchTasks + SearchTaskDispatcher
//     (search_tasks.py, search_queue.py).
//   - KnowledgeBaseRepository.publish/confirm_publication (needs the lease
//     journal) and the schema migrate CLI (migrate.py).
//   - Concrete Redis (StateCache/TransientStore) and RocketMQ (EventPublisher)
//     clients for CacheSync; configured endpoints are logged and reads fall back
//     to the authoritative database.
type Stores struct {
	Settings StateSettings
	DB       *sql.DB

	APIKeys        *APIKeyRepository
	KnowledgeBases *KnowledgeBaseRepository
	Documents      *DocumentRepository
	Catalogs       *CatalogRepository

	CacheSync *CacheSynchronizer
	Blobs     *BlobStore
	Views     *CachedReadViews
	Results   *ResultSnapshots
	Artifacts *ArtifactRegistry

	dialect    Dialect
	closeOnce  sync.Once
	closeError error
}

// createOptions carries the keyword arguments of StateStores.create.
type createOptions struct {
	settings     *StateSettings
	createSchema *bool
}

// Option adjusts Create.
type Option func(*createOptions)

// WithSettings uses resolved settings instead of reading the environment.
func WithSettings(settings StateSettings) Option {
	return func(options *createOptions) {
		options.settings = &settings
	}
}

// WithSchemaCreation overrides whether the schema is created. The resolved
// AutoSchema setting still applies: createSchema and auto_schema, exactly like
// the Python classmethod.
func WithSchemaCreation(createSchema bool) Option {
	return func(options *createOptions) {
		options.createSchema = &createSchema
	}
}

// Create resolves the state settings from the environment (or the override
// settings) and opens the store. It mirrors StateStores.create.
func Create(dataRoot string, options ...Option) (*Stores, error) {
	resolvedOptions := createOptions{}
	for _, option := range options {
		option(&resolvedOptions)
	}
	resolved := StateSettings{}
	if resolvedOptions.settings != nil {
		resolved = *resolvedOptions.settings
	} else {
		settings, err := SettingsFromEnv(dataRoot)
		if err != nil {
			return nil, err
		}
		resolved = settings
	}
	createSchema := resolved.AutoSchema
	if resolvedOptions.createSchema != nil {
		createSchema = *resolvedOptions.createSchema && resolved.AutoSchema
	}
	return New(resolved, createSchema)
}

// New opens the configured engine and, when createSchema is true, creates
// missing tables and applies the additive migrations. Failures are reported as
// StateError("状态数据库初始化失败：...") like StateStores.__init__.
func New(settings StateSettings, createSchema bool) (*Stores, error) {
	database, dialect, err := openDatabase(settings)
	if err != nil {
		return nil, err
	}
	stores := &Stores{
		Settings: settings,
		DB:       database,
		dialect:  dialect,
	}
	if createSchema {
		if err := stores.ensureSchema(context.Background()); err != nil {
			_ = database.Close()
			return nil, newStateError("状态数据库初始化失败：" + err.Error())
		}
	}
	// Cache sync is assembled after the schema (like the Python constructor) and
	// before the repositories, because every repository flushes its own keys.
	cacheSync, err := NewDefaultCacheSynchronizer(stores)
	if err != nil {
		_ = database.Close()
		return nil, err
	}
	stores.CacheSync = cacheSync
	stores.APIKeys = newAPIKeyRepository(database, dialect, settings.EncryptionKey, cacheSync)
	stores.KnowledgeBases = newKnowledgeBaseRepository(database, dialect, cacheSync)
	stores.Documents = newDocumentRepository(database, dialect, cacheSync)
	stores.Catalogs = newCatalogRepository(database, dialect, cacheSync)
	stores.Blobs = newBlobStore(database, dialect)
	stores.Views = NewCachedReadViews(stores)

	snapshotSettings, err := SnapshotSettingsFromEnv()
	if err != nil {
		_ = cacheSync.Close()
		_ = database.Close()
		return nil, err
	}
	stores.Results = NewResultSnapshots(stores, snapshotSettings, nil)
	// When the async phase injects the Redis backend it can also provide the
	// expiring TransientStore that owns search-result snapshots.
	if provider, ok := cacheSync.Backend.(TransientProvider); ok {
		stores.Results.Transient = provider.TransientStore()
	}
	maxArtifactBytes := int64(snapshotSettings.MaxArtifactBytes)
	artifacts, err := NewArtifactRegistry(stores, float64(snapshotSettings.TTLSeconds), nil, &maxArtifactBytes)
	if err != nil {
		_ = cacheSync.Close()
		_ = database.Close()
		return nil, err
	}
	stores.Artifacts = artifacts
	warnIfUnencrypted(settings)
	return stores, nil
}

// Dialect reports the SQL flavor of the opened database.
func (s *Stores) Dialect() Dialect { return s.dialect }

// Describe renders the secret-free state summary returned by /health, matching
// StateStores.describe.
func (s *Stores) Describe() map[string]any { return Describe(s.Settings) }

// Ping runs the lightweight connectivity check used by StateStores.ping.
func (s *Stores) Ping(ctx context.Context) error {
	var one int
	return s.DB.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// Close releases the cache clients and the database handle. It is safe to call
// more than once.
func (s *Stores) Close() error {
	s.closeOnce.Do(func() {
		var firstErr error
		if s.CacheSync != nil {
			if err := s.CacheSync.Close(); err != nil {
				firstErr = err
			}
		}
		if err := s.DB.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.closeError = firstErr
	})
	return s.closeError
}

// warnIfUnencrypted mirrors store._warn_if_unencrypted.
func warnIfUnencrypted(settings StateSettings) {
	if settings.IsMySQL && settings.EncryptionKey == "" {
		log.Printf("RAG_STATE_ENCRYPTION_KEY is not set: provider and API keys may not be readable on other instances")
	}
}

// openDatabase builds the database handle for the configured URL.
func openDatabase(settings StateSettings) (*sql.DB, Dialect, error) {
	if settings.IsMySQL || strings.HasPrefix(strings.ToLower(settings.URL), "mysql") {
		database, err := openMySQL(settings)
		return database, DialectMySQL, err
	}
	database, err := openSQLite(settings)
	return database, DialectSQLite, err
}

// openSQLite mirrors store._create_engine's SQLite branch: a file-backed
// database keeps check_same_thread=False semantics (database/sql pools
// connections), while ":memory:" uses a single shared connection like
// SQLAlchemy's StaticPool.
func openSQLite(settings StateSettings) (*sql.DB, error) {
	database := settings.SQLitePath
	if cut, _, found := strings.Cut(database, "?"); found {
		database = cut
	}
	if database == "" || database == ":memory:" || strings.HasPrefix(settings.URL, "sqlite+pysqlite:///:memory:") {
		handle, err := sql.Open(sqliteDriverName, ":memory:")
		if err != nil {
			return nil, err
		}
		handle.SetMaxOpenConns(1)
		return handle, nil
	}
	database = config.ExpandPath(database)
	if !strings.HasPrefix(database, "file:") {
		if err := os.MkdirAll(filepath.Dir(database), 0o755); err != nil {
			return nil, fmt.Errorf("Cannot create the state database directory: %s", filepath.Dir(database))
		}
	}
	// The pysqlite driver's default connect timeout is 5 seconds; the modernc
	// driver spells the same setting as _busy_timeout milliseconds.
	dsn := filepath.ToSlash(database)
	if strings.HasPrefix(database, "file:") {
		dsn = database
	}
	if !strings.Contains(dsn, "?") {
		dsn += "?_busy_timeout=5000"
	} else {
		dsn += "&_busy_timeout=5000"
	}
	return sql.Open(sqliteDriverName, dsn)
}

// openMySQL mirrors store._create_engine's MySQL branch, including the
// pool_size/max_overflow/pool_recycle mapping and an optional CA bundle for
// RAG_MYSQL_SSL_CA. pool_timeout has no database/sql equivalent (a connection
// wait is bounded by the context instead).
func openMySQL(settings StateSettings) (*sql.DB, error) {
	config := mysql.NewConfig()
	config.User = settings.User
	config.Passwd = settings.Password
	config.Net = "tcp"
	host := settings.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := settings.Port
	if port == 0 {
		port = 3306
	}
	config.Addr = net.JoinHostPort(host, strconv.Itoa(port))
	config.DBName = settings.Database
	// The Python URL always requests charset=utf8mb4; timestamps are stored as
	// strings/doubles, so no time parsing is needed.
	config.ParseTime = false
	config.Params = map[string]string{"charset": "utf8mb4"}
	if strings.TrimSpace(settings.SSLCA) != "" {
		tlsConfig, err := mysqlTLSConfig(settings)
		if err != nil {
			return nil, err
		}
		config.TLS = tlsConfig
	}
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return nil, err
	}
	handle := sql.OpenDB(connector)
	handle.SetMaxOpenConns(maxIntValue(1, settings.PoolSize+settings.MaxOverflow))
	handle.SetMaxIdleConns(maxIntValue(0, settings.PoolSize))
	handle.SetConnMaxLifetime(time.Duration(settings.PoolRecycle) * time.Second)
	return handle, nil
}

// mysqlTLSConfig loads the configured CA bundle. The pymysql URL only passes
// ssl_ca, so the Go client verifies the server certificate against that CA and
// the configured host. Exact pymysql TLS verification semantics are tracked as
// a TODO for the MySQL deployment smoke test.
func mysqlTLSConfig(settings StateSettings) (*tls.Config, error) {
	pem, err := os.ReadFile(config.ExpandPath(settings.SSLCA))
	if err != nil {
		return nil, fmt.Errorf("Cannot read RAG_MYSQL_SSL_CA: %s", settings.SSLCA)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("RAG_MYSQL_SSL_CA does not contain a valid PEM certificate")
	}
	return &tls.Config{
		RootCAs:    pool,
		ServerName: settings.Host,
		MinVersion: tls.VersionTLS12,
	}, nil
}

func maxIntValue(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ensureSchema creates missing tables (with their indexes) and then applies the
// additive migrations, in the same order as the Python constructor.
func (s *Stores) ensureSchema(ctx context.Context) error {
	for _, table := range schemaTables {
		exists, err := s.hasTable(ctx, table.Name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := s.DB.ExecContext(ctx, table.DDL(s.dialect)); err != nil {
			return err
		}
		for _, statement := range table.IndexDDL(s.dialect) {
			if _, err := s.DB.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
	}
	if err := s.migrateSoftDelete(ctx); err != nil {
		return err
	}
	return s.migrateDocumentOutbox(ctx)
}

// hasTable reports whether the given table exists in the current database.
func (s *Stores) hasTable(ctx context.Context, name string) (bool, error) {
	query := "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ? LIMIT 1"
	if s.dialect == DialectMySQL {
		query = "SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ? LIMIT 1"
	}
	var found int
	err := s.DB.QueryRowContext(ctx, query, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// tableColumns returns the current column names of one table.
func (s *Stores) tableColumns(ctx context.Context, table string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT * FROM "+quoteIdentifier(s.dialect, table)+" WHERE 1 = 0")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return rows.Columns()
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (s *Stores) booleanType() string {
	if s.dialect == DialectMySQL {
		return "BOOL"
	}
	return "BOOLEAN"
}

// migrateSoftDelete mirrors schema_migrations.migrate_soft_delete: add the
// document tombstone without rewriting existing data. Concurrent startup may
// race the ALTER; only an independently confirmed existing column is accepted.
func (s *Stores) migrateSoftDelete(ctx context.Context) error {
	exists, err := s.hasTable(ctx, "documents")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	columns, err := s.tableColumns(ctx, "documents")
	if err != nil {
		return err
	}
	if containsString(columns, "del_flag") {
		return nil
	}
	statement := "ALTER TABLE " + quoteIdentifier(s.dialect, "documents") +
		" ADD COLUMN " + quoteIdentifier(s.dialect, "del_flag") + " " + s.booleanType() + " NOT NULL DEFAULT 0"
	if _, err := s.DB.ExecContext(ctx, statement); err != nil {
		confirmed, confirmErr := s.tableColumns(ctx, "documents")
		if confirmErr == nil && containsString(confirmed, "del_flag") {
			return nil
		}
		return err
	}
	return nil
}

// migrateDocumentOutbox mirrors schema_migrations.migrate_document_outbox:
// upgrade the retired delivery metadata without changing pending generations.
func (s *Stores) migrateDocumentOutbox(ctx context.Context) error {
	const table = "document_task_outbox"
	additions := []struct {
		name       string
		definition string
	}{
		{"owner", "VARCHAR(32) NULL"},
		{"lease_until", "DOUBLE NOT NULL DEFAULT 0"},
		{"attempts", "INTEGER NOT NULL DEFAULT 0"},
		{"last_error", "TEXT NULL"},
	}
	exists, err := s.hasTable(ctx, table)
	if err != nil || !exists {
		return err
	}
	columns, err := s.tableColumns(ctx, table)
	if err != nil {
		return err
	}
	for _, addition := range additions {
		if containsString(columns, addition.name) {
			continue
		}
		statement := "ALTER TABLE " + quoteIdentifier(s.dialect, table) +
			" ADD COLUMN " + quoteIdentifier(s.dialect, addition.name) + " " + addition.definition
		if _, execErr := s.DB.ExecContext(ctx, statement); execErr != nil {
			confirmed, confirmErr := s.tableColumns(ctx, table)
			if confirmErr == nil && containsString(confirmed, addition.name) {
				continue
			}
			return execErr
		}
		columns = append(columns, addition.name)
	}
	return nil
}
