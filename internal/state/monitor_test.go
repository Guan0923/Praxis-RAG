package state

import (
	"context"
	"math"
	"testing"
	"time"
)

// monitorTestArtifactID builds a canonical-UUID-shaped identifier with a fresh
// random tail, so every inserted row stays unique both across repeated runs and
// within a single run.
func monitorTestArtifactID(t *testing.T) string {
	t.Helper()
	return "00000000-0000-4000-8000-" + monitorTestID(t, 12)
}

// monitorTestID returns one 32/36 character identifier of the requested length.
func monitorTestID(t *testing.T, length int) string {
	t.Helper()
	raw, err := randomHex(length / 2)
	if err != nil {
		t.Fatalf("randomHex() error = %v", err)
	}
	return raw
}

func monitorTestExec(t *testing.T, stores *Stores, statement string, arguments ...any) {
	t.Helper()
	if _, err := stores.DB.ExecContext(context.Background(), statement, arguments...); err != nil {
		t.Fatalf("insert %q error = %v", statement, err)
	}
}

// TestMonitorStatsAggregates inserts one row per monitored state and asserts the
// aggregate counters, ages and table counts of MonitorStats.
func TestMonitorStatsAggregates(t *testing.T) {
	stores := openMySQLTestStores(t)
	ctx := context.Background()
	suffix := monitorTestID(t, 12)
	namespace := "go-monitor-" + suffix
	now := time.Now()
	recentWindow := 24 * time.Hour
	nowUnix := float64(now.Unix())
	queuedAge := 2 * 60 * 60.0    // 2h
	activeSearchAge := 20 * 60.0  // 20m
	pendingOutboxAge := 30 * 60.0 // 30m

	artifactQuota := monitorTestArtifactID(t)
	jobStatement := "INSERT INTO document_jobs (artifact_id, namespace, name, name_key, size, " +
		"sha256, configuration_version, status, stage, lease_until, created_at, updated_at) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	insertJob := func(id, name, status, stage string, leaseUntil, createdAt, updatedAt float64) {
		t.Helper()
		monitorTestExec(t, stores, jobStatement, id, namespace, name, name+"-key", 3,
			"sha-"+suffix, 1, status, stage, leaseUntil, createdAt, updatedAt)
	}
	// One queued job (oldest queue age), one stuck processing job, one active
	// processing job, one recent + one stale terminal job per outcome.
	insertJob(artifactQuota, "queued.txt", DocumentJobQueued, "", 0,
		nowUnix-queuedAge, nowUnix-queuedAge)
	insertJob(monitorTestArtifactID(t), "stuck.txt", DocumentJobProcessing, "parse",
		nowUnix-60, nowUnix-3600, nowUnix-3600)
	insertJob(monitorTestArtifactID(t), "active.txt", DocumentJobProcessing, "split",
		nowUnix+300, nowUnix-1800, nowUnix-30)
	insertJob(monitorTestArtifactID(t), "done.txt", DocumentJobSucceeded, "publish",
		0, nowUnix-300, nowUnix-60)
	insertJob(monitorTestArtifactID(t), "done-old.txt", DocumentJobSucceeded, "publish",
		0, nowUnix-172800, nowUnix-172800)
	insertJob(monitorTestArtifactID(t), "failed.txt", DocumentJobFailed, "parse",
		0, nowUnix-300, nowUnix-60)
	insertJob(monitorTestArtifactID(t), "failed-old.txt", DocumentJobFailed, "parse",
		0, nowUnix-172800, nowUnix-172800)

	outboxStatement := "INSERT INTO document_job_outbox (id, artifact_id, namespace, name, " +
		"status, retry_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)"
	monitorTestExec(t, stores, outboxStatement, monitorTestID(t, 32), artifactQuota,
		namespace, "queued.txt", "PENDING", nowUnix-3600, nowUnix-pendingOutboxAge)
	monitorTestExec(t, stores, outboxStatement, monitorTestID(t, 32), artifactQuota,
		namespace, "queued.txt", "PUBLISHED", nowUnix-60, nowUnix-60)

	searchStatement := "INSERT INTO search_tasks (result_id, namespace, arguments, status, " +
		"stage, attempts, lease_until, retry_at, deadline, created_at, updated_at) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	insertSearch := func(status, stage string, createdAt, updatedAt float64) {
		t.Helper()
		monitorTestExec(t, stores, searchStatement,
			monitorTestArtifactID(t), namespace, "{}",
			status, stage, 1, 0.0, 0.0, createdAt+120, createdAt, updatedAt)
	}
	insertSearch(SearchTaskQueued, "admission", nowUnix-600, nowUnix-600)
	insertSearch(SearchTaskProcessing, "retrieve", nowUnix-activeSearchAge, nowUnix-30)
	insertSearch(SearchTaskSucceeded, "done", nowUnix-600, nowUnix-60)
	insertSearch(SearchTaskFailed, "done", nowUnix-600, nowUnix-60)
	insertSearch(SearchTaskTimedOut, "done", nowUnix-172800, nowUnix-172800)

	uploadStatement := "INSERT INTO upload_operations (id, namespace, name, document_id, " +
		"collection, owner, status, cleanup_pending, created_at, updated_at) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	insertUpload := func(status string, cleanupPending bool) {
		t.Helper()
		monitorTestExec(t, stores, uploadStatement, monitorTestID(t, 32), namespace,
			"upload.txt", "doc-"+suffix, "collection-"+suffix, monitorTestID(t, 32),
			status, cleanupPending, nowUnix-60, nowUnix-30)
	}
	insertUpload(UploadRunning, false)
	insertUpload(UploadCommitted, true)
	insertUpload(UploadCommitted, false)

	monitorTestExec(t, stores,
		"INSERT INTO documents (namespace, name, document_id, size, sha256, chunking, "+
			"collection, chunk_count, modified_at, indexed_at, diagnostics, skipped_object_count) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		namespace, "doc.txt", "doc-"+suffix, 3, "sha-"+suffix, "{}",
		"collection-"+suffix, 1, "", "", "[]", 0)
	monitorTestExec(t, stores,
		"INSERT INTO knowledge_bases (namespace, collection, settings, version, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?)",
		namespace, "collection-"+suffix, "{}", 1, "", "")
	monitorTestExec(t, stores,
		"INSERT INTO search_result_snapshots (id, namespace, content, created_at, expires_at) "+
			"VALUES (?, ?, ?, ?, ?)",
		monitorTestArtifactID(t), namespace, "{}", nowUnix, nowUnix+600)
	monitorTestExec(t, stores,
		"INSERT INTO artifact_snapshots (id, namespace, identity, document_id, version, kind, "+
			"name, mime_type, content, created_at, expires_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		monitorTestArtifactID(t), namespace, "identity-"+suffix, "doc-"+suffix,
		nil, "image", "a.png", "image/png", []byte("png"), nowUnix, nowUnix+600)

	t.Cleanup(func() {
		cleanup := context.Background()
		for _, statement := range []string{
			"DELETE FROM document_job_outbox WHERE namespace = ?",
			"DELETE FROM document_jobs WHERE namespace = ?",
			"DELETE FROM search_tasks WHERE namespace = ?",
			"DELETE FROM upload_operations WHERE namespace = ?",
			"DELETE FROM search_result_snapshots WHERE namespace = ?",
			"DELETE FROM artifact_snapshots WHERE namespace = ?",
			"DELETE FROM documents WHERE namespace = ?",
			"DELETE FROM knowledge_bases WHERE namespace = ?",
		} {
			if _, err := stores.DB.ExecContext(cleanup, statement, namespace); err != nil {
				t.Logf("cleanup %q error = %v", statement, err)
			}
		}
	})

	stats, err := stores.MonitorStats(ctx, now, recentWindow)
	if err != nil {
		t.Fatalf("MonitorStats() error = %v", err)
	}
	if stats == nil {
		t.Fatal("MonitorStats() returned nil")
	}

	wantJobStatuses := map[string]int{
		DocumentJobQueued: 1, DocumentJobProcessing: 2,
		DocumentJobSucceeded: 2, DocumentJobFailed: 2,
	}
	for status, want := range wantJobStatuses {
		if got := stats.DocumentJobsByStatus[status]; got != want {
			t.Fatalf("DocumentJobsByStatus[%q] = %d, want %d (map %v)",
				status, got, want, stats.DocumentJobsByStatus)
		}
	}
	if got := stats.DocumentJobsByStage; len(got) != 2 || got["parse"] != 1 || got["split"] != 1 {
		t.Fatalf("DocumentJobsByStage = %v", got)
	}
	if math.Abs(stats.OldestQueuedSeconds-queuedAge) > 5 {
		t.Fatalf("OldestQueuedSeconds = %f, want ~%f", stats.OldestQueuedSeconds, queuedAge)
	}
	if stats.StuckProcessing != 1 {
		t.Fatalf("StuckProcessing = %d, want 1", stats.StuckProcessing)
	}
	if stats.FailedRecent != 1 {
		t.Fatalf("FailedRecent = %d, want 1", stats.FailedRecent)
	}
	if stats.JobsSucceededRecent != 1 {
		t.Fatalf("JobsSucceededRecent = %d, want 1", stats.JobsSucceededRecent)
	}

	if got := stats.OutboxByStatus; len(got) != 2 || got["PENDING"] != 1 || got["PUBLISHED"] != 1 {
		t.Fatalf("OutboxByStatus = %v", got)
	}
	if math.Abs(stats.OldestPendingOutboxSeconds-pendingOutboxAge) > 5 {
		t.Fatalf("OldestPendingOutboxSeconds = %f, want ~%f",
			stats.OldestPendingOutboxSeconds, pendingOutboxAge)
	}

	for _, status := range []string{
		SearchTaskQueued, SearchTaskProcessing, SearchTaskSucceeded,
		SearchTaskFailed, SearchTaskTimedOut,
	} {
		if got := stats.SearchByStatus[status]; got != 1 {
			t.Fatalf("SearchByStatus[%q] = %d, want 1 (map %v)", status, got, stats.SearchByStatus)
		}
	}
	if math.Abs(stats.OldestActiveSearchSeconds-activeSearchAge) > 5 {
		t.Fatalf("OldestActiveSearchSeconds = %f, want ~%f",
			stats.OldestActiveSearchSeconds, activeSearchAge)
	}
	if stats.SearchesRecent != 2 {
		t.Fatalf("SearchesRecent = %d, want 2", stats.SearchesRecent)
	}

	if stats.UnfinishedUploads != 2 {
		t.Fatalf("UnfinishedUploads = %d, want 2", stats.UnfinishedUploads)
	}
	if stats.Documents != 1 || stats.KnowledgeBases != 1 ||
		stats.ResultSnapshots != 1 || stats.ArtifactSnapshots != 1 {
		t.Fatalf("library counts = %d/%d/%d/%d, want 1/1/1/1",
			stats.Documents, stats.KnowledgeBases, stats.ResultSnapshots, stats.ArtifactSnapshots)
	}
}

// TestMonitorStatsEmptyState asserts the zero-value fallbacks the Console relies
// on when the database has no rows yet.
func TestMonitorStatsEmptyState(t *testing.T) {
	stores := openMySQLTestStores(t)
	stats, err := stores.MonitorStats(context.Background(), time.Now(), 24*time.Hour)
	if err != nil {
		t.Fatalf("MonitorStats() error = %v", err)
	}
	for _, status := range []string{
		DocumentJobQueued, DocumentJobProcessing, DocumentJobSucceeded, DocumentJobFailed,
	} {
		if got, present := stats.DocumentJobsByStatus[status]; !present || got != 0 {
			t.Fatalf("DocumentJobsByStatus[%q] = %d, present = %v", status, got, present)
		}
	}
	for _, status := range []string{
		SearchTaskQueued, SearchTaskProcessing, SearchTaskSucceeded,
		SearchTaskFailed, SearchTaskTimedOut,
	} {
		if got, present := stats.SearchByStatus[status]; !present || got != 0 {
			t.Fatalf("SearchByStatus[%q] = %d, present = %v", status, got, present)
		}
	}
	if stats.DocumentJobsByStage == nil || stats.OutboxByStatus == nil {
		t.Fatalf("nil maps: stage = %v outbox = %v",
			stats.DocumentJobsByStage, stats.OutboxByStatus)
	}
	if stats.OldestQueuedSeconds != 0 || stats.OldestPendingOutboxSeconds != 0 ||
		stats.OldestActiveSearchSeconds != 0 {
		t.Fatalf("empty ages = %v/%v/%v", stats.OldestQueuedSeconds,
			stats.OldestPendingOutboxSeconds, stats.OldestActiveSearchSeconds)
	}
	if stats.Documents != 0 || stats.KnowledgeBases != 0 ||
		stats.ResultSnapshots != 0 || stats.ArtifactSnapshots != 0 {
		t.Fatalf("library counts = %d/%d/%d/%d, want 0/0/0/0",
			stats.Documents, stats.KnowledgeBases, stats.ResultSnapshots, stats.ArtifactSnapshots)
	}
}

// TestMonitorStatsQueryFailure asserts a broken database surfaces as an error
// instead of a silently partial snapshot.
func TestMonitorStatsQueryFailure(t *testing.T) {
	stores := openMySQLTestStores(t)
	if err := stores.DB.Close(); err != nil {
		t.Fatalf("DB.Close() error = %v", err)
	}
	stats, err := stores.MonitorStats(context.Background(), time.Now(), 24*time.Hour)
	if err == nil {
		t.Fatalf("MonitorStats() error = nil, stats = %+v", stats)
	}
	if stats != nil {
		t.Fatalf("MonitorStats() returned partial stats: %+v", stats)
	}
}
