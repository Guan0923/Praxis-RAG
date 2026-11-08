package redisstate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// newTestBackend starts an in-process Redis and returns an enabled backend that
// injects it. The backend is always closed before the server.
func newTestBackend(t *testing.T) (*Client, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	settings := state.DefaultCacheSettings()
	settings.URL = "redis://" + server.Addr() + "/0"
	backend, err := New(settings, WithRedisClient(client))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !backend.Enabled() {
		t.Fatalf("backend must be enabled")
	}
	t.Cleanup(func() { _ = backend.Close() })
	return backend, server
}

func TestDisabledModeIsANoop(t *testing.T) {
	backend := NewDisabled()
	if backend.Enabled() {
		t.Fatalf("disabled backend reports enabled")
	}
	hit, value, err := backend.Get(context.Background(), "documents", "manuals")
	if err != nil || hit || value != nil {
		t.Fatalf("Get = (%v, %v, %v), want a miss", hit, value, err)
	}
	stored, err := backend.Put(context.Background(), "documents", "manuals", 1, map[string]any{"a": 1}, false)
	if err != nil || stored {
		t.Fatalf("Put = (%v, %v), want (false, nil)", stored, err)
	}
	transient := backend.TransientStore()
	if _, _, err := transient.Get("search-result", "id"); err == nil {
		t.Fatalf("disabled transient Get must fail")
	} else {
		var unavailable *TransientUnavailable
		if !errors.As(err, &unavailable) || unavailable.Message != "Redis is required for queued operations" {
			t.Fatalf("disabled transient error = %v", err)
		}
	}
	if _, err := transient.Put("search-result", "id", map[string]any{"a": 1}, 10, true); err == nil {
		t.Fatalf("disabled transient Put must fail")
	}
}

func TestNewFromEnvHonoursDisabledSwitch(t *testing.T) {
	t.Setenv("RAG_REDIS_ENABLED", "false")
	t.Setenv("RAG_REDIS_URL", "redis://127.0.0.1:1/0")
	backend, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	defer backend.Close()
	if backend.Enabled() {
		t.Fatalf("RAG_REDIS_ENABLED=false must disable the cache even with a URL")
	}
	if _, _, err := backend.Get(context.Background(), "documents", "manuals"); err != nil {
		t.Fatalf("disabled Get must not error: %v", err)
	}
}

func TestKeyPairAndCacheRoundTrip(t *testing.T) {
	backend, server := newTestBackend(t)
	ctx := context.Background()
	value := map[string]any{"name": "manual.txt", "size": float64(12)}

	payloadKey, revisionKey := backend.PayloadKey("documents", "manuals"), backend.RevisionKey("documents", "manuals")
	if !strings.HasPrefix(payloadKey, "rag:state:v1:{") || !strings.HasSuffix(payloadKey, "}:payload") {
		t.Fatalf("payload key = %q", payloadKey)
	}
	if !strings.HasSuffix(revisionKey, "}:revision") {
		t.Fatalf("revision key = %q", revisionKey)
	}
	if strings.SplitN(payloadKey, ":", 4)[2] != strings.SplitN(revisionKey, ":", 4)[2] {
		t.Fatalf("payload and revision must share one hash slot")
	}

	stored, err := backend.Put(ctx, "documents", "manuals", 7, value, false)
	if err != nil || !stored {
		t.Fatalf("Put = (%v, %v)", stored, err)
	}
	if keys := server.Keys(); len(keys) != 2 {
		t.Fatalf("redis keys = %v", keys)
	}
	if ttl := server.TTL(payloadKey); ttl != 60*time.Second {
		t.Fatalf("payload TTL = %v, want 60s", ttl)
	}
	if ttl := server.TTL(revisionKey); ttl != 0 {
		t.Fatalf("watermark must not expire, TTL = %v", ttl)
	}
	raw, err := server.Get(payloadKey)
	if err != nil {
		t.Fatalf("raw payload: %v", err)
	}
	if !strings.Contains(raw, `"schema":1`) || !strings.Contains(raw, `"revision":"7"`) {
		t.Fatalf("payload envelope = %s", raw)
	}

	hit, cached, err := backend.Get(ctx, "documents", "manuals")
	if err != nil || !hit {
		t.Fatalf("Get = (%v, %v, %v)", hit, cached, err)
	}
	decoded, ok := cached.(map[string]any)
	if !ok || decoded["name"] != "manual.txt" {
		t.Fatalf("cached value = %#v", cached)
	}
	if number, ok := decoded["size"].(json.Number); !ok || number.String() != "12" {
		t.Fatalf("cached number = %#v", decoded["size"])
	}
}

func TestCacheRevisionWatermarkRules(t *testing.T) {
	backend, server := newTestBackend(t)
	ctx := context.Background()
	if stored, err := backend.Put(ctx, "documents", "manuals", 5, map[string]any{"v": "five"}, false); err != nil || !stored {
		t.Fatalf("initial Put = (%v, %v)", stored, err)
	}
	// A newer revision wins.
	if stored, err := backend.Put(ctx, "documents", "manuals", 6, map[string]any{"v": "six"}, false); err != nil || !stored {
		t.Fatalf("newer Put = (%v, %v)", stored, err)
	}
	// An older revision is ignored.
	if stored, err := backend.Put(ctx, "documents", "manuals", 5, map[string]any{"v": "stale"}, false); err != nil || stored {
		t.Fatalf("older Put = (%v, %v), want (false, nil)", stored, err)
	}
	// Equal revision refills a payload and may publish a deletion.
	if stored, err := backend.Put(ctx, "documents", "manuals", 6, map[string]any{"v": "refill"}, false); err != nil || !stored {
		t.Fatalf("equal revision Put = (%v, %v)", stored, err)
	}
	if stored, err := backend.Put(ctx, "documents", "manuals", 6, nil, true); err != nil || !stored {
		t.Fatalf("deletion Put = (%v, %v)", stored, err)
	}
	hit, value, err := backend.Get(ctx, "documents", "manuals")
	if err != nil || !hit || value != nil {
		t.Fatalf("deleted Get = (%v, %#v, %v)", hit, value, err)
	}
	// Equal revision cannot undo a deletion.
	if stored, err := backend.Put(ctx, "documents", "manuals", 6, map[string]any{"v": "resurrected"}, false); err != nil || stored {
		t.Fatalf("resurrection Put = (%v, %v), want (false, nil)", stored, err)
	}
	// An expired payload is a miss even though the watermark persists.
	server.FastForward(61 * time.Second)
	hit, _, err = backend.Get(ctx, "documents", "manuals")
	if err != nil || hit {
		t.Fatalf("expired Get = (%v, %v), want miss", hit, err)
	}
}

func TestCacheCorruptionAndMiss(t *testing.T) {
	backend, server := newTestBackend(t)
	ctx := context.Background()
	payloadKey := backend.PayloadKey("documents", "manuals")
	revisionKey := backend.RevisionKey("documents", "manuals")
	server.Set(payloadKey, "not json")
	server.Set(revisionKey, "1:0")
	if _, _, err := backend.Get(ctx, "documents", "manuals"); err == nil {
		t.Fatalf("corrupt envelope must be CacheUnavailable")
	} else {
		var unavailable *state.CacheUnavailable
		if !errors.As(err, &unavailable) || unavailable.Message != "State cache read unavailable" {
			t.Fatalf("corruption error = %v", err)
		}
	}

	// Watermark mismatch is a miss, not an error.
	server.Set(payloadKey, `{"schema":1,"revision":"2","deleted":false,"value":{"a":1}}`)
	server.Set(revisionKey, "1:0")
	if hit, _, err := backend.Get(ctx, "documents", "manuals"); err != nil || hit {
		t.Fatalf("mismatched watermark = (%v, %v), want miss", hit, err)
	}

	// A remaining legacy watermark with a missing payload is a miss.
	server.Del(payloadKey)
	if hit, _, err := backend.Get(ctx, "documents", "manuals"); err != nil || hit {
		t.Fatalf("missing payload = (%v, %v), want miss", hit, err)
	}
}

func TestCacheInvalidPutRevisionsAndClosedClient(t *testing.T) {
	backend, _ := newTestBackend(t)
	ctx := context.Background()
	if _, err := backend.Put(ctx, "documents", "manuals", -1, nil, false); err == nil {
		t.Fatalf("negative revision must fail")
	} else {
		var valueError *state.ValueError
		if !errors.As(err, &valueError) || valueError.Message != "Cache revision must be a nonnegative integer" {
			t.Fatalf("revision error = %v", err)
		}
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, _, err := backend.Get(ctx, "documents", "manuals"); err == nil {
		t.Fatalf("Get after Close must fail")
	} else {
		var unavailable *state.CacheUnavailable
		if !errors.As(err, &unavailable) || unavailable.Message != "State cache is closed" {
			t.Fatalf("closed error = %v", err)
		}
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("second close must be a no-op: %v", err)
	}
}

func TestUnavailableRedisIsMapped(t *testing.T) {
	settings := state.DefaultCacheSettings()
	settings.URL = "redis://127.0.0.1:1/0"
	settings.TimeoutSeconds = 0.05
	backend, err := New(settings)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer backend.Close()
	if _, _, err := backend.Get(context.Background(), "documents", "manuals"); err == nil {
		t.Fatalf("unreachable redis must be CacheUnavailable")
	} else {
		var unavailable *state.CacheUnavailable
		if !errors.As(err, &unavailable) || unavailable.Message != "State cache read unavailable" {
			t.Fatalf("unreachable error = %v", err)
		}
	}
	transient := backend.TransientStore()
	if _, _, err := transient.Get("search-result", "id"); err == nil {
		t.Fatalf("unreachable transient must fail")
	} else {
		var unavailable *TransientUnavailable
		if !errors.As(err, &unavailable) || unavailable.Message != "Redis task read unavailable" {
			t.Fatalf("unreachable transient error = %v", err)
		}
	}
}

func TestTransientRoundTripTTLAndNX(t *testing.T) {
	backend, server := newTestBackend(t)
	transient := backend.Transient()
	key := transient.Key("search-result", "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a")
	if !strings.Contains(key, ":transient:{") {
		t.Fatalf("transient key = %q", key)
	}
	value := map[string]any{"result_id": "abc", "status": "queued", "stage": "queued"}
	stored, err := transient.Put("search-result", "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a", value, 1.2, true)
	if err != nil || !stored {
		t.Fatalf("Put = (%v, %v)", stored, err)
	}
	if ttl := server.TTL(key); ttl != 2*time.Second {
		t.Fatalf("ceil TTL = %v, want 2s", ttl)
	}
	// NX refuses to replace.
	if stored, err := transient.Put("search-result", "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a", map[string]any{"status": "other"}, 10, true); err != nil || stored {
		t.Fatalf("NX Put = (%v, %v), want (false, nil)", stored, err)
	}
	current, found, err := transient.Get("search-result", "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a")
	if err != nil || !found {
		t.Fatalf("Get = (%v, %v)", found, err)
	}
	if current.(map[string]any)["status"] != "queued" {
		t.Fatalf("transient value = %#v", current)
	}
	// A non-positive TTL selects the default snapshot TTL.
	if stored, err := transient.Put("search-result", "other", map[string]any{"a": 1}, 0, false); err != nil || !stored {
		t.Fatalf("default TTL Put = (%v, %v)", stored, err)
	}
	if ttl := server.TTL(transient.Key("search-result", "other")); ttl != 3600*time.Second {
		t.Fatalf("default TTL = %v, want 3600s", ttl)
	}
	deleted, err := transient.Delete("search-result", "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a")
	if err != nil || deleted != 1 {
		t.Fatalf("Delete = (%d, %v)", deleted, err)
	}
	if _, found, err := transient.Get("search-result", "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a"); err != nil || found {
		t.Fatalf("Get after delete = (%v, %v)", found, err)
	}
	if _, err := transient.Put("search-result", "bad", "not-an-object", 10, false); err == nil {
		t.Fatalf("non-object transient state must fail")
	}
}

func TestTransientUpdateAndLock(t *testing.T) {
	backend, _ := newTestBackend(t)
	transient := backend.Transient()
	ctx := context.Background()
	_ = ctx
	identity := "lock-target"
	if _, err := transient.Update("documents", identity, func(current map[string]any) (map[string]any, error) {
		if current != nil {
			t.Fatalf("missing entry must be nil")
		}
		return map[string]any{"count": 1}, nil
	}, nil); err != nil {
		t.Fatalf("Update create: %v", err)
	}
	updated, err := transient.Update("documents", identity, func(current map[string]any) (map[string]any, error) {
		count, _ := current["count"].(json.Number).Int64()
		current["count"] = count + 1
		return current, nil
	}, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if count, _ := updated["count"].(int64); count != 2 {
		t.Fatalf("updated = %#v", updated)
	}
	// nil aborts the update without writing.
	unchanged, err := transient.Update("documents", identity, func(map[string]any) (map[string]any, error) {
		return nil, nil
	}, nil)
	if err != nil || unchanged != nil {
		t.Fatalf("nil update = (%#v, %v)", unchanged, err)
	}
	callbackFailure := errors.New("callback failed")
	if _, err := transient.Update("documents", identity, func(map[string]any) (map[string]any, error) {
		return nil, callbackFailure
	}, nil); !errors.Is(err, callbackFailure) {
		t.Fatalf("callback error not preserved: %v", err)
	}

	token, acquired, err := transient.Lock("reindex", identity, 30)
	if err != nil || !acquired || token == "" {
		t.Fatalf("Lock = (%q, %v, %v)", token, acquired, err)
	}
	if _, acquired, err := transient.Lock("reindex", identity, 30); err != nil || acquired {
		t.Fatalf("second Lock = (%v, %v), want not acquired", acquired, err)
	}
	if released, err := transient.Unlock("reindex", identity, "wrong-token"); err != nil || released {
		t.Fatalf("wrong-token Unlock = (%v, %v)", released, err)
	}
	if released, err := transient.Unlock("reindex", identity, token); err != nil || !released {
		t.Fatalf("Unlock = (%v, %v)", released, err)
	}
	if _, acquired, err := transient.Lock("reindex", identity, 30); err != nil || !acquired {
		t.Fatalf("Lock after unlock = (%v, %v)", acquired, err)
	}
}

func TestRedisURLParsing(t *testing.T) {
	options, err := redisOptionsFromURL("redis://:secret@example.com:6380/2", 0.3)
	if err != nil {
		t.Fatalf("redisOptionsFromURL: %v", err)
	}
	if options.Addr != "example.com:6380" || options.Password != "secret" || options.DB != 2 {
		t.Fatalf("options = %+v", options)
	}
	if options.DialTimeout != 300*time.Millisecond || options.ReadTimeout != 300*time.Millisecond ||
		options.WriteTimeout != 300*time.Millisecond {
		t.Fatalf("timeouts = %v %v %v", options.DialTimeout, options.ReadTimeout, options.WriteTimeout)
	}
	if _, err := redisOptionsFromURL("not-a-url", 0.3); err == nil {
		t.Fatalf("invalid URL must fail")
	}
	if _, err := redisOptionsFromURL("redis://host:1/0", 0.3); err != nil {
		t.Fatalf("plain URL must parse: %v", err)
	}
}

func TestNewValidatesSettings(t *testing.T) {
	settings := state.DefaultCacheSettings()
	settings.TTLSeconds = 0
	if _, err := New(settings); err == nil {
		t.Fatalf("invalid TTL must be rejected")
	}
	settings = state.DefaultCacheSettings()
	settings.Prefix = ""
	if _, err := New(settings); err == nil {
		t.Fatalf("empty prefix must be rejected")
	}
}

func TestEnabledWithoutURLIsCacheUnavailable(t *testing.T) {
	settings := state.DefaultCacheSettings()
	enabled := true
	settings.Enabled = &enabled
	settings.URL = ""
	backend, err := New(settings)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer backend.Close()
	if _, _, err := backend.Get(context.Background(), "documents", "manuals"); err == nil {
		t.Fatalf("missing URL must be CacheUnavailable")
	} else {
		var unavailable *state.CacheUnavailable
		if !errors.As(err, &unavailable) || unavailable.Message != "State cache connection unavailable" {
			t.Fatalf("missing URL error = %v", err)
		}
	}
}

func TestConcurrentCacheAndTransientAccess(t *testing.T) {
	backend, _ := newTestBackend(t)
	ctx := context.Background()
	transient := backend.Transient()
	done := make(chan struct{})
	for worker := 0; worker < 8; worker++ {
		go func(worker int) {
			defer func() { done <- struct{}{} }()
			for iteration := 0; iteration < 20; iteration++ {
				_, _, _ = backend.Get(ctx, "documents", "manuals")
				_, _ = backend.Put(ctx, "documents", "manuals", int64(iteration), map[string]any{"worker": worker}, false)
				_, _ = transient.Put("search-result", "id", map[string]any{"iteration": iteration}, 10, false)
				_, _, _ = transient.Get("search-result", "id")
			}
		}(worker)
	}
	for worker := 0; worker < 8; worker++ {
		<-done
	}
	if hit, _, err := backend.Get(ctx, "documents", "manuals"); err != nil || !hit {
		t.Fatalf("final cache read = (%v, %v)", hit, err)
	}
}

func TestAttachWiresStateStores(t *testing.T) {
	backend, _ := newTestBackend(t)
	stores := &state.Stores{
		CacheSync: &state.CacheSynchronizer{},
		Results:   &state.ResultSnapshots{},
	}
	if err := Attach(stores, backend); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if stores.CacheSync.Backend != state.StateCache(backend) {
		t.Fatalf("cache backend not attached")
	}
	if stores.Results.Transient == nil {
		t.Fatalf("transient store not attached")
	}
	if err := Attach(nil, backend); err == nil {
		t.Fatalf("Attach with nil stores must fail")
	}
	if err := Attach(stores, nil); err == nil {
		t.Fatalf("Attach with nil client must fail")
	}
}

func TestResultSnapshotsWithRedisTransient(t *testing.T) {
	backend, server := newTestBackend(t)
	settings := state.DefaultSnapshotSettings()
	settings.TTLSeconds = 3600
	snapshots := &state.ResultSnapshots{Settings: settings, Transient: backend.TransientStore()}

	results := make([]any, 0, 25)
	for index := 0; index < 25; index++ {
		results = append(results, map[string]any{"rank": index + 1, "text": "evidence"})
	}
	payload := map[string]any{"query": "manual", "results": results}
	saved, err := snapshots.Save("manuals", payload, nil, nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	resultID, _ := saved["result_id"].(string)
	if resultID == "" || saved["snapshot_available"] != true {
		t.Fatalf("saved = %#v", saved)
	}
	if ttl := server.TTL(backend.Transient().Key("search-result", resultID)); ttl < 3598*time.Second || ttl > 3600*time.Second {
		t.Fatalf("snapshot TTL = %v", ttl)
	}

	page, err := snapshots.Get("manuals", resultID, 1, 10)
	if err != nil {
		t.Fatalf("Get page 1: %v", err)
	}
	if len(page["results"].([]any)) != 10 || page["total_results"] != 25 || page["next_page"] != 2 {
		t.Fatalf("page 1 = %#v", page)
	}
	lastPage, err := snapshots.Get("manuals", resultID, 3, 10)
	if err != nil {
		t.Fatalf("Get page 3: %v", err)
	}
	if len(lastPage["results"].([]any)) != 5 || lastPage["next_page"] != nil {
		t.Fatalf("page 3 = %#v", lastPage)
	}
	if _, err := snapshots.Get("manuals", resultID, 4, 10); err == nil {
		t.Fatalf("out-of-range page must fail")
	}
	if _, err := snapshots.Get("other", resultID, 1, 10); err == nil {
		t.Fatalf("foreign namespace read must fail")
	}
	if _, err := snapshots.Get("manuals", "not-a-uuid", 1, 10); err == nil {
		t.Fatalf("invalid result id must fail")
	}

	// A missing transient store degrades writes exactly like Redis being absent.
	snapshots.Transient = nil
	if _, err := snapshots.Save("manuals", payload, nil, nil); err == nil {
		t.Fatalf("nil transient Save must fail")
	}
}
