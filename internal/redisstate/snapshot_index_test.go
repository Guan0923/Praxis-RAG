package redisstate

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// memberSet 把 ZSET 成员转成集合，便于断言"包含/不包含"。
func memberSet(members []string) map[string]struct{} {
	set := make(map[string]struct{}, len(members))
	for _, member := range members {
		set[member] = struct{}{}
	}
	return set
}

// TestResultSnapshotIndexAddAndCountActive 覆盖正常路径：Add 写入 ZSET，
// CountActive 先清理过期成员再计数。
func TestResultSnapshotIndexAddAndCountActive(t *testing.T) {
	backend, server := newTestBackend(t)
	index := backend.ResultSnapshotIndex()
	if got, want := index.Key(), backend.Settings().Prefix+":index:search-results"; got != want {
		t.Fatalf("Key() = %q, want %q", got, want)
	}
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	entries := []struct {
		identity  string
		expiresAt float64
	}{
		{"active-a", float64(now.Add(time.Hour).Unix())},
		{"active-b", float64(now.Add(2 * time.Hour).Unix())},
		{"expired", float64(now.Add(-time.Minute).Unix())},
	}
	for _, entry := range entries {
		if err := index.Add(ctx, entry.identity, entry.expiresAt); err != nil {
			t.Fatalf("Add(%q) error = %v", entry.identity, err)
		}
	}
	count, err := index.CountActive(ctx, now)
	if err != nil {
		t.Fatalf("CountActive() error = %v", err)
	}
	if count != 2 {
		t.Fatalf("CountActive() = %d, want 2 (expired member removed)", count)
	}
	members, err := server.ZMembers(index.Key())
	if err != nil {
		t.Fatalf("ZMembers() error = %v", err)
	}
	set := memberSet(members)
	if len(members) != 2 || set["active-a"] == struct{}{} && set["active-b"] == struct{}{} && set["expired"] != struct{}{} {
		// 上面的条件写反时也要能报错，这里单独断言。
	}
	if _, ok := set["expired"]; ok {
		t.Fatalf("expired member was not cleaned: %v", members)
	}
	for _, identity := range []string{"active-a", "active-b"} {
		if _, ok := set[identity]; !ok {
			t.Fatalf("member %q missing after Add: %v", identity, members)
		}
	}
	// 再次计数是幂等的，不会重复累加。
	if again, err := index.CountActive(ctx, now); err != nil || again != 2 {
		t.Fatalf("second CountActive() = %d, %v", again, err)
	}
}

// TestResultSnapshotIndexAddRejectsInvalidArguments 断言非法身份/过期时间在
// 访问 Redis 前被拒绝。
func TestResultSnapshotIndexAddRejectsInvalidArguments(t *testing.T) {
	backend, _ := newTestBackend(t)
	index := backend.ResultSnapshotIndex()
	ctx := context.Background()
	var valueErr *state.ValueError
	if err := index.Add(ctx, "", 1); !errors.As(err, &valueErr) {
		t.Fatalf("empty identity error = %v, want *state.ValueError", err)
	}
	if err := index.Add(ctx, "id", math.NaN()); !errors.As(err, &valueErr) {
		t.Fatalf("NaN expiry error = %v, want *state.ValueError", err)
	}
	if err := index.Add(ctx, "id", math.Inf(1)); !errors.As(err, &valueErr) {
		t.Fatalf("Inf expiry error = %v, want *state.ValueError", err)
	}
}

// TestResultSnapshotIndexDisabledReportsUnavailable 断言 RAG_REDIS_ENABLED=false
// 时索引返回错误，让监控页显示未知而不是 0。
func TestResultSnapshotIndexDisabledReportsUnavailable(t *testing.T) {
	index := NewDisabled().ResultSnapshotIndex()
	ctx := context.Background()
	var unavailable *TransientUnavailable
	if err := index.Add(ctx, "id", 1); !errors.As(err, &unavailable) {
		t.Fatalf("disabled Add error = %v, want *TransientUnavailable", err)
	}
	if _, err := index.CountActive(ctx, time.Now()); !errors.As(err, &unavailable) {
		t.Fatalf("disabled CountActive error = %v, want *TransientUnavailable", err)
	}
}

// TestResultSnapshotIndexUnavailableBackend 断言 Redis 断开时索引返回错误而不是
// 伪造计数。
func TestResultSnapshotIndexUnavailableBackend(t *testing.T) {
	backend, server := newTestBackend(t)
	server.Close()
	index := backend.ResultSnapshotIndex()
	ctx := context.Background()
	var unavailable *TransientUnavailable
	if err := index.Add(ctx, "id", 1); !errors.As(err, &unavailable) {
		t.Fatalf("closed backend Add error = %v, want *TransientUnavailable", err)
	}
	if _, err := index.CountActive(ctx, time.Now()); !errors.As(err, &unavailable) {
		t.Fatalf("closed backend CountActive error = %v, want *TransientUnavailable", err)
	}
}

// TestAttachIndexesSavedSnapshotsAndCountsActive 覆盖 task-2 的接线要求：
// Attach 之后 state.ResultSnapshots.Save 会写入 ZSET，CountActive 能计数，
// 过期成员在计数时被清理，且快照本身仍可正常读取。
func TestAttachIndexesSavedSnapshotsAndCountsActive(t *testing.T) {
	backend, server := newTestBackend(t)
	const nowSeconds = 1_700_000_000
	clock := func() float64 { return nowSeconds }
	settings := state.DefaultSnapshotSettings()
	snapshots := state.NewResultSnapshots(nil, settings, clock)
	stores := &state.Stores{
		CacheSync: &state.CacheSynchronizer{},
		Results:   snapshots,
	}
	if err := Attach(stores, backend); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if stores.Results.Index == nil {
		t.Fatal("Attach did not wire the result snapshot index")
	}
	saved, err := stores.Results.Save("ns", map[string]any{
		"results": []any{map[string]any{"text": "evidence"}},
	}, nil, nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	resultID, _ := saved["result_id"].(string)
	if resultID == "" {
		t.Fatalf("saved = %#v", saved)
	}
	index := backend.ResultSnapshotIndex()
	members, err := server.ZMembers(index.Key())
	if err != nil {
		t.Fatalf("ZMembers: %v", err)
	}
	if len(members) != 1 || members[0] != resultID {
		t.Fatalf("index members = %v, want exactly %q", members, resultID)
	}
	score, err := server.ZScore(index.Key(), resultID)
	if err != nil {
		t.Fatalf("ZScore: %v", err)
	}
	if want := float64(nowSeconds + settings.TTLSeconds); score != want {
		t.Fatalf("index score = %v, want expiry %v", score, want)
	}
	count, err := index.CountActive(context.Background(), time.Unix(nowSeconds, 0))
	if err != nil || count != 1 {
		t.Fatalf("CountActive() = %d, %v, want 1", count, err)
	}
	// 过期成员在 CountActive 时被清理，不会虚增有效快照数。
	if err := index.Add(context.Background(), "stale", float64(nowSeconds-1)); err != nil {
		t.Fatalf("Add(stale): %v", err)
	}
	count, err = index.CountActive(context.Background(), time.Unix(nowSeconds, 0))
	if err != nil || count != 1 {
		t.Fatalf("CountActive() after stale = %d, %v, want 1", count, err)
	}
	members, err = server.ZMembers(index.Key())
	if err != nil {
		t.Fatalf("ZMembers after cleanup: %v", err)
	}
	if _, ok := memberSet(members)["stale"]; ok {
		t.Fatalf("stale member was not cleaned: %v", members)
	}
	// 索引不影响快照本身的保存与读取。
	if _, err := stores.Results.Get("ns", resultID, 1, 10); err != nil {
		t.Fatalf("Get after Save: %v", err)
	}
}
