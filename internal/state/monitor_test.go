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
	queuedAge := 2 * 60 * 60.0 // 2h

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

	// cache_revisions: one clean key (watermark caught up), one dirty key
	// (watermark behind revision) and one failed key (last refresh recorded an
	// error while the watermark itself is current). No cache_outbox exists.
	cacheStatement := "INSERT INTO cache_revisions (kind, scope, revision, refreshed_revision, " +
		"attempts, last_error, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)"
	monitorTestExec(t, stores, cacheStatement, "documents", "clean-"+suffix, 3, 3, 0, nil, nowUnix-60)
	monitorTestExec(t, stores, cacheStatement, "documents", "dirty-"+suffix, 5, 4, 0, nil, nowUnix-60)
	monitorTestExec(t, stores, cacheStatement, "documents", "failed-"+suffix, 2, 2, 3,
		"CacheUnavailable: redis timeout", nowUnix-60)

	// Uploads cover every category: two in progress, one failed, one rolling
	// back and one failed rollback, plus a committed operation whose old-version
	// cleanup is still pending (it is unfinished through cleanup_pending only).
	uploadStatement := "INSERT INTO upload_operations (id, namespace, name, document_id, " +
		"collection, owner, status, cleanup_pending, created_at, updated_at) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	insertUpload := func(status string, cleanupPending bool) {
		t.Helper()
		monitorTestExec(t, stores, uploadStatement, monitorTestID(t, 32), namespace,
			"upload.txt", "doc-"+suffix, "collection-"+suffix, monitorTestID(t, 32),
			status, cleanupPending, nowUnix-60, nowUnix-30)
	}
	insertUpload(UploadIntent, false)
	insertUpload(UploadRunning, false)
	insertUpload(UploadFailed, false)
	insertUpload(UploadRollingBack, false)
	insertUpload(UploadRollbackFailed, false)
	insertUpload(UploadCommitted, true)
	insertUpload(UploadCommitted, false)

	// One live document and one soft-deleted tombstone: only the live record may
	// be counted by library.documents.
	documentStatement := "INSERT INTO documents (namespace, name, document_id, size, sha256, " +
		"chunking, collection, chunk_count, modified_at, indexed_at, diagnostics, " +
		"skipped_object_count, del_flag) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	monitorTestExec(t, stores, documentStatement, namespace, "doc.txt", "doc-"+suffix, 3,
		"sha-"+suffix, "{}", "collection-"+suffix, 1, "", "", "[]", 0, false)
	monitorTestExec(t, stores, documentStatement, namespace, "deleted.txt", "deleted-"+suffix, 3,
		"sha-deleted-"+suffix, "{}", "collection-"+suffix, 1, "", "", "[]", 0, true)
	monitorTestExec(t, stores,
		"INSERT INTO knowledge_bases (namespace, collection, settings, version, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?)",
		namespace, "collection-"+suffix, "{}", 1, "", "")
	monitorTestExec(t, stores,
		"INSERT INTO artifact_snapshots (id, namespace, identity, document_id, version, kind, "+
			"name, mime_type, content, created_at, expires_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		monitorTestArtifactID(t), namespace, "identity-"+suffix, "doc-"+suffix,
		nil, "image", "a.png", "image/png", []byte("png"), nowUnix, nowUnix+600)

	// The activity counter is the only source of the recent-search number: search
	// runs synchronously, so no queue rows can be added on top of it.
	for attempt := 0; attempt < 3; attempt++ {
		if err := stores.Activity.Record(ctx, ActivitySearch, now); err != nil {
			t.Fatalf("Activity.Record() error = %v", err)
		}
	}
	if err := stores.Activity.Record(ctx, ActivitySearch, now.Add(-2*recentWindow)); err != nil {
		t.Fatalf("Activity.Record(old) error = %v", err)
	}

	t.Cleanup(func() {
		cleanup := context.Background()
		for _, statement := range []string{
			"DELETE FROM document_jobs WHERE namespace = ?",
			"DELETE FROM cache_revisions WHERE scope = ?",
			"DELETE FROM upload_operations WHERE namespace = ?",
			"DELETE FROM artifact_snapshots WHERE namespace = ?",
			"DELETE FROM documents WHERE namespace = ?",
			"DELETE FROM knowledge_bases WHERE namespace = ?",
		} {
			scope := namespace
			if statement == "DELETE FROM cache_revisions WHERE scope = ?" {
				// The three monitor rows use suffixed scopes; delete them by their
				// common prefix through three explicit deletes below.
				continue
			}
			if _, err := stores.DB.ExecContext(cleanup, statement, scope); err != nil {
				t.Logf("cleanup %q error = %v", statement, err)
			}
		}
		for _, scope := range []string{"clean-" + suffix, "dirty-" + suffix, "failed-" + suffix} {
			if _, err := stores.DB.ExecContext(cleanup,
				"DELETE FROM cache_revisions WHERE scope = ?", scope); err != nil {
				t.Logf("cleanup cache_revisions error = %v", err)
			}
		}
		if _, err := stores.DB.ExecContext(cleanup,
			"DELETE FROM activity_counters WHERE metric = ?", ActivitySearch); err != nil {
			t.Logf("cleanup activity_counters error = %v", err)
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

	// Only the three recorded activity counts are inside the window; search is
	// synchronous and has no queue rows that could leak into the counter.
	if stats.SearchesRecent != 3 {
		t.Fatalf("SearchesRecent = %d, want 3 (activity counter only)", stats.SearchesRecent)
	}
	if stats.CacheDirtyKeys != 1 {
		t.Fatalf("CacheDirtyKeys = %d, want 1", stats.CacheDirtyKeys)
	}
	if stats.CacheRefreshFailures != 1 {
		t.Fatalf("CacheRefreshFailures = %d, want 1", stats.CacheRefreshFailures)
	}

	wantUploadStatuses := map[string]int{
		UploadIntent: 1, UploadRunning: 1, UploadFailed: 1,
		UploadRollingBack: 1, UploadRollbackFailed: 1, UploadCommitted: 2,
	}
	for status, want := range wantUploadStatuses {
		if got := stats.UploadsByStatus[status]; got != want {
			t.Fatalf("UploadsByStatus[%q] = %d, want %d (map %v)",
				status, got, want, stats.UploadsByStatus)
		}
	}
	if stats.UploadsInProgress != 2 || stats.UploadsFailed != 2 ||
		stats.UploadsRollingBack != 1 || stats.UploadsCleanupPending != 1 {
		t.Fatalf("upload categories = in_progress %d, failed %d, rolling_back %d, cleanup_pending %d; want 2/2/1/1",
			stats.UploadsInProgress, stats.UploadsFailed,
			stats.UploadsRollingBack, stats.UploadsCleanupPending)
	}
	// 5 unfinished-status rows + 1 committed row whose cleanup is pending; the
	// union never double counts a row that is active AND cleanup-pending.
	if stats.UnfinishedUploads != 6 {
		t.Fatalf("UnfinishedUploads = %d, want 6", stats.UnfinishedUploads)
	}

	if stats.Documents != 1 {
		t.Fatalf("Documents = %d, want 1 (the del_flag=1 tombstone must not count)", stats.Documents)
	}
	if stats.KnowledgeBases != 1 || stats.ArtifactSnapshots != 1 {
		t.Fatalf("library counts = documents %d, knowledge bases %d, artifacts %d; want 1/1/1",
			stats.Documents, stats.KnowledgeBases, stats.ArtifactSnapshots)
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
	// The five unfinished upload states stay present so the Console renders a
	// complete category row on an empty database.
	for _, status := range []string{
		UploadIntent, UploadRunning, UploadFailed, UploadRollingBack, UploadRollbackFailed,
	} {
		if got, present := stats.UploadsByStatus[status]; !present || got != 0 {
			t.Fatalf("UploadsByStatus[%q] = %d, present = %v", status, got, present)
		}
	}
	if stats.UploadsInProgress != 0 || stats.UploadsFailed != 0 ||
		stats.UploadsRollingBack != 0 || stats.UploadsCleanupPending != 0 ||
		stats.UnfinishedUploads != 0 {
		t.Fatalf("empty upload counters = %d/%d/%d/%d/%d",
			stats.UploadsInProgress, stats.UploadsFailed, stats.UploadsRollingBack,
			stats.UploadsCleanupPending, stats.UnfinishedUploads)
	}
	if stats.CacheDirtyKeys != 0 || stats.CacheRefreshFailures != 0 {
		t.Fatalf("empty CDC counters = dirty %d, failures %d",
			stats.CacheDirtyKeys, stats.CacheRefreshFailures)
	}
	if stats.Documents != 0 || stats.KnowledgeBases != 0 || stats.ArtifactSnapshots != 0 {
		t.Fatalf("library counts = %d/%d/%d, want 0/0/0",
			stats.Documents, stats.KnowledgeBases, stats.ArtifactSnapshots)
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
