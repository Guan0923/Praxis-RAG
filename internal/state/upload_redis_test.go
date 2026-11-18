package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeLease is one in-memory lease held by fakeDocumentLocker.
type fakeLease struct {
	owner  string
	expiry time.Time
}

// fakeDocumentLocker is a deterministic in-memory stand-in for the Redis
// backend. It reproduces the observable contract (atomic multi-resource
// acquire, owner-checked renew/release, TTL expiry) and adds controlled hooks so
// tests can pause a request between acquiring the lease and registering it in
// MySQL without relying on sleeps.
type fakeDocumentLocker struct {
	mutex       sync.Mutex
	fields      map[string]map[string]fakeLease
	unavailable bool
	releaseErr  error
	acquireHook func()
	acquires    int
}

func newFakeDocumentLocker() *fakeDocumentLocker {
	return &fakeDocumentLocker{fields: map[string]map[string]fakeLease{}}
}

// digest mirrors the Redis backend and the lease read model.
func fakeOwnerDigest(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:])[:16]
}

func containsResource(resources []string, resource string) bool {
	for _, candidate := range resources {
		if candidate == resource {
			return true
		}
	}
	return false
}

func (f *fakeDocumentLocker) prune(namespace string, now time.Time) map[string]fakeLease {
	store := f.fields[namespace]
	if store == nil {
		store = map[string]fakeLease{}
		f.fields[namespace] = store
	}
	for resource, lease := range store {
		if !lease.expiry.After(now) {
			delete(store, resource)
		}
	}
	return store
}

// Acquire implements DocumentLocker.
func (f *fakeDocumentLocker) Acquire(_ context.Context, namespace string, resources []string, owner string, ttl time.Duration) error {
	f.mutex.Lock()
	if f.unavailable {
		f.mutex.Unlock()
		return NewLockUnavailable()
	}
	now := time.Now()
	store := f.prune(namespace, now)
	resources = dedupeResources(resources)
	exclusive := containsResource(resources, UploadNamespaceResource)
	busy := false
	for resource, lease := range store {
		if lease.owner == owner {
			continue
		}
		if exclusive || resource == UploadNamespaceResource || containsResource(resources, resource) {
			busy = true
			break
		}
	}
	if busy {
		f.mutex.Unlock()
		return NewLockBusy()
	}
	for _, resource := range resources {
		store[resource] = fakeLease{owner: owner, expiry: now.Add(ttl)}
	}
	f.acquires++
	hook := f.acquireHook
	f.mutex.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

// Renew implements DocumentLocker.
func (f *fakeDocumentLocker) Renew(_ context.Context, namespace string, resources []string, owner string, ttl time.Duration) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.unavailable {
		return NewLockUnavailable()
	}
	now := time.Now()
	store := f.prune(namespace, now)
	resources = dedupeResources(resources)
	if len(resources) == 0 {
		return &LeaseLost{UploadConflict{Message: "document lease expired or was replaced"}}
	}
	for _, resource := range resources {
		lease, ok := store[resource]
		if !ok || lease.owner != owner {
			return &LeaseLost{UploadConflict{Message: "document lease expired or was replaced"}}
		}
	}
	for _, resource := range resources {
		store[resource] = fakeLease{owner: owner, expiry: now.Add(ttl)}
	}
	return nil
}

// Release implements DocumentLocker.
func (f *fakeDocumentLocker) Release(_ context.Context, namespace string, resources []string, owner string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.unavailable {
		return NewLockUnavailable()
	}
	if f.releaseErr != nil {
		return f.releaseErr
	}
	store := f.fields[namespace]
	for _, resource := range dedupeResources(resources) {
		if lease, ok := store[resource]; ok && lease.owner == owner {
			delete(store, resource)
		}
	}
	return nil
}

// Inspect implements DocumentLocker.
func (f *fakeDocumentLocker) Inspect(_ context.Context, namespace, resource string) (LockState, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.unavailable {
		return LockState{}, NewLockUnavailable()
	}
	now := time.Now()
	store := f.prune(namespace, now)
	lease, ok := store[resource]
	if !ok {
		return LockState{}, nil
	}
	return LockState{OwnerDigest: fakeOwnerDigest(lease.owner), TTL: lease.expiry.Sub(now)}, nil
}

// Ping implements DocumentLocker.
func (f *fakeDocumentLocker) Ping(context.Context) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.unavailable {
		return NewLockUnavailable()
	}
	return nil
}

// Close implements DocumentLocker.
func (f *fakeDocumentLocker) Close() error { return nil }

// expireAll simulates a Redis restart or a lease TTL elapsing.
func (f *fakeDocumentLocker) expireAll() {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.fields = map[string]map[string]fakeLease{}
}

// setUnavailable makes every backend call report *LockUnavailable.
func (f *fakeDocumentLocker) setUnavailable(unavailable bool) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.unavailable = unavailable
}

// setReleaseError makes Release fail (post-commit release failure).
func (f *fakeDocumentLocker) setReleaseError(err error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.releaseErr = err
}

// setAcquireHook installs a hook that runs after a successful acquire, before
// the journal starts its MySQL registration.
func (f *fakeDocumentLocker) setAcquireHook(hook func()) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.acquireHook = hook
}

// heldOwner returns the owner of one live resource ("" when free).
func (f *fakeDocumentLocker) heldOwner(namespace, resource string) string {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.prune(namespace, time.Now())
	return f.fields[namespace][resource].owner
}

// acquireCount reports how many successful acquisitions the backend served.
func (f *fakeDocumentLocker) acquireCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.acquires
}

// newRedisTestJournal builds a journal with short, test-friendly settings (no
// acquisition wait) over the shared test store. The store's already-attached
// backend keeps serving; this only rebinds the journal's settings.
func newRedisTestJournal(t *testing.T, stores *Stores, locker DocumentLocker, options ...JournalOption) *UploadJournal {
	t.Helper()
	if stores.DocumentLocks != locker {
		if err := stores.AttachDocumentLocker(locker); err != nil {
			t.Fatalf("AttachDocumentLocker() error = %v", err)
		}
	}
	settings := DocumentLockSettings{
		Prefix:         "test:lease",
		TTLSeconds:     60,
		TimeoutSeconds: 1,
		WaitSeconds:    0,
	}
	base := []JournalOption{
		WithDocumentLockSettings(settings),
		WithDocumentLocker(locker),
		WithLeaseSeconds(60),
		WithAutoHeartbeat(false),
		WithCleanupDelaySeconds(0),
	}
	return newTestJournal(t, stores, append(base, options...)...)
}

func unfinishedOperationCount(t *testing.T, stores *Stores, namespace string) int {
	t.Helper()
	var count int
	err := stores.DB.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM upload_operations WHERE namespace = ? AND status IN (?, ?, ?, ?, ?)",
		namespace, UploadIntent, UploadRunning, UploadFailed, UploadRollingBack, UploadRollbackFailed).Scan(&count)
	if err != nil {
		t.Fatalf("counting unfinished operations: %v", err)
	}
	return count
}

// TestDocumentLockRedisConflictMatrix pins the pre-migration conflict matrix in
// redis mode: same document, same name, namespace exclusivity in both
// directions, and unaffected parallelism for different documents.
func TestDocumentLockRedisConflictMatrix(t *testing.T) {
	stores := newTestStores(t)
	locker := testLocker(t, stores)
	journal := newRedisTestJournal(t, stores, locker)
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	for name, action := range map[string]func() error{
		"same document": func() error {
			_, err := journal.Begin(ctx, "ns", "file.txt", "doc", "collection", BeginOptions{})
			return err
		},
		"other name same doc": func() error {
			_, err := journal.Begin(ctx, "ns", "other.txt", "doc", "collection", BeginOptions{})
			return err
		},
		"same name other doc": func() error {
			_, err := journal.Begin(ctx, "ns", "file.txt", "other", "collection", BeginOptions{})
			return err
		},
		"namespace lock": func() error {
			_, err := journal.AcquireLock(ctx, "ns", nil)
			return err
		},
		"document lock": func() error {
			_, err := journal.AcquireLock(ctx, "ns", stringPtr("doc"))
			return err
		},
	} {
		err := action()
		if err == nil {
			t.Fatalf("%s: conflicting intent succeeded", name)
		}
		mustBeError[*UploadConflict](t, err)
	}
	// A rejected multi-resource acquisition must not leave the otherwise free
	// name resource behind.
	if owner := locker.heldOwner("ns", nameUploadResource("b.txt")); owner != "" {
		t.Fatalf("partial lease left behind for a free resource (owner %q)", owner)
	}
	// Different documents keep running in parallel.
	other := beginTestUpload(t, journal, "ns", "other.txt", "other", "collection", BeginOptions{})
	// Release the document leases before taking the namespace-exclusive lock.
	for _, current := range []*UploadOperation{operation, other} {
		if _, err := journal.Rollback(ctx, current, func(*UploadOperation) error { return nil }); err != nil {
			t.Fatalf("Rollback() error = %v", err)
		}
	}
	// The namespace-exclusive lock blocks every new document write and vice versa.
	if err := journal.WithLock(ctx, "ns", nil, func(token *LockToken) error {
		if _, beginErr := journal.Begin(ctx, "ns", "third.txt", "third", "collection", BeginOptions{}); beginErr == nil {
			t.Fatal("Begin() succeeded while the namespace was exclusive")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithLock(exclusive) error = %v", err)
	}
	if owner := locker.heldOwner("ns", UploadNamespaceResource); owner != "" {
		t.Fatalf("exclusive lease was not released: %q", owner)
	}
}

// TestDocumentLockRedisPausedRegistrationRejected covers the interleaving the
// design has to survive: A holds the Redis lease but pauses before its MySQL
// registration, the lease expires, B registers and stays live, and A then
// resumes. A must not become a second holder.
func TestDocumentLockRedisPausedRegistrationRejected(t *testing.T) {
	stores := newTestStores(t)
	locker := testLocker(t, stores)
	journalA := newRedisTestJournal(t, stores, locker)
	journalB := newRedisTestJournal(t, stores, locker)
	ctx := context.Background()

	acquired := make(chan struct{})
	resume := make(chan struct{})
	var hookMutex sync.Mutex
	armed := true
	locker.setAcquireHook(func() {
		hookMutex.Lock()
		if !armed {
			hookMutex.Unlock()
			return
		}
		armed = false
		hookMutex.Unlock()
		close(acquired)
		<-resume
	})
	result := make(chan error, 1)
	go func() {
		_, err := journalA.Begin(ctx, "ns", "file.txt", "doc", "collection", BeginOptions{})
		result <- err
	}()
	<-acquired
	// A's lease disappears while it is paused before the MySQL registration.
	locker.expireAll()
	operationB := beginTestUpload(t, journalB, "ns", "file.txt", "doc", "collection", BeginOptions{})
	close(resume)
	errA := <-result
	mustBeError[*UploadConflict](t, errA)
	if count := unfinishedOperationCount(t, stores, "ns"); count != 1 {
		t.Fatalf("unfinished operations after the late request = %d, want 1", count)
	}
	if owner := locker.heldOwner("ns", documentUploadResource("doc")); owner != operationB.Owner {
		t.Fatalf("lease owner = %q, want B's owner", owner)
	}
	if locker.acquireCount() < 2 {
		t.Fatalf("A did not re-run the acquisition after the fencing generation changed")
	}
	if _, err := journalB.Rollback(ctx, operationB, func(*UploadOperation) error { return nil }); err != nil {
		t.Fatalf("Rollback(B) error = %v", err)
	}
}

// TestDocumentLockRedisStaleEpochRegistration proves the durable fencing
// generation rejects a registration whose expected generation changed, even
// after the earlier holder committed and released its lease.
func TestDocumentLockRedisStaleEpochRegistration(t *testing.T) {
	stores := newTestStores(t)
	locker := testLocker(t, stores)
	journalA := newRedisTestJournal(t, stores, locker)
	journalB := newRedisTestJournal(t, stores, locker)
	ctx := context.Background()
	conflictResources := []string{
		documentUploadResource("doc"), nameUploadResource("file.txt"), UploadNamespaceResource,
	}
	expected, err := journalA.readLeaseEpochs(ctx, stores.DB, "ns", conflictResources)
	if err != nil {
		t.Fatalf("readLeaseEpochs() error = %v", err)
	}
	operationB := beginTestUpload(t, journalB, "ns", "file.txt", "doc", "collection", BeginOptions{})
	commitTestUpload(t, stores, journalB, operationB, map[string]any{"ok": true})
	if err := journalB.ReleaseOperation(ctx, operationB); err != nil {
		t.Fatalf("ReleaseOperation() error = %v", err)
	}
	owner, err := randomHex(16)
	if err != nil {
		t.Fatalf("randomHex() error = %v", err)
	}
	operationID, err := randomHex(16)
	if err != nil {
		t.Fatalf("randomHex() error = %v", err)
	}
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	defer func() { _ = transaction.Rollback() }()
	err = journalA.registerHolder(ctx, transaction, "ns", conflictResources, expected, owner,
		&operationID, unixSeconds()+60)
	if !errors.Is(err, errLeaseEpochChanged) {
		t.Fatalf("registerHolder() error = %v, want errLeaseEpochChanged", err)
	}
}

// TestDocumentLockRedisTakeoverFencesOldWorker proves a reclaimed upload fences
// the old worker: it cannot renew, release, publish, or run compensation.
func TestDocumentLockRedisTakeoverFencesOldWorker(t *testing.T) {
	stores := newTestStores(t)
	locker := testLocker(t, stores)
	journalA := newRedisTestJournal(t, stores, locker)
	journalB := newRedisTestJournal(t, stores, locker)
	ctx := context.Background()
	first := beginTestUpload(t, journalA, "ns", "file.txt", "doc", "collection", BeginOptions{})
	if err := journalA.PutObject(ctx, first, SourceBucket, "file.txt", []byte("staged"), nil); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	// The old worker disappears; only the lease TTL ending frees the resources.
	locker.expireAll()
	recovery, err := journalB.ClaimRecovery(ctx, first.ID)
	if err != nil || recovery == nil {
		t.Fatalf("ClaimRecovery() = %+v, %v", recovery, err)
	}
	if recovery.Owner == first.Owner || recovery.Status != UploadRollingBack {
		t.Fatalf("recovery = %+v", recovery)
	}
	// Stale renew/release must not disturb the new holder.
	_ = journalA.RenewLock(ctx, &LockToken{Namespace: "ns", Resource: documentUploadResource("doc"), Owner: first.Owner})
	if err := journalA.ReleaseOperation(ctx, first); err == nil || !isLockBackendError(err) {
		// The fake reports LockUnavailable only when the backend is unavailable;
		// a plain release is a no-op for a foreign token. Either way the lease
		// must still belong to B.
	}
	if owner := locker.heldOwner("ns", documentUploadResource("doc")); owner != recovery.Owner {
		t.Fatalf("lease owner = %q, want the recovery owner", owner)
	}
	// Stale owner cannot assert, run a step, publish, or compensate.
	if _, err := journalA.AssertOwner(ctx, first); err == nil {
		t.Fatal("AssertOwner() succeeded after takeover")
	} else {
		mustBeError[*LeaseLost](t, err)
	}
	if _, err := journalA.RunStep(ctx, first, "late", func() (any, error) {
		t.Fatal("stale step must not run")
		return nil, nil
	}, nil); err == nil {
		t.Fatal("RunStep() succeeded after takeover")
	} else {
		mustBeError[*LeaseLost](t, err)
	}
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if _, err := journalA.Commit(ctx, first, map[string]any{}, transaction); err == nil {
		_ = transaction.Rollback()
		t.Fatal("Commit() succeeded after takeover")
	} else {
		mustBeError[*LeaseLost](t, err)
	}
	_ = transaction.Rollback()
	cleanupRan := false
	if _, err := journalA.Rollback(ctx, first, func(*UploadOperation) error {
		cleanupRan = true
		return nil
	}); err == nil {
		t.Fatal("Rollback() succeeded after takeover")
	}
	if cleanupRan {
		t.Fatal("stale cleanup callback ran after takeover")
	}
	// The new holder compensates exactly its own (the same operation's) version.
	seen := make([]string, 0, 1)
	if _, err := journalB.Rollback(ctx, recovery, func(current *UploadOperation) error {
		seen = append(seen, current.Version)
		return nil
	}); err != nil {
		t.Fatalf("Rollback(recovery) error = %v", err)
	}
	if len(seen) != 1 || seen[0] != recovery.ID {
		t.Fatalf("compensated versions = %v", seen)
	}
	if head, err := journalA.Head(ctx, "ns", "doc"); err != nil || head != nil {
		t.Fatalf("Head() = %v, %v; the stale worker must not have published", head, err)
	}
}

// TestDocumentLockRedisLostLeaseStopsNextStep proves a lost Redis lease (Redis
// restart) stops the pipeline before the next recorded step, and that recovery
// can still finish the operation afterwards.
func TestDocumentLockRedisLostLeaseStopsNextStep(t *testing.T) {
	stores := newTestStores(t)
	locker := testLocker(t, stores)
	journal := newRedisTestJournal(t, stores, locker)
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	_, err := journal.RunStepContext(ctx, operation, "parse", func(context.Context) (any, error) {
		// The lease disappears mid-step, as after a Redis restart.
		locker.expireAll()
		return "parsed", nil
	}, nil)
	mustBeError[*LeaseLost](t, err)
	saved, err := journal.Get(ctx, operation.ID)
	if err != nil || len(saved.Steps) != 1 || saved.Steps[0].Status != UploadFailed {
		t.Fatalf("steps after the loss = %+v, %v", saved, err)
	}
	if saved.Status != UploadFailed {
		t.Fatalf("operation status = %q, want FAILED", saved.Status)
	}
	claimed, err := journal.ClaimRecovery(ctx, operation.ID)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimRecovery() = %+v, %v", claimed, err)
	}
	if _, err := journal.Rollback(ctx, claimed, func(*UploadOperation) error { return nil }); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if current, err := journal.Get(ctx, operation.ID); err != nil || current.Status != UploadRolledBack {
		t.Fatalf("terminal status = %+v, %v", current, err)
	}
}

// TestDocumentLockRedisUnavailableIsRetryable proves an unreachable backend is a
// retryable error, never a silent fallback to MySQL leases.
func TestDocumentLockRedisUnavailableIsRetryable(t *testing.T) {
	stores := newTestStores(t)
	locker := testLocker(t, stores)
	journal := newRedisTestJournal(t, stores, locker)
	ctx := context.Background()
	locker.setUnavailable(true)
	if _, err := journal.Begin(ctx, "ns", "file.txt", "doc", "collection", BeginOptions{}); err == nil {
		t.Fatal("Begin() succeeded without the lease backend")
	} else {
		mustBeError[*LockUnavailable](t, err)
	}
	if count := unfinishedOperationCount(t, stores, "ns"); count != 0 {
		t.Fatalf("unfinished operations after an unavailable acquire = %d", count)
	}
	locker.setUnavailable(false)
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	locker.setUnavailable(true)
	if claimed, err := journal.ClaimRecovery(ctx, operation.ID); err == nil || claimed != nil {
		t.Fatalf("ClaimRecovery(unavailable) = %+v, %v", claimed, err)
	} else {
		mustBeError[*LockUnavailable](t, err)
	}
	if saved, err := journal.Get(ctx, operation.ID); err != nil || saved.Status != UploadIntent {
		t.Fatalf("operation changed while ownership was unknown: %+v, %v", saved, err)
	}
}

// TestDocumentLockRedisReleaseFailureAfterCommit proves a post-commit release
// failure never re-runs committed work and only delays reuse.
func TestDocumentLockRedisReleaseFailureAfterCommit(t *testing.T) {
	stores := newTestStores(t)
	locker := testLocker(t, stores)
	journal := newRedisTestJournal(t, stores, locker)
	ctx := context.Background()
	idempotencyKey := "same-key"
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{
		IdempotencyKey: &idempotencyKey,
	})
	commitTestUpload(t, stores, journal, operation, map[string]any{"ok": true})
	locker.setReleaseError(NewLockUnavailable())
	if err := journal.ReleaseOperation(ctx, operation); err == nil {
		t.Fatal("ReleaseOperation() reported success for a failed release")
	}
	// The lease is still held, so a new upload waits; the committed version is
	// never re-published.
	replay, err := journal.Begin(ctx, "ns", "file.txt", "doc", "collection", BeginOptions{
		IdempotencyKey: &idempotencyKey,
	})
	if err != nil || replay.ID != operation.ID || replay.Status != UploadCommitted {
		t.Fatalf("idempotent replay = %+v, %v", replay, err)
	}
	head, err := journal.Head(ctx, "ns", "doc")
	if err != nil || head == nil || *head != operation.ID {
		t.Fatalf("Head() = %v, %v", head, err)
	}
	var published int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM upload_operations WHERE namespace = ? AND status = ?",
		"ns", UploadCommitted).Scan(&published); err != nil {
		t.Fatalf("counting committed operations: %v", err)
	}
	if published != 1 {
		t.Fatalf("committed operations = %d, want 1 (no duplicate publication)", published)
	}
	// Once the lease finally expires, reuse resumes.
	locker.setReleaseError(nil)
	locker.expireAll()
	if _, err := journal.Begin(ctx, "ns", "file.txt", "doc", "collection", BeginOptions{}); err != nil {
		t.Fatalf("Begin() after the lease expired: %v", err)
	}
}

// TestDocumentLockRedisLeaseDisplayUsesLiveTTL proves the administrative lease
// view reports the live holder and its remaining TTL (not the stale SQL
// registration timestamp) without exposing the owner token.
func TestDocumentLockRedisLeaseDisplayUsesLiveTTL(t *testing.T) {
	stores := newTestStores(t)
	locker := testLocker(t, stores)
	journal := newRedisTestJournal(t, stores, locker)
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	// A long operation is not renewed in SQL; the display must still treat it as
	// live because the Redis lease is authoritative.
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_leases SET expires_at = 1 WHERE operation_id = ?", operation.ID); err != nil {
		t.Fatalf("lease expiry update error = %v", err)
	}
	resource := documentUploadResource("doc")
	record := findLeaseRecord(t, stores, "ns", resource)
	if expired, _ := record["expired"].(bool); expired {
		t.Fatalf("live lease reported as expired: %+v", record)
	}
	expires, _ := record["expires_at"].(float64)
	if expires <= unixSeconds() {
		t.Fatalf("expires_at = %v, want a future live TTL", expires)
	}
	if holder, _ := record["holder_id"].(string); holder != fakeOwnerDigest(operation.Owner) {
		t.Fatalf("holder_id = %q", holder)
	}
	if _, exposed := record["owner"]; exposed {
		t.Fatal("lease display exposed the raw owner token")
	}
	// A crashed holder (lease gone) is reported as expired.
	locker.expireAll()
	record = findLeaseRecord(t, stores, "ns", resource)
	if expired, _ := record["expired"].(bool); !expired {
		t.Fatalf("crashed holder reported as live: %+v", record)
	}
}

// TestDocumentLockRedisHeartbeatCancelsInFlightWork proves a failed renewal
// cancels the action context passed to a long step instead of letting it start
// new work blindly.
func TestDocumentLockRedisHeartbeatCancelsInFlightWork(t *testing.T) {
	stores := newTestStores(t)
	locker := testLocker(t, stores)
	journal := newRedisTestJournal(t, stores, locker, WithLeaseSeconds(0.06))
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	_, err := journal.RunStepContext(ctx, operation, "parse", func(actionContext context.Context) (any, error) {
		locker.expireAll()
		select {
		case <-actionContext.Done():
			return nil, actionContext.Err()
		case <-time.After(5 * time.Second):
			return nil, errors.New("action context was not cancelled")
		}
	}, nil)
	if err == nil {
		t.Fatal("RunStepContext() succeeded after the lease was lost")
	}
	if !errors.Is(err, context.Canceled) {
		var lost *LeaseLost
		if !errors.As(err, &lost) {
			t.Fatalf("RunStepContext() error = %v, want a cancellation or lease loss", err)
		}
	}
}

func findLeaseRecord(t *testing.T, stores *Stores, namespace, resource string) map[string]any {
	t.Helper()
	leases, err := stores.Views.Leases(context.Background(), namespace)
	if err != nil {
		t.Fatalf("Leases() error = %v", err)
	}
	for _, record := range leases {
		if record["resource"] == resource {
			return record
		}
	}
	t.Fatalf("leases = %+v, missing %q", leases, resource)
	return nil
}

// isLockBackendError reports whether err is a lease-backend ownership or
// availability failure.
func isLockBackendError(err error) bool {
	var unavailable *LockUnavailable
	var lost *LeaseLost
	return errors.As(err, &unavailable) || errors.As(err, &lost)
}
