package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeIndexPublisher struct {
	versions map[string]bool
	deleted  [][2]string
	failures int
}

func newFakeIndexPublisher() *fakeIndexPublisher {
	return &fakeIndexPublisher{versions: map[string]bool{}}
}

func fakeVersionKey(documentID, version string) string { return documentID + "\x00" + version }

func (f *fakeIndexPublisher) DeleteVersion(_ context.Context, documentID, version string) error {
	f.deleted = append(f.deleted, [2]string{documentID, version})
	if f.failures > 0 {
		f.failures--
		return &OSError{message: "controlled Qdrant outage"}
	}
	delete(f.versions, fakeVersionKey(documentID, version))
	return nil
}

func newTestCoordinator(t *testing.T, stores *Stores, root string) *UploadCoordinator {
	t.Helper()
	stores.Uploads.CleanupDelaySeconds = 0
	stores.Uploads.LeaseSeconds = 30
	stores.Uploads.AutoHeartbeat = false
	coordinator, err := NewUploadCoordinator(stores, root)
	if err != nil {
		t.Fatalf("NewUploadCoordinator() error = %v", err)
	}
	t.Cleanup(coordinator.Close)
	return coordinator
}

func coordinatorWorkspace(t *testing.T, coordinator *UploadCoordinator, operation *UploadOperation, node *string) string {
	t.Helper()
	path := filepath.Join(coordinator.WorkspaceRoot, "rag-txn-"+operation.ID)
	if _, err := coordinator.Journal.RegisterWorkspace(context.Background(), operation, path, node); err != nil {
		t.Fatalf("RegisterWorkspace() error = %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "source.txt"), []byte("staged"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func commitCoordinatorOperation(t *testing.T, coordinator *UploadCoordinator, operation *UploadOperation) *UploadOperation {
	return commitTestUpload(t, coordinator.Journal.stores, coordinator.Journal, operation, map[string]any{"ok": true})
}

func noIndexFactory(t *testing.T) IndexStoreFactory {
	return func(*UploadOperation) (IndexPublisher, error) {
		t.Fatal("Qdrant factory must not be called")
		return nil, nil
	}
}

func TestCoordinatorExpiredRecoveryIsVersionScopedAndFencesOldOwner(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	ctx := context.Background()
	old := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	commitCoordinatorOperation(t, coordinator, old)

	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	if err := journal.Intent(ctx, operation, "stage_qdrant", nil); err != nil {
		t.Fatalf("Intent() error = %v", err)
	}
	if err := journal.PutObject(ctx, operation, SourceBucket, "file.txt", []byte("staged"), nil); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	path := coordinatorWorkspace(t, coordinator, operation, nil)
	unrelated := filepath.Join(coordinator.WorkspaceRoot, "rag-txn-unregistered")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	store := newFakeIndexPublisher()
	for _, version := range []string{old.ID, operation.ID, "future"} {
		store.versions[fakeVersionKey("doc", version)] = true
	}
	expireUploadLeases(t, stores, operation.ID)
	seen := make([][2]string, 0)
	factory := func(current *UploadOperation) (IndexPublisher, error) {
		seen = append(seen, [2]string{current.Collection, current.Namespace})
		return store, nil
	}
	recovered := coordinator.Recover(ctx, factory, nil)
	if len(recovered) != 1 || recovered[0].Status != UploadRolledBack {
		t.Fatalf("Recover() = %+v", recovered)
	}
	if len(seen) != 1 || seen[0] != [2]string{"collection", "ns"} {
		t.Fatalf("factory saw %v", seen)
	}
	if len(store.deleted) != 1 || store.deleted[0] != [2]string{"doc", operation.ID} {
		t.Fatalf("deleted = %v", store.deleted)
	}
	if !store.versions[fakeVersionKey("doc", old.ID)] || !store.versions[fakeVersionKey("doc", "future")] {
		t.Fatalf("unrelated versions were deleted: %v", store.versions)
	}
	if head, err := journal.Head(ctx, "ns", "doc"); err != nil || head == nil || *head != old.ID {
		t.Fatalf("Head() = %v, %v", head, err)
	}
	if _, found, err := journal.GetObject(ctx, operation.ID, SourceBucket, "file.txt"); err != nil || found {
		t.Fatalf("rolled-back object survived: found=%v err=%v", found, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still exists: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unregistered directory was removed: %v", err)
	}
	recovered = coordinator.Recover(ctx, factory, nil)
	if len(recovered) != 1 || recovered[0].Status != UploadRolledBack {
		t.Fatalf("second Recover() = %+v", recovered)
	}
	if len(store.deleted) != 2 {
		t.Fatalf("rolled-back tombstones were not reaped: %v", store.deleted)
	}
	if _, err := coordinator.Compensate(ctx, operation, store); err == nil {
		t.Fatal("stale compensate succeeded")
	} else {
		mustBeError[*LeaseLost](t, err)
	}
}

func TestCoordinatorLiveUploadsAreNeverClaimed(t *testing.T) {
	for _, failed := range []bool{false, true} {
		stores := newTestStores(t)
		coordinator := newTestCoordinator(t, stores, t.TempDir())
		journal := coordinator.Journal
		operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
		path := coordinatorWorkspace(t, coordinator, operation, nil)
		if err := journal.Intent(context.Background(), operation, "stage_qdrant", nil); err != nil {
			t.Fatalf("Intent() error = %v", err)
		}
		if failed {
			if err := journal.Fail(context.Background(), operation, &OSError{}, nil); err != nil {
				t.Fatalf("Fail() error = %v", err)
			}
		}
		recovered := coordinator.Recover(context.Background(), noIndexFactory(t), nil)
		if len(recovered) != 0 {
			t.Fatalf("live upload was recovered: %+v", recovered)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("live workspace was removed: %v", err)
		}
		current, err := journal.Get(context.Background(), operation.ID)
		if err != nil || current.Owner != operation.Owner {
			t.Fatalf("owner changed: %+v, %v", current, err)
		}
	}
}

func TestCoordinatorPreVectorRollbackNeverResolvesIndex(t *testing.T) {
	for _, step := range []string{"", "receive_source", "embedding", "stage_mysql"} {
		stores := newTestStores(t)
		coordinator := newTestCoordinator(t, stores, t.TempDir())
		journal := coordinator.Journal
		operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
		path := coordinatorWorkspace(t, coordinator, operation, nil)
		if step != "" {
			if err := journal.Intent(context.Background(), operation, step, nil); err != nil {
				t.Fatalf("Intent() error = %v", err)
			}
		}
		expireUploadLeases(t, stores, operation.ID)
		recovered := coordinator.Recover(context.Background(), noIndexFactory(t), nil)
		if len(recovered) != 1 || recovered[0].Status != UploadRolledBack {
			t.Fatalf("step %q: Recover() = %+v", step, recovered)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("step %q: workspace still exists", step)
		}
	}
}

func TestCoordinatorImmediateCompensationHonorsVectorIntents(t *testing.T) {
	for _, step := range []string{"stage_qdrant", "qdrant", "qdrant.upsert", "qdrant_write"} {
		stores := newTestStores(t)
		coordinator := newTestCoordinator(t, stores, t.TempDir())
		journal := coordinator.Journal
		operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
		path := coordinatorWorkspace(t, coordinator, operation, nil)
		if err := journal.Intent(context.Background(), operation, step, nil); err != nil {
			t.Fatalf("Intent() error = %v", err)
		}
		store := newFakeIndexPublisher()
		result, err := coordinator.Compensate(context.Background(), operation, store)
		if err != nil || result.Status != UploadRolledBack {
			t.Fatalf("step %q: Compensate() = %+v, %v", step, result, err)
		}
		if len(store.deleted) != 1 || store.deleted[0] != [2]string{"doc", operation.ID} {
			t.Fatalf("step %q: deleted = %v", step, store.deleted)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("step %q: workspace still exists", step)
		}
	}
}

func TestCoordinatorRollbackOutageRetainsObjectsAndRetries(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	path := coordinatorWorkspace(t, coordinator, operation, nil)
	if err := journal.Intent(ctx, operation, "stage_qdrant", nil); err != nil {
		t.Fatalf("Intent() error = %v", err)
	}
	if err := journal.PutObject(ctx, operation, SourceBucket, "file.txt", []byte("recoverable"), nil); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	store := newFakeIndexPublisher()
	store.failures = 1
	expireUploadLeases(t, stores, operation.ID)
	if recovered := coordinator.Recover(ctx, func(*UploadOperation) (IndexPublisher, error) { return store, nil }, nil); len(recovered) != 0 {
		t.Fatalf("failed rollback was reported recovered: %+v", recovered)
	}
	current, err := journal.Get(ctx, operation.ID)
	if err != nil || current.Status != UploadRollbackFailed {
		t.Fatalf("status = %+v, %v", current, err)
	}
	if content, found, err := journal.GetObject(ctx, operation.ID, SourceBucket, "file.txt"); err != nil || !found || string(content) != "recoverable" {
		t.Fatalf("object after failed rollback = %q, %v, %v", content, found, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("workspace was removed by a failed rollback: %v", err)
	}
	if recovered := coordinator.Recover(ctx, noIndexFactory(t), nil); len(recovered) != 0 {
		t.Fatalf("failed rollback with a live lease was recovered: %+v", recovered)
	}
	expireUploadLeases(t, stores, operation.ID)
	recovered := coordinator.Recover(ctx, func(*UploadOperation) (IndexPublisher, error) { return store, nil }, nil)
	if len(recovered) != 1 || recovered[0].Status != UploadRolledBack {
		t.Fatalf("retry Recover() = %+v", recovered)
	}
	if len(store.deleted) != 2 {
		t.Fatalf("deleted = %v", store.deleted)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still exists after retry")
	}
}

func TestCoordinatorWorkspaceFailurePreservesRollbackRetry(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	path := coordinatorWorkspace(t, coordinator, operation, nil)
	if err := journal.Intent(ctx, operation, "stage_qdrant", nil); err != nil {
		t.Fatalf("Intent() error = %v", err)
	}
	if err := journal.PutObject(ctx, operation, SourceBucket, "file.txt", []byte("staged"), nil); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	store := newFakeIndexPublisher()
	originalRemoveAll := coordinator.removeAll
	coordinator.removeAll = func(string) error { return &PermissionError{Message: "controlled file lock"} }
	if _, err := coordinator.Compensate(ctx, operation, store); err == nil {
		t.Fatal("compensate succeeded despite workspace failure")
	}
	coordinator.removeAll = originalRemoveAll
	current, err := journal.Get(ctx, operation.ID)
	if err != nil || current.Status != UploadRollbackFailed {
		t.Fatalf("status = %+v, %v", current, err)
	}
	if content, found, err := journal.GetObject(ctx, operation.ID, SourceBucket, "file.txt"); err != nil || !found || string(content) != "staged" {
		t.Fatalf("object = %q, %v, %v", content, found, err)
	}
	expireUploadLeases(t, stores, operation.ID)
	recovered := coordinator.Recover(ctx, func(*UploadOperation) (IndexPublisher, error) { return store, nil }, nil)
	if len(recovered) != 1 || recovered[0].Status != UploadRolledBack {
		t.Fatalf("Recover() = %+v", recovered)
	}
	if len(store.deleted) != 2 {
		t.Fatalf("deleted = %v", store.deleted)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still exists")
	}
}

func TestCoordinatorFactoryOutageRecordedAsRollbackFailure(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	if err := journal.Intent(ctx, operation, "stage_qdrant", nil); err != nil {
		t.Fatalf("Intent() error = %v", err)
	}
	expireUploadLeases(t, stores, operation.ID)
	coordinator.Recover(ctx, func(*UploadOperation) (IndexPublisher, error) {
		return nil, &OSError{message: "factory unavailable"}
	}, nil)
	current, err := journal.Get(ctx, operation.ID)
	if err != nil || current.Status != UploadRollbackFailed {
		t.Fatalf("status = %+v, %v", current, err)
	}
}

func TestCoordinatorOneFailedRecoveryDoesNotBlockOthers(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	ctx := context.Background()
	first := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	second := beginTestUpload(t, journal, "ns", "other.txt", "other", "collection", BeginOptions{})
	if err := journal.Intent(ctx, first, "stage_qdrant", nil); err != nil {
		t.Fatalf("Intent(first) error = %v", err)
	}
	expireUploadLeases(t, stores, first.ID)
	expireUploadLeases(t, stores, second.ID)
	store := newFakeIndexPublisher()
	store.failures = 1
	coordinator.Recover(ctx, func(*UploadOperation) (IndexPublisher, error) { return store, nil }, nil)
	firstCurrent, err := journal.Get(ctx, first.ID)
	if err != nil || firstCurrent.Status != UploadRollbackFailed {
		t.Fatalf("first status = %+v, %v", firstCurrent, err)
	}
	secondCurrent, err := journal.Get(ctx, second.ID)
	if err != nil || secondCurrent.Status != UploadRolledBack {
		t.Fatalf("second status = %+v, %v", secondCurrent, err)
	}
}

func TestCoordinatorCommittedCleanupOnlyDeletesItsOldVersion(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	ctx := context.Background()
	first := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	commitCoordinatorOperation(t, coordinator, first)
	second := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	secondPath := coordinatorWorkspace(t, coordinator, second, nil)
	commitCoordinatorOperation(t, coordinator, second)
	third := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	commitCoordinatorOperation(t, coordinator, third)
	store := newFakeIndexPublisher()
	for _, operation := range []*UploadOperation{first, second, third} {
		store.versions[fakeVersionKey("doc", operation.ID)] = true
	}
	result, err := coordinator.Cleanup(ctx, second, store)
	if err != nil || result.CleanupPending {
		t.Fatalf("Cleanup() = %+v, %v", result, err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != [2]string{"doc", first.ID} {
		t.Fatalf("deleted = %v", store.deleted)
	}
	if !store.versions[fakeVersionKey("doc", second.ID)] || !store.versions[fakeVersionKey("doc", third.ID)] {
		t.Fatalf("unrelated versions were deleted: %v", store.versions)
	}
	if head, err := journal.Head(ctx, "ns", "doc"); err != nil || head == nil || *head != third.ID {
		t.Fatalf("Head() = %v, %v", head, err)
	}
	if _, err := os.Stat(secondPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second workspace still exists")
	}
	if _, err := coordinator.Compensate(ctx, second, store); err == nil {
		t.Fatal("compensate of a committed version succeeded")
	} else {
		mustBeError[*LeaseLost](t, err)
	}
	recovered := coordinator.Recover(ctx, func(*UploadOperation) (IndexPublisher, error) { return store, nil }, nil)
	if len(recovered) != 1 || recovered[0].Status != UploadCommitted {
		t.Fatalf("Recover() = %+v", recovered)
	}
	if len(store.deleted) != 2 || store.deleted[1] != [2]string{"doc", second.ID} {
		t.Fatalf("deleted = %v", store.deleted)
	}
	if !store.versions[fakeVersionKey("doc", third.ID)] {
		t.Fatalf("active version was deleted: %v", store.versions)
	}
}

func TestCoordinatorCommittedCleanupFailureStaysPending(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	ctx := context.Background()
	first := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	commitCoordinatorOperation(t, coordinator, first)
	second := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	path := coordinatorWorkspace(t, coordinator, second, nil)
	commitCoordinatorOperation(t, coordinator, second)
	store := newFakeIndexPublisher()
	store.failures = 1
	result, err := coordinator.Cleanup(ctx, second, store)
	if err != nil || result.Status != UploadCommitted || !result.CleanupPending {
		t.Fatalf("Cleanup() = %+v, %v", result, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace was not removed")
	}
	recovered := coordinator.Recover(ctx, func(*UploadOperation) (IndexPublisher, error) { return store, nil }, nil)
	if len(recovered) != 1 {
		t.Fatalf("Recover() = %+v", recovered)
	}
	current, err := journal.Get(ctx, second.ID)
	if err != nil || current.CleanupPending {
		t.Fatalf("cleanup still pending: %+v, %v", current, err)
	}
	if len(store.deleted) != 2 {
		t.Fatalf("deleted = %v", store.deleted)
	}
	if head, err := journal.Head(ctx, "ns", "doc"); err != nil || head == nil || *head != second.ID {
		t.Fatalf("Head() = %v, %v", head, err)
	}
}

func TestCoordinatorCleanupGraceDefersVectorsButNotWorkspace(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	first := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	commitCoordinatorOperation(t, coordinator, first)
	second := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	path := coordinatorWorkspace(t, coordinator, second, nil)
	commitCoordinatorOperation(t, coordinator, second)
	journal.CleanupDelaySeconds = 3600
	recovered := coordinator.Recover(context.Background(), noIndexFactory(t), nil)
	if len(recovered) != 1 || recovered[0].Status != UploadCommitted || !recovered[0].CleanupPending {
		t.Fatalf("Recover() = %+v", recovered)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace was not removed despite the vector grace period")
	}
}

func TestCoordinatorFirstCommitWorkspaceRecoveredWithoutOldVersion(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	path := coordinatorWorkspace(t, coordinator, operation, nil)
	committed := commitCoordinatorOperation(t, coordinator, operation)
	if committed.CleanupPending {
		t.Fatal("first commit unexpectedly needs cleanup")
	}
	if unfinished, err := journal.Unfinished(ctx, nil); err != nil || len(unfinished) != 0 {
		t.Fatalf("Unfinished() = %v, %v", unfinished, err)
	}
	recovered := coordinator.Recover(ctx, noIndexFactory(t), nil)
	if len(recovered) != 1 || recovered[0].Status != UploadCommitted || recovered[0].Workspace != nil {
		t.Fatalf("Recover() = %+v", recovered)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still exists")
	}
	if recovered := coordinator.Recover(ctx, noIndexFactory(t), nil); len(recovered) != 0 {
		t.Fatalf("second Recover() = %+v", recovered)
	}
}

func TestCoordinatorFirstCommitWorkspaceFailureRetryable(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	path := coordinatorWorkspace(t, coordinator, operation, nil)
	commitCoordinatorOperation(t, coordinator, operation)
	originalRemoveAll := coordinator.removeAll
	coordinator.removeAll = func(string) error { return &PermissionError{Message: "controlled file lock"} }
	result, err := coordinator.Cleanup(ctx, operation, nil)
	if err != nil || result.Status != UploadCommitted || !result.CleanupPending {
		t.Fatalf("Cleanup() = %+v, %v", result, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("workspace was removed despite the failure: %v", err)
	}
	coordinator.removeAll = originalRemoveAll
	recovered := coordinator.Recover(ctx, noIndexFactory(t), nil)
	if len(recovered) != 1 || recovered[0].CleanupPending {
		t.Fatalf("Recover() = %+v", recovered)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still exists")
	}
}

func TestCoordinatorWorkspaceCleanupNeverRemovesRemoteNode(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	path := coordinatorWorkspace(t, coordinator, operation, stringPtr(coordinator.Node+"-remote"))
	expireUploadLeases(t, stores, operation.ID)
	recovered := coordinator.Recover(context.Background(), noIndexFactory(t), nil)
	if len(recovered) != 1 || recovered[0].Status != UploadRolledBack {
		t.Fatalf("Recover() = %+v", recovered)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("remote-node workspace was removed: %v", err)
	}
}

func TestCoordinatorUnsafeRegisteredWorkspaceNotDeleted(t *testing.T) {
	for _, kind := range []string{"unrelated", "other_id", "nested", "relative", "traversal"} {
		stores := newTestStores(t)
		coordinator := newTestCoordinator(t, stores, t.TempDir())
		journal := coordinator.Journal
		operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
		root := coordinator.WorkspaceRoot
		var path string
		switch kind {
		case "unrelated":
			path = filepath.Join(root, "not-a-transaction")
		case "other_id":
			path = filepath.Join(root, "rag-txn-"+strings.Repeat("0", 32))
		case "nested":
			path = filepath.Join(root, "nested", "rag-txn-"+operation.ID)
		case "relative":
			path = filepath.Join("rag-txn-"+operation.ID, "")
		default:
			if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			// Keep the raw ".." part: filepath.Join would clean it away.
			path = root + string(os.PathSeparator) + "nested" + string(os.PathSeparator) +
				".." + string(os.PathSeparator) + "rag-txn-" + operation.ID
		}
		if filepath.IsAbs(path) {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("%s: MkdirAll() error = %v", kind, err)
			}
		}
		if _, err := journal.RegisterWorkspace(context.Background(), operation, path, nil); err != nil {
			t.Fatalf("%s: RegisterWorkspace() error = %v", kind, err)
		}
		if _, err := coordinator.Compensate(context.Background(), operation, nil); err == nil {
			t.Fatalf("%s: compensate accepted an unsafe workspace", kind)
		} else if !strings.Contains(err.Error(), "Unsafe") {
			t.Fatalf("%s: error = %v, want Unsafe", kind, err)
		}
		current, err := journal.Get(context.Background(), operation.ID)
		if err != nil || current.Status != UploadRollbackFailed {
			t.Fatalf("%s: status = %+v, %v", kind, current, err)
		}
		if filepath.IsAbs(path) {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("%s: unsafe workspace was deleted: %v", kind, err)
			}
		}
	}
}

func TestCoordinatorRemoteRollbackWorkspaceRecoversOnOriginalHost(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	path := coordinatorWorkspace(t, coordinator, operation, stringPtr("original-upload-node"))
	expireUploadLeases(t, stores, operation.ID)
	if recovered := coordinator.Recover(ctx, noIndexFactory(t), nil); len(recovered) != 1 || recovered[0].Status != UploadRolledBack {
		t.Fatalf("Recover() = %+v", recovered)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("remote workspace was removed on the wrong host: %v", err)
	}
	coordinator.Node = "original-upload-node"
	recovered := coordinator.Recover(ctx, noIndexFactory(t), nil)
	if len(recovered) != 1 || recovered[0].Workspace == nil || *recovered[0].Workspace != path {
		t.Fatalf("Recover() on original host = %+v", recovered)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still exists after original-host recovery")
	}
	// A late parser on the original node recreates the directory: reap again.
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if recovered := coordinator.Recover(ctx, noIndexFactory(t), nil); len(recovered) != 1 {
		t.Fatalf("late workspace was not reaped: %+v", recovered)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late workspace still exists")
	}
}

func TestCoordinatorMissingRegisteredWorkspaceIsIdempotent(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	path := filepath.Join(coordinator.WorkspaceRoot, "rag-txn-"+operation.ID+"-parse")
	if _, err := journal.RegisterWorkspace(context.Background(), operation, path, nil); err != nil {
		t.Fatalf("RegisterWorkspace() error = %v", err)
	}
	result, err := coordinator.Compensate(context.Background(), operation, nil)
	if err != nil || result.Status != UploadRolledBack {
		t.Fatalf("Compensate() = %+v, %v", result, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected workspace")
	}
}

func TestCoordinatorNamespaceFilterApplies(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	ctx := context.Background()
	left := beginTestUpload(t, journal, "left", "file.txt", "doc", "collection", BeginOptions{})
	leftPath := coordinatorWorkspace(t, coordinator, left, nil)
	commitCoordinatorOperation(t, coordinator, left)
	right := beginTestUpload(t, journal, "right", "file.txt", "doc", "collection", BeginOptions{})
	rightPath := coordinatorWorkspace(t, coordinator, right, nil)
	expireUploadLeases(t, stores, right.ID)
	coordinator.Recover(ctx, noIndexFactory(t), stringPtr("right"))
	if _, err := os.Stat(leftPath); err != nil {
		t.Fatalf("left workspace was removed by a right-only recovery: %v", err)
	}
	if _, err := os.Stat(rightPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("right workspace still exists")
	}
	current, err := journal.Get(ctx, left.ID)
	if err != nil || current.Status != UploadCommitted {
		t.Fatalf("left status = %+v, %v", current, err)
	}
}

func TestCoordinatorRestartUsesPersistedJournal(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	ctx := context.Background()
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	path := coordinatorWorkspace(t, coordinator, operation, nil)
	if err := journal.Intent(ctx, operation, "stage_qdrant", nil); err != nil {
		t.Fatalf("Intent() error = %v", err)
	}
	expireUploadLeases(t, stores, operation.ID)
	// The lease expiry also releases the backend's lease so the restart recovery
	// can take over.
	settings := stores.Settings
	locker := stores.DocumentLocks
	workspaceRoot := coordinator.WorkspaceRoot
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
	restarted := newTestCoordinator(t, restartedStores, workspaceRoot)
	store := newFakeIndexPublisher()
	recovered := restarted.Recover(ctx, func(*UploadOperation) (IndexPublisher, error) { return store, nil }, nil)
	if len(recovered) != 1 || recovered[0].Status != UploadRolledBack {
		t.Fatalf("Recover() after restart = %+v", recovered)
	}
	if len(store.deleted) != 1 || store.deleted[0] != [2]string{"doc", operation.ID} {
		t.Fatalf("deleted = %v", store.deleted)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still exists after restart recovery")
	}
}

func TestCoordinatorCleanupWorkspaceUsesRegisteredPath(t *testing.T) {
	stores := newTestStores(t)
	coordinator := newTestCoordinator(t, stores, t.TempDir())
	journal := coordinator.Journal
	operation := beginTestUpload(t, journal, "ns", "file.txt", "doc", "collection", BeginOptions{})
	path := coordinatorWorkspace(t, coordinator, operation, nil)
	unrelated := filepath.Join(coordinator.WorkspaceRoot, "unrelated")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	supplied := *operation
	supplied.Workspace = &unrelated
	removed, err := coordinator.CleanupWorkspace(context.Background(), &supplied)
	if err != nil || !removed {
		t.Fatalf("CleanupWorkspace() = %v, %v", removed, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registered workspace still exists")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("caller-supplied unrelated path was removed: %v", err)
	}
	if removed, err := coordinator.CleanupWorkspace(context.Background(), &supplied); err != nil || !removed {
		t.Fatalf("second CleanupWorkspace() = %v, %v", removed, err)
	}
}
