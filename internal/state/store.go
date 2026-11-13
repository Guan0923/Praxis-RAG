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
	"strconv"
	"strings"
	"sync"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/Guan0923/xinghe_rag/internal/config"
)

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
	return false
}

// isAlreadyExists reports schema DDL that lost a creation race to another
// process. Concurrent test binaries share one schema, so "table already exists"
// (1050) and "duplicate key name" (1061) mean the desired object is present.
func isAlreadyExists(err error) bool {
	var mysqlError *mysql.MySQLError
	if errors.As(err, &mysqlError) {
		switch mysqlError.Number {
		case 1050, 1061:
			return true
		}
	}
	return false
}

// Stores is one shared database handle plus every repository in a service
// process. It mirrors rag_mcp.state.store.StateStores.
//
// TODO(G2/G7) still deferred from the state-layer port:
//   - Concrete Kafka document/search notification publishers and the Redis
//     StateCache/TransientStore clients; the narrow interfaces and durable
//     outbox state machines are in place for the worker binaries.
//   - Worker main packages (rag-document-worker / rag-search-worker /
//     rag-dead-letter-worker), broker-side DLQ consumption and Debezium bridge
//     deployment.
//   - KnowledgeBaseRepository.publish/confirm_publication (needs the lease
//     journal) and the schema migrate CLI (migrate.py).
//   - UploadCoordinator is constructed by the runtime (NewUploadCoordinator) so
//     it can inject the Qdrant-backed IndexPublisher and schedule Recover; the
//     journal itself is attached here as Stores.Uploads.
type Stores struct {
	Settings StateSettings
	DB       *sql.DB

	APIKeys        *APIKeyRepository
	Users          *UserRepository
	Sessions       *UserSessionRepository
	Avatars        *UserAvatarRepository
	EmailCodes     *EmailVerificationRepository
	AppSettings    *AppSettingsRepository
	Permissions    *PermissionRepository
	Invitations    *InvitationRepository
	ModelProfiles  *ModelProfileRepository
	KnowledgeBases *KnowledgeBaseRepository
	Documents      *DocumentRepository
	Catalogs       *CatalogRepository

	CacheSync *CacheSynchronizer
	Blobs     *BlobStore
	Views     *CachedReadViews
	Results   *ResultSnapshots
	Artifacts *ArtifactRegistry
	Uploads   *UploadJournal

	DocumentJobs     *DocumentJobs
	DocumentBatches  *DocumentBatches
	SearchTasks      *SearchTasks
	SearchDispatcher *SearchTaskDispatcher

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
	resolved, err := resolveSettings(settings)
	if err != nil {
		return nil, err
	}
	settings = resolved
	database, err := openDatabase(settings)
	if err != nil {
		return nil, err
	}
	stores := &Stores{
		Settings: settings,
		DB:       database,
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
	stores.APIKeys = newAPIKeyRepository(database, settings.EncryptionKey, cacheSync)
	stores.Users = newUserRepository(database)
	stores.Sessions = newUserSessionRepository(database)
	stores.Avatars = newUserAvatarRepository(database)
	stores.EmailCodes = newEmailVerificationRepository(database)
	stores.AppSettings = newAppSettingsRepository(database)
	stores.Permissions = newPermissionRepository(database)
	stores.Invitations = newInvitationRepository(database)
	stores.ModelProfiles = newModelProfileRepository(database)
	stores.KnowledgeBases = newKnowledgeBaseRepository(database, cacheSync)
	stores.Documents = newDocumentRepository(database, cacheSync)
	stores.Catalogs = newCatalogRepository(database, cacheSync)
	stores.Blobs = newBlobStore(database)
	uploads, err := NewUploadJournal(stores)
	if err != nil {
		_ = cacheSync.Close()
		_ = database.Close()
		return nil, err
	}
	stores.Uploads = uploads
	documentJobs, err := NewDocumentJobs(stores)
	if err != nil {
		uploads.Close()
		_ = cacheSync.Close()
		_ = database.Close()
		return nil, err
	}
	stores.DocumentJobs = documentJobs
	stores.DocumentBatches = NewDocumentBatches(stores)
	if _, err := DocumentQueueSettingsFromEnv(); err != nil {
		uploads.Close()
		_ = cacheSync.Close()
		_ = database.Close()
		return nil, err
	}
	stores.Views = NewCachedReadViews(stores)

	snapshotSettings, err := SnapshotSettingsFromEnv()
	if err != nil {
		uploads.Close()
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
		uploads.Close()
		_ = cacheSync.Close()
		_ = database.Close()
		return nil, err
	}
	stores.Artifacts = artifacts
	searchTasks, err := NewSearchTasks(stores)
	if err != nil {
		uploads.Close()
		_ = cacheSync.Close()
		_ = database.Close()
		return nil, err
	}
	stores.SearchTasks = searchTasks
	stores.SearchDispatcher = NewSearchTaskDispatcher(stores, nil)
	warnIfUnencrypted(settings)
	return stores, nil
}

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
		if s.SearchDispatcher != nil {
			if err := s.SearchDispatcher.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if s.CacheSync != nil {
			if err := s.CacheSync.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if s.Uploads != nil {
			s.Uploads.Close()
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
	if settings.EncryptionKey == "" {
		log.Printf("RAG_STATE_ENCRYPTION_KEY is not set: provider and API keys may not be readable on other instances")
	}
}

// openDatabase builds the MySQL database handle for the configured settings.
func openDatabase(settings StateSettings) (*sql.DB, error) {
	return openMySQL(settings)
}

// resolveSettings fills the connection fields from the URL when a caller passes
// only RAG_STATE_URL, so Describe and the MySQL DSN see the same values.
func resolveSettings(settings StateSettings) (StateSettings, error) {
	if settings.Host != "" || strings.TrimSpace(settings.URL) == "" {
		return settings, nil
	}
	parsed, err := config.ParseStateURL(settings.URL)
	if err != nil {
		return StateSettings{}, err
	}
	parsed.EncryptionKey = settings.EncryptionKey
	parsed.Echo = settings.Echo
	parsed.AutoSchema = settings.AutoSchema
	parsed.PoolSize = settings.PoolSize
	parsed.MaxOverflow = settings.MaxOverflow
	parsed.PoolRecycle = settings.PoolRecycle
	parsed.PoolTimeout = settings.PoolTimeout
	return parsed, nil
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
	// strings/doubles, so no time parsing is needed. The driver applies utf8mb4
	// by default; passing "charset" through Params makes the driver run
	// `SET charset=...`, which MySQL rejects with error 1193.
	config.ParseTime = false
	// The Go MySQL driver otherwise sends every parameterised statement through
	// COM_STMT_PREPARE/EXECUTE. Each server-side prepared statement pays an extra
	// round trip, which on Windows/Docker Desktop MySQL links costs ~40ms per
	// statement (measured: text protocol ~0.5ms, prepared ~44ms), and the state
	// layer issues several statements per write. Interpolating parameters keeps
	// the same escaping guarantees while using a single COM_QUERY packet.
	config.InterpolateParams = true
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
	existing, err := s.tableNames(ctx)
	if err != nil {
		return err
	}
	for _, table := range schemaTables {
		if existing[table.Name] {
			continue
		}
		if _, err := s.DB.ExecContext(ctx, table.DDL()); err != nil && !isAlreadyExists(err) {
			return err
		}
		for _, statement := range table.IndexDDL() {
			if _, err := s.DB.ExecContext(ctx, statement); err != nil && !isAlreadyExists(err) {
				return err
			}
		}
	}
	if err := s.migrateSoftDelete(ctx); err != nil {
		return err
	}
	if err := s.migrateKnowledgeBaseOwner(ctx); err != nil {
		return err
	}
	if err := s.migrateAPIKeyOwner(ctx); err != nil {
		return err
	}
	if err := s.migrateUserProfile(ctx); err != nil {
		return err
	}
	return s.migrateDocumentOutbox(ctx)
}

// tableNames lists the current tables in one round trip; checking tables one by
// one through information_schema costs tens of milliseconds per table on MySQL.
func (s *Stores) tableNames(ctx context.Context) (map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	names := make(map[string]bool, len(schemaTables))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names[name] = true
	}
	return names, rows.Err()
}

// hasTable reports whether the given table exists in the current database.
func (s *Stores) hasTable(ctx context.Context, name string) (bool, error) {
	const query = "SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ? LIMIT 1"
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
	rows, err := s.DB.QueryContext(ctx, "SELECT * FROM "+quoteIdentifier(table)+" WHERE 1 = 0")
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
	statement := "ALTER TABLE " + quoteIdentifier("documents") +
		" ADD COLUMN " + quoteIdentifier("del_flag") + " BOOL NOT NULL DEFAULT 0"
	if _, err := s.DB.ExecContext(ctx, statement); err != nil {
		confirmed, confirmErr := s.tableColumns(ctx, "documents")
		if confirmErr == nil && containsString(confirmed, "del_flag") {
			return nil
		}
		return err
	}
	return nil
}

// migrateKnowledgeBaseOwner adds the ownership column to an existing
// knowledge_bases table. Rows created before ownership existed stay NULL, which
// the access layer treats as "administrator-only". Concurrent startup may race
// the ALTER; only an independently confirmed existing column is accepted.
func (s *Stores) migrateKnowledgeBaseOwner(ctx context.Context) error {
	const table = "knowledge_bases"
	exists, err := s.hasTable(ctx, table)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	columns, err := s.tableColumns(ctx, table)
	if err != nil {
		return err
	}
	if containsString(columns, "owner_user_id") {
		return nil
	}
	statement := "ALTER TABLE " + quoteIdentifier(table) +
		" ADD COLUMN " + quoteIdentifier("owner_user_id") + " VARCHAR(32) COLLATE utf8mb4_bin NULL"
	if _, err := s.DB.ExecContext(ctx, statement); err != nil {
		confirmed, confirmErr := s.tableColumns(ctx, table)
		if confirmErr == nil && containsString(confirmed, "owner_user_id") {
			return nil
		}
		return err
	}
	return nil
}

// migrateAPIKeyOwner adds the per-account ownership column (and its index) to an
// existing api_keys table. Rows created before ownership existed stay NULL, which
// the HTTP layer treats as administrator-only. Concurrent startup may race the
// ALTER/CREATE INDEX; only an independently confirmed existing object is
// accepted.
func (s *Stores) migrateAPIKeyOwner(ctx context.Context) error {
	const (
		table      = "api_keys"
		column     = "owner_user_id"
		indexName  = "ix_api_keys_owner"
		columnType = "VARCHAR(32) COLLATE utf8mb4_bin NULL"
	)
	exists, err := s.hasTable(ctx, table)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	columns, err := s.tableColumns(ctx, table)
	if err != nil {
		return err
	}
	if !containsString(columns, column) {
		statement := "ALTER TABLE " + quoteIdentifier(table) +
			" ADD COLUMN " + quoteIdentifier(column) + " " + columnType
		if _, execErr := s.DB.ExecContext(ctx, statement); execErr != nil {
			confirmed, confirmErr := s.tableColumns(ctx, table)
			if confirmErr != nil || !containsString(confirmed, column) {
				return execErr
			}
		}
	}
	if exists, err := s.hasIndex(ctx, table, indexName); err != nil {
		return err
	} else if exists {
		return nil
	}
	statement := "CREATE INDEX " + quoteIdentifier(indexName) +
		" ON " + quoteIdentifier(table) + " (" + quoteIdentifier(column) + ")"
	if _, err := s.DB.ExecContext(ctx, statement); err != nil {
		confirmed, confirmErr := s.hasIndex(ctx, table, indexName)
		if confirmErr == nil && confirmed {
			return nil
		}
		return err
	}
	return nil
}

// hasIndex reports whether one named secondary index exists on the table.
func (s *Stores) hasIndex(ctx context.Context, table, index string) (bool, error) {
	const query = "SELECT 1 FROM information_schema.statistics " +
		"WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ? LIMIT 1"
	var found int
	err := s.DB.QueryRowContext(ctx, query, table, index).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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
		statement := "ALTER TABLE " + quoteIdentifier(table) +
			" ADD COLUMN " + quoteIdentifier(addition.name) + " " + addition.definition
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

// migrateUserProfile adds the account profile columns (email/email_verified/
// age/gender) and the uq_users_email unique index to an existing users table.
// The columns are nullable or defaulted, so rows created before the profile
// existed stay valid. Concurrent startup may race the ALTER/CREATE INDEX; only an
// independently confirmed existing object is accepted.
func (s *Stores) migrateUserProfile(ctx context.Context) error {
	const (
		table     = "users"
		indexName = "uq_users_email"
	)
	additions := []struct {
		name       string
		definition string
	}{
		{"email", "VARCHAR(254) COLLATE utf8mb4_bin NULL"},
		{"email_verified", "BOOL NOT NULL DEFAULT 0"},
		{"age", "INTEGER NULL"},
		{"gender", "VARCHAR(16) NULL"},
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
		statement := "ALTER TABLE " + quoteIdentifier(table) +
			" ADD COLUMN " + quoteIdentifier(addition.name) + " " + addition.definition
		if _, execErr := s.DB.ExecContext(ctx, statement); execErr != nil {
			confirmed, confirmErr := s.tableColumns(ctx, table)
			if confirmErr == nil && containsString(confirmed, addition.name) {
				columns = append(columns, addition.name)
				continue
			}
			return execErr
		}
		columns = append(columns, addition.name)
	}
	// The unique index keeps e-mail addresses unique while MySQL still allows
	// many NULL rows for accounts without one.
	if exists, err := s.hasIndex(ctx, table, indexName); err != nil {
		return err
	} else if exists {
		return nil
	}
	statement := "CREATE UNIQUE INDEX " + quoteIdentifier(indexName) +
		" ON " + quoteIdentifier(table) + " (" + quoteIdentifier("email") + ")"
	if _, err := s.DB.ExecContext(ctx, statement); err != nil {
		confirmed, confirmErr := s.hasIndex(ctx, table, indexName)
		if confirmErr == nil && confirmed {
			return nil
		}
		return err
	}
	return nil
}
