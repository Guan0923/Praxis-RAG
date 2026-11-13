package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// HeartbeatRoleDocumentWorker is the runtime_heartbeats role of the document
// worker process. The monitoring page reads the newest row of this role to tell
// whether the worker is actually running, which the static service config
// cannot show.
const HeartbeatRoleDocumentWorker = "document-worker"

const (
	// heartbeatRoleMaxLength and heartbeatInstanceMaxLength match the
	// runtime_heartbeats column widths.
	heartbeatRoleMaxLength     = 64
	heartbeatInstanceMaxLength = 200
	// heartbeatRetention is how long a heartbeat row survives after its last
	// write. Record prunes older rows so a long-gone worker instance stops
	// being counted as running.
	heartbeatRetention = 24 * time.Hour
)

// HeartbeatSnapshot is the newest recorded heartbeat of one role plus the
// number of instances that recorded inside the requested freshness window.
type HeartbeatSnapshot struct {
	// UpdatedAt is the newest heartbeat time; zero when Found is false.
	UpdatedAt time.Time
	// Instances counts the role's instances that recorded a heartbeat inside
	// the freshness window passed to Latest.
	Instances int
	// Found reports whether the role has any heartbeat row at all.
	Found bool
}

// RuntimeHeartbeats stores periodic worker liveness heartbeats (the Go-only
// runtime_heartbeats monitoring table). There is one row per (role, instance);
// every beat upserts it, so the newest updated_at is the liveness evidence.
type RuntimeHeartbeats struct {
	db *sql.DB
}

func newRuntimeHeartbeats(database *sql.DB) *RuntimeHeartbeats {
	return &RuntimeHeartbeats{db: database}
}

// Record upserts one instance heartbeat and then prunes rows older than the
// 24 hour retention window. The prune is best effort: housekeeping must never
// turn a successful liveness write into a reported failure.
func (h *RuntimeHeartbeats) Record(ctx context.Context, role, instance string, now time.Time) error {
	if h == nil || h.db == nil {
		return newStateError("Worker 心跳存储未装配")
	}
	normalizedRole, err := normalizeHeartbeatRole(role)
	if err != nil {
		return err
	}
	normalizedInstance, err := normalizeHeartbeatInstance(instance)
	if err != nil {
		return err
	}
	timestamp := heartbeatTimeSeconds(now)
	const statement = "INSERT INTO runtime_heartbeats (role, instance, updated_at) VALUES (?, ?, ?) " +
		"ON DUPLICATE KEY UPDATE updated_at = ?"
	if _, err := h.db.ExecContext(ctx, statement,
		normalizedRole, normalizedInstance, timestamp, timestamp); err != nil {
		return fmt.Errorf("Worker 心跳写入失败：%w", err)
	}
	_, _ = h.db.ExecContext(ctx, "DELETE FROM runtime_heartbeats WHERE updated_at < ?",
		heartbeatTimeSeconds(now.Add(-heartbeatRetention)))
	return nil
}

// Latest returns the newest heartbeat of role and how many of its instances
// recorded inside staleAfter (measured from the current clock). A stale row is
// still reported as found with its old UpdatedAt: the caller decides whether
// that age means the worker stopped.
func (h *RuntimeHeartbeats) Latest(ctx context.Context, role string, staleAfter time.Duration) (HeartbeatSnapshot, error) {
	if h == nil || h.db == nil {
		return HeartbeatSnapshot{}, newStateError("Worker 心跳存储未装配")
	}
	normalizedRole, err := normalizeHeartbeatRole(role)
	if err != nil {
		return HeartbeatSnapshot{}, err
	}
	if staleAfter < 0 {
		staleAfter = 0
	}
	freshSince := heartbeatTimeSeconds(time.Now().Add(-staleAfter))
	var latest sql.NullFloat64
	var instances int64
	if err := h.db.QueryRowContext(ctx,
		"SELECT MAX(updated_at), COALESCE(SUM(updated_at >= ?), 0) FROM runtime_heartbeats WHERE role = ?",
		freshSince, normalizedRole).Scan(&latest, &instances); err != nil {
		return HeartbeatSnapshot{}, fmt.Errorf("Worker 心跳读取失败：%w", err)
	}
	snapshot := HeartbeatSnapshot{Instances: int(instances)}
	if latest.Valid {
		snapshot.Found = true
		snapshot.UpdatedAt = heartbeatTimeFromSeconds(latest.Float64)
	}
	return snapshot, nil
}

// heartbeatTimeSeconds renders a time as the DOUBLE seconds stored in
// runtime_heartbeats.updated_at.
func heartbeatTimeSeconds(t time.Time) float64 {
	return float64(t.Unix()) + float64(t.Nanosecond())/float64(time.Second)
}

// heartbeatTimeFromSeconds converts the stored DOUBLE seconds back to a time.
func heartbeatTimeFromSeconds(seconds float64) time.Time {
	whole := int64(seconds)
	nanos := int64((seconds - float64(whole)) * float64(time.Second))
	return time.Unix(whole, nanos)
}

// normalizeHeartbeatRole validates one role name against its column width.
func normalizeHeartbeatRole(role string) (string, error) {
	role = strings.TrimSpace(role)
	if role == "" {
		return "", newValueError("Worker 心跳角色不能为空")
	}
	if utf8.RuneCountInString(role) > heartbeatRoleMaxLength {
		return "", newValueError(fmt.Sprintf("Worker 心跳角色长度不能超过 %d 个字符", heartbeatRoleMaxLength))
	}
	return role, nil
}

// normalizeHeartbeatInstance validates one instance identifier against its
// column width.
func normalizeHeartbeatInstance(instance string) (string, error) {
	instance = strings.TrimSpace(instance)
	if instance == "" {
		return "", newValueError("Worker 心跳实例不能为空")
	}
	if utf8.RuneCountInString(instance) > heartbeatInstanceMaxLength {
		return "", newValueError(fmt.Sprintf("Worker 心跳实例长度不能超过 %d 个字符", heartbeatInstanceMaxLength))
	}
	return instance, nil
}
