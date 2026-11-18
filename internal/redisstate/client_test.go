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

	"github.com/Guan0923/Praxis-RAG/internal/state"
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

// lockManuals prepares a live lease for the document-list cache tests.
func lockManuals(t *testing.T, backend *Client, ctx context.Context) string {
	t.Helper()
	const token = "manuals-lease"
	if held, err := backend.AcquireBackfillLock(ctx, "documents", "manuals", token, time.Second); err != nil || !held {
		t.Fatalf("AcquireBackfillLock = (%v, %v), want (true, nil)", held, err)
	}
	return token
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
	stored, err := backend.Put(context.Background(), "documents", "manuals", "disabled", 1, map[string]any{"a": 1}, false)
	if err != nil || stored {
		t.Fatalf("Put = (%v, %v), want (false, nil)", stored, err)
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

	token := lockManuals(t, backend, ctx)
	stored, err := backend.Put(ctx, "documents", "manuals", token, 7, value, false)
	if err != nil || !stored {
		t.Fatalf("Put = (%v, %v)", stored, err)
	}
	if err := backend.ReleaseBackfillLock(ctx, "documents", "manuals", token); err != nil {
		t.Fatalf("ReleaseBackfillLock: %v", err)
	}
	if keys := server.Keys(); len(keys) != 2 {
		t.Fatalf("redis keys = %v", keys)
	}
	// The payload TTL is randomized per write in [48, 60] seconds by default.
	if ttl := server.TTL(payloadKey); ttl < 48*time.Second || ttl > 60*time.Second {
		t.Fatalf("payload TTL = %v, want within 48~60s", ttl)
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
	token := lockManuals(t, backend, ctx)
	if stored, err := backend.Put(ctx, "documents", "manuals", token, 5, map[string]any{"v": "five"}, false); err != nil || !stored {
		t.Fatalf("initial Put = (%v, %v)", stored, err)
	}
	// A newer revision wins.
	if stored, err := backend.Put(ctx, "documents", "manuals", token, 6, map[string]any{"v": "six"}, false); err != nil || !stored {
		t.Fatalf("newer Put = (%v, %v)", stored, err)
	}
	// An older revision is ignored.
	if stored, err := backend.Put(ctx, "documents", "manuals", token, 5, map[string]any{"v": "stale"}, false); err != nil || stored {
		t.Fatalf("older Put = (%v, %v), want (false, nil)", stored, err)
	}
	// Equal revision refills a payload and may publish a deletion.
	if stored, err := backend.Put(ctx, "documents", "manuals", token, 6, map[string]any{"v": "refill"}, false); err != nil || !stored {
		t.Fatalf("equal revision Put = (%v, %v)", stored, err)
	}
	if stored, err := backend.Put(ctx, "documents", "manuals", token, 6, nil, true); err != nil || !stored {
		t.Fatalf("deletion Put = (%v, %v)", stored, err)
	}
	hit, value, err := backend.Get(ctx, "documents", "manuals")
	if err != nil || !hit || value != nil {
		t.Fatalf("deleted Get = (%v, %#v, %v)", hit, value, err)
	}
	// Equal revision cannot undo a deletion.
	if stored, err := backend.Put(ctx, "documents", "manuals", token, 6, map[string]any{"v": "resurrected"}, false); err != nil || stored {
		t.Fatalf("resurrection Put = (%v, %v), want (false, nil)", stored, err)
	}
	// An expired payload is a miss even though the watermark persists.
	server.FastForward(61 * time.Second)
	hit, _, err = backend.Get(ctx, "documents", "manuals")
	if err != nil || hit {
		t.Fatalf("expired Get = (%v, %v), want miss", hit, err)
	}
}

func TestCacheRevisionBeyondLuaIntegerPrecision(t *testing.T) {
	backend, _ := newTestBackend(t)
	ctx := context.Background()
	const older int64 = 1 << 53
	token := lockManuals(t, backend, ctx)
	if stored, err := backend.Put(ctx, "documents", "manuals", token, older+1, "new", false); err != nil || !stored {
		t.Fatalf("newer Put = (%v, %v)", stored, err)
	}
	if stored, err := backend.Put(ctx, "documents", "manuals", token, older, "old", false); err != nil || stored {
		t.Fatalf("older Put = (%v, %v), want (false, nil)", stored, err)
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
