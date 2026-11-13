package state

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRuntimeHeartbeatsLatest verifies the newest heartbeat, the per-instance
// upsert and the instance count inside the freshness window.
func TestRuntimeHeartbeatsLatest(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	now := time.Now()

	snapshot, err := stores.Heartbeats.Latest(ctx, HeartbeatRoleDocumentWorker, 30*time.Minute)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if snapshot.Found || snapshot.Instances != 0 || !snapshot.UpdatedAt.IsZero() {
		t.Fatalf("empty Latest() = %+v, want not found", snapshot)
	}

	if err := stores.Heartbeats.Record(ctx, HeartbeatRoleDocumentWorker, "worker-a", now); err != nil {
		t.Fatalf("Record(worker-a) error = %v", err)
	}
	if err := stores.Heartbeats.Record(ctx, HeartbeatRoleDocumentWorker, "worker-b", now.Add(-10*time.Minute)); err != nil {
		t.Fatalf("Record(worker-b) error = %v", err)
	}
	// A repeated beat of the same instance updates its row, not a second one.
	if err := stores.Heartbeats.Record(ctx, HeartbeatRoleDocumentWorker, "worker-a", now); err != nil {
		t.Fatalf("Record(worker-a repeat) error = %v", err)
	}
	var rows int
	if err := stores.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM runtime_heartbeats").Scan(&rows); err != nil {
		t.Fatalf("counting heartbeat rows: %v", err)
	}
	if rows != 2 {
		t.Fatalf("heartbeat rows = %d, want 2", rows)
	}

	snapshot, err = stores.Heartbeats.Latest(ctx, HeartbeatRoleDocumentWorker, 30*time.Minute)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if !snapshot.Found || snapshot.Instances != 2 {
		t.Fatalf("Latest() = %+v, want found with 2 instances", snapshot)
	}
	if age := time.Since(snapshot.UpdatedAt); age < 0 || age > time.Minute {
		t.Fatalf("UpdatedAt age = %v, want ~0", age)
	}

	// Only instances that beat inside the freshness window are counted.
	snapshot, err = stores.Heartbeats.Latest(ctx, HeartbeatRoleDocumentWorker, 5*time.Minute)
	if err != nil {
		t.Fatalf("Latest(short window) error = %v", err)
	}
	if snapshot.Instances != 1 {
		t.Fatalf("Latest(short window) instances = %d, want 1", snapshot.Instances)
	}

	// Roles are independent.
	snapshot, err = stores.Heartbeats.Latest(ctx, "search-worker", 30*time.Minute)
	if err != nil {
		t.Fatalf("Latest(other role) error = %v", err)
	}
	if snapshot.Found {
		t.Fatalf("Latest(other role) = %+v, want not found", snapshot)
	}
}

// TestRuntimeHeartbeatsRecordPrunesStaleRows asserts the 24 hour retention
// cleanup keeps a long-gone instance out of the counter.
func TestRuntimeHeartbeatsRecordPrunesStaleRows(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	now := time.Now()
	if err := stores.Heartbeats.Record(ctx, HeartbeatRoleDocumentWorker, "worker-a", now); err != nil {
		t.Fatalf("Record(worker-a) error = %v", err)
	}
	if err := stores.Heartbeats.Record(ctx, HeartbeatRoleDocumentWorker, "worker-b", now); err != nil {
		t.Fatalf("Record(worker-b) error = %v", err)
	}
	// Simulate an instance that stopped more than the retention window ago.
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE runtime_heartbeats SET updated_at = ? WHERE instance = ?",
		heartbeatTimeSeconds(now.Add(-25*time.Hour)), "worker-b"); err != nil {
		t.Fatalf("aging worker-b heartbeat: %v", err)
	}
	if err := stores.Heartbeats.Record(ctx, HeartbeatRoleDocumentWorker, "worker-a", now); err != nil {
		t.Fatalf("Record(worker-a after aging) error = %v", err)
	}
	var rows int
	if err := stores.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM runtime_heartbeats").Scan(&rows); err != nil {
		t.Fatalf("counting heartbeat rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("heartbeat rows = %d after prune, want 1", rows)
	}
	snapshot, err := stores.Heartbeats.Latest(ctx, HeartbeatRoleDocumentWorker, 24*time.Hour)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if !snapshot.Found || snapshot.Instances != 1 {
		t.Fatalf("Latest() = %+v, want found with 1 instance", snapshot)
	}
}

// TestRuntimeHeartbeatsValidation asserts invalid identities are rejected and
// an unassembled store fails explicitly.
func TestRuntimeHeartbeatsValidation(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	now := time.Now()
	cases := []struct {
		name     string
		role     string
		instance string
	}{
		{"blank role", " ", "worker"},
		{"blank instance", HeartbeatRoleDocumentWorker, "  "},
		{"overlong role", strings.Repeat("r", heartbeatRoleMaxLength+1), "worker"},
		{"overlong instance", HeartbeatRoleDocumentWorker, strings.Repeat("i", heartbeatInstanceMaxLength+1)},
	}
	for _, testCase := range cases {
		if err := stores.Heartbeats.Record(ctx, testCase.role, testCase.instance, now); err == nil {
			t.Fatalf("Record(%s) error = nil, want a validation error", testCase.name)
		}
	}
	if _, err := (*RuntimeHeartbeats)(nil).Latest(ctx, HeartbeatRoleDocumentWorker, time.Minute); err == nil {
		t.Fatal("Latest(detached) error = nil, want a state error")
	}
}
