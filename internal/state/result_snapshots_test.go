package state

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
)

// memoryTransient is the in-memory TransientStore used by the snapshot tests;
// it reproduces the Redis semantics (JSON detach, NX, TTL ceiling recording).
type memoryTransient struct {
	mu      sync.Mutex
	values  map[string]any
	lastTTL map[string]float64
}

func newMemoryTransient() *memoryTransient {
	return &memoryTransient{values: map[string]any{}, lastTTL: map[string]float64{}}
}

func transientKey(kind, identity string) string { return kind + "\x00" + identity }

func cloneTransientValue(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var detached any
	if err := json.Unmarshal(encoded, &detached); err != nil {
		return nil, err
	}
	return detached, nil
}

func (m *memoryTransient) Get(kind, identity string) (any, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, found := m.values[transientKey(kind, identity)]
	return value, found, nil
}

func (m *memoryTransient) Put(kind, identity string, value any, ttlSeconds float64, nx bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := transientKey(kind, identity)
	if nx {
		if _, exists := m.values[key]; exists {
			return false, nil
		}
	}
	detached, err := cloneTransientValue(value)
	if err != nil {
		return false, err
	}
	m.values[key] = detached
	m.lastTTL[key] = ttlSeconds
	return true, nil
}

func (m *memoryTransient) Delete(kind, identity string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := transientKey(kind, identity)
	if _, exists := m.values[key]; !exists {
		return 0, nil
	}
	delete(m.values, key)
	return 1, nil
}

func (m *memoryTransient) has(kind, identity string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, found := m.values[transientKey(kind, identity)]
	return found
}

func newTestSnapshots(stores *Stores, settings SnapshotSettings, clock func() float64) *ResultSnapshots {
	snapshots := NewResultSnapshots(stores, settings, clock)
	snapshots.Transient = newMemoryTransient()
	return snapshots
}

func TestResultPaginationDetachesFullResponse(t *testing.T) {
	snapshots := newTestSnapshots(newTestStores(t), DefaultSnapshotSettings(), nil)
	results := make([]any, 0, 23)
	for index := 0; index < 23; index++ {
		results = append(results, map[string]any{
			"rank": index + 1, "text": "text " + itoa(index), "score": float64(index) / 100,
		})
	}
	payload := map[string]any{
		"results": results,
		"debug":   map[string]any{"retrieval": "hybrid", "timings": map[string]any{"rerank": 0.25}},
		"custom":  map[string]any{"nested": []any{1, map[string]any{"value": true}}},
	}
	saved, err := snapshots.Save("ns", payload, nil, nil)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	resources := payload["results"].([]any)
	resources[0].(map[string]any)["text"] = "mutated"
	saved["debug"].(map[string]any)["timings"].(map[string]any)["rerank"] = 99
	resultID := saved["result_id"].(string)
	page, err := snapshots.Get("ns", resultID, 2, 10)
	if err != nil {
		t.Fatalf("Get(page 2) error = %v", err)
	}
	pageResults := page["results"].([]any)
	if pageResults[0].(map[string]any)["text"] != "text 10" {
		t.Fatalf("page 2 first text = %v", pageResults[0].(map[string]any)["text"])
	}
	if page["total_results"] != 23 || page["next_page"] != 3 {
		t.Fatalf("page metadata = total %v next %v", page["total_results"], page["next_page"])
	}
	replay, err := snapshots.Get("ns", resultID, 1, 10)
	if err != nil {
		t.Fatalf("Get(replay) error = %v", err)
	}
	replayFirst := replay["results"].([]any)[0].(map[string]any)
	if replayFirst["text"] != "text 0" {
		t.Fatalf("replay first text = %v", replayFirst["text"])
	}
	debug := replay["debug"].(map[string]any)["timings"].(map[string]any)
	if debug["rerank"] != 0.25 {
		t.Fatalf("replay debug rerank = %v", debug["rerank"])
	}
	expires := replay["expires_at"].(float64)
	created := replay["created_at"].(float64)
	if expires-created != 3600 {
		t.Fatalf("expires-created = %v", expires-created)
	}
	if _, err := snapshots.Get("other", resultID, 1, 10); err == nil {
		t.Fatal("cross-namespace Get() succeeded")
	} else {
		mustBeError[*SnapshotUnavailable](t, err)
	}
}

// TestResultFactRowSurvivesRedisLoss asserts the MySQL fact row is the source
// of truth: losing the cache entry (or the whole cache) never loses a result,
// and the read path backfills the cache again.
func TestResultFactRowSurvivesRedisLoss(t *testing.T) {
	stores := newTestStores(t)
	snapshots := newTestSnapshots(stores, DefaultSnapshotSettings(), nil)
	saved, err := snapshots.Save("ns", map[string]any{
		"results": []any{map[string]any{"text": "durable"}},
	}, nil, nil)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	resultID := saved["result_id"].(string)
	// Clear only the Redis cache: the fact row recovers the result and repairs
	// the cache.
	if _, err := snapshots.Transient.Delete("search-result", resultID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	page, err := snapshots.Get("ns", resultID, 1, 10)
	if err != nil {
		t.Fatalf("Get(after cache clear) error = %v", err)
	}
	if page["results"].([]any)[0].(map[string]any)["text"] != "durable" {
		t.Fatalf("recovered page = %+v", page)
	}
	if !snapshots.Transient.(*memoryTransient).has("search-result", resultID) {
		t.Fatal("Get() did not backfill the Redis cache")
	}
	// A total cache outage still serves results from MySQL.
	snapshots.Transient = nil
	if page, err = snapshots.Get("ns", resultID, 1, 10); err != nil || page == nil {
		t.Fatalf("Get(cache disabled) = %+v, %v", page, err)
	}
	// Save also works without any cache: MySQL is authoritative.
	persisted, err := snapshots.Save("ns", map[string]any{"results": []any{}}, nil, nil)
	if err != nil {
		t.Fatalf("Save(cache disabled) error = %v", err)
	}
	second := persisted["result_id"].(string)
	if _, err := snapshots.Get("ns", second, 1, 10); err != nil {
		t.Fatalf("Get(second) error = %v", err)
	}
	// Only when both the cache and the fact row are gone is the result gone.
	if _, err := stores.DB.ExecContext(context.Background(),
		"DELETE FROM search_result_snapshots WHERE id = ?", resultID); err != nil {
		t.Fatalf("delete fact row error = %v", err)
	}
	if _, err := snapshots.Get("ns", resultID, 1, 10); err == nil {
		t.Fatal("deleted fact row was recovered")
	} else {
		mustBeError[*SnapshotUnavailable](t, err)
	}
}

func TestResultExpiryEviction(t *testing.T) {
	stores := newTestStores(t)
	now := 1000.0
	snapshots := newTestSnapshots(stores, DefaultSnapshotSettings(), func() float64 { return now })
	saved, err := snapshots.Save("ns", map[string]any{"results": []any{}}, nil, nil)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	resultID := saved["result_id"].(string)
	now = saved["expires_at"].(float64)
	_, err = snapshots.Get("ns", resultID, 1, 10)
	unavailable := mustBeError[*SnapshotUnavailable](t, err)
	if containsSubstring(unavailable.Message, "credential") {
		t.Fatalf("expiry message leaked details: %q", unavailable.Message)
	}
	// The fact row is expired too, so purging it is the final cleanup.
	removed, err := snapshots.PurgeExpired(100)
	if err != nil || removed != 1 {
		t.Fatalf("PurgeExpired() = %d, %v", removed, err)
	}
	if removed, err := snapshots.PurgeExpired(100); err != nil || removed != 0 {
		t.Fatalf("second PurgeExpired() = %d, %v", removed, err)
	}
	for _, limit := range []int{0, -1, 1001} {
		if _, err := snapshots.PurgeExpired(limit); err == nil {
			t.Fatalf("PurgeExpired(%d) succeeded", limit)
		} else {
			mustBeError[*ValueError](t, err)
		}
	}
}

func TestResultSizeLimitAndEmptyPage(t *testing.T) {
	snapshots := newTestSnapshots(newTestStores(t), DefaultSnapshotSettings(), nil)
	snapshots.Settings.MaxResultBytes = 10
	_, err := snapshots.Save("ns", map[string]any{"results": []any{map[string]any{"text": "too large"}}}, nil, nil)
	if unavailable := mustBeError[*SnapshotUnavailable](t, err); !containsSubstring(unavailable.Message, "size") {
		t.Fatalf("size-limit message = %q", unavailable.Message)
	}
	snapshots.Settings = DefaultSnapshotSettings()
	saved, err := snapshots.Save("ns", map[string]any{"results": []any{}}, nil, nil)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	resultID := saved["result_id"].(string)
	page, err := snapshots.Get("ns", resultID, 1, 10)
	if err != nil || page["next_page"] != nil {
		t.Fatalf("empty page = %+v, %v", page, err)
	}
	if _, err := snapshots.Get("ns", resultID, 2, 10); err == nil {
		t.Fatal("missing page 2 succeeded")
	} else {
		mustBeError[*ValueError](t, err)
	}
}

func TestResultTTLUsesResultSettings(t *testing.T) {
	stores := newTestStores(t)
	snapshots := newTestSnapshots(stores, SnapshotSettings{
		TTLSeconds: 19, MaxResultBytes: 16 * 1024 * 1024,
		MaxArtifactBytes: 64 * 1024 * 1024, MaxArtifactsPerSearch: 100,
	}, nil)
	saved, err := snapshots.Save("ns", map[string]any{"results": []any{}}, nil, nil)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	resultID := saved["result_id"].(string)
	transient := snapshots.Transient.(*memoryTransient)
	transient.mu.Lock()
	ttl := transient.lastTTL[transientKey("search-result", resultID)]
	transient.mu.Unlock()
	if ttl != 19 {
		t.Fatalf("stored TTL = %v, want 19", ttl)
	}
	if saved["expires_at"].(float64)-saved["created_at"].(float64) != 19 {
		t.Fatalf("expires-created = %v", saved["expires_at"].(float64)-saved["created_at"].(float64))
	}
}

// TestCorruptCacheFallsBackToFactRow asserts a poisoned or corrupt cache entry
// cannot hide (or replace) the authoritative MySQL result.
func TestCorruptCacheFallsBackToFactRow(t *testing.T) {
	snapshots := newTestSnapshots(newTestStores(t), DefaultSnapshotSettings(), nil)
	saved, err := snapshots.Save("ns", map[string]any{"results": []any{map[string]any{"text": "original"}}}, nil, nil)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	resultID := saved["result_id"].(string)
	corruptValues := []any{
		[]byte("not json"),
		[]byte{0xff},
		[]any{},
		nil,
		map[string]any{},
		map[string]any{"namespace": "ns", "expires_at": 99999999999.0, "payload": nil},
		[]byte(`{"namespace":"ns","expires_at":NaN,"payload":{}}`),
	}
	for index, corrupt := range corruptValues {
		if _, err := snapshots.Transient.Put("search-result", resultID, corrupt, 3600, false); err != nil {
			t.Fatalf("corrupt Put(%d) error = %v", index, err)
		}
		page, err := snapshots.Get("ns", resultID, 1, 10)
		if err != nil {
			t.Fatalf("corrupt snapshot %d was not recovered: %v", index, err)
		}
		if page["results"].([]any)[0].(map[string]any)["text"] != "original" {
			t.Fatalf("corrupt snapshot %d returned %+v", index, page)
		}
	}
}

func TestResultUUIDIsGloballyImmutableAcrossTenants(t *testing.T) {
	snapshots := newTestSnapshots(newTestStores(t), DefaultSnapshotSettings(), nil)
	saved, err := snapshots.Save("ns", map[string]any{"results": []any{map[string]any{"text": "original"}}}, nil, nil)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	resultID := saved["result_id"].(string)
	for _, namespace := range []string{"ns", "other"} {
		_, err := snapshots.Save(namespace, map[string]any{"results": []any{}}, &resultID, nil)
		if unavailable := mustBeError[*SnapshotUnavailable](t, err); !containsSubstring(unavailable.Message, "already exists") {
			t.Fatalf("duplicate message = %q", unavailable.Message)
		}
	}
	replay, err := snapshots.Get("ns", resultID, 1, 10)
	if err != nil || len(replay["results"].([]any)) != 1 {
		t.Fatalf("original snapshot was replaced: %+v, %v", replay, err)
	}
}

func TestResultWrongTenantOrIdentityIsUnavailable(t *testing.T) {
	for _, field := range []string{"namespace", "result_id"} {
		t.Run(field, func(t *testing.T) {
			snapshots := newTestSnapshots(newTestStores(t), DefaultSnapshotSettings(), nil)
			saved, err := snapshots.Save("ns", map[string]any{"results": []any{}}, nil, nil)
			if err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			resultID := saved["result_id"].(string)
			value, found, err := snapshots.Transient.Get("search-result", resultID)
			if err != nil || !found {
				t.Fatalf("Transient.Get() = %v, %v, %v", value, found, err)
			}
			envelope := value.(map[string]any)
			if field == "namespace" {
				envelope["namespace"] = "other"
			} else {
				payload := envelope["payload"].(map[string]any)
				payload["result_id"] = otherUUID(t)
			}
			if _, err := snapshots.Transient.Put("search-result", resultID, envelope, 3600, false); err != nil {
				t.Fatalf("Put() error = %v", err)
			}
			// A poisoned cache entry is discarded in favour of the fact row: the
			// tenant/identity checks still run on the recovered envelope.
			if _, err := snapshots.Get("ns", resultID, 1, 10); err != nil {
				t.Fatalf("Get() after cache poison = %v", err)
			}
			if _, err := snapshots.Get("other", resultID, 1, 10); err == nil {
				t.Fatal("cross-namespace Get() succeeded")
			} else {
				mustBeError[*SnapshotUnavailable](t, err)
			}
		})
	}
}

func TestResultInvalidPagingAndIdentity(t *testing.T) {
	snapshots := newTestSnapshots(newTestStores(t), DefaultSnapshotSettings(), nil)
	saved, err := snapshots.Save("ns", map[string]any{"results": []any{}}, nil, nil)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	resultID := saved["result_id"].(string)
	for _, paging := range []struct {
		page, size int
	}{{0, 10}, {1, 0}, {1, 101}} {
		if _, err := snapshots.Get("ns", resultID, paging.page, paging.size); err == nil {
			t.Fatalf("Get(%d, %d) succeeded", paging.page, paging.size)
		} else {
			mustBeError[*ValueError](t, err)
		}
	}
	for _, invalid := range []string{"", "not-a-uuid", "ABCDEFAB-1234-1234-1234-123456789ABC"} {
		if _, err := snapshots.Get("ns", invalid, 1, 10); err == nil {
			t.Fatalf("Get(%q) succeeded", invalid)
		} else {
			mustBeError[*SnapshotUnavailable](t, err)
		}
	}
}

func TestResultSaveTxCommitsWithCallerTransaction(t *testing.T) {
	stores := newTestStores(t)
	snapshots := newTestSnapshots(stores, DefaultSnapshotSettings(), nil)
	ctx := context.Background()
	// A rolled-back caller transaction leaves neither a fact row nor a cache
	// entry behind.
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	saved, err := snapshots.SaveTx(ctx, transaction, "ns",
		map[string]any{"results": []any{map[string]any{"text": "rolled back"}}}, nil, nil)
	if err != nil {
		t.Fatalf("SaveTx() error = %v", err)
	}
	resultID := saved["result_id"].(string)
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if _, err := snapshots.Get("ns", resultID, 1, 10); err == nil {
		t.Fatal("rolled-back SaveTx() was readable")
	}
	// A committed caller transaction is durable, and the cache is not touched
	// inside the transaction.
	transaction, err = stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	saved, err = snapshots.SaveTx(ctx, transaction, "ns",
		map[string]any{"results": []any{map[string]any{"text": "committed"}}}, nil, nil)
	if err != nil {
		t.Fatalf("SaveTx(commit) error = %v", err)
	}
	committedID := saved["result_id"].(string)
	if snapshots.Transient.(*memoryTransient).has("search-result", committedID) {
		t.Fatal("SaveTx() wrote the cache before the caller committed")
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	page, err := snapshots.Get("ns", committedID, 1, 10)
	if err != nil || page["results"].([]any)[0].(map[string]any)["text"] != "committed" {
		t.Fatalf("Get(committed) = %+v, %v", page, err)
	}
}

func TestResultPrepareValidation(t *testing.T) {
	snapshots := newTestSnapshots(newTestStores(t), DefaultSnapshotSettings(), nil)
	invalidUUID := "not-a-uuid"
	for name, payload := range map[string]map[string]any{
		"invalid namespace": {},
		"results not list":  {"results": "not-a-list"},
	} {
		namespace := "ns"
		if name == "invalid namespace" {
			namespace = ""
		}
		if _, err := snapshots.Prepare(namespace, payload, nil, nil); err == nil {
			t.Fatalf("%s: Prepare() succeeded", name)
		} else {
			mustBeError[*ValueError](t, err)
		}
	}
	if _, err := snapshots.Prepare("ns", map[string]any{"results": []any{}}, &invalidUUID, nil); err == nil {
		t.Fatal("Prepare() accepted a noncanonical result id")
	}
	nan := math.NaN()
	if _, err := snapshots.Prepare("ns", map[string]any{"results": []any{}}, nil, &nan); err == nil {
		t.Fatal("Prepare() accepted a NaN creation time")
	}
	expired := float64(0)
	if _, err := snapshots.Prepare("ns", map[string]any{"results": []any{}}, nil, &expired); err == nil {
		t.Fatal("Prepare() accepted an already-expired result")
	} else if unavailable := mustBeError[*SnapshotUnavailable](t, err); !containsSubstring(unavailable.Message, "expired") {
		t.Fatalf("expired message = %q", unavailable.Message)
	}
}

func TestResultConcurrentSameUUIDPublishesOnce(t *testing.T) {
	snapshots := newTestSnapshots(newTestStores(t), DefaultSnapshotSettings(), nil)
	resultID := otherUUID(t)
	type outcome struct {
		saved map[string]any
		err   error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for _, text := range []string{"first", "second"} {
		go func(text string) {
			<-start
			saved, err := snapshots.Save("ns", map[string]any{
				"results": []any{map[string]any{"text": text}},
			}, &resultID, nil)
			results <- outcome{saved: saved, err: err}
		}(text)
	}
	close(start)
	successes := make([]map[string]any, 0, 1)
	for index := 0; index < 2; index++ {
		result := <-results
		if result.err == nil {
			successes = append(successes, result.saved)
			continue
		}
		var unavailable *SnapshotUnavailable
		if !errors.As(result.err, &unavailable) {
			t.Fatalf("concurrent Save() error = %v", result.err)
		}
	}
	if len(successes) != 1 {
		t.Fatalf("concurrent Save() successes = %d, want 1", len(successes))
	}
	replay, err := snapshots.Get("ns", resultID, 1, 10)
	if err != nil || replay["results"].([]any)[0].(map[string]any)["text"] != successes[0]["results"].([]any)[0].(map[string]any)["text"] {
		t.Fatalf("replay after concurrent save = %+v, %v", replay, err)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func otherUUID(t *testing.T) string {
	t.Helper()
	value, err := uuid4String()
	if err != nil {
		t.Fatalf("uuid4String() error = %v", err)
	}
	return value
}
