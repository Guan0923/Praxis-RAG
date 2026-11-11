package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// documentOutboxPendingStatus is the stored value of a queued outbox row.
// document_job_outbox.status is a case-sensitive identifier column, so the
// aggregate query must use the same upper-case literal that DocumentJobs writes
// (see the "PENDING" inserts in document_jobs.go).
const documentOutboxPendingStatus = "PENDING"

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

	// OutboxByStatus counts document_job_outbox rows per status; the keys are
	// the stored values (PENDING, PUBLISHED).
	OutboxByStatus map[string]int
	// OldestPendingOutboxSeconds is the age of the oldest PENDING outbox row.
	OldestPendingOutboxSeconds float64

	// SearchByStatus counts search_tasks per status with the five known states
	// always present (zero when absent).
	SearchByStatus map[string]int
	// OldestActiveSearchSeconds is the age of the oldest queued/processing task.
	OldestActiveSearchSeconds float64
	// SearchesRecent counts finished searches updated inside the recent window.
	SearchesRecent int

	// UnfinishedUploads counts upload operations that are still active or whose
	// old-version cleanup is pending (the UploadJournal.Unfinished predicate).
	UnfinishedUploads int

	// Documents, KnowledgeBases, ResultSnapshots and ArtifactSnapshots are
	// plain table counts of the library state.
	Documents         int
	KnowledgeBases    int
	ResultSnapshots   int
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

	stats.OutboxByStatus, err = s.monitorGroupCounts(ctx,
		"SELECT status, COUNT(*) FROM document_job_outbox GROUP BY status")
	if err != nil {
		return nil, fmt.Errorf("document_job_outbox 状态聚合失败：%w", err)
	}
	if stats.OutboxByStatus == nil {
		stats.OutboxByStatus = map[string]int{}
	}

	stats.OldestPendingOutboxSeconds, err = s.monitorOldestAgeSeconds(ctx,
		"SELECT MIN(created_at) FROM document_job_outbox WHERE status = ?",
		now, documentOutboxPendingStatus)
	if err != nil {
		return nil, fmt.Errorf("document_job_outbox 最旧待投递事件查询失败：%w", err)
	}

	searchByStatus, err := s.monitorGroupCounts(ctx,
		"SELECT status, COUNT(*) FROM search_tasks GROUP BY status")
	if err != nil {
		return nil, fmt.Errorf("search_tasks 状态聚合失败：%w", err)
	}
	stats.SearchByStatus = monitorWithFallbackKeys(searchByStatus,
		SearchTaskQueued, SearchTaskProcessing, SearchTaskSucceeded, SearchTaskFailed, SearchTaskTimedOut)

	stats.OldestActiveSearchSeconds, err = s.monitorOldestAgeSeconds(ctx,
		"SELECT MIN(created_at) FROM search_tasks WHERE status IN (?, ?)",
		now, SearchTaskQueued, SearchTaskProcessing)
	if err != nil {
		return nil, fmt.Errorf("search_tasks 最旧活动任务查询失败：%w", err)
	}

	stats.SearchesRecent, err = s.monitorCount(ctx,
		"SELECT COUNT(*) FROM search_tasks WHERE updated_at >= ? AND status IN (?, ?, ?)",
		recentStartSeconds, SearchTaskSucceeded, SearchTaskFailed, SearchTaskTimedOut)
	if err != nil {
		return nil, fmt.Errorf("search_tasks 近期完成统计失败：%w", err)
	}

	// The predicate mirrors UploadJournal.Unfinished; only the count is needed.
	stats.UnfinishedUploads, err = s.monitorCount(ctx,
		"SELECT COUNT(*) FROM upload_operations WHERE (status IN (?, ?, ?, ?, ?) OR cleanup_pending = 1)",
		UploadIntent, UploadRunning, UploadFailed, UploadRollingBack, UploadRollbackFailed)
	if err != nil {
		return nil, fmt.Errorf("upload_operations 未完成统计失败：%w", err)
	}

	if err := s.DB.QueryRowContext(ctx,
		"SELECT (SELECT COUNT(*) FROM documents), (SELECT COUNT(*) FROM knowledge_bases), "+
			"(SELECT COUNT(*) FROM search_result_snapshots), (SELECT COUNT(*) FROM artifact_snapshots)").
		Scan(&stats.Documents, &stats.KnowledgeBases, &stats.ResultSnapshots, &stats.ArtifactSnapshots); err != nil {
		return nil, fmt.Errorf("资料库计数查询失败：%w", err)
	}
	return stats, nil
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
