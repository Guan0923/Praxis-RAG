package state

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// OSError makes the persisted error type name match Python's "OSError".
type OSError struct{ message string }

func (e *OSError) Error() string {
	if e.message == "" {
		return "controlled failure"
	}
	return e.message
}

func newTestJournal(t *testing.T, stores *Stores, options ...JournalOption) *UploadJournal {
	t.Helper()
	base := []JournalOption{WithLeaseSeconds(60), WithAutoHeartbeat(false), WithCleanupDelaySeconds(0)}
	journal, err := NewUploadJournal(stores, append(base, options...)...)
	if err != nil {
		t.Fatalf("NewUploadJournal() error = %v", err)
	}
	t.Cleanup(journal.Close)
	return journal
}

func beginTestUpload(t *testing.T, journal *UploadJournal, namespace, name, documentID, collection string, options BeginOptions) *UploadOperation {
	t.Helper()
	operation, err := journal.Begin(context.Background(), namespace, name, documentID, collection, options)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	return operation
}

func commitTestUpload(t *testing.T, stores *Stores, journal *UploadJournal, operation *UploadOperation, result any) *UploadOperation {
	t.Helper()
	transaction, err := stores.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	defer func() { _ = transaction.Rollback() }()
	committed, err := journal.Commit(context.Background(), operation, result, transaction)
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("tx.Commit() error = %v", err)
	}
	// The publishing transaction is durable; the runtime releases the Redis
	// lease right after commit (documents_publish.go publishDocument).
	if err := journal.ReleaseOperation(context.Background(), committed); err != nil {
		t.Fatalf("ReleaseOperation() error = %v", err)
	}
	return committed
}

func expireUploadLeases(t *testing.T, stores *Stores, operationID string) {
	t.Helper()
	testLocker(t, stores).expireAll()
}

func stringPtr(value string) *string { return &value }

func TestUploadIntentStepsWorkspaceAndImmutableObjects(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	if len(operation.ID) != 32 || operation.ID != operation.Version || operation.Status != UploadIntent {
		t.Fatalf("unexpected operation: %+v", operation)
	}
	if _, err := journal.RegisterWorkspace(ctx, operation, "workspace\\"+operation.ID, stringPtr("node-one")); err != nil {
		t.Fatalf("RegisterWorkspace() error = %v", err)
	}
	saved, err := journal.Get(ctx, operation.ID)
	if err != nil || saved.Workspace == nil || operation.Workspace == nil || *saved.Workspace != *operation.Workspace {
		t.Fatalf("Get() workspace = %+v, %v", saved, err)
	}
	if saved.WorkspaceNode == nil || *saved.WorkspaceNode != "node-one" {
		t.Fatalf("workspace node = %+v", saved.WorkspaceNode)
	}
	if _, err := journal.RegisterWorkspace(ctx, operation, "other", nil); err == nil {
		t.Fatal("RegisterWorkspace() accepted a different path")
	}
	result, err := journal.RunStep(ctx, operation, "source", func() (any, error) {
		persisted, err := journal.Get(ctx, operation.ID)
		if err != nil {
			return nil, err
		}
		if persisted.Status != UploadRunning {
			return nil, errors.New("operation not RUNNING during action")
		}
		if len(persisted.Steps) != 1 || persisted.Steps[0].Status != UploadRunning {
			return nil, errors.New("step not RUNNING during action")
		}
		if err := journal.PutObject(ctx, operation, SourceBucket, "file.txt", []byte("hello"),
			map[string]any{"ref": "source"}); err != nil {
			return nil, err
		}
		return 42, nil
	}, map[string]any{"key": "file.txt"})
	if err != nil || result != 42 {
		t.Fatalf("RunStep() = %v, %v", result, err)
	}
	if err := journal.PutObject(ctx, operation, SourceBucket, "file.txt", []byte("hello"),
		map[string]any{"ref": "source"}); err != nil {
		t.Fatalf("identical PutObject() error = %v", err)
	}
	err = journal.PutObject(ctx, operation, SourceBucket, "file.txt", []byte("changed"), nil)
	if conflict := mustBeError[*UploadConflict](t, err); !strings.Contains(conflict.Message, "immutable") {
		t.Fatalf("immutability message = %q", conflict.Message)
	}
	if _, err := journal.RunStep(ctx, operation, "source", func() (any, error) {
		t.Fatal("duplicate step action must not run")
		return nil, nil
	}, nil); err == nil {
		t.Fatal("duplicate RunStep() succeeded")
	}
	content, found, err := journal.GetObject(ctx, operation.ID, SourceBucket, "file.txt")
	if err != nil || !found || string(content) != "hello" {
		t.Fatalf("GetObject() = %q, %v, %v", content, found, err)
	}
	if _, found, err := journal.GetObject(ctx, operation.ID, SourceBucket, "missing"); err != nil || found {
		t.Fatalf("GetObject(missing) found=%v err=%v", found, err)
	}
}

func TestUploadCommitAtomicWithDocumentsBlobsAndReplay(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	idempotencyKey := "key"
	fingerprint := "sha"
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{
		IdempotencyKey: &idempotencyKey,
		Fingerprint:    &fingerprint,
	})
	if err := journal.PutObject(ctx, operation, SourceBucket, "file.txt", []byte("new"), nil); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := stores.Blobs.PutTx(ctx, transaction, "ns", SourceBucket, "file.txt", []byte("new"), nil); err != nil {
		t.Fatalf("Blobs.PutTx() error = %v", err)
	}
	if _, err := journal.Commit(ctx, operation, map[string]any{"file": "file.txt"}, transaction); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if head, err := journal.Head(ctx, "ns", "doc"); err != nil || head != nil {
		t.Fatalf("Head() after rollback = %v, %v", head, err)
	}
	current, err := journal.Get(ctx, operation.ID)
	if err != nil || current.Status != UploadIntent {
		t.Fatalf("status after rollback = %+v, %v", current, err)
	}
	if content, found, err := stores.Blobs.Get(ctx, "ns", SourceBucket, "file.txt"); err != nil || found {
		t.Fatalf("blob survived rollback: %q, %v, %v", content, found, err)
	}

	transaction, err = stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := stores.Documents.UpsertTx(ctx, transaction, "ns", DocumentRecord{
		Name: "file.txt", DocumentID: "doc", Collection: "collection",
	}); err != nil {
		t.Fatalf("Documents.UpsertTx() error = %v", err)
	}
	if err := stores.Blobs.PutTx(ctx, transaction, "ns", SourceBucket, "file.txt", []byte("new"), nil); err != nil {
		t.Fatalf("Blobs.PutTx() error = %v", err)
	}
	if _, err := journal.Commit(ctx, operation, map[string]any{"file": "file.txt"}, transaction); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("Commit tx error = %v", err)
	}
	replay, err := journal.Begin(ctx, "ns", "file.txt", "doc", "collection", BeginOptions{
		IdempotencyKey: &idempotencyKey,
		Fingerprint:    &fingerprint,
	})
	if err != nil {
		t.Fatalf("replay Begin() error = %v", err)
	}
	if replay.ID != operation.ID || replay.Status != UploadCommitted {
		t.Fatalf("replay = %+v", replay)
	}
	if result, ok := replay.Result.(map[string]any); !ok || result["file"] != "file.txt" {
		t.Fatalf("replay result = %#v", replay.Result)
	}
	active, err := journal.ActiveVersions(ctx, "ns")
	if err != nil || active["doc"] != operation.ID {
		t.Fatalf("ActiveVersions() = %v, %v", active, err)
	}
	if record, err := stores.Documents.Get(ctx, "ns", "file.txt"); err != nil || record == nil || record.DocumentID != "doc" {
		t.Fatalf("Documents.Get() = %+v, %v", record, err)
	}
	unfinished, err := journal.Unfinished(ctx, nil)
	if err != nil || len(unfinished) != 0 {
		t.Fatalf("Unfinished() = %v, %v", unfinished, err)
	}
	different := "different"
	if _, err := journal.Begin(ctx, "ns", "file.txt", "doc", "collection", BeginOptions{
		IdempotencyKey: &idempotencyKey, Fingerprint: &different,
	}); err == nil {
		t.Fatal("Begin() with a different fingerprint succeeded")
	}
	if _, err := journal.Rollback(ctx, operation, func(*UploadOperation) error {
		t.Fatal("committed rollback callback must not run")
		return nil
	}); err == nil {
		t.Fatal("Rollback() of a committed version succeeded")
	}
	if content, found, err := journal.GetObject(ctx, operation.ID, SourceBucket, "file.txt"); err != nil || !found || string(content) != "new" {
		t.Fatalf("committed object = %q, %v, %v", content, found, err)
	}
}

func TestUploadDocumentNamespaceAndNameLocks(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
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
		if err := action(); err == nil {
			t.Fatalf("%s: conflicting intent succeeded", name)
		} else {
			mustBeError[*UploadConflict](t, err)
		}
	}
	other := beginTestUpload(t, journal, "ns", "other.txt", "other", "collection", BeginOptions{})
	if _, err := journal.Rollback(ctx, other, func(*UploadOperation) error { return nil }); err != nil {
		t.Fatalf("Rollback(other) error = %v", err)
	}
	if _, err := journal.Rollback(ctx, operation, func(*UploadOperation) error { return nil }); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if err := journal.WithLock(ctx, "ns", nil, func(token *LockToken) error {
		if err := journal.AssertLock(ctx, token); err != nil {
			return err
		}
		if _, err := journal.Begin(ctx, "ns", "file.txt", "doc", "collection", BeginOptions{}); err == nil {
			t.Fatal("Begin() succeeded while the namespace lock was held")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithLock() error = %v", err)
	}
	beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
}

func TestUploadExpiredRecoveryFencesOldWorkerAndPreservesActive(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	first := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	if err := journal.PutObject(ctx, first, SourceBucket, "file.txt", []byte("active"), nil); err != nil {
		t.Fatalf("PutObject(active) error = %v", err)
	}
	commitTestUpload(t, stores, journal, first, map[string]any{"ok": true})

	failedKey := "failed"
	failed := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{
		IdempotencyKey: &failedKey,
	})
	if err := journal.PutObject(ctx, failed, SourceBucket, "file.txt", []byte("staged"), nil); err != nil {
		t.Fatalf("PutObject(staged) error = %v", err)
	}
	if failed.OldVersion == nil || *failed.OldVersion != first.ID {
		t.Fatalf("old_version = %v, want %s", failed.OldVersion, first.ID)
	}
	if claimed, err := journal.ClaimRecovery(ctx, failed.ID); err != nil || claimed != nil {
		t.Fatalf("ClaimRecovery(live) = %+v, %v", claimed, err)
	}
	if _, err := journal.Begin(ctx, "ns", "file.txt", "doc", "collection", BeginOptions{
		IdempotencyKey: &failedKey,
	}); err == nil {
		t.Fatal("Begin() with an unfinished idempotency key succeeded")
	}
	expireUploadLeases(t, stores, failed.ID)
	recovery, err := journal.ClaimRecovery(ctx, failed.ID)
	if err != nil || recovery == nil {
		t.Fatalf("ClaimRecovery(expired) = %+v, %v", recovery, err)
	}
	if recovery.Owner == failed.Owner {
		t.Fatal("recovery reused the expired owner")
	}
	for name, action := range map[string]func() error{
		"assert owner": func() error { _, err := journal.AssertOwner(ctx, failed); return err },
		"run step": func() error {
			_, err := journal.RunStep(ctx, failed, "late", func() (any, error) {
				t.Fatal("stale action must not run")
				return nil, nil
			}, nil)
			return err
		},
		"put object": func() error {
			return journal.PutObject(ctx, failed, SourceBucket, "x", []byte("bad"), nil)
		},
		"commit": func() error {
			transaction, err := stores.DB.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = transaction.Rollback() }()
			_, err = journal.Commit(ctx, failed, map[string]any{}, transaction)
			return err
		},
		"rollback": func() error {
			_, err := journal.Rollback(ctx, failed, func(*UploadOperation) error {
				t.Fatal("stale cleanup must not run")
				return nil
			})
			return err
		},
	} {
		if err := action(); err == nil {
			t.Fatalf("%s: stale worker action succeeded", name)
		} else {
			mustBeError[*LeaseLost](t, err)
		}
	}
	seen := make([]string, 0)
	if _, err := journal.Rollback(ctx, recovery, func(current *UploadOperation) error {
		seen = append(seen, current.Collection+"/"+current.DocumentID+"/"+current.Version)
		return nil
	}); err != nil {
		t.Fatalf("rollback recovery error = %v", err)
	}
	if len(seen) != 1 || seen[0] != "collection/doc/"+failed.ID {
		t.Fatalf("rollback callback saw %v", seen)
	}
	active, err := journal.ActiveVersions(ctx, "ns")
	if err != nil || active["doc"] != first.ID {
		t.Fatalf("ActiveVersions() = %v, %v", active, err)
	}
	if content, found, err := journal.GetObject(ctx, first.ID, SourceBucket, "file.txt"); err != nil || !found || string(content) != "active" {
		t.Fatalf("active object = %q, %v, %v", content, found, err)
	}
	if _, found, err := journal.GetObject(ctx, failed.ID, SourceBucket, "file.txt"); err != nil || found {
		t.Fatalf("rolled-back object survived: found=%v err=%v", found, err)
	}
	if claimed, err := journal.ClaimRecovery(ctx, failed.ID); err != nil || claimed != nil {
		t.Fatalf("second ClaimRecovery() = %+v, %v", claimed, err)
	}
}

func TestUploadFailuresAuditedAndRollbackRetries(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	if err := journal.PutObject(ctx, operation, SourceBucket, "file.txt", []byte("staged"), nil); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	actionErr := &OSError{message: "secret must not be persisted"}
	if _, err := journal.RunStep(ctx, operation, "network", func() (any, error) {
		return nil, actionErr
	}, nil); err == nil {
		t.Fatal("RunStep() succeeded")
	}
	saved, err := journal.Get(ctx, operation.ID)
	if err != nil || saved.Status != UploadFailed {
		t.Fatalf("status = %+v, %v", saved, err)
	}
	if saved.Error == nil || *saved.Error != "OSError" {
		t.Fatalf("error type = %v, want OSError", saved.Error)
	}
	if len(saved.Steps) != 1 || saved.Steps[0].Status != UploadFailed {
		t.Fatalf("steps = %+v", saved.Steps)
	}
	if claimed, err := journal.ClaimRecovery(ctx, operation.ID); err != nil || claimed != nil {
		t.Fatalf("ClaimRecovery(FAILED) = %+v, %v", claimed, err)
	}
	if _, err := journal.Rollback(ctx, operation, func(*UploadOperation) error { return &OSError{} }); err == nil {
		t.Fatal("failing rollback succeeded")
	}
	saved, err = journal.Get(ctx, operation.ID)
	if err != nil || saved.Status != UploadRollbackFailed {
		t.Fatalf("status after failed rollback = %+v, %v", saved, err)
	}
	if content, found, err := journal.GetObject(ctx, operation.ID, SourceBucket, "file.txt"); err != nil || !found || string(content) != "staged" {
		t.Fatalf("object after failed rollback = %q, %v, %v", content, found, err)
	}
	expireUploadLeases(t, stores, operation.ID)
	recovery, err := journal.ClaimRecovery(ctx, operation.ID)
	if err != nil || recovery == nil {
		t.Fatalf("ClaimRecovery(ROLLBACK_FAILED) = %+v, %v", recovery, err)
	}
	if _, err := journal.Rollback(ctx, recovery, func(*UploadOperation) error { return nil }); err != nil {
		t.Fatalf("recovery rollback error = %v", err)
	}
	saved, err = journal.Get(ctx, operation.ID)
	if err != nil || saved.Status != UploadRolledBack {
		t.Fatalf("status after recovery = %+v, %v", saved, err)
	}
	if saved.Steps[0].Status != UploadFailed {
		t.Fatalf("step status was overwritten: %+v", saved.Steps[0])
	}
}

func TestUploadFenceAfterSideEffectKeepsUncertainStep(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	_, err := journal.RunStep(context.Background(), operation, "uncertain", func() (any, error) {
		expireUploadLeases(t, stores, operation.ID)
		claimed, claimErr := journal.ClaimRecovery(context.Background(), operation.ID)
		if claimErr != nil || claimed == nil {
			return nil, errors.New("recovery claim failed")
		}
		return nil, nil
	}, nil)
	mustBeError[*LeaseLost](t, err)
	saved, getErr := journal.Get(context.Background(), operation.ID)
	if getErr != nil || len(saved.Steps) != 1 || saved.Steps[0].Status != UploadRunning {
		t.Fatalf("uncertain step = %+v, %v", saved, getErr)
	}
}

func TestUploadCommitHeartbeatScopeAndCleanupRetry(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	first := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	if err := journal.PutObject(ctx, first, SourceBucket, "file.txt", []byte("old"), nil); err != nil {
		t.Fatalf("PutObject(old) error = %v", err)
	}
	if err := journal.WithHeartbeat(ctx, first, func() error {
		committed := commitTestUpload(t, stores, journal, first, map[string]any{"ok": true})
		if committed.Status != UploadCommitted {
			t.Fatalf("committed status = %q", committed.Status)
		}
		return nil
	}); err != nil {
		t.Fatalf("commit under heartbeat error = %v", err)
	}
	second := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	if err := journal.PutObject(ctx, second, SourceBucket, "file.txt", []byte("new"), nil); err != nil {
		t.Fatalf("PutObject(new) error = %v", err)
	}
	commitTestUpload(t, stores, journal, second, map[string]any{"ok": true})
	oldVersion, err := journal.OldVersion(ctx, second)
	if err != nil || oldVersion == nil || *oldVersion != first.ID {
		t.Fatalf("OldVersion() = %v, %v", oldVersion, err)
	}
	unfinished, err := journal.Unfinished(ctx, stringPtr("ns"))
	if err != nil || len(unfinished) != 1 || !unfinished[0].CleanupPending {
		t.Fatalf("Unfinished() = %+v, %v", unfinished, err)
	}
	if _, err := journal.Cleanup(ctx, second, func(*UploadOperation) error { return &OSError{} }); err == nil {
		t.Fatal("failing cleanup succeeded")
	}
	current, err := journal.Get(ctx, second.ID)
	if err != nil || !current.CleanupPending {
		t.Fatalf("cleanup_pending after failure = %+v, %v", current, err)
	}
	seen := make([]string, 0)
	if _, err := journal.Cleanup(ctx, second, func(current *UploadOperation) error {
		if current.OldVersion != nil {
			seen = append(seen, *current.OldVersion)
		}
		return nil
	}); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if len(seen) != 1 || seen[0] != first.ID {
		t.Fatalf("cleanup saw %v", seen)
	}
	if _, err := journal.Cleanup(ctx, second, func(*UploadOperation) error {
		t.Fatal("already-clean cleanup callback ran")
		return nil
	}); err != nil {
		t.Fatalf("second Cleanup() error = %v", err)
	}
	if _, found, err := journal.GetObject(ctx, first.ID, SourceBucket, "file.txt"); err != nil || found {
		t.Fatalf("old object survived cleanup: found=%v err=%v", found, err)
	}
	if content, found, err := journal.GetObject(ctx, second.ID, SourceBucket, "file.txt"); err != nil || !found || string(content) != "new" {
		t.Fatalf("new object = %q, %v, %v", content, found, err)
	}
	if head, err := journal.Head(ctx, "ns", "doc"); err != nil || head == nil || *head != second.ID {
		t.Fatalf("Head() = %v, %v", head, err)
	}
	if unfinished, err := journal.Unfinished(ctx, nil); err != nil || len(unfinished) != 0 {
		t.Fatalf("Unfinished() = %v, %v", unfinished, err)
	}
}

func TestUploadDefaultCleanupDelay(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	first := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	commitTestUpload(t, stores, journal, first, map[string]any{"ok": true})
	second := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	commitTestUpload(t, stores, journal, second, map[string]any{"ok": true})
	delayed, err := NewUploadJournal(stores, WithAutoHeartbeat(false))
	if err != nil {
		t.Fatalf("NewUploadJournal() error = %v", err)
	}
	defer delayed.Close()
	if delayed.CleanupDelaySeconds < 3600 {
		t.Fatalf("default cleanup delay = %v", delayed.CleanupDelaySeconds)
	}
	result, err := delayed.Cleanup(ctx, second, func(*UploadOperation) error {
		t.Fatal("too-early cleanup callback ran")
		return nil
	})
	if err != nil || !result.CleanupPending {
		t.Fatalf("delayed cleanup = %+v, %v", result, err)
	}
}

func TestUploadHeartbeatRenewsDuringLongCallback(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores, WithLeaseSeconds(3.0), WithAutoHeartbeat(false))
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	result, err := journal.RunStep(context.Background(), operation, "slow", func() (any, error) {
		time.Sleep(2500 * time.Millisecond)
		claimed, claimErr := journal.ClaimRecovery(context.Background(), operation.ID)
		if claimErr != nil {
			return nil, claimErr
		}
		if claimed != nil {
			return nil, errors.New("live lease was stolen")
		}
		return "ok", nil
	}, nil)
	if err != nil || result != "ok" {
		t.Fatalf("RunStep() = %v, %v", result, err)
	}
	if _, err := journal.Rollback(context.Background(), operation, func(*UploadOperation) error { return nil }); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
}

func TestUploadAutomaticHeartbeatCoversWorkBetweenSteps(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores, WithLeaseSeconds(3.0), WithAutoHeartbeat(true))
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	time.Sleep(2500 * time.Millisecond)
	if claimed, err := journal.ClaimRecovery(ctx, operation.ID); err != nil || claimed != nil {
		t.Fatalf("ClaimRecovery() = %+v, %v", claimed, err)
	}
	if _, err := journal.AssertOwner(ctx, operation); err != nil {
		t.Fatalf("AssertOwner() error = %v", err)
	}
	if _, err := journal.Rollback(ctx, operation, func(*UploadOperation) error { return nil }); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
}

func TestUploadDurableRestartAndIdempotency(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	idempotencyKey := "same"
	var waitGroup sync.WaitGroup
	start := make(chan struct{})
	results := make([]*UploadOperation, 2)
	errs := make([]error, 2)
	for index := 0; index < 2; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			<-start
			results[index], errs[index] = journal.Begin(ctx, "ns", "file.txt", "doc", "collection", BeginOptions{
				IdempotencyKey: &idempotencyKey,
			})
		}(index)
	}
	close(start)
	waitGroup.Wait()
	successes := 0
	for index := 0; index < 2; index++ {
		if errs[index] == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent Begin() successes = %d, errors = %v", successes, errs)
	}
	var operation *UploadOperation
	for index := 0; index < 2; index++ {
		if results[index] != nil {
			operation = results[index]
		}
	}
	settings := stores.Settings
	locker := stores.DocumentLocks
	if err := stores.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	restartedStores, err := New(settings, true)
	if err != nil {
		t.Fatalf("New(restarted) error = %v", err)
	}
	if locker != nil {
		if err := restartedStores.AttachDocumentLocker(locker); err != nil {
			t.Fatalf("AttachDocumentLocker(restarted) error = %v", err)
		}
	}
	defer func() { _ = restartedStores.Close() }()
	restarted := newTestJournal(t, restartedStores)
	reloaded, err := restarted.Get(ctx, operation.ID)
	if err != nil || reloaded.Status != UploadIntent {
		t.Fatalf("restarted Get() = %+v, %v", reloaded, err)
	}
	if claimed, err := restarted.ClaimRecovery(ctx, operation.ID); err != nil || claimed != nil {
		t.Fatalf("restarted ClaimRecovery(live) = %+v, %v", claimed, err)
	}
	expireUploadLeases(t, restartedStores, operation.ID)
	recovery, err := restarted.ClaimRecovery(ctx, operation.ID)
	if err != nil || recovery == nil {
		t.Fatalf("restarted ClaimRecovery(expired) = %+v, %v", recovery, err)
	}
	if _, err := restarted.Rollback(ctx, recovery, func(*UploadOperation) error { return nil }); err != nil {
		t.Fatalf("restarted Rollback() error = %v", err)
	}
	if current, err := restarted.Get(ctx, operation.ID); err != nil || current.Status != UploadRolledBack {
		t.Fatalf("restarted journal status = %+v, %v", current, err)
	}
}

func TestUploadRemoveHeadRequiresCoveringLock(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	commitTestUpload(t, stores, journal, operation, map[string]any{"ok": true})
	otherToken, err := journal.AcquireLock(ctx, "other", nil)
	if err != nil {
		t.Fatalf("AcquireLock(other) error = %v", err)
	}
	defer func() { _ = journal.ReleaseLock(ctx, otherToken) }()
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	err = journal.RemoveHead(ctx, "ns", "doc", transaction, otherToken)
	_ = transaction.Rollback()
	mustBeError[*LeaseLost](t, err)

	if err := journal.WithLock(ctx, "ns", nil, func(token *LockToken) error {
		transaction, err := stores.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := journal.RemoveHead(ctx, "ns", "doc", transaction, token); err != nil {
			_ = transaction.Rollback()
			return err
		}
		if err := transaction.Rollback(); err != nil {
			return err
		}
		if head, err := journal.Head(ctx, "ns", "doc"); err != nil || head == nil {
			return errors.New("head changed by rolled back transaction")
		}
		transaction, err = stores.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := journal.RemoveHead(ctx, "ns", "doc", transaction, token); err != nil {
			_ = transaction.Rollback()
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("WithLock() error = %v", err)
	}
	active, err := journal.ActiveVersions(ctx, "ns")
	if err != nil || len(active) != 0 {
		t.Fatalf("ActiveVersions() = %v, %v", active, err)
	}
}

func TestUploadObjectsRollBackWithCallerTransaction(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := journal.PutObjectTx(ctx, transaction, operation, SourceBucket, "file.txt", []byte("bytes"), nil); err != nil {
		t.Fatalf("PutObjectTx() error = %v", err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if _, found, err := journal.GetObject(ctx, operation.ID, SourceBucket, "file.txt"); err != nil || found {
		t.Fatalf("object survived rollback: found=%v err=%v", found, err)
	}
}

func TestUploadPurgeDocumentObjects(t *testing.T) {
	stores := newTestStores(t)
	journal := newTestJournal(t, stores)
	ctx := context.Background()
	first := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	commitTestUpload(t, stores, journal, first, map[string]any{"ok": true})
	second := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	commitTestUpload(t, stores, journal, second, map[string]any{"ok": true})
	if err := journal.PutObject(ctx, first, SourceBucket, "file.txt", []byte("old"), nil); err != nil {
		// first is committed; PutObject rejects committed operations by design.
		mustBeError[*LeaseLost](t, err)
	}
	if err := journal.WithLock(ctx, "ns", nil, func(token *LockToken) error {
		transaction, err := stores.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := journal.RemoveHead(ctx, "ns", "doc", transaction, token); err != nil {
			_ = transaction.Rollback()
			return err
		}
		changed, err := journal.PurgeDocumentObjects(ctx, "ns", "doc", transaction, token)
		if err != nil {
			_ = transaction.Rollback()
			return err
		}
		if len(changed) != 1 || changed[0] != second.ID {
			return errors.New("purge did not report the pending operation")
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("WithLock() error = %v", err)
	}
	current, err := journal.Get(ctx, second.ID)
	if err != nil || current.CleanupPending {
		t.Fatalf("cleanup_pending after purge = %+v, %v", current, err)
	}
}
