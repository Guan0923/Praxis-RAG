package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UploadConflict reports another upload/lock, an immutable artifact, or an
// idempotency-key mismatch (UploadConflict in Python).
type UploadConflict struct{ Message string }

// Error implements error.
func (e *UploadConflict) Error() string { return e.Message }

func newUploadConflict(message string) *UploadConflict { return &UploadConflict{Message: message} }

// DocumentExistsConflict reports a published document that already uses the
// requested name. It keeps the UploadConflict identity and message, and adds a
// typed target so the batch upload path can classify the outcome as "skipped"
// instead of a generic failure.
type DocumentExistsConflict struct{ UploadConflict }

// Unwrap lets errors.As find the embedded UploadConflict.
func (e *DocumentExistsConflict) Unwrap() error { return &e.UploadConflict }

func newDocumentExistsConflict(message string) *DocumentExistsConflict {
	return &DocumentExistsConflict{UploadConflict{Message: message}}
}

// DocumentQueuedConflict reports an in-flight document job with the requested
// name.
type DocumentQueuedConflict struct{ UploadConflict }

// Unwrap lets errors.As find the embedded UploadConflict.
func (e *DocumentQueuedConflict) Unwrap() error { return &e.UploadConflict }

func newDocumentQueuedConflict(message string) *DocumentQueuedConflict {
	return &DocumentQueuedConflict{UploadConflict{Message: message}}
}

// LeaseLost reports that the worker is no longer authorized to mutate the
// upload. In Python it subclasses UploadConflict.
type LeaseLost struct{ UploadConflict }

// Error implements error.
func (e *LeaseLost) Error() string { return e.Message }

// Unwrap lets errors.As find the embedded UploadConflict, matching Python's
// `except UploadConflict` behavior.
func (e *LeaseLost) Unwrap() error { return &e.UploadConflict }

func newLeaseLost(message string) *LeaseLost {
	return &LeaseLost{UploadConflict: UploadConflict{Message: message}}
}

// Upload operation statuses.
const (
	UploadIntent         = "INTENT"
	UploadRunning        = "RUNNING"
	UploadFailed         = "FAILED"
	UploadRollingBack    = "ROLLING_BACK"
	UploadRollbackFailed = "ROLLBACK_FAILED"
	UploadCommitted      = "COMMITTED"
	UploadRolledBack     = "ROLLED_BACK"
)

// Lease resource names shared by uploads and generic locks.
const (
	UploadGuardResource     = "namespace:guard"
	UploadNamespaceResource = "namespace:exclusive"
	uploadRollbackStepName  = "rollback"
	uploadPublishStepName   = "publish"
	uploadQdrantIntentStep  = "stage_qdrant"
)

var uploadActiveStatuses = map[string]struct{}{
	UploadIntent:         {},
	UploadRunning:        {},
	UploadFailed:         {},
	UploadRollingBack:    {},
	UploadRollbackFailed: {},
}

// UploadStep is one durable step row.
type UploadStep struct {
	OperationID string   `json:"operation_id"`
	Name        string   `json:"name"`
	Status      string   `json:"status"`
	Details     any      `json:"details"`
	Error       *string  `json:"error"`
	StartedAt   float64  `json:"started_at"`
	FinishedAt  *float64 `json:"finished_at"`
}

// UploadOperation is a durable upload operation with its steps.
type UploadOperation struct {
	ID             string       `json:"id"`
	Namespace      string       `json:"namespace"`
	Name           string       `json:"name"`
	DocumentID     string       `json:"document_id"`
	Collection     string       `json:"collection"`
	IdempotencyKey *string      `json:"idempotency_key"`
	Fingerprint    *string      `json:"fingerprint"`
	Owner          string       `json:"owner"`
	OldVersion     *string      `json:"old_version"`
	Status         string       `json:"status"`
	Result         any          `json:"result"`
	Error          *string      `json:"error"`
	CleanupPending bool         `json:"cleanup_pending"`
	Workspace      *string      `json:"workspace"`
	WorkspaceNode  *string      `json:"workspace_node"`
	CreatedAt      float64      `json:"created_at"`
	UpdatedAt      float64      `json:"updated_at"`
	Version        string       `json:"version"`
	Steps          []UploadStep `json:"steps"`
}

// LockToken authorizes generic namespace/document operations.
type LockToken struct {
	Namespace string
	Resource  string
	Owner     string
}

func documentUploadResource(documentID string) string {
	return "document:" + sha256Hex(documentID)
}

func nameUploadResource(name string) string {
	return "name:" + sha256Hex(name)
}

// JournalOption configures an UploadJournal.
type JournalOption func(*journalOptions)

type journalOptions struct {
	leaseSeconds        *float64
	autoHeartbeat       *bool
	cleanupDelaySeconds *float64
	documentLocker      DocumentLocker
	documentLock        *DocumentLockSettings
}

// WithDocumentLocker attaches the cross-instance lease backend and enables the
// Redis lease mode for this journal (tests and embedded callers).
func WithDocumentLocker(locker DocumentLocker) JournalOption {
	return func(options *journalOptions) { options.documentLocker = locker }
}

// WithDocumentLockSettings overrides the RAG_DOCUMENT_LOCK_* configuration.
func WithDocumentLockSettings(settings DocumentLockSettings) JournalOption {
	return func(options *journalOptions) { options.documentLock = &settings }
}

// WithLeaseSeconds overrides RAG_UPLOAD_LEASE_SECONDS.
func WithLeaseSeconds(seconds float64) JournalOption {
	return func(options *journalOptions) { options.leaseSeconds = &seconds }
}

// WithAutoHeartbeat enables or disables automatic lease renewal.
func WithAutoHeartbeat(enabled bool) JournalOption {
	return func(options *journalOptions) { options.autoHeartbeat = &enabled }
}

// WithCleanupDelaySeconds overrides RAG_UPLOAD_CLEANUP_DELAY_SECONDS.
func WithCleanupDelaySeconds(seconds float64) JournalOption {
	return func(options *journalOptions) { options.cleanupDelaySeconds = &seconds }
}

// UploadJournal is the durable, fenced upload transaction journal. It is a port
// of rag_mcp.state.uploads.UploadJournal.
type UploadJournal struct {
	stores              *Stores
	LeaseSeconds        float64
	CleanupDelaySeconds float64
	AutoHeartbeat       bool
	// Node identifies this process for workspace ownership (hostname).
	Node string

	heartbeats map[string]*leaseHeartbeat
	mutex      sync.Mutex

	// documentLock is the resolved lease-backend configuration. When its mode is
	// redis the long-lived lease lives in Redis and MySQL only holds the holder
	// registration plus the fencing generation.
	documentLock DocumentLockSettings
	lockerMutex  sync.RWMutex
	locker       DocumentLocker

	closeOnce sync.Once
}

// NewUploadJournal builds the journal with Python-equivalent validation.
func NewUploadJournal(stores *Stores, options ...JournalOption) (*UploadJournal, error) {
	resolved := journalOptions{}
	for _, option := range options {
		option(&resolved)
	}
	leaseSeconds := 120.0
	if resolved.leaseSeconds != nil {
		leaseSeconds = *resolved.leaseSeconds
	} else if raw := strings.TrimSpace(os.Getenv("RAG_UPLOAD_LEASE_SECONDS")); raw != "" {
		value, err := parseFloatEnv("RAG_UPLOAD_LEASE_SECONDS", raw)
		if err != nil {
			return nil, err
		}
		leaseSeconds = value
	}
	if math.IsNaN(leaseSeconds) || math.IsInf(leaseSeconds, 0) || leaseSeconds <= 0 {
		return nil, newValueError("lease_seconds must be finite and positive")
	}
	cleanupDelay := 3600.0
	if resolved.cleanupDelaySeconds != nil {
		cleanupDelay = *resolved.cleanupDelaySeconds
	} else if raw := strings.TrimSpace(os.Getenv("RAG_UPLOAD_CLEANUP_DELAY_SECONDS")); raw != "" {
		value, err := parseFloatEnv("RAG_UPLOAD_CLEANUP_DELAY_SECONDS", raw)
		if err != nil {
			return nil, err
		}
		cleanupDelay = value
	}
	if math.IsNaN(cleanupDelay) || math.IsInf(cleanupDelay, 0) || cleanupDelay < 0 {
		return nil, newValueError("cleanup_delay_seconds must be finite and nonnegative")
	}
	autoHeartbeat := true
	if resolved.autoHeartbeat != nil {
		autoHeartbeat = *resolved.autoHeartbeat
	}
	// Redis is the only lease backend. When the environment carries a lock
	// address the settings are validated eagerly so a broken TTL/prefix fails
	// construction; without an address (and without an injected backend) the
	// journal starts unconfigured and every write path that needs the lease
	// reports *LockUnavailable instead of falling back to MySQL leases.
	lockSettings := DefaultDocumentLockSettings()
	if resolved.documentLock != nil {
		lockSettings = *resolved.documentLock
	} else if documentLockURLFromEnv() != "" {
		parsed, err := DocumentLockSettingsFromEnv()
		if err != nil {
			return nil, err
		}
		lockSettings = parsed
	}
	if resolved.leaseSeconds != nil && resolved.documentLock == nil {
		lockSettings.TTLSeconds = leaseSeconds
	}
	locker := resolved.documentLocker
	if locker == nil && stores != nil {
		locker = stores.DocumentLocks
	}
	if locker != nil {
		// An injected backend is authoritative; the URL requirement only applies
		// when the journal has to rely on the environment.
		if lockSettings.TTLSeconds <= 0 {
			lockSettings.TTLSeconds = leaseSeconds
		}
	}
	node, _ := os.Hostname()
	return &UploadJournal{
		stores:              stores,
		LeaseSeconds:        leaseSeconds,
		CleanupDelaySeconds: cleanupDelay,
		AutoHeartbeat:       autoHeartbeat,
		Node:                node,
		heartbeats:          map[string]*leaseHeartbeat{},
		documentLock:        lockSettings,
		locker:              locker,
	}, nil
}

func parseFloatEnv(name, raw string) (float64, error) {
	value, err := strconvParseFloat(raw)
	if err != nil {
		return 0, newValueError(name + " must be a number")
	}
	return value, nil
}

func strconvParseFloat(raw string) (float64, error) {
	return strconv.ParseFloat(raw, 64)
}

// close stops lease renewal threads without releasing durable leases.
func (j *UploadJournal) Close() {
	j.closeOnce.Do(func() {
		j.mutex.Lock()
		heartbeats := j.heartbeats
		j.heartbeats = map[string]*leaseHeartbeat{}
		j.mutex.Unlock()
		for _, heartbeat := range heartbeats {
			heartbeat.stop()
		}
	})
}

// cacheKeys returns the sorted, de-duplicated display keys of one change.
func (j *UploadJournal) cacheKeys(namespace string, operationID *string, heads, leases bool) []CacheKey {
	keys := make([]CacheKey, 0, 4)
	if operationID != nil {
		keys = append(keys,
			CacheKey{Kind: "upload_status", Scope: *operationID},
			CacheKey{Kind: "upload_list", Scope: namespace})
	}
	if heads {
		keys = append(keys, CacheKey{Kind: "heads", Scope: namespace})
	}
	if leases {
		keys = append(keys, CacheKey{Kind: "leases", Scope: namespace})
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].Kind != keys[right].Kind {
			return keys[left].Kind < keys[right].Kind
		}
		return keys[left].Scope < keys[right].Scope
	})
	unique := keys[:0]
	for _, key := range keys {
		if len(unique) == 0 || unique[len(unique)-1] != key {
			unique = append(unique, key)
		}
	}
	return unique
}

func (j *UploadJournal) touchCache(ctx context.Context, tx *sql.Tx, namespace string, operationID *string, heads, leases bool) error {
	keys := j.cacheKeys(namespace, operationID, heads, leases)
	if len(keys) == 0 {
		return nil
	}
	_, err := touch(ctx, tx, keys...)
	return err
}

// forUpdate returns the row-locking suffix.
func (j *UploadJournal) forUpdate() string {
	return " FOR UPDATE"
}

// dbNow reads the database clock so every writer shares one time base.
func (j *UploadJournal) dbNow(ctx context.Context, querier viewQuerier) (float64, error) {
	const query = "SELECT UNIX_TIMESTAMP(CURRENT_TIMESTAMP(6))"
	var value float64
	if err := querier.QueryRowContext(ctx, query).Scan(&value); err != nil {
		return 0, err
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, newValueError("database clock is not finite")
	}
	return value, nil
}

const uploadOperationColumns = "id, namespace, name, document_id, collection, idempotency_key, " +
	"fingerprint, owner, old_version, status, result, error, cleanup_pending, workspace, " +
	"workspace_node, created_at, updated_at"

const uploadStepColumns = "operation_id, name, status, details, error, started_at, finished_at"

func scanUploadOperation(scanner rowScanner) (*UploadOperation, error) {
	var (
		operation      UploadOperation
		idempotencyKey sql.NullString
		fingerprint    sql.NullString
		oldVersion     sql.NullString
		result         sql.NullString
		errorText      sql.NullString
		workspace      sql.NullString
		workspaceNode  sql.NullString
	)
	if err := scanner.Scan(&operation.ID, &operation.Namespace, &operation.Name, &operation.DocumentID,
		&operation.Collection, &idempotencyKey, &fingerprint, &operation.Owner, &oldVersion,
		&operation.Status, &result, &errorText, &operation.CleanupPending, &workspace,
		&workspaceNode, &operation.CreatedAt, &operation.UpdatedAt); err != nil {
		return nil, err
	}
	operation.Version = operation.ID
	operation.IdempotencyKey = nullStringPointer(idempotencyKey)
	operation.Fingerprint = nullStringPointer(fingerprint)
	operation.OldVersion = nullStringPointer(oldVersion)
	operation.Error = nullStringPointer(errorText)
	operation.Workspace = nullStringPointer(workspace)
	operation.WorkspaceNode = nullStringPointer(workspaceNode)
	if result.Valid {
		if err := json.Unmarshal([]byte(result.String), &operation.Result); err != nil {
			return nil, err
		}
	}
	return &operation, nil
}

func scanUploadStep(scanner rowScanner) (*UploadStep, error) {
	var (
		step       UploadStep
		details    string
		errorText  sql.NullString
		finishedAt sql.NullFloat64
	)
	if err := scanner.Scan(&step.OperationID, &step.Name, &step.Status, &details, &errorText,
		&step.StartedAt, &finishedAt); err != nil {
		return nil, err
	}
	step.Error = nullStringPointer(errorText)
	if finishedAt.Valid {
		value := finishedAt.Float64
		step.FinishedAt = &value
	}
	if err := json.Unmarshal([]byte(details), &step.Details); err != nil {
		return nil, err
	}
	return &step, nil
}

func nullStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}

// Get returns one operation with its steps ordered by started_at then name.
func (j *UploadJournal) Get(ctx context.Context, operationID string) (*UploadOperation, error) {
	operation, err := scanUploadOperation(j.stores.DB.QueryRowContext(ctx,
		"SELECT "+uploadOperationColumns+" FROM upload_operations WHERE id = ?", operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, newLookupError(operationID)
	}
	if err != nil {
		return nil, err
	}
	return j.loadSteps(ctx, operation)
}

func (j *UploadJournal) loadSteps(ctx context.Context, operation *UploadOperation) (*UploadOperation, error) {
	rows, err := j.stores.DB.QueryContext(ctx,
		"SELECT "+uploadStepColumns+" FROM upload_steps WHERE operation_id = ? ORDER BY started_at, name", operation.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	steps := make([]UploadStep, 0)
	for rows.Next() {
		step, err := scanUploadStep(rows)
		if err != nil {
			return nil, err
		}
		steps = append(steps, *step)
	}
	operation.Steps = steps
	return operation, rows.Err()
}

func (j *UploadJournal) operationByID(ctx context.Context, querier viewQuerier, operationID string, locked bool) (*UploadOperation, error) {
	query := "SELECT " + uploadOperationColumns + " FROM upload_operations WHERE id = ?"
	if locked {
		query += j.forUpdate()
	}
	operation, err := scanUploadOperation(querier.QueryRowContext(ctx, query, operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, newLookupError(operationID)
	}
	return operation, err
}

// Unfinished returns active operations plus committed operations whose
// old-version cleanup is still pending.
func (j *UploadJournal) Unfinished(ctx context.Context, namespace *string) ([]*UploadOperation, error) {
	query := "SELECT " + uploadOperationColumns + " FROM upload_operations WHERE " +
		"(status IN (?, ?, ?, ?, ?) OR cleanup_pending = 1)"
	arguments := []any{UploadIntent, UploadRunning, UploadFailed, UploadRollingBack, UploadRollbackFailed}
	if namespace != nil {
		query += " AND namespace = ?"
		arguments = append(arguments, *namespace)
	}
	query += " ORDER BY created_at"
	rows, err := j.stores.DB.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	operations := make([]*UploadOperation, 0)
	for rows.Next() {
		operation, err := scanUploadOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}

// BeginOptions carries the optional idempotency/fingerprint inputs of Begin.
type BeginOptions struct {
	IdempotencyKey *string
	Fingerprint    *string
	OnBegin        func(ctx context.Context, tx *sql.Tx, operation *UploadOperation) error
}

// Begin persists INTENT and the resource leases atomically; a committed
// idempotency key replays the stored result. The long-lived lease is taken in
// Redis first and the holder is then registered in MySQL under the observed
// fencing generation.
func (j *UploadJournal) Begin(ctx context.Context, namespace, name, documentID, collection string, options BeginOptions) (*UploadOperation, error) {
	return j.beginRedis(ctx, namespace, name, documentID, collection, options)
}

// forShare returns the shared row-locking suffix: concurrent readers of the
// same row proceed in parallel while an exclusive writer waits for them.
func (j *UploadJournal) forShare() string {
	return " FOR SHARE"
}

// guard serializes namespace lock acquisition with a persistent lease row.
func (j *UploadJournal) guard(ctx context.Context, tx *sql.Tx, namespace string) error {
	return j.guardResource(ctx, tx, namespace, UploadGuardResource)
}

// guardName serializes only uploads that share one document name. Different
// names keep independent lease rows and therefore admit concurrently, while
// the same name still runs its check-then-insert under one lock.
func (j *UploadJournal) guardName(ctx context.Context, tx *sql.Tx, namespace, name string) error {
	return j.guardResource(ctx, tx, namespace, nameUploadResource(name))
}

// guardResource takes the row lock of one persistent lease resource.
func (j *UploadJournal) guardResource(ctx context.Context, tx *sql.Tx, namespace, resource string) error {
	if err := j.insertIgnoreLease(ctx, tx, namespace, resource, "", nil, 0); err != nil {
		return err
	}
	var (
		ignoredNamespace, ignoredResource, ignoredOwner string
		ignoredOperation                                sql.NullString
		ignoredExpires                                  float64
	)
	return tx.QueryRowContext(ctx,
		"SELECT namespace, resource, owner, operation_id, expires_at FROM document_leases "+
			"WHERE namespace = ? AND resource = ?"+j.forUpdate(),
		namespace, resource).
		Scan(&ignoredNamespace, &ignoredResource, &ignoredOwner, &ignoredOperation, &ignoredExpires)
}

func (j *UploadJournal) insertIgnoreLease(ctx context.Context, tx *sql.Tx, namespace, resource, owner string, operationID *string, expiresAt float64) error {
	arguments := []any{namespace, resource, owner, nilIfNil(operationID), expiresAt}
	_, err := tx.ExecContext(ctx,
		"INSERT INTO document_leases (namespace, resource, owner, operation_id, expires_at) "+
			"VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE namespace = namespace", arguments...)
	return err
}

func (j *UploadJournal) insertIgnoreHead(ctx context.Context, tx *sql.Tx, namespace, documentID, version string, updatedAt float64) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO document_heads (namespace, document_id, version, updated_at) VALUES (?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE namespace = namespace",
		namespace, documentID, version, updatedAt)
	return err
}

func (j *UploadJournal) insertIgnoreStep(ctx context.Context, tx *sql.Tx, operationID, name, status, details string, startedAt float64) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO upload_steps (operation_id, name, status, details, error, started_at, finished_at) "+
			"VALUES (?, ?, ?, ?, NULL, ?, NULL) ON DUPLICATE KEY UPDATE operation_id = operation_id",
		operationID, name, status, details, startedAt)
	return err
}

type uploadLeaseRow struct {
	Resource    string
	Owner       string
	OperationID sql.NullString
	ExpiresAt   float64
}

func (j *UploadJournal) leaseRows(ctx context.Context, querier viewQuerier, namespace string, includeGuard bool) ([]uploadLeaseRow, error) {
	query := "SELECT resource, owner, operation_id, expires_at FROM document_leases WHERE namespace = ?"
	if !includeGuard {
		query += " AND resource != ?"
	}
	query += j.forUpdate()
	arguments := []any{namespace}
	if !includeGuard {
		arguments = append(arguments, UploadGuardResource)
	}
	rows, err := querier.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	records := make([]uploadLeaseRow, 0)
	for rows.Next() {
		var record uploadLeaseRow
		if err := rows.Scan(&record.Resource, &record.Owner, &record.OperationID, &record.ExpiresAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func nilIfNil(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func (j *UploadJournal) owned(ctx context.Context, tx *sql.Tx, operation *UploadOperation, statuses map[string]struct{}) (*UploadOperation, error) {
	if err := j.guard(ctx, tx, operation.Namespace); err != nil {
		return nil, err
	}
	row, err := j.operationByID(ctx, tx, operation.ID, true)
	if err != nil {
		return nil, err
	}
	if row.Owner != operation.Owner {
		return nil, newLeaseLost("Upload ownership or state changed")
	}
	if _, allowed := statuses[row.Status]; !allowed {
		return nil, newLeaseLost("Upload ownership or state changed")
	}
	held, err := j.leaseRows(ctx, tx, row.Namespace, false)
	if err != nil {
		return nil, err
	}
	leaseByResource := make(map[string]uploadLeaseRow, len(held))
	for _, lease := range held {
		leaseByResource[lease.Resource] = lease
	}
	// Liveness is authoritative in Redis and was checked when this step
	// started; the SQL row is the fencing record, so only the holder identity
	// matters here (no expiry check, no periodic SQL renewal).
	for _, resource := range j.resources(row) {
		lease, ok := leaseByResource[resource]
		if !ok || lease.Owner != row.Owner || !lease.OperationID.Valid ||
			lease.OperationID.String != row.ID {
			return nil, newLeaseLost("Upload lease expired or was replaced")
		}
	}
	return row, nil
}

func (j *UploadJournal) resources(operation *UploadOperation) []string {
	return []string{documentUploadResource(operation.DocumentID), nameUploadResource(operation.Name)}
}

func (j *UploadJournal) releaseLeases(ctx context.Context, tx *sql.Tx, operation *UploadOperation) error {
	// Keep the row so its fencing generation stays monotonic; only the holder
	// fields are cleared. Deleting the row would reset the generation to zero
	// and let a stale request match it again.
	_, err := tx.ExecContext(ctx,
		"UPDATE document_leases SET owner = '', operation_id = NULL, expires_at = 0 "+
			"WHERE namespace = ? AND owner = ? AND operation_id = ?",
		operation.Namespace, operation.Owner, operation.ID)
	return err
}

// AssertOwner revalidates ownership and lease liveness in its own transaction.
// The Redis lease is renewed first (outside the SQL transaction), so a lost
// lease stops the pipeline before the next side effect.
func (j *UploadJournal) AssertOwner(ctx context.Context, operation *UploadOperation) (*UploadOperation, error) {
	if err := j.assertRedisLease(ctx, operation); err != nil {
		return nil, err
	}
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transaction.Rollback() }()
	row, err := j.owned(ctx, transaction, operation, uploadActiveStatuses)
	if err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return j.loadSteps(ctx, row)
}

// AssertOwnerTx revalidates ownership inside the caller's transaction.
func (j *UploadJournal) AssertOwnerTx(ctx context.Context, tx *sql.Tx, operation *UploadOperation) (*UploadOperation, error) {
	return j.owned(ctx, tx, operation, uploadActiveStatuses)
}

// Intent commits a step intent before an externally coordinated publication.
func (j *UploadJournal) Intent(ctx context.Context, operation *UploadOperation, name string, details map[string]any) error {
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	row, err := j.owned(ctx, transaction, operation, map[string]struct{}{UploadIntent: {}, UploadRunning: {}})
	if err != nil {
		return err
	}
	now, err := j.dbNow(ctx, transaction)
	if err != nil {
		return err
	}
	encoded, err := marshalJSON(detailsOrEmpty(details))
	if err != nil {
		return err
	}
	if err := j.touchCache(ctx, transaction, row.Namespace, &row.ID, false, false); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO upload_steps (operation_id, name, status, details, error, started_at, finished_at) "+
			"VALUES (?, ?, ?, ?, NULL, ?, NULL)", row.ID, name, UploadRunning, encoded, now); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_operations SET status = ?, updated_at = ? WHERE id = ?", UploadRunning, now, row.ID); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	return nil
}

func detailsOrEmpty(details map[string]any) map[string]any {
	if details == nil {
		return map[string]any{}
	}
	return details
}

// Step is the Python-compatible alias of RunStep used by service orchestration.
func (j *UploadJournal) Step(ctx context.Context, operation *UploadOperation, name string, action func() (any, error), details map[string]any) (any, error) {
	return j.RunStep(ctx, operation, name, action, details)
}

// RunStep durably records RUNNING before action and DONE only after fencing.
func (j *UploadJournal) RunStep(ctx context.Context, operation *UploadOperation, name string, action func() (any, error), details map[string]any) (any, error) {
	return j.RunStepContext(ctx, operation, name, func(context.Context) (any, error) { return action() }, details)
}

// RunStepContext is RunStep with a cancellable action context. The derived
// context is cancelled when lease renewal loses ownership or cannot confirm it,
// so in-flight parse/embed/index calls stop starting new work and abort.
func (j *UploadJournal) RunStepContext(ctx context.Context, operation *UploadOperation, name string, action func(context.Context) (any, error), details map[string]any) (any, error) {
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transaction.Rollback() }()
	row, err := j.owned(ctx, transaction, operation, map[string]struct{}{UploadIntent: {}, UploadRunning: {}})
	if err != nil {
		return nil, err
	}
	now, err := j.dbNow(ctx, transaction)
	if err != nil {
		return nil, err
	}
	encoded, err := marshalJSON(detailsOrEmpty(details))
	if err != nil {
		return nil, err
	}
	if err := j.touchCache(ctx, transaction, row.Namespace, &row.ID, false, false); err != nil {
		return nil, err
	}
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO upload_steps (operation_id, name, status, details, error, started_at, finished_at) "+
			"VALUES (?, ?, ?, ?, NULL, ?, NULL)", row.ID, name, UploadRunning, encoded, now); err != nil {
		if isUniqueViolation(err) {
			return nil, newUploadConflict("Step name already exists; recover instead of replaying actions")
		}
		return nil, err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_operations SET status = ?, updated_at = ? WHERE id = ?", UploadRunning, now, row.ID); err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}

	var result any
	actionErr := j.WithHeartbeatContext(ctx, operation, func(actionContext context.Context) error {
		value, err := action(actionContext)
		if err != nil {
			return err
		}
		result = value
		return nil
	})
	if actionErr != nil {
		if err := j.Fail(ctx, operation, actionErr, &name); err != nil {
			var leaseLost *LeaseLost
			if !errors.As(err, &leaseLost) {
				return nil, err
			}
		}
		return nil, actionErr
	}
	transaction, err = j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transaction.Rollback() }()
	row, err = j.owned(ctx, transaction, operation, map[string]struct{}{UploadRunning: {}})
	if err != nil {
		return nil, err
	}
	finishedAt, err := j.dbNow(ctx, transaction)
	if err != nil {
		return nil, err
	}
	if err := j.touchCache(ctx, transaction, row.Namespace, &row.ID, false, false); err != nil {
		return nil, err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_steps SET status = ?, finished_at = ? WHERE operation_id = ? AND name = ?",
		"DONE", finishedAt, row.ID, name); err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// Fail marks the operation failed but retains the live lease until rollback or
// expiry.
func (j *UploadJournal) Fail(ctx context.Context, operation *UploadOperation, cause error, step *string) error {
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	row, err := j.owned(ctx, transaction, operation, map[string]struct{}{
		UploadIntent: {}, UploadRunning: {}, UploadFailed: {},
	})
	if err != nil {
		return err
	}
	var errorType any
	if cause != nil {
		errorType = errorTypeName(cause)
	}
	now, err := j.dbNow(ctx, transaction)
	if err != nil {
		return err
	}
	if err := j.touchCache(ctx, transaction, row.Namespace, &row.ID, false, false); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_operations SET status = ?, error = ?, updated_at = ? WHERE id = ?",
		UploadFailed, errorType, now, row.ID); err != nil {
		return err
	}
	if step != nil {
		if _, err := transaction.ExecContext(ctx,
			"UPDATE upload_steps SET status = ?, error = ?, finished_at = ? WHERE operation_id = ? AND name = ?",
			UploadFailed, errorType, now, row.ID, *step); err != nil {
			return err
		}
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	return nil
}

// ClaimRecovery returns a fresh owner for expired work, or nil for live and
// terminal work. A takeover is only allowed after the Redis lease proves the
// previous holder is gone; a valid MySQL expires_at alone never authorizes
// stealing a running upload.
func (j *UploadJournal) ClaimRecovery(ctx context.Context, operationID string) (*UploadOperation, error) {
	return j.claimRecoveryRedis(ctx, operationID)
}

// Rollback compensates this version, then deletes staged objects; it never
// modifies document heads.
func (j *UploadJournal) Rollback(ctx context.Context, operation *UploadOperation, cleanup func(*UploadOperation) error) (*UploadOperation, error) {
	row, err := j.rollbackStart(ctx, operation)
	if err != nil {
		return nil, err
	}
	defer j.stopHeartbeat(operation)
	if cleanupErr := j.WithHeartbeat(ctx, operation, func() error { return cleanup(row) }); cleanupErr != nil {
		return j.recordRollbackFailure(ctx, operation, cleanupErr)
	}
	if err := j.rollbackFinish(ctx, operation); err != nil {
		return j.recordRollbackFailure(ctx, operation, err)
	}
	return j.Get(ctx, operation.ID)
}

// rollbackStart fences ownership and records the ROLLING_BACK intent.
func (j *UploadJournal) rollbackStart(ctx context.Context, operation *UploadOperation) (*UploadOperation, error) {
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transaction.Rollback() }()
	row, err := j.owned(ctx, transaction, operation, uploadActiveStatuses)
	if err != nil {
		return nil, err
	}
	var headVersion sql.NullString
	err = transaction.QueryRowContext(ctx,
		"SELECT version FROM document_heads WHERE namespace = ? AND document_id = ?",
		row.Namespace, row.DocumentID).Scan(&headVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if headVersion.Valid && headVersion.String == row.ID {
		return nil, newUploadConflict("Cannot roll back the active version")
	}
	now, err := j.dbNow(ctx, transaction)
	if err != nil {
		return nil, err
	}
	if err := j.touchCache(ctx, transaction, row.Namespace, &row.ID, false, false); err != nil {
		return nil, err
	}
	details, err := marshalJSON(map[string]any{"version": row.ID})
	if err != nil {
		return nil, err
	}
	if err := j.insertIgnoreStep(ctx, transaction, row.ID, uploadRollbackStepName, UploadRunning, details, now); err != nil {
		return nil, err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_steps SET status = ?, error = NULL, finished_at = NULL WHERE operation_id = ? AND name = ?",
		UploadRunning, row.ID, uploadRollbackStepName); err != nil {
		return nil, err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_operations SET status = ?, updated_at = ? WHERE id = ?", UploadRollingBack, now, row.ID); err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return row, nil
}

// rollbackFinish deletes staged objects, marks the operation ROLLED_BACK and
// releases its leases.
func (j *UploadJournal) rollbackFinish(ctx context.Context, operation *UploadOperation) error {
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	row, err := j.owned(ctx, transaction, operation, map[string]struct{}{
		UploadRollingBack: {}, UploadRollbackFailed: {},
	})
	if err != nil {
		return err
	}
	now, err := j.dbNow(ctx, transaction)
	if err != nil {
		return err
	}
	if err := j.touchCache(ctx, transaction, row.Namespace, &row.ID, false, true); err != nil {
		return err
	}
	if err := deleteUploadObjectsTx(ctx, transaction, row.ID); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_steps SET status = ?, finished_at = ? WHERE operation_id = ? AND name = ?",
		"DONE", now, row.ID, uploadRollbackStepName); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_operations SET status = ?, updated_at = ? WHERE id = ?", UploadRolledBack, now, row.ID); err != nil {
		return err
	}
	if err := j.releaseLeases(ctx, transaction, operation); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	// The fence is durable now; release the Redis lease. A failure only delays
	// reuse until the TTL and must not re-run the rollback.
	_ = j.ReleaseOperation(ctx, row)
	return nil
}

// recordRollbackFailure persists ROLLBACK_FAILED without leaking the error text.
func (j *UploadJournal) recordRollbackFailure(ctx context.Context, operation *UploadOperation, cause error) (*UploadOperation, error) {
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, cause
	}
	defer func() { _ = transaction.Rollback() }()
	row, err := j.owned(ctx, transaction, operation, map[string]struct{}{
		UploadRollingBack: {}, UploadRollbackFailed: {},
	})
	if err != nil {
		return nil, cause
	}
	now, err := j.dbNow(ctx, transaction)
	if err != nil {
		return nil, cause
	}
	if err := j.touchCache(ctx, transaction, row.Namespace, &row.ID, false, false); err != nil {
		return nil, cause
	}
	errorType := errorTypeName(cause)
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_steps SET status = ?, error = ?, finished_at = ? WHERE operation_id = ? AND name = ?",
		UploadFailed, errorType, now, row.ID, uploadRollbackStepName); err != nil {
		return nil, cause
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_operations SET status = ?, error = ?, updated_at = ? WHERE id = ?",
		UploadRollbackFailed, errorType, now, row.ID); err != nil {
		return nil, cause
	}
	if err := transaction.Commit(); err != nil {
		return nil, cause
	}
	return nil, cause
}

// CommitNoop finishes a deduplicated request without changing the document head.
func (j *UploadJournal) CommitNoop(ctx context.Context, operation *UploadOperation, result any) error {
	encoded, err := marshalJSON(result)
	if err != nil {
		return err
	}
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	row, err := j.owned(ctx, transaction, operation, map[string]struct{}{UploadIntent: {}, UploadRunning: {}})
	if err != nil {
		return err
	}
	now, err := j.dbNow(ctx, transaction)
	if err != nil {
		return err
	}
	if err := j.touchCache(ctx, transaction, row.Namespace, &row.ID, false, true); err != nil {
		return err
	}
	if err := deleteUploadObjectsTx(ctx, transaction, row.ID); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_operations SET status = ?, result = ?, error = NULL, cleanup_pending = 0, updated_at = ? WHERE id = ?",
		UploadCommitted, encoded, now, row.ID); err != nil {
		return err
	}
	if err := j.releaseLeases(ctx, transaction, operation); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	j.stopHeartbeat(operation)
	_ = j.ReleaseOperation(ctx, row)
	return nil
}

// Commit fences and publishes with the caller's documents/blob transaction; it
// never commits that transaction.
func (j *UploadJournal) Commit(ctx context.Context, operation *UploadOperation, result any, tx *sql.Tx) (*UploadOperation, error) {
	if tx == nil {
		return nil, newValueError("commit requires the caller's transaction")
	}
	encoded, err := marshalJSON(result)
	if err != nil {
		return nil, err
	}
	row, err := j.owned(ctx, tx, operation, map[string]struct{}{UploadIntent: {}, UploadRunning: {}})
	if err != nil {
		return nil, err
	}
	if err := j.touchCache(ctx, tx, row.Namespace, &row.ID, true, true); err != nil {
		return nil, err
	}
	now, err := j.dbNow(ctx, tx)
	if err != nil {
		return nil, err
	}
	needsCleanup := row.OldVersion != nil
	if !needsCleanup {
		var marker string
		err := tx.QueryRowContext(ctx,
			"SELECT name FROM upload_steps WHERE operation_id = ? AND name = ? LIMIT 1",
			row.ID, uploadQdrantIntentStep).Scan(&marker)
		if err == nil {
			needsCleanup = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if err := j.insertIgnoreHead(ctx, tx, row.Namespace, row.DocumentID, row.ID, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE document_heads SET version = ?, updated_at = ? WHERE namespace = ? AND document_id = ?",
		row.ID, now, row.Namespace, row.DocumentID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE upload_operations SET status = ?, result = ?, error = NULL, cleanup_pending = ?, updated_at = ? WHERE id = ?",
		UploadCommitted, encoded, needsCleanup, now, row.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE upload_steps SET status = ?, finished_at = ? WHERE operation_id = ? AND name = ?",
		"", now, row.ID, uploadPublishStepName); err != nil {
		return nil, err
	}
	if err := j.releaseLeases(ctx, tx, operation); err != nil {
		return nil, err
	}
	row.Status = UploadCommitted
	row.CleanupPending = needsCleanup
	row.UpdatedAt = now
	if err := json.Unmarshal([]byte(encoded), &row.Result); err != nil {
		return nil, err
	}
	j.stopHeartbeat(operation)
	return row, nil
}

// Cleanup retries post-commit old-version cleanup; it never deletes the current
// head and honours the cleanup grace period.
func (j *UploadJournal) Cleanup(ctx context.Context, operation *UploadOperation, cleanupOld func(*UploadOperation) error) (*UploadOperation, error) {
	current, err := j.Get(ctx, operation.ID)
	if err != nil {
		return nil, err
	}
	if current.Status != UploadCommitted {
		return nil, newUploadConflict("Old-version cleanup requires COMMITTED")
	}
	if !current.CleanupPending {
		return current, nil
	}
	now, err := j.dbNow(ctx, j.stores.DB)
	if err != nil {
		return nil, err
	}
	if now < current.UpdatedAt+j.CleanupDelaySeconds {
		return current, nil
	}
	var result *UploadOperation
	lockErr := j.WithLock(ctx, current.Namespace, nil, func(token *LockToken) error {
		reloaded, err := j.Get(ctx, current.ID)
		if err != nil {
			return err
		}
		if !reloaded.CleanupPending {
			result = reloaded
			return nil
		}
		active, err := j.ActiveVersions(ctx, reloaded.Namespace)
		if err != nil {
			return err
		}
		if reloaded.OldVersion != nil {
			for _, version := range active {
				if version == *reloaded.OldVersion {
					return newUploadConflict("Cannot clean an active version")
				}
			}
		}
		if err := cleanupOld(reloaded); err != nil {
			return err
		}
		transaction, err := j.stores.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = transaction.Rollback() }()
		if err := j.AssertLockTx(ctx, transaction, token); err != nil {
			return err
		}
		if err := j.touchCache(ctx, transaction, reloaded.Namespace, &reloaded.ID, false, false); err != nil {
			return err
		}
		if reloaded.OldVersion != nil {
			if err := deleteUploadObjectsTx(ctx, transaction, *reloaded.OldVersion); err != nil {
				return err
			}
		}
		if _, err := transaction.ExecContext(ctx,
			"UPDATE upload_operations SET cleanup_pending = 0, updated_at = ? WHERE id = ? AND status = ?",
			now, reloaded.ID, UploadCommitted); err != nil {
			return err
		}
		if err := transaction.Commit(); err != nil {
			return err
		}
		return nil
	})
	if lockErr != nil {
		return nil, lockErr
	}
	if result != nil {
		return result, nil
	}
	return j.Get(ctx, operation.ID)
}

// ActiveVersions returns the published head version of every document.
func (j *UploadJournal) ActiveVersions(ctx context.Context, namespace string) (map[string]string, error) {
	rows, err := j.stores.DB.QueryContext(ctx,
		"SELECT document_id, version FROM document_heads WHERE namespace = ?", namespace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	versions := make(map[string]string)
	for rows.Next() {
		var documentID, version string
		if err := rows.Scan(&documentID, &version); err != nil {
			return nil, err
		}
		versions[documentID] = version
	}
	return versions, rows.Err()
}

// Head returns the published version of one document, nil when unpublished.
func (j *UploadJournal) Head(ctx context.Context, namespace, documentID string) (*string, error) {
	var version string
	err := j.stores.DB.QueryRowContext(ctx,
		"SELECT version FROM document_heads WHERE namespace = ? AND document_id = ?",
		namespace, documentID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &version, nil
}

// RemoveHead publishes a deletion in the caller's transaction under a covering
// lock.
func (j *UploadJournal) RemoveHead(ctx context.Context, namespace, documentID string, tx *sql.Tx, token *LockToken) error {
	if tx == nil {
		return newValueError("remove_head requires the caller's transaction")
	}
	if token == nil || token.Namespace != namespace ||
		(token.Resource != UploadNamespaceResource && token.Resource != documentUploadResource(documentID)) {
		return newLeaseLost("Lock does not cover this document")
	}
	if err := j.AssertLockTx(ctx, tx, token); err != nil {
		return err
	}
	var version string
	err := tx.QueryRowContext(ctx,
		"SELECT version FROM document_heads WHERE namespace = ? AND document_id = ?",
		namespace, documentID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := j.touchCache(ctx, tx, namespace, nil, true, false); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		"DELETE FROM document_heads WHERE namespace = ? AND document_id = ?", namespace, documentID)
	return err
}

// PurgeDocumentObjects removes every staged/committed object of one document and
// returns the committed operation ids whose display cache changed.
func (j *UploadJournal) PurgeDocumentObjects(ctx context.Context, namespace, documentID string, tx *sql.Tx, token *LockToken) ([]string, error) {
	if tx == nil {
		return nil, newValueError("purge_document_objects requires the caller's transaction")
	}
	if token == nil || token.Namespace != namespace ||
		(token.Resource != UploadNamespaceResource && token.Resource != documentUploadResource(documentID)) {
		return nil, newLeaseLost("Lock does not cover this document")
	}
	if err := j.AssertLockTx(ctx, tx, token); err != nil {
		return nil, err
	}
	hashes, err := contentHashesTx(ctx, tx,
		"SELECT DISTINCT content_hash FROM upload_objects WHERE content_hash IS NOT NULL AND operation_id IN "+
			"(SELECT id FROM upload_operations WHERE namespace = ? AND document_id = ?)",
		namespace, documentID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM upload_objects WHERE operation_id IN (SELECT id FROM upload_operations WHERE namespace = ? AND document_id = ?)",
		namespace, documentID); err != nil {
		return nil, err
	}
	if err := retireContentHashesTx(ctx, tx, hashes, unixSeconds()); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT id FROM upload_operations WHERE namespace = ? AND document_id = ? AND status = ? AND cleanup_pending = 1",
		namespace, documentID, UploadCommitted)
	if err != nil {
		return nil, err
	}
	changed := make([]string, 0)
	for rows.Next() {
		var operationID string
		if err := rows.Scan(&operationID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		changed = append(changed, operationID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(changed) > 0 {
		keys := []CacheKey{{Kind: "upload_list", Scope: namespace}}
		for _, operationID := range changed {
			keys = append(keys, CacheKey{Kind: "upload_status", Scope: operationID})
		}
		if _, err := touch(ctx, tx, keys...); err != nil {
			return nil, err
		}
		arguments := make([]any, 0, len(changed))
		placeholders := make([]string, 0, len(changed))
		for _, operationID := range changed {
			placeholders = append(placeholders, "?")
			arguments = append(arguments, operationID)
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE upload_operations SET cleanup_pending = 0 WHERE id IN ("+strings.Join(placeholders, ", ")+")",
			arguments...); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

// OldVersion returns the version this operation replaced, if any.
func (j *UploadJournal) OldVersion(ctx context.Context, operation *UploadOperation) (*string, error) {
	current, err := j.Get(ctx, operation.ID)
	if err != nil {
		return nil, err
	}
	return current.OldVersion, nil
}

// PutObject stores immutable bytes for this version (identical retries allowed).
func (j *UploadJournal) PutObject(ctx context.Context, operation *UploadOperation, bucket, key string, content []byte, metadata map[string]any) error {
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := j.PutObjectTx(ctx, transaction, operation, bucket, key, content, metadata); err != nil {
		return err
	}
	return transaction.Commit()
}

// PutObjectTx stores immutable bytes inside the caller's transaction.
func (j *UploadJournal) PutObjectTx(ctx context.Context, tx *sql.Tx, operation *UploadOperation, bucket, key string, content []byte, metadata map[string]any) error {
	if _, err := j.owned(ctx, tx, operation, map[string]struct{}{UploadIntent: {}, UploadRunning: {}}); err != nil {
		return err
	}
	var encodedMetadata any
	if metadata != nil {
		encoded, err := marshalJSON(metadata)
		if err != nil {
			return err
		}
		encodedMetadata = encoded
	} else {
		encodedMetadata = nil
	}
	var existingContent []byte
	var existingHash sql.NullString
	var existingMetadata sql.NullString
	contentHash := sha256Sum(content)
	err := tx.QueryRowContext(ctx,
		"SELECT content, content_hash, metadata FROM upload_objects WHERE operation_id = ? AND bucket = ? AND `key` = ?"+j.forUpdate(),
		operation.ID, bucket, key).Scan(&existingContent, &existingHash, &existingMetadata)
	if err == nil {
		matchesMetadata := false
		if encodedMetadata == nil {
			matchesMetadata = !existingMetadata.Valid
		} else if existingMetadata.Valid {
			matchesMetadata = existingMetadata.String == encodedMetadata.(string)
		}
		matchesContent := string(existingContent) == string(content)
		if existingHash.Valid && existingHash.String != "" {
			matchesContent = existingHash.String == contentHash
		}
		if !matchesContent || !matchesMetadata {
			return newUploadConflict("Upload objects are immutable")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now, err := j.dbNow(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := insertContentObjectTx(ctx, tx, content, now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		"INSERT INTO upload_objects (operation_id, bucket, `key`, content, content_hash, metadata, created_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)",
		operation.ID, bucket, key, []byte{}, contentHash, encodedMetadata, now)
	return err
}

// GetObject returns staged bytes for one version, found=false when absent.
func (j *UploadJournal) GetObject(ctx context.Context, version, bucket, key string) ([]byte, bool, error) {
	var inlineContent []byte
	var storedContent []byte
	var contentHash sql.NullString
	err := j.stores.DB.QueryRowContext(ctx,
		"SELECT u.content, c.content, u.content_hash FROM upload_objects u "+
			"LEFT JOIN content_objects c ON c.hash = u.content_hash "+
			"WHERE u.operation_id = ? AND u.bucket = ? AND u.`key` = ?",
		version, bucket, key).Scan(&inlineContent, &storedContent, &contentHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if contentHash.Valid && contentHash.String != "" {
		return storedContent, true, nil
	}
	return inlineContent, true, nil
}

// RegisterWorkspace persists a caller-managed workspace before mkdir; the
// persisted path is immutable.
func (j *UploadJournal) RegisterWorkspace(ctx context.Context, operation *UploadOperation, path string, node *string) (string, error) {
	workspaceNode := j.Node
	if node != nil {
		workspaceNode = *node
	}
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = transaction.Rollback() }()
	row, err := j.owned(ctx, transaction, operation, map[string]struct{}{UploadIntent: {}, UploadRunning: {}})
	if err != nil {
		return "", err
	}
	if row.Workspace != nil && (*row.Workspace != path || row.WorkspaceNode == nil || *row.WorkspaceNode != workspaceNode) {
		return "", newUploadConflict("Workspace is immutable")
	}
	now, err := j.dbNow(ctx, transaction)
	if err != nil {
		return "", err
	}
	if err := j.touchCache(ctx, transaction, row.Namespace, &row.ID, false, false); err != nil {
		return "", err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_operations SET workspace = ?, workspace_node = ?, updated_at = ? WHERE id = ?",
		path, workspaceNode, now, row.ID); err != nil {
		return "", err
	}
	if err := transaction.Commit(); err != nil {
		return "", err
	}
	operation.Workspace = &path
	operation.WorkspaceNode = &workspaceNode
	return path, nil
}

// AcquireLock takes the generic exclusive namespace lock (documentID nil) or the
// document lock shared with uploads. The lease is taken in Redis and the holder
// is registered in MySQL before the caller starts side effects.
func (j *UploadJournal) AcquireLock(ctx context.Context, namespace string, documentID *string) (*LockToken, error) {
	return j.acquireLockRedis(ctx, namespace, documentID)
}

// AssertLock validates a token in its own transaction.
func (j *UploadJournal) AssertLock(ctx context.Context, token *LockToken) error {
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := j.AssertLockTx(ctx, transaction, token); err != nil {
		return err
	}
	return transaction.Commit()
}

// AssertLockTx validates a token inside the caller's transaction.
func (j *UploadJournal) AssertLockTx(ctx context.Context, tx *sql.Tx, token *LockToken) error {
	if token == nil {
		return newLeaseLost("Generic lock expired or was replaced")
	}
	if err := j.guard(ctx, tx, token.Namespace); err != nil {
		return err
	}
	var (
		owner     string
		operation sql.NullString
		expires   float64
	)
	err := tx.QueryRowContext(ctx,
		"SELECT owner, operation_id, expires_at FROM document_leases WHERE namespace = ? AND resource = ?"+j.forUpdate(),
		token.Namespace, token.Resource).Scan(&owner, &operation, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return newLeaseLost("Generic lock expired or was replaced")
	}
	if err != nil {
		return err
	}
	if owner != token.Owner {
		return newLeaseLost("Generic lock expired or was replaced")
	}
	// The SQL row is the fencing record; liveness lives in Redis and a
	// takeover rewrites the owner, so no expiry check is needed (and the row
	// is deliberately not renewed on a timer).
	return nil
}

// RenewLock extends a generic lock lease; it only touches Redis.
func (j *UploadJournal) RenewLock(ctx context.Context, token *LockToken) error {
	locker, err := j.documentLocker()
	if err != nil {
		return err
	}
	return locker.Renew(ctx, token.Namespace, []string{token.Resource}, token.Owner, j.lockTTL())
}

// ReleaseLock removes a generic lock when the caller still owns it.
func (j *UploadJournal) ReleaseLock(ctx context.Context, token *LockToken) error {
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := j.guard(ctx, transaction, token.Namespace); err != nil {
		return err
	}
	var marker string
	err = transaction.QueryRowContext(ctx,
		"SELECT resource FROM document_leases WHERE namespace = ? AND resource = ? AND owner = ?",
		token.Namespace, token.Resource, token.Owner).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing recorded; make sure our own Redis field is gone anyway.
		if locker, lockerErr := j.documentLocker(); lockerErr == nil {
			return locker.Release(ctx, token.Namespace, []string{token.Resource}, token.Owner)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := j.touchCache(ctx, transaction, token.Namespace, nil, false, true); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE document_leases SET owner = '', operation_id = NULL, expires_at = 0 "+
			"WHERE namespace = ? AND resource = ? AND owner = ?",
		token.Namespace, token.Resource, token.Owner); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	if locker, lockerErr := j.documentLocker(); lockerErr == nil {
		return locker.Release(ctx, token.Namespace, []string{token.Resource}, token.Owner)
	}
	return nil
}

// WithLock runs action under the generic lock with automatic renewal.
func (j *UploadJournal) WithLock(ctx context.Context, namespace string, documentID *string, action func(*LockToken) error) error {
	token, err := j.AcquireLock(ctx, namespace, documentID)
	if err != nil {
		return err
	}
	heartbeat := j.startRenewHeartbeat(func() error { return j.RenewLock(ctx, token) })
	actionErr := action(token)
	heartbeat.stop()
	defer func() { _ = j.ReleaseLock(ctx, token) }()
	if actionErr != nil {
		return actionErr
	}
	if heartbeatError := heartbeat.errorValue(); heartbeatError != nil {
		return newLeaseLost("Generic lock heartbeat failed")
	}
	return j.AssertLock(ctx, token)
}

// WithHeartbeat runs action while renewing the upload lease, then fences again.
func (j *UploadJournal) WithHeartbeat(ctx context.Context, operation *UploadOperation, action func() error) error {
	return j.WithHeartbeatContext(ctx, operation, func(context.Context) error { return action() })
}

// WithHeartbeatContext is WithHeartbeat with a cancellable action context. The
// context is cancelled as soon as renewal loses ownership or cannot confirm it,
// which stops in-flight external calls from starting new work.
func (j *UploadJournal) WithHeartbeatContext(ctx context.Context, operation *UploadOperation, action func(context.Context) error) error {
	if _, err := j.AssertOwner(ctx, operation); err != nil {
		return err
	}
	actionContext, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeat := j.startRenewHeartbeatContext(func() error { return j.renew(operation) }, cancel)
	actionErr := action(actionContext)
	heartbeat.stop()
	if actionErr != nil {
		return actionErr
	}
	if heartbeatError := heartbeat.errorValue(); heartbeatError != nil {
		var unavailable *LockUnavailable
		if errors.As(heartbeatError, &unavailable) {
			// Ownership is unknown, not demonstrably lost: retryable.
			return unavailable
		}
		return newLeaseLost("Upload heartbeat failed")
	}
	// A step may outlive the last heartbeat tick, so confirm the Redis lease is
	// still ours before recording DONE. Callers must not commit the operation
	// inside the action; a caller that does commit (publishDocument releases the
	// lease right after its transaction) must not fail this confirmation.
	if err := j.confirmLeaseAfterAction(ctx, operation); err != nil {
		return err
	}
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := j.guard(ctx, transaction, operation.Namespace); err != nil {
		return err
	}
	row, err := j.operationByID(ctx, transaction, operation.ID, true)
	if err != nil {
		return err
	}
	if !(row.Owner == operation.Owner && (row.Status == UploadCommitted || row.Status == UploadRolledBack)) {
		if _, err := j.owned(ctx, transaction, operation, uploadActiveStatuses); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

// confirmLeaseAfterAction renews the Redis lease after the action returns, but
// only while the operation is still owned by this caller and unfinished: a
// caller that committed inside the action also released the lease, and that
// durable commit must not be reported as a lost lease.
func (j *UploadJournal) confirmLeaseAfterAction(ctx context.Context, operation *UploadOperation) error {
	saved, err := j.operationByID(ctx, j.stores.DB, operation.ID, false)
	if err != nil {
		return err
	}
	if saved.Owner == operation.Owner &&
		(saved.Status == UploadCommitted || saved.Status == UploadRolledBack) {
		return nil
	}
	return j.assertRedisLease(ctx, operation)
}

func (j *UploadJournal) renew(operation *UploadOperation) error {
	return j.renewRedis(operation)
}

// leaseHeartbeat renews a lease on a background interval, mirroring
// uploads._Heartbeat. It never joins while the caller may hold a transaction.
type leaseHeartbeat struct {
	stopChannel chan struct{}
	done        chan struct{}
	mutex       sync.Mutex
	err         error
}

func (j *UploadJournal) startRenewHeartbeat(renew func() error) *leaseHeartbeat {
	return j.startRenewHeartbeatContext(renew, nil)
}

// startRenewHeartbeatContext renews on an interval. When renewal fails it
// records the error and, if cancel is non-nil, signals in-flight work to stop.
func (j *UploadJournal) startRenewHeartbeatContext(renew func() error, cancel context.CancelFunc) *leaseHeartbeat {
	heartbeat := &leaseHeartbeat{stopChannel: make(chan struct{}), done: make(chan struct{})}
	interval := time.Duration(j.LeaseSeconds / 3 * float64(time.Second))
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	go func() {
		defer close(heartbeat.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeat.stopChannel:
				return
			case <-ticker.C:
				if err := renew(); err != nil {
					heartbeat.mutex.Lock()
					heartbeat.err = err
					heartbeat.mutex.Unlock()
					if cancel != nil {
						cancel()
					}
					return
				}
			}
		}
	}()
	return heartbeat
}

func (h *leaseHeartbeat) stop() {
	select {
	case <-h.stopChannel:
		return
	default:
		close(h.stopChannel)
	}
}

func (h *leaseHeartbeat) errorValue() error {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return h.err
}

func (j *UploadJournal) startHeartbeat(operation *UploadOperation) {
	if !j.AutoHeartbeat {
		return
	}
	j.mutex.Lock()
	defer j.mutex.Unlock()
	if _, exists := j.heartbeats[operation.ID]; exists {
		return
	}
	j.heartbeats[operation.ID] = j.startRenewHeartbeat(func() error { return j.renew(operation) })
}

func (j *UploadJournal) stopHeartbeat(operation *UploadOperation) {
	j.mutex.Lock()
	heartbeat, exists := j.heartbeats[operation.ID]
	if exists {
		delete(j.heartbeats, operation.ID)
	}
	j.mutex.Unlock()
	if exists && heartbeat != nil {
		heartbeat.stop()
	}
}
