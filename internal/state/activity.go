package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ActivitySearch is the activity counter metric for one retrieval execution.
// Every synchronous REST/MCP search and every queued search worker run goes
// through runtime.Service.Search, so this counter covers all of them while
// search_tasks only holds the queued subset.
const ActivitySearch = "search"

const (
	// activityMetricMaxLength is the width of activity_counters.metric.
	activityMetricMaxLength = 64
	// activityBucketSeconds is the width of one activity counter bucket.
	// Counters are stored per (metric, hour) row so the monitoring read stays
	// bounded instead of growing one row per event.
	activityBucketSeconds int64 = int64(time.Hour / time.Second)
)

// ActivityCounters stores coarse-grained monotonic activity counters in hourly
// buckets (the Go-only activity_counters monitoring table).
type ActivityCounters struct {
	db *sql.DB
}

func newActivityCounters(database *sql.DB) *ActivityCounters {
	return &ActivityCounters{db: database}
}

// Record increments metric's counter in the hourly bucket that contains now.
// Counters are additive only: when a write is lost the total is one lower, but
// an existing bucket is never overwritten.
func (c *ActivityCounters) Record(ctx context.Context, metric string, now time.Time) error {
	if c == nil || c.db == nil {
		return newStateError("活动计数器未装配")
	}
	normalized, err := normalizeActivityMetric(metric)
	if err != nil {
		return err
	}
	const statement = "INSERT INTO activity_counters (metric, window_start, `count`) VALUES (?, ?, 1) " +
		"ON DUPLICATE KEY UPDATE `count` = `count` + 1"
	if _, err := c.db.ExecContext(ctx, statement, normalized, activityBucketStart(now)); err != nil {
		return fmt.Errorf("活动计数器写入失败：%w", err)
	}
	return nil
}

// Count returns the total of metric's buckets in the window that starts at
// since. Buckets are hour-aligned, so the result always contains the whole
// bucket holding since (at most one extra hour of counts) and never loses the
// current partial bucket. A metric without rows returns 0.
func (c *ActivityCounters) Count(ctx context.Context, metric string, since time.Time) (int, error) {
	if c == nil || c.db == nil {
		return 0, newStateError("活动计数器未装配")
	}
	normalized, err := normalizeActivityMetric(metric)
	if err != nil {
		return 0, err
	}
	var total sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(`count`), 0) FROM activity_counters WHERE metric = ? AND window_start >= ?",
		normalized, activityBucketStart(since)).Scan(&total); err != nil {
		return 0, fmt.Errorf("活动计数器读取失败：%w", err)
	}
	if !total.Valid {
		return 0, nil
	}
	return int(total.Int64), nil
}

// activityBucketStart floors t to the start of its hourly counter bucket.
func activityBucketStart(t time.Time) int64 {
	seconds := t.Unix()
	start := seconds - seconds%activityBucketSeconds
	// Go's % truncates toward zero; keep the floor behaviour before the epoch.
	if seconds < 0 && seconds%activityBucketSeconds != 0 {
		start -= activityBucketSeconds
	}
	return start
}

// normalizeActivityMetric validates one metric name against its column width.
func normalizeActivityMetric(metric string) (string, error) {
	metric = strings.TrimSpace(metric)
	if metric == "" {
		return "", newValueError("活动计数器指标不能为空")
	}
	if utf8.RuneCountInString(metric) > activityMetricMaxLength {
		return "", newValueError(fmt.Sprintf("活动计数器指标长度不能超过 %d 个字符", activityMetricMaxLength))
	}
	return metric, nil
}
