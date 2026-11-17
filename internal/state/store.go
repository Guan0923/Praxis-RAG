package state

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
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
//   - Concrete Kafka notification publishers; the durable task state machines
//     are in place for the worker binaries, while Debezium owns notification
//     delivery (no outbox relay).
//   - Worker main packages (rag-document-worker / rag-cache-worker /
//     rag-dead-letter-worker), broker-side DLQ consumption and deployment.
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
	Artifacts *ArtifactRegistry
	Uploads   *UploadJournal

	DocumentJobs    *DocumentJobs
	DocumentBatches *DocumentBatches

	// DocumentLocks is the cross-instance lease backend (Redis in production)
	// attached by the process entry point. Nil keeps the legacy MySQL lease mode;
	// a redis-mode configuration without a backend fails writes instead of
	// silently falling back.
	DocumentLocks DocumentLocker

	// FeishuConnections stores each Console account's Feishu OAuth grant;
	// FeishuOAuthStates stores short-lived callback state digests.
	FeishuConnections *FeishuConnectionRepository
	FeishuOAuthStates *FeishuOAuthStateRepository

	// Activity and Heartbeats are the Go-only monitoring data sources: hourly
	// activity counters (every retrieval execution) and Worker liveness rows.
	Activity   *ActivityCounters
	Heartbeats *RuntimeHeartbeats

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
	stores.FeishuConnections = NewFeishuConnectionRepository(stores, settings.EncryptionKey)
	stores.FeishuOAuthStates = NewFeishuOAuthStateRepository(stores)
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
	maxArtifactBytes := int64(snapshotSettings.MaxArtifactBytes)
	artifacts, err := NewArtifactRegistry(stores, float64(snapshotSettings.TTLSeconds), nil, &maxArtifactBytes)
	if err != nil {
		uploads.Close()
		_ = cacheSync.Close()
		_ = database.Close()
		return nil, err
	}
	stores.Artifacts = artifacts
	// Monitoring data sources need no cache flush: they are write-mostly
	// counters/heartbeats read by the Console monitor page only.
	stores.Activity = newActivityCounters(database)
	stores.Heartbeats = newRuntimeHeartbeats(database)
	warnIfUnencrypted(settings)
	return stores, nil
}

// Describe renders the secret-free state summary returned by /health, matching
// StateStores.describe.
func (s *Stores) Describe() map[string]any { return Describe(s.Settings) }

// AttachDocumentLocker enables the cross-instance document lease backend on this
// store and its upload journal. Every process that writes, recovers or cleans
// document state must call it with the same configuration.
func (s *Stores) AttachDocumentLocker(locker DocumentLocker) error {
	if locker == nil {
		return newValueError("document locker is required")
	}
	s.DocumentLocks = locker
	if s.Uploads != nil {
		return s.Uploads.SetDocumentLocker(locker)
	}
	return nil
}

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
			if err := s.CacheSync.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if s.Uploads != nil {
			s.Uploads.Close()
		}
		if s.DocumentLocks != nil {
			if err := s.DocumentLocks.Close(); err != nil && firstErr == nil {
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
	if settings.EncryptionKey == "" {
		slog.Warn("RAG_STATE_ENCRYPTION_KEY is not set: provider and API keys may not be readable on other instances")
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
	if err := s.migrateScheduleVersions(ctx); err != nil {
		return err
	}
	if err := s.migrateCacheRevisionWatermark(ctx); err != nil {
		return err
	}
	if err := s.migrateDocumentLeaseFencing(ctx); err != nil {
		return err
	}
	if err := s.migrateVisionModelConfig(ctx); err != nil {
		return err
	}
	if err := s.migrateModelProfileOwners(ctx); err != nil {
		return err
	}
	return s.migrateContentStore(ctx)
}

// migrateModelProfileOwners turns the shared model-configuration registry into a
// per-account one:
//
//  1. add owner_user_id when the column is missing;
//  2. assign every legacy row (owner NULL) to the first administrator, so the
//     profiles that used to be global stay available to exactly one account and
//     no other account silently inherits administrator credentials;
//  3. replace the old global unique key (kind, name) with the per-owner one
//     (owner_user_id, kind, name), otherwise two accounts could not both keep a
//     profile named "default".
//
// Every step is idempotent and tolerates a concurrent startup: a failed ALTER or
// index swap is only accepted after the resulting object is confirmed present.
func (s *Stores) migrateModelProfileOwners(ctx context.Context) error {
	const (
		table       = "model_profiles"
		column      = "owner_user_id"
		legacyIndex = "uq_model_profiles_kind_name"
		ownerIndex  = "uq_model_profiles_owner_kind_name"
		ownerKeyIx  = "ix_model_profiles_owner"
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
			" ADD COLUMN " + quoteIdentifier(column) + " VARCHAR(32) COLLATE utf8mb4_bin NULL"
		if _, execErr := s.DB.ExecContext(ctx, statement); execErr != nil {
			confirmed, confirmErr := s.tableColumns(ctx, table)
			if confirmErr != nil || !containsString(confirmed, column) {
				return execErr
			}
		}
	}
	// Backfill: legacy rows belong to the first administrator. When no
	// administrator exists yet (a fresh database) there is nothing to own, so the
	// rows keep NULL and stay administrator-only after the first admin is created
	// by the Console bootstrap. A missing users table (a partially migrated
	// database) skips the backfill instead of failing startup.
	usersPresent, err := s.hasTable(ctx, "users")
	if err != nil {
		return err
	}
	if usersPresent {
		if _, err := s.DB.ExecContext(ctx,
			"UPDATE "+quoteIdentifier(table)+" SET "+quoteIdentifier(column)+
				" = (SELECT id FROM users WHERE role = 'admin' ORDER BY created_at, id LIMIT 1)"+
				" WHERE "+quoteIdentifier(column)+" IS NULL"); err != nil {
			return err
		}
	}
	present, err := s.hasIndex(ctx, table, legacyIndex)
	if err != nil {
		return err
	}
	if present {
		if _, err := s.DB.ExecContext(ctx, "ALTER TABLE "+quoteIdentifier(table)+" DROP INDEX "+quoteIdentifier(legacyIndex)); err != nil {
			if stillPresent, checkErr := s.hasIndex(ctx, table, legacyIndex); checkErr != nil || stillPresent {
				return err
			}
		}
	}
	healthy, err := s.hasIndex(ctx, table, ownerIndex)
	if err != nil {
		return err
	}
	if !healthy {
		statement := "CREATE UNIQUE INDEX " + quoteIdentifier(ownerIndex) + " ON " + quoteIdentifier(table) +
			" (" + quoteIdentifier(column) + ", " + quoteIdentifier("kind") + ", " + quoteIdentifier("name") + ")"
		if _, execErr := s.DB.ExecContext(ctx, statement); execErr != nil && !isAlreadyExists(execErr) {
			confirmed, confirmErr := s.hasIndex(ctx, table, ownerIndex)
			if confirmErr != nil || !confirmed {
				return execErr
			}
		}
	}
	return s.ensureTableIndex(ctx, table, ownerKeyIx, column)
}

// migrateContentStore adds the content-addressed reference columns and indexes
// to an existing database. Bytes already stored inline stay readable through the
// repository fallbacks and are moved into content_objects by the bounded
// background backfill; no startup row rewrite happens here.
func (s *Stores) migrateContentStore(ctx context.Context) error {
	if err := s.ensureTableColumn(ctx, "upload_objects", "content_hash", "VARCHAR(64) COLLATE utf8mb4_bin NULL"); err != nil {
		return err
	}
	if err := s.ensureTableIndex(ctx, "upload_objects", "ix_upload_objects_content", "content_hash"); err != nil {
		return err
	}
	if err := s.ensureTableIndex(ctx, "blobs", "ix_blobs_sha256", "sha256"); err != nil {
		return err
	}
	if err := s.ensureTableColumn(ctx, "content_objects", "retired_at", "DOUBLE NULL"); err != nil {
		return err
	}
	return s.ensureTableIndex(ctx, "content_objects", "ix_content_objects_retired", "retired_at")
}

// ensureTableColumn adds one nullable column to an existing table. Concurrent
// startup may race the ALTER; only an independently confirmed existing column is
// accepted.
func (s *Stores) ensureTableColumn(ctx context.Context, table, column, definition string) error {
	exists, err := s.hasTable(ctx, table)
	if err != nil || !exists {
		return err
	}
	columns, err := s.tableColumns(ctx, table)
	if err != nil {
		return err
	}
	if containsString(columns, column) {
		return nil
	}
	statement := "ALTER TABLE " + quoteIdentifier(table) +
		" ADD COLUMN " + quoteIdentifier(column) + " " + definition
	if _, execErr := s.DB.ExecContext(ctx, statement); execErr != nil {
		confirmed, confirmErr := s.tableColumns(ctx, table)
		if confirmErr == nil && containsString(confirmed, column) {
			return nil
		}
		return execErr
	}
	return nil
}

// ensureTableIndex creates one secondary index when it is absent.
func (s *Stores) ensureTableIndex(ctx context.Context, table, indexName, column string) error {
	exists, err := s.hasTable(ctx, table)
	if err != nil || !exists {
		return err
	}
	if found, indexErr := s.hasIndex(ctx, table, indexName); indexErr != nil || found {
		return indexErr
	}
	statement := "CREATE INDEX " + quoteIdentifier(indexName) +
		" ON " + quoteIdentifier(table) + " (" + quoteIdentifier(column) + ")"
	if _, execErr := s.DB.ExecContext(ctx, statement); execErr != nil {
		confirmed, confirmErr := s.hasIndex(ctx, table, indexName)
		if confirmErr == nil && confirmed {
			return nil
		}
		return execErr
	}
	return nil
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

// columnAddition is one additive `ALTER TABLE ... ADD COLUMN` migration.
type columnAddition struct {
	name       string
	definition string
}

// migrateAddColumns applies missing additive columns to existing tables. It is
// idempotent and tolerates concurrent startup: an ALTER that loses the race is
// accepted only after the column is independently confirmed present. Tables
// that do not exist are skipped.
func (s *Stores) migrateAddColumns(ctx context.Context, additions map[string][]columnAddition) error {
	tables := make([]string, 0, len(additions))
	for table := range additions {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		exists, err := s.hasTable(ctx, table)
		if err != nil || !exists {
			if err != nil {
				return err
			}
			continue
		}
		columns, err := s.tableColumns(ctx, table)
		if err != nil {
			return err
		}
		for _, addition := range additions[table] {
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
	}
	return nil
}

// migrateScheduleVersions adds the Debezium scheduling generation to existing
// document_jobs tables. New rows default to 1; existing rows get 1 as well,
// which is the correct starting generation for the cutover backfill.
func (s *Stores) migrateScheduleVersions(ctx context.Context) error {
	return s.migrateAddColumns(ctx, map[string][]columnAddition{
		"document_jobs": {{name: "schedule_version", definition: "BIGINT NOT NULL DEFAULT 1"}},
	})
}

// migrateDocumentLeaseFencing adds the durable fencing generation to
// document_leases. Existing rows start at zero; the generation only ever
// increases, so a registration that was paused across a takeover is rejected
// even after the row was released and re-taken.
func (s *Stores) migrateDocumentLeaseFencing(ctx context.Context) error {
	return s.migrateAddColumns(ctx, map[string][]columnAddition{
		"document_leases": {{name: "fencing_epoch", definition: "BIGINT NOT NULL DEFAULT 0"}},
	})
}

// migrateCacheRevisionWatermark turns cache_revisions from a bare version row
// into a dirty-state/refresh-watermark row. The columns are additive; existing
// rows start with refreshed_revision=0 (dirty), no recorded error and no
// timestamp, so the cache consumer rebuilds every tracked key once.
func (s *Stores) migrateCacheRevisionWatermark(ctx context.Context) error {
	return s.migrateAddColumns(ctx, map[string][]columnAddition{
		"cache_revisions": {
			{name: "refreshed_revision", definition: "BIGINT NOT NULL DEFAULT 0"},
			{name: "attempts", definition: "INTEGER NOT NULL DEFAULT 0"},
			{name: "last_error", definition: "VARCHAR(128) COLLATE utf8mb4_bin NULL"},
			{name: "updated_at", definition: "DOUBLE NOT NULL DEFAULT 0"},
		},
	})
}

// migrateVisionModelConfig is the one-shot, idempotent upgrade that removes the
// retired local OCR model configuration:
//
//   - delete saved model profiles of the retired "ocr" kind, which no longer
//     exists in the Console model kinds;
//   - strip the legacy "ocr"/"ocr_model" keys from knowledge_bases.settings.
//     That column is a LONGTEXT JSON document rather than a JSON column, so the
//     keys are parsed and rewritten in Go; rows with invalid JSON are skipped
//     unchanged so one corrupt row cannot block startup.
//
// Re-running the migration is a no-op and concurrent startup is tolerated
// because every statement is idempotent.
func (s *Stores) migrateVisionModelConfig(ctx context.Context) error {
	exists, err := s.hasTable(ctx, "model_profiles")
	if err != nil {
		return err
	}
	if exists {
		if _, err := s.DB.ExecContext(ctx, "DELETE FROM model_profiles WHERE kind = 'ocr'"); err != nil {
			return err
		}
	}
	exists, err = s.hasTable(ctx, "knowledge_bases")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT namespace, settings FROM knowledge_bases")
	if err != nil {
		return err
	}
	type settingsUpdate struct {
		namespace string
		settings  string
	}
	var updates []settingsUpdate
	for rows.Next() {
		var namespace, rawSettings string
		if err := rows.Scan(&namespace, &rawSettings); err != nil {
			_ = rows.Close()
			return err
		}
		cleaned, changed := stripLegacyOCRKeys(rawSettings)
		if changed {
			updates = append(updates, settingsUpdate{namespace: namespace, settings: cleaned})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, update := range updates {
		if _, err := s.DB.ExecContext(ctx,
			"UPDATE knowledge_bases SET settings = ? WHERE namespace = ?",
			update.settings, update.namespace); err != nil {
			return err
		}
	}
	return nil
}

// stripLegacyOCRKeys removes the retired "ocr" and "ocr_model" keys from one
// knowledge-base settings JSON document. It returns the original text unchanged
// (and false) when the document is invalid or has no legacy keys, so the
// migration only rewrites rows that need it.
func stripLegacyOCRKeys(rawSettings string) (string, bool) {
	var settings map[string]any
	if err := json.Unmarshal([]byte(rawSettings), &settings); err != nil {
		return rawSettings, false
	}
	changed := false
	for _, key := range []string{"ocr", "ocr_model"} {
		if _, ok := settings[key]; ok {
			delete(settings, key)
			changed = true
		}
	}
	if !changed {
		return rawSettings, false
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return rawSettings, false
	}
	return string(encoded), true
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
