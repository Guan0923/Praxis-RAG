package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MonitorStats is the read-only queue and library snapshot behind the Console
// monitoring page. Every field comes from a COUNT/MIN/GROUP BY aggregate: the
// queries never select job content, search arguments, error payloads, or any
// other unbounded column.
type MonitorStats struct {
	// DocumentJobsByStatus counts document_jobs per status with the four
	// terminal/active states always present (zero when absent).
	DocumentJobsByStatus map[string]int
	// DocumentJobsByStage counts only processing jobs, per pipeline stage.
	DocumentJobsByStage map[string]int
	// OldestQueuedSeconds is the age of the oldest queued job (0 when none).
	OldestQueuedSeconds float64
	// StuckProcessing counts processing jobs whose lease already expired.
	StuckProcessing int
	// FailedRecent counts failed jobs updated inside the recent window.
	FailedRecent int
	// JobsSucceededRecent counts succeeded jobs updated inside the recent window.
	JobsSucceededRecent int

	// SearchesRecent counts every retrieval execution inside the recent window.
	// Search is synchronous: REST and MCP calls both pass through
	// runtime.Service.Search and are counted by the activity counter.
	SearchesRecent int

	// CacheDirtyKeys counts read-model keys whose Redis value is behind the
	// database revision (refreshed_revision < revision).
	CacheDirtyKeys int
	// CacheRefreshFailures counts read-model keys whose last refresh attempt
	// recorded an error (last_error IS NOT NULL).
	CacheRefreshFailures int

	// UploadsByStatus counts upload_operations per stored status. The five
	// unfinished states (INTENT/RUNNING/FAILED/ROLLING_BACK/ROLLBACK_FAILED) are
	// always present, zero when absent.
	UploadsByStatus map[string]int
	// UploadsInProgress counts upload operations that are still admitting or
	// running (INTENT + RUNNING).
	UploadsInProgress int
	// UploadsFailed counts upload operations that failed and still need
	// attention: FAILED plus a failed rollback (ROLLBACK_FAILED).
	UploadsFailed int
	// UploadsRollingBack counts upload operations whose rollback is currently
	// running (ROLLING_BACK).
	UploadsRollingBack int
	// UploadsCleanupPending counts upload operations whose old-version cleanup
	// is pending, whatever their status (the cleanup_pending flag).
	UploadsCleanupPending int
	// UnfinishedUploads counts upload operations that are still active or whose
	// old-version cleanup is pending (the UploadJournal.Unfinished predicate):
	// the union of the unfinished statuses and cleanup_pending, never the sum of
	// the category counters above.
	UnfinishedUploads int

	// Documents counts live (not soft-deleted) document records.
	// KnowledgeBases and ArtifactSnapshots are plain table counts of the library
	// state. Artifact snapshots back the independently readable evidence files
	// attached to search responses.
	Documents         int
	KnowledgeBases    int
	ArtifactSnapshots int
}

// MonitorStats aggregates the monitoring counters in one call. The caller
// chooses the recent-activity window (the Console page uses 24h) and passes the
// reference time so tests and callers share one consistent clock. A database
// error on any query aborts the snapshot; the caller decides how to degrade.
func (s *Stores) MonitorStats(ctx context.Context, now time.Time, recentWindow time.Duration) (*MonitorStats, error) {
	if recentWindow < 0 {
		return nil, newValueError("monitor recent window must not be negative")
	}
	nowSeconds := float64(now.Unix())
	recentStartSeconds := float64(now.Add(-recentWindow).Unix())
	stats := &MonitorStats{}

	jobsByStatus, err := s.monitorGroupCounts(ctx,
		"SELECT status, COUNT(*) FROM document_jobs GROUP BY status")
	if err != nil {
		return nil, fmt.Errorf("document_jobs 状态聚合失败：%w", err)
	}
	stats.DocumentJobsByStatus = monitorWithFallbackKeys(jobsByStatus,
		DocumentJobQueued, DocumentJobProcessing, DocumentJobSucceeded, DocumentJobFailed)

	stats.DocumentJobsByStage, err = s.monitorGroupCounts(ctx,
		"SELECT stage, COUNT(*) FROM document_jobs WHERE status = ? GROUP BY stage",
		DocumentJobProcessing)
	if err != nil {
		return nil, fmt.Errorf("document_jobs 阶段聚合失败：%w", err)
	}

	stats.OldestQueuedSeconds, err = s.monitorOldestAgeSeconds(ctx,
		"SELECT MIN(created_at) FROM document_jobs WHERE status = ?",
		now, DocumentJobQueued)
	if err != nil {
		return nil, fmt.Errorf("document_jobs 最旧排队任务查询失败：%w", err)
	}

	stats.StuckProcessing, err = s.monitorCount(ctx,
		"SELECT COUNT(*) FROM document_jobs WHERE status = ? AND lease_until < ?",
		DocumentJobProcessing, nowSeconds)
	if err != nil {
		return nil, fmt.Errorf("document_jobs 租约过期统计失败：%w", err)
	}

	stats.FailedRecent, err = s.monitorCount(ctx,
		"SELECT COUNT(*) FROM document_jobs WHERE status = ? AND updated_at >= ?",
		DocumentJobFailed, recentStartSeconds)
	if err != nil {
		return nil, fmt.Errorf("document_jobs 近期失败统计失败：%w", err)
	}

	stats.JobsSucceededRecent, err = s.monitorCount(ctx,
		"SELECT COUNT(*) FROM document_jobs WHERE status = ? AND updated_at >= ?",
		DocumentJobSucceeded, recentStartSeconds)
	if err != nil {
		return nil, fmt.Errorf("document_jobs 近期成功统计失败：%w", err)
	}

	// Every retrieval execution is counted by the hourly activity counter, so
	// synchronous searches (REST/MCP) are included.
	if s.Activity == nil {
		return nil, newStateError("活动计数器未装配")
	}
	stats.SearchesRecent, err = s.Activity.Count(ctx, ActivitySearch, now.Add(-recentWindow))
	if err != nil {
		return nil, fmt.Errorf("检索活动计数查询失败：%w", err)
	}

	// Cache convergence is observable from the revision watermark rows: a key is
	// dirty while its refreshed_revision is behind revision (or its last refresh
	// failed), no outbox table is involved anymore.
	filter, kinds := activeCacheKindFilter()
	stats.CacheDirtyKeys, err = s.monitorCount(ctx,
		"SELECT COUNT(*) FROM cache_revisions WHERE refreshed_revision < revision AND "+filter, kinds...)
	if err != nil {
		return nil, fmt.Errorf("cache_revisions 脏 key 统计失败：%w", err)
	}
	stats.CacheRefreshFailures, err = s.monitorCount(ctx,
		"SELECT COUNT(*) FROM cache_revisions WHERE last_error IS NOT NULL AND "+filter, kinds...)
	if err != nil {
		return nil, fmt.Errorf("cache_revisions 刷新失败统计失败：%w", err)
	}

	uploadCounts, err := s.monitorUploadCounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("upload_operations 分类统计失败：%w", err)
	}
	stats.UploadsByStatus = monitorWithFallbackKeys(uploadCounts.byStatus,
		UploadIntent, UploadRunning, UploadFailed, UploadRollingBack, UploadRollbackFailed)
	stats.UploadsInProgress = uploadCounts.inProgress
	stats.UploadsFailed = uploadCounts.failed
	stats.UploadsRollingBack = uploadCounts.rollingBack
	stats.UploadsCleanupPending = uploadCounts.cleanupPending
	stats.UnfinishedUploads = uploadCounts.unfinished

	// Soft-deleted documents (del_flag = 1) are tombstones, so the library count
	// must only include live records. Artifact snapshots are the independently
	// readable evidence files; search results are no longer stored.
	if err := s.DB.QueryRowContext(ctx,
		"SELECT (SELECT COUNT(*) FROM documents WHERE del_flag = 0), "+
			"(SELECT COUNT(*) FROM knowledge_bases), (SELECT COUNT(*) FROM artifact_snapshots)").
		Scan(&stats.Documents, &stats.KnowledgeBases, &stats.ArtifactSnapshots); err != nil {
		return nil, fmt.Errorf("资料库计数查询失败：%w", err)
	}
	return stats, nil
}

// MonitorIndexedCollections lists the Qdrant collections that live document
// records are indexed into, ordered by name. The monitoring probe verifies
// exactly these collections instead of the single RAG_COLLECTION fallback name:
// every knowledge base stores its own generated collection name in its settings,
// so the fallback is not required to exist, and a knowledge base without
// documents legitimately has no Qdrant collection yet. Soft-deleted documents
// are tombstones and do not require their collection to remain.
func (s *Stores) MonitorIndexedCollections(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT DISTINCT collection FROM documents WHERE del_flag = 0 AND collection <> '' ORDER BY collection")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	collections := make([]string, 0, 8)
	for rows.Next() {
		var collection string
		if err := rows.Scan(&collection); err != nil {
			return nil, err
		}
		collections = append(collections, collection)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return collections, nil
}

// monitorGroupCounts runs a "SELECT <key>, COUNT(*) ... GROUP BY <key>" query.
func (s *Stores) monitorGroupCounts(ctx context.Context, query string, arguments ...any) (map[string]int, error) {
	rows, err := s.DB.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	counts := make(map[string]int)
	for rows.Next() {
		var key string
		var count int
		if err := rows.Scan(&key, &count); err != nil {
			return nil, err
		}
		counts[key] = count
	}
	return counts, rows.Err()
}

// monitorCount runs a single COUNT(*) query.
func (s *Stores) monitorCount(ctx context.Context, query string, arguments ...any) (int, error) {
	var count int
	err := s.DB.QueryRowContext(ctx, query, arguments...).Scan(&count)
	return count, err
}

// monitorOldestAgeSeconds runs a MIN(created_at) query and returns its age at
// now in seconds, clamped to zero. A missing row (NULL) is zero seconds.
func (s *Stores) monitorOldestAgeSeconds(ctx context.Context, query string, now time.Time, arguments ...any) (float64, error) {
	var oldest sql.NullFloat64
	if err := s.DB.QueryRowContext(ctx, query, arguments...).Scan(&oldest); err != nil {
		return 0, err
	}
	if !oldest.Valid {
		return 0, nil
	}
	age := float64(now.Unix()) - oldest.Float64
	if age < 0 {
		return 0, nil
	}
	return age, nil
}

// monitorWithFallbackKeys guarantees that every known status is present so the
// Console renders a complete queue table even on an empty database.
func monitorWithFallbackKeys(values map[string]int, required ...string) map[string]int {
	if values == nil {
		values = make(map[string]int, len(required))
	}
	for _, key := range required {
		if _, present := values[key]; !present {
			values[key] = 0
		}
	}
	return values
}

// monitorUploadCounts is the per-(status, cleanup_pending) cross tab of the
// upload journal. One grouped read keeps every upload counter consistent with
// the others: the category counters are additive views of the same rows while
// UnfinishedUploads stays the union predicate (a row is counted once even when
// its status is active and its cleanup is pending).
type monitorUploadCounts struct {
	byStatus       map[string]int
	inProgress     int
	failed         int
	rollingBack    int
	cleanupPending int
	unfinished     int
}

// monitorUploadCounts aggregates upload_operations by status and cleanup flag.
func (s *Stores) monitorUploadCounts(ctx context.Context) (monitorUploadCounts, error) {
	result := monitorUploadCounts{byStatus: make(map[string]int)}
	rows, err := s.DB.QueryContext(ctx,
		"SELECT status, cleanup_pending, COUNT(*) FROM upload_operations GROUP BY status, cleanup_pending")
	if err != nil {
		return monitorUploadCounts{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var status string
		var cleanupPending sql.NullBool
		var count int
		if err := rows.Scan(&status, &cleanupPending, &count); err != nil {
			return monitorUploadCounts{}, err
		}
		pending := cleanupPending.Valid && cleanupPending.Bool
		result.byStatus[status] += count
		if pending {
			result.cleanupPending += count
		}
		switch status {
		case UploadIntent, UploadRunning:
			result.inProgress += count
		case UploadFailed, UploadRollbackFailed:
			// A failed rollback is a terminal failure that still needs cleanup,
			// so it belongs to the failure category, not the in-flight rollback.
			result.failed += count
		case UploadRollingBack:
			result.rollingBack += count
		}
		if pending || uploadStatusUnfinished(status) {
			result.unfinished += count
		}
	}
	if err := rows.Err(); err != nil {
		return monitorUploadCounts{}, err
	}
	return result, nil
}

// uploadStatusUnfinished reports the statuses of the UploadJournal.Unfinished
// predicate.
func uploadStatusUnfinished(status string) bool {
	switch status {
	case UploadIntent, UploadRunning, UploadFailed, UploadRollingBack, UploadRollbackFailed:
		return true
	default:
		return false
	}
}
