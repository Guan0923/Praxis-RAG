package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// documentLockAttempts bounds the acquire/register retry loop. A retry only
// happens when the durable fencing generation changed after the Redis lease was
// taken (another process took over while this request was paused); a live Redis
// conflict returns the pre-migration conflict response immediately.
const documentLockAttempts = 3

// errLeaseEpochChanged reports that a resource's fencing generation changed
// between reading it (before the Redis acquire) and registering the holder
// (after it). The caller releases its own lease and retries the acquisition.
var errLeaseEpochChanged = errors.New("document lease generation changed")

// leaseEpoch maps one resource to the durable generation observed before the
// Redis lease was taken. A missing resource is generation zero.
type leaseEpoch map[string]int64

// redisLeaseMode reports whether long-lived leases live in Redis.
func (j *UploadJournal) redisLeaseMode() bool { return j.documentLock.RedisMode() }

// documentLocker returns the attached backend, or a retryable error. A
// redis-mode journal without a backend must never fall back to MySQL leases:
// a mixed fleet would hold two incompatible locks.
func (j *UploadJournal) documentLocker() (DocumentLocker, error) {
	j.lockerMutex.RLock()
	locker := j.locker
	j.lockerMutex.RUnlock()
	if locker == nil {
		return nil, &LockUnavailable{Message: "document lock backend is not configured"}
	}
	return locker, nil
}

// SetDocumentLocker attaches the cross-instance lease backend and switches the
// journal to redis mode. It must be called before the process starts serving
// document writes; later calls are rejected so a running operation never
// changes backends underneath itself.
func (j *UploadJournal) SetDocumentLocker(locker DocumentLocker) error {
	if locker == nil {
		return newValueError("document locker is required")
	}
	j.lockerMutex.Lock()
	defer j.lockerMutex.Unlock()
	if j.locker != nil && j.locker != locker {
		return newValueError("document locker is already configured")
	}
	j.locker = locker
	j.documentLock.Mode = DocumentLockModeRedis
	return nil
}

// lockLeaseSeconds returns the Redis lease length. It follows the upload lease
// setting (RAG_UPLOAD_LEASE_SECONDS / RAG_REDIS_LOCK_TTL_SECONDS).
func (j *UploadJournal) lockLeaseSeconds() float64 {
	if j.documentLock.TTLSeconds > 0 {
		return j.documentLock.TTLSeconds
	}
	return j.LeaseSeconds
}

// lockTTL returns the Redis lease duration.
func (j *UploadJournal) lockTTL() time.Duration {
	return time.Duration(j.lockLeaseSeconds() * float64(time.Second))
}

// acquireLease takes the Redis lease, retrying only while the backend is
// temporarily unavailable. Lock conflicts return immediately.
func (j *UploadJournal) acquireLease(ctx context.Context, locker DocumentLocker, namespace string, resources []string, owner string) error {
	deadline := time.Now().Add(time.Duration(j.documentLock.WaitSeconds * float64(time.Second)))
	for {
		err := locker.Acquire(ctx, namespace, dedupeResources(resources), owner, j.lockTTL())
		if err == nil {
			return nil
		}
		var unavailable *LockUnavailable
		if !errors.As(err, &unavailable) || !time.Now().Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// releaseLease releases this acquisition's own resources; it never removes a
// token it does not hold.
func (j *UploadJournal) releaseLease(ctx context.Context, locker DocumentLocker, namespace string, resources []string, owner string) error {
	return locker.Release(ctx, namespace, dedupeResources(resources), owner)
}

// ReleaseOperation releases the Redis lease of a finished operation. It is
// called after the publishing transaction committed; a failure only delays
// reuse until the lease TTL and must never re-run the committed work.
func (j *UploadJournal) ReleaseOperation(ctx context.Context, operation *UploadOperation) error {
	if !j.redisLeaseMode() || operation == nil {
		return nil
	}
	locker, err := j.documentLocker()
	if err != nil {
		return nil
	}
	return j.releaseLease(ctx, locker, operation.Namespace, j.resources(operation), operation.Owner)
}

// assertRedisLease confirms this operation still holds the Redis lease and
// extends it. It runs before a new side effect starts, so a lost lease stops
// the pipeline before the next external write instead of at the next heartbeat.
func (j *UploadJournal) assertRedisLease(ctx context.Context, operation *UploadOperation) error {
	if !j.redisLeaseMode() {
		return nil
	}
	locker, err := j.documentLocker()
	if err != nil {
		return err
	}
	return locker.Renew(ctx, operation.Namespace, j.resources(operation), operation.Owner, j.lockTTL())
}

// readLeaseEpochs reads the durable generation of each resource. It is a
// non-locking read taken before the Redis lease: the registration transaction
// then verifies the generation is still the same, which is what rejects a
// request that was paused between acquiring the lease and registering it.
func (j *UploadJournal) readLeaseEpochs(ctx context.Context, querier viewQuerier, namespace string, resources []string) (leaseEpoch, error) {
	resources = dedupeResources(resources)
	epochs := make(leaseEpoch, len(resources))
	for _, resource := range resources {
		epochs[resource] = 0
	}
	if len(resources) == 0 {
		return epochs, nil
	}
	arguments := append([]any{namespace}, stringArguments(resources)...)
	rows, err := querier.QueryContext(ctx,
		"SELECT resource, fencing_epoch FROM document_leases WHERE namespace = ? AND resource IN ("+
			inPlaceholders(len(resources))+")", arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var resource string
		var epoch int64
		if err := rows.Scan(&resource, &epoch); err != nil {
			return nil, err
		}
		epochs[resource] = epoch
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return epochs, nil
}

// registerHolder is the short fencing transaction: verify every generation,
// invalidate holders the caller proved gone (their Redis lease was free), then
// take the rows for this owner. It runs before any side effect, so a stale
// request that resumes after a takeover is rejected here.
func (j *UploadJournal) registerHolder(
	ctx context.Context,
	tx *sql.Tx,
	namespace string,
	resources []string,
	expected leaseEpoch,
	owner string,
	operationID *string,
	expiresAt float64,
) error {
	resources = dedupeResources(resources)
	for _, resource := range resources {
		if err := j.insertIgnoreLease(ctx, tx, namespace, resource, "", nil, 0); err != nil {
			return err
		}
	}
	arguments := append([]any{namespace}, stringArguments(resources)...)
	rows, err := tx.QueryContext(ctx,
		"SELECT resource, owner, operation_id, fencing_epoch FROM document_leases "+
			"WHERE namespace = ? AND resource IN ("+inPlaceholders(len(resources))+")"+j.forUpdate(),
		arguments...)
	if err != nil {
		return err
	}
	type registrationRow struct {
		resource string
		owner    string
		epoch    int64
	}
	held := make([]registrationRow, 0, len(resources))
	for rows.Next() {
		var row registrationRow
		var operation sql.NullString
		if err := rows.Scan(&row.resource, &row.owner, &operation, &row.epoch); err != nil {
			_ = rows.Close()
			return err
		}
		held = append(held, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range held {
		if row.epoch != expected[row.resource] {
			return errLeaseEpochChanged
		}
	}
	if err := j.touchCache(ctx, tx, namespace, nil, false, true); err != nil {
		return err
	}
	for _, row := range held {
		if row.owner == "" || row.owner == owner {
			continue
		}
		// We hold the Redis lease for this resource, so the recorded holder can
		// no longer be alive: invalidate it before taking the row.
		if _, err := tx.ExecContext(ctx,
			"UPDATE document_leases SET owner = '', operation_id = NULL, expires_at = 0, "+
				"fencing_epoch = fencing_epoch + 1 WHERE namespace = ? AND resource = ?",
			namespace, row.resource); err != nil {
			return err
		}
	}
	for _, resource := range resources {
		if _, err := tx.ExecContext(ctx,
			"UPDATE document_leases SET owner = ?, operation_id = ?, expires_at = ?, "+
				"fencing_epoch = fencing_epoch + 1 WHERE namespace = ? AND resource = ?",
			owner, nilIfNil(operationID), expiresAt, namespace, resource); err != nil {
			return err
		}
	}
	return nil
}

// idempotencyReplay answers a request whose idempotency key already produced a
// committed operation. It runs before the lease acquisition so a lingering lease
// (for example a release that failed after commit) can never turn an idempotent
// replay into a spurious conflict. The registration transaction repeats the
// check authoritatively.
func (j *UploadJournal) idempotencyReplay(
	ctx context.Context,
	namespace, name, documentID, collection string,
	options BeginOptions,
) (*UploadOperation, error) {
	if options.IdempotencyKey == nil {
		return nil, nil
	}
	existing, err := scanUploadOperation(j.stores.DB.QueryRowContext(ctx,
		"SELECT "+uploadOperationColumns+" FROM upload_operations WHERE namespace = ? AND idempotency_key = ?",
		namespace, *options.IdempotencyKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fingerprintMismatch := options.Fingerprint != nil &&
		(existing.Fingerprint == nil || *existing.Fingerprint != *options.Fingerprint)
	if fingerprintMismatch || existing.Name != name || existing.DocumentID != documentID ||
		existing.Collection != collection {
		return nil, newUploadConflict("Idempotency key has different input")
	}
	if existing.Status == UploadCommitted {
		return j.loadSteps(ctx, existing)
	}
	if _, active := uploadActiveStatuses[existing.Status]; active {
		return nil, newUploadConflict("Idempotency key is unfinished; recover its operation")
	}
	return nil, newUploadConflict("Idempotency key is unfinished; recover its operation")
}

// beginRedis is the redis-mode Begin: lease first (Redis, atomic multi-resource),
// then register the holder in MySQL under the observed generation.
func (j *UploadJournal) beginRedis(
	ctx context.Context,
	namespace, name, documentID, collection string,
	options BeginOptions,
) (*UploadOperation, error) {
	if replay, err := j.idempotencyReplay(ctx, namespace, name, documentID, collection, options); err != nil || replay != nil {
		return replay, err
	}
	locker, err := j.documentLocker()
	if err != nil {
		return nil, err
	}
	operationID, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	resources := []string{documentUploadResource(documentID), nameUploadResource(name)}
	conflictResources := append(append([]string{}, resources...), UploadNamespaceResource)
	for attempt := 0; attempt < documentLockAttempts; attempt++ {
		owner, err := randomHex(16)
		if err != nil {
			return nil, err
		}
		expected, err := j.readLeaseEpochs(ctx, j.stores.DB, namespace, conflictResources)
		if err != nil {
			return nil, err
		}
		if err := j.acquireLease(ctx, locker, namespace, resources, owner); err != nil {
			return nil, j.mapLeaseError(err)
		}
		operation, holds, err := j.beginRegister(ctx, namespace, name, documentID, collection,
			operationID, owner, resources, conflictResources, expected, options)
		if err == nil {
			if !holds {
				// An idempotency replay only needed the lease for serialization.
				_ = j.releaseLease(ctx, locker, namespace, resources, owner)
			}
			return operation, nil
		}
		_ = j.releaseLease(ctx, locker, namespace, resources, owner)
		if errors.Is(err, errLeaseEpochChanged) {
			continue
		}
		return nil, err
	}
	return nil, newUploadConflict("Concurrent upload conflicts with this intent")
}

// beginRegister writes the INTENT row inside the short registration transaction
// and returns holds=true only when this call became the durable holder.
func (j *UploadJournal) beginRegister(
	ctx context.Context,
	namespace, name, documentID, collection, operationID, owner string,
	resources, conflictResources []string,
	expected leaseEpoch,
	options BeginOptions,
) (*UploadOperation, bool, error) {
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := j.guard(ctx, transaction, namespace); err != nil {
		return nil, false, err
	}
	if options.IdempotencyKey != nil {
		existing, err := scanUploadOperation(transaction.QueryRowContext(ctx,
			"SELECT "+uploadOperationColumns+" FROM upload_operations WHERE namespace = ? AND idempotency_key = ?"+j.forUpdate(),
			namespace, *options.IdempotencyKey))
		if err == nil {
			fingerprintMismatch := options.Fingerprint != nil &&
				(existing.Fingerprint == nil || *existing.Fingerprint != *options.Fingerprint)
			if fingerprintMismatch || existing.Name != name || existing.DocumentID != documentID ||
				existing.Collection != collection {
				return nil, false, newUploadConflict("Idempotency key has different input")
			}
			if existing.Status == UploadCommitted {
				loaded, err := j.loadSteps(ctx, existing)
				if err != nil {
					return nil, false, err
				}
				if err := transaction.Commit(); err != nil {
					return nil, false, err
				}
				return loaded, false, nil
			}
			return nil, false, newUploadConflict("Idempotency key is unfinished; recover its operation")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
	}
	var unresolved string
	err = transaction.QueryRowContext(ctx,
		"SELECT id FROM upload_operations WHERE namespace = ? AND status IN (?, ?, ?, ?, ?) "+
			"AND (document_id = ? OR name = ?) LIMIT 1",
		namespace, UploadIntent, UploadRunning, UploadFailed, UploadRollingBack, UploadRollbackFailed,
		documentID, name).Scan(&unresolved)
	if err == nil {
		return nil, false, newUploadConflict("Previous upload must be recovered before another intent")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	now, err := j.dbNow(ctx, transaction)
	if err != nil {
		return nil, false, err
	}
	if err := j.registerHolder(ctx, transaction, namespace, conflictResources, expected, owner,
		&operationID, now+j.lockLeaseSeconds()); err != nil {
		return nil, false, err
	}
	var oldVersion sql.NullString
	query := "SELECT version FROM document_heads WHERE namespace = ? AND document_id = ?" + j.forUpdate()
	if err := transaction.QueryRowContext(ctx, query, namespace, documentID).Scan(&oldVersion); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	row := &UploadOperation{
		ID:             operationID,
		Namespace:      namespace,
		Name:           name,
		DocumentID:     documentID,
		Collection:     collection,
		IdempotencyKey: options.IdempotencyKey,
		Fingerprint:    options.Fingerprint,
		Owner:          owner,
		OldVersion:     nullStringPointer(oldVersion),
		Status:         UploadIntent,
		CreatedAt:      now,
		UpdatedAt:      now,
		Version:        operationID,
	}
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO upload_operations (id, namespace, name, document_id, collection, idempotency_key, "+
			"fingerprint, owner, old_version, status, result, error, cleanup_pending, workspace, "+
			"workspace_node, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, 0, NULL, NULL, ?, ?)",
		row.ID, row.Namespace, row.Name, row.DocumentID, row.Collection, row.IdempotencyKey, row.Fingerprint,
		row.Owner, row.OldVersion, row.Status, row.CreatedAt, row.UpdatedAt); err != nil {
		if isUniqueViolation(err) {
			return nil, false, newUploadConflict("Concurrent upload conflicts with this intent")
		}
		return nil, false, err
	}
	// Same bloom registration contract as the legacy path: register before the
	// commit, abort retryably when neither registration nor a confirmed global
	// stop succeeds.
	if err := j.registerOperationID(ctx, row.ID); err != nil {
		return nil, false, err
	}
	if options.OnBegin != nil {
		if err := options.OnBegin(ctx, transaction, row); err != nil {
			return nil, false, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return nil, false, err
	}
	j.startHeartbeat(row)
	return row, true, nil
}

// acquireLockRedis is the redis-mode generic lock acquisition.
func (j *UploadJournal) acquireLockRedis(ctx context.Context, namespace string, documentID *string) (*LockToken, error) {
	locker, err := j.documentLocker()
	if err != nil {
		return nil, err
	}
	resource := UploadNamespaceResource
	if documentID != nil {
		resource = documentUploadResource(*documentID)
	}
	resources := []string{resource}
	conflictResources := dedupeResources([]string{resource, UploadNamespaceResource})
	for attempt := 0; attempt < documentLockAttempts; attempt++ {
		owner, err := randomHex(16)
		if err != nil {
			return nil, err
		}
		expected, err := j.readLeaseEpochs(ctx, j.stores.DB, namespace, conflictResources)
		if err != nil {
			return nil, err
		}
		if err := j.acquireLease(ctx, locker, namespace, resources, owner); err != nil {
			return nil, j.mapLeaseError(err)
		}
		token := &LockToken{Namespace: namespace, Resource: resource, Owner: owner}
		if err := j.registerLockHolder(ctx, token, conflictResources, expected); err != nil {
			_ = j.releaseLease(ctx, locker, namespace, resources, owner)
			if errors.Is(err, errLeaseEpochChanged) {
				continue
			}
			return nil, err
		}
		return token, nil
	}
	return nil, newUploadConflict("Upload resource is leased")
}

// registerLockHolder records a generic lock token in MySQL under the observed
// generations.
func (j *UploadJournal) registerLockHolder(ctx context.Context, token *LockToken, conflictResources []string, expected leaseEpoch) error {
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := j.guard(ctx, transaction, token.Namespace); err != nil {
		return err
	}
	now, err := j.dbNow(ctx, transaction)
	if err != nil {
		return err
	}
	if err := j.registerHolder(ctx, transaction, token.Namespace, conflictResources, expected,
		token.Owner, nil, now+j.lockLeaseSeconds()); err != nil {
		return err
	}
	return transaction.Commit()
}

// claimRecoveryRedis takes over an unfinished operation only after the Redis
// lease proves the previous holder is gone.
func (j *UploadJournal) claimRecoveryRedis(ctx context.Context, operationID string) (*UploadOperation, error) {
	locker, err := j.documentLocker()
	if err != nil {
		return nil, err
	}
	snapshot, err := j.Get(ctx, operationID)
	if err != nil {
		return nil, err
	}
	if _, active := uploadActiveStatuses[snapshot.Status]; !active {
		return nil, nil
	}
	resources := j.resources(snapshot)
	conflictResources := dedupeResources(append(append([]string{}, resources...), UploadNamespaceResource))
	for attempt := 0; attempt < documentLockAttempts; attempt++ {
		owner, err := randomHex(16)
		if err != nil {
			return nil, err
		}
		expected, err := j.readLeaseEpochs(ctx, j.stores.DB, snapshot.Namespace, conflictResources)
		if err != nil {
			return nil, err
		}
		if err := j.acquireLease(ctx, locker, snapshot.Namespace, resources, owner); err != nil {
			var busy *LockBusy
			if errors.As(err, &busy) {
				return nil, nil
			}
			return nil, err
		}
		claimed, err := j.claimRecoveryRegister(ctx, operationID, owner, snapshot.Namespace, resources, conflictResources, expected)
		if err == nil {
			if claimed == nil {
				_ = j.releaseLease(ctx, locker, snapshot.Namespace, resources, owner)
				return nil, nil
			}
			j.stopHeartbeat(snapshot)
			loaded, err := j.loadSteps(ctx, claimed)
			if err != nil {
				return nil, err
			}
			j.startHeartbeat(loaded)
			return loaded, nil
		}
		_ = j.releaseLease(ctx, locker, snapshot.Namespace, resources, owner)
		if errors.Is(err, errLeaseEpochChanged) {
			continue
		}
		return nil, err
	}
	return nil, nil
}

// claimRecoveryRegister rewrites the durable holder inside one short
// transaction, re-checking the operation is still unfinished. The namespace
// guard is taken first so the lock order matches every other registration
// transaction (guard -> operation row -> lease rows).
func (j *UploadJournal) claimRecoveryRegister(
	ctx context.Context,
	operationID, owner, namespace string,
	resources, conflictResources []string,
	expected leaseEpoch,
) (*UploadOperation, error) {
	transaction, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := j.guard(ctx, transaction, namespace); err != nil {
		return nil, err
	}
	row, err := j.operationByID(ctx, transaction, operationID, true)
	if err != nil {
		return nil, err
	}
	if _, active := uploadActiveStatuses[row.Status]; !active {
		return nil, nil
	}
	now, err := j.dbNow(ctx, transaction)
	if err != nil {
		return nil, err
	}
	if err := j.registerHolder(ctx, transaction, row.Namespace, conflictResources, expected, owner,
		&operationID, now+j.lockLeaseSeconds()); err != nil {
		return nil, err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_operations SET owner = ?, status = ?, updated_at = ? WHERE id = ?",
		owner, UploadRollingBack, now, operationID); err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	row.Owner = owner
	row.Status = UploadRollingBack
	row.UpdatedAt = now
	return row, nil
}

// renewRedis extends the Redis lease only; the MySQL registration is not a
// heartbeat and is never rewritten on a timer.
func (j *UploadJournal) renewRedis(operation *UploadOperation) error {
	locker, err := j.documentLocker()
	if err != nil {
		return err
	}
	return locker.Renew(context.Background(), operation.Namespace, j.resources(operation),
		operation.Owner, j.lockTTL())
}

// mapLeaseError converts a backend conflict to the existing upload conflict
// contract. Unavailable/lost errors keep their identity so callers can tell a
// retryable outage from a real ownership loss.
func (j *UploadJournal) mapLeaseError(err error) error {
	var busy *LockBusy
	if errors.As(err, &busy) {
		return newUploadConflict("Upload resource is leased")
	}
	return err
}

// dedupeResources keeps the first occurrence order and removes duplicates.
func dedupeResources(resources []string) []string {
	seen := make(map[string]struct{}, len(resources))
	cleaned := make([]string, 0, len(resources))
	for _, resource := range resources {
		if _, present := seen[resource]; present {
			continue
		}
		seen[resource] = struct{}{}
		cleaned = append(cleaned, resource)
	}
	return cleaned
}
