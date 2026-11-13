package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/sparse"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// newMaintenanceService builds a runtime without a background loop so tests can
// invoke one pass deterministically.
func newMaintenanceService(t *testing.T) (*Service, *state.Stores) {
	t.Helper()
	qdrantFake, qdrantURL := newFakeKBQdrant(t, "")
	service, stores := newKBService(t, qdrantURL)
	_ = qdrantFake
	return service, stores
}

func TestMaintenanceRecoversStaleUploads(t *testing.T) {
	service, stores := newMaintenanceService(t)
	ctx := context.Background()
	journal, err := state.NewUploadJournal(stores, state.WithLeaseSeconds(5.0), state.WithAutoHeartbeat(false))
	if err != nil {
		t.Fatalf("NewUploadJournal() error = %v", err)
	}
	documentID := chunking.StableID("doc", sparse.CaseFold("crashed.txt"))
	operation, err := journal.Begin(ctx, "alpha", "crashed.txt", documentID, "knowledge_test", state.BeginOptions{})
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	workspace := filepath.Join(os.TempDir(), "rag-txn-"+operation.ID)
	if _, err := journal.RegisterWorkspace(ctx, operation, workspace, nil); err != nil {
		t.Fatalf("RegisterWorkspace() error = %v", err)
	}
	if err := journal.Intent(ctx, operation, "stage_qdrant", map[string]any{"version": operation.ID}); err != nil {
		t.Fatalf("Intent() error = %v", err)
	}
	time.Sleep(6 * time.Second)

	service.maintenancePass(ctx)

	recovered, err := stores.Uploads.Get(ctx, operation.ID)
	if err != nil || recovered == nil {
		t.Fatalf("Uploads.Get() = %v, %v", recovered, err)
	}
	if recovered.Status != state.UploadRolledBack && recovered.Status != state.UploadRollbackFailed {
		t.Errorf("stale upload status = %q, want rolled back", recovered.Status)
	}
	if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
		t.Errorf("stale workspace was not reclaimed: %v", statErr)
	}
}

func TestMaintenanceExpiresSearchTasks(t *testing.T) {
	service, stores := newMaintenanceService(t)
	ctx := context.Background()
	clock := float64(time.Now().Unix())
	stores.SearchTasks.Clock = func() float64 { return clock }
	envelope, err := stores.SearchTasks.Submit(ctx, "alpha", map[string]any{"query": "stale"}, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit() error = %v", err)
	}
	clock += 1_000

	service.maintenancePass(ctx)

	_, err = stores.SearchTasks.Get(ctx, "alpha", envelope.ResultID, 1, 10)
	var unavailable *state.SnapshotUnavailable
	if !errors.As(err, &unavailable) ||
		unavailable.Message != "Search timed out before completion; submit a new search" {
		t.Fatalf("expired task read error = %v", err)
	}
}

func TestMaintenanceSweepsAndGCsDocumentJobs(t *testing.T) {
	service, stores := newMaintenanceService(t)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx,
		testKnowledgeBaseSettings(t, "alpha", "http://127.0.0.1:1", "knowledge_test")); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	admission, err := stores.DocumentJobs.Submit(ctx, "alpha", "stale.md", []byte("stale"), nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Submit() error = %v", err)
	}
	// Backdate the stranded job beyond the sweep window. The sweep increments
	// schedule_version, which is the only signal Debezium needs to deliver a
	// fresh notification.
	past := float64(time.Now().Unix()) - 10_000
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET updated_at = ? WHERE artifact_id = ?", past, admission.ArtifactID); err != nil {
		t.Fatalf("backdate job: %v", err)
	}

	service.maintenancePass(ctx)

	swept, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || swept == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", swept, err)
	}
	if swept.ScheduleVersion != 2 {
		t.Errorf("schedule_version after sweep = %d, want 2", swept.ScheduleVersion)
	}
	if swept.Status != state.DocumentJobQueued {
		t.Errorf("swept job status = %q, want queued", swept.Status)
	}

	// A terminal job past the retention window is garbage-collected.
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", job, err)
	}
	// The sweep requeued the job with a new schedule_version, so the fresh
	// notification is claimable immediately.
	notification, err := state.DocumentJobEnvelope(swept)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope() error = %v", err)
	}
	task, err := stores.DocumentJobs.Claim(ctx, notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() after sweep = %v, %v", task, err)
	}
	failure := "failed"
	if err := stores.DocumentJobs.Fail(ctx, task, false, &failure); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	terminalPast := float64(time.Now().Unix()) - 40*24*3600
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET updated_at = ?, created_at = ? WHERE artifact_id = ?",
		terminalPast, terminalPast, admission.ArtifactID); err != nil {
		t.Fatalf("backdate terminal job: %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET status = ?, error = ? WHERE artifact_id = ?",
		state.DocumentJobFailed, "failed", admission.ArtifactID); err != nil {
		t.Fatalf("mark terminal job: %v", err)
	}
	service.maintenancePass(ctx)
	remaining, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Get() after GC error = %v", err)
	}
	if remaining != nil {
		t.Errorf("terminal job past retention was not GCed: %+v", remaining)
	}
}

func TestMaintenancePurgesExpiredArtifacts(t *testing.T) {
	service, stores := newMaintenanceService(t)
	ctx := context.Background()
	expired := float64(time.Now().Unix()) - 60
	artifact, err := stores.Artifacts.Register(ctx, "alpha", "doc-expired", nil,
		"chunk", "expired.txt", "text/plain", []byte("bytes"), &expired)
	if err != nil {
		t.Fatalf("Artifacts.Register() error = %v", err)
	}
	service.maintenancePass(ctx)
	var remaining int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM artifact_snapshots WHERE id = ?", artifact.ArtifactID).Scan(&remaining); err != nil {
		t.Fatalf("count artifact: %v", err)
	}
	if remaining != 0 {
		t.Errorf("expired artifact rows = %d, want 0", remaining)
	}
}

// TestBackgroundLifecycle pins the background loop contract: it starts
// idempotently, expires stale search tasks through the periodic pass and stops
// idempotently. Delivery itself is Debezium's responsibility.
func TestBackgroundLifecycle(t *testing.T) {
	service, stores := newMaintenanceService(t)
	ctx := context.Background()
	if err := service.StartBackground(ctx,
		WithMaintenanceInterval(20*time.Millisecond),
		WithInitialMaintenancePass(false),
	); err != nil {
		t.Fatalf("StartBackground() error = %v", err)
	}
	if !service.BackgroundRunning() {
		t.Fatalf("BackgroundRunning() = false after start")
	}
	// Starting twice is idempotent.
	if err := service.StartBackground(ctx); err != nil {
		t.Fatalf("second StartBackground() error = %v", err)
	}
	if !service.BackgroundRunning() {
		t.Fatalf("BackgroundRunning() = false after second start")
	}

	// The periodic loop must expire a task whose deadline has passed.
	clock := float64(time.Now().Unix())
	stores.SearchTasks.Clock = func() float64 { return clock }
	notification, err := stores.SearchTasks.Submit(ctx, "alpha", map[string]any{"query": "loop"}, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit() error = %v", err)
	}
	clock += 1_000
	deadline := time.Now().Add(60 * time.Second)
	expired := false
	for time.Now().Before(deadline) {
		_, err := stores.SearchTasks.Get(ctx, "alpha", notification.ResultID, 1, 10)
		var unavailable *state.SnapshotUnavailable
		if errors.As(err, &unavailable) {
			expired = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !expired {
		t.Fatalf("background loop did not expire the stale search task")
	}

	service.StopBackground()
	if service.BackgroundRunning() {
		t.Fatalf("BackgroundRunning() = true after stop")
	}
	// Stopping twice is safe.
	service.StopBackground()
}
