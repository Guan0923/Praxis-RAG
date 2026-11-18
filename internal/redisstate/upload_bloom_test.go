package redisstate

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// newUploadBloomTestBackend starts an in-process Redis and returns the bitmap
// backend plus the raw client (for direct key assertions).
func newUploadBloomTestBackend(t *testing.T) (*UploadBloomBackend, *redis.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	settings := state.DefaultUploadBloomSettings()
	settings.Mode = state.UploadBloomModeOn
	settings.URL = "redis://" + server.Addr() + "/0"
	settings.Shards = 4
	settings.MinCapacity = 100
	backend, err := NewUploadBloomBackend(settings, WithRedisClient(client))
	if err != nil {
		t.Fatalf("NewUploadBloomBackend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return backend, client
}

// TestUploadBloomBackendRoundTrip drives the full lifecycle against a real
// in-process Redis: build lease acquisition, bit registration, probe, atomic
// activation, kill switch, and generation expiry.
func TestUploadBloomBackendRoundTrip(t *testing.T) {
	backend, client := newUploadBloomTestBackend(t)
	ctx := context.Background()

	// Initial state: nothing deployed.
	backendState, err := backend.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if backendState.Active != nil || backendState.Building != nil || backendState.Disabled {
		t.Fatalf("initial state = %+v, want empty", backendState)
	}

	// A probe against an undeployed generation falls back (no active meta).
	params := state.UploadBloomParameters{Bits: 1600, Shards: 4, Hashes: 7, Capacity: 100}
	generation := state.UploadBloomGeneration{Generation: 1, Params: params}

	// Build lease: acquired, then a second acquisition loses.
	acquired, ok, err := backend.AcquireBuild(ctx, 1, params, time.Minute)
	if err != nil || !ok {
		t.Fatalf("AcquireBuild = (%+v, %v, %v)", acquired, ok, err)
	}
	_, ok, err = backend.AcquireBuild(ctx, 1, params, time.Minute)
	if err != nil || ok {
		t.Fatalf("second AcquireBuild must lose the lease: (%v, %v)", ok, err)
	}

	// Register an ID and probe it.
	id := fmt.Sprintf("%032x", 7)
	if err := backend.SetBits(ctx, generation, []string{id}); err != nil {
		t.Fatalf("SetBits: %v", err)
	}
	present, shardMissing, err := backend.Probe(ctx, generation, id)
	if err != nil || shardMissing || !present {
		t.Fatalf("Probe(registered) = (%v, %v, %v)", present, shardMissing, err)
	}
	present, shardMissing, err = backend.Probe(ctx, generation, fmt.Sprintf("%032x", 999))
	if err != nil || shardMissing || present {
		t.Fatalf("Probe(unregistered) = (%v, %v, %v)", present, shardMissing, err)
	}

	// Activation publishes meta atomically and clears the kill switch.
	if _, err := backend.Disable(ctx); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := backend.Activate(ctx, generation); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := backend.ReleaseBuild(ctx, 1); err != nil {
		t.Fatalf("ReleaseBuild: %v", err)
	}
	backendState, err = backend.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if backendState.Active == nil || backendState.Active.Generation != 1 {
		t.Fatalf("active = %+v after Activate", backendState.Active)
	}
	if backendState.Disabled {
		t.Fatal("Activate must clear the kill switch")
	}
	if backendState.Building != nil {
		t.Fatalf("ReleaseBuild must drop the building payload: %+v", backendState.Building)
	}

	// Shard loss: deleting one shard key makes probes report shardMissing.
	shardKey := backend.shardKey(1, int(0))
	if err := client.Del(ctx, shardKey).Err(); err != nil {
		t.Fatalf("deleting shard 0: %v", err)
	}
	_, shardMissing, err = backend.Probe(ctx, generation, id)
	if err != nil || !shardMissing {
		t.Fatalf("a lost shard must report shardMissing: (%v, %v)", shardMissing, err)
	}

	// Disable is verifiable: the kill switch is visible through State.
	confirmed, err := backend.Disable(ctx)
	if err != nil || !confirmed {
		t.Fatalf("Disable = (%v, %v)", confirmed, err)
	}
	backendState, err = backend.State(ctx)
	if err != nil || !backendState.Disabled {
		t.Fatalf("State after Disable = %+v, %v", backendState, err)
	}

	// Generation expiry retires the shards with a TTL. (Shard 0 was deleted
	// above; check a surviving shard's key.)
	if err := backend.ExpireGeneration(ctx, generation, time.Hour); err != nil {
		t.Fatalf("ExpireGeneration: %v", err)
	}
	ttl, err := client.TTL(ctx, backend.shardKey(1, 1)).Result()
	if err != nil || ttl <= 0 {
		t.Fatalf("shard TTL after ExpireGeneration = (%v, %v)", ttl, err)
	}
}

// TestUploadBloomBackendUnavailable verifies every failure surfaces as
// *state.BloomUnavailable (callers fall back to MySQL rather than fail).
func TestUploadBloomBackendUnavailable(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr:       server.Addr(),
		MaxRetries: -1,
		DialTimeout: 100 * time.Millisecond,
	})
	t.Cleanup(func() { _ = client.Close() })
	settings := state.DefaultUploadBloomSettings()
	settings.URL = "redis://" + server.Addr() + "/0"
	backend, err := NewUploadBloomBackend(settings, WithRedisClient(client))
	if err != nil {
		t.Fatalf("NewUploadBloomBackend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	ctx := context.Background()
	params := state.UploadBloomParameters{Bits: 1600, Shards: 4, Hashes: 7, Capacity: 100}
	generation := state.UploadBloomGeneration{Generation: 1, Params: params}

	// Close the underlying server: every operation must report BloomUnavailable.
	server.Close()
	if _, err := backend.State(ctx); err == nil {
		t.Fatal("State on a closed server must fail")
	} else if !isBloomUnavailable(t, err) {
		t.Fatalf("State error = %v, want *state.BloomUnavailable", err)
	}
	if err := backend.SetBits(ctx, generation, []string{"x"}); !isBloomUnavailable(t, err) {
		t.Fatalf("SetBits error = %v, want *state.BloomUnavailable", err)
	}
	if _, _, err := backend.Probe(ctx, generation, "x"); !isBloomUnavailable(t, err) {
		t.Fatalf("Probe error = %v, want *state.BloomUnavailable", err)
	}
	if _, _, err := backend.AcquireBuild(ctx, 2, params, time.Minute); !isBloomUnavailable(t, err) {
		t.Fatalf("AcquireBuild error = %v, want *state.BloomUnavailable", err)
	}
}

func isBloomUnavailable(t *testing.T, err error) bool {
	t.Helper()
	if err == nil {
		return false
	}
	var unavailable *state.BloomUnavailable
	return errors.As(err, &unavailable)
}

// TestUploadBloomBackendDisabledMode checks the no-URL configuration stays a
// disabled backend that never dials.
func TestUploadBloomBackendDisabledMode(t *testing.T) {
	settings := state.DefaultUploadBloomSettings()
	settings.Mode = state.UploadBloomModeOff
	settings.URL = ""
	backend, err := NewUploadBloomBackend(settings)
	if err != nil {
		t.Fatalf("NewUploadBloomBackend: %v", err)
	}
	if backend.Enabled() {
		t.Fatal("an off-mode backend must report disabled")
	}
	ctx := context.Background()
	if _, err := backend.State(ctx); !isBloomUnavailable(t, err) {
		t.Fatalf("State on a disabled backend = %v, want *state.BloomUnavailable", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
