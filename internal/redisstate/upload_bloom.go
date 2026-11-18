// Package redisstate's upload bloom backend: the Redis bitmap storage behind
// internal/state.UploadBloomBackend.
//
// Storage layout (all keys under the configured prefix, one hash tag per
// generation so Redis Cluster keeps a generation in one slot group):
//
//	<prefix>:meta              -> JSON {generation, bits, shards, hashes, capacity}
//	<prefix>:building          -> JSON {generation, bits, shards, hashes, capacity} + TTL (build lease)
//	<prefix>:disabled          -> "1" (global kill switch, no TTL: a rebuild clears it)
//	<prefix>:g{N}:{shard}      -> Redis string bitmap, one bit per stored ID bit
//
// Meta is one string; reading and switching it is atomic (GET/SET). The kill
// switch is a separate key so Enable's meta read and Disable's write never
// interleave into a stale combination. This file is the storage half; the
// decision half (when to trust a negative) lives in internal/state.
package redisstate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// uploadBloomDisabledValue is the kill switch payload.
const uploadBloomDisabledValue = "1"

// UploadBloomBackend is the Redis bitmap implementation of
// state.UploadBloomBackend. Construction is lazy (an unreachable Redis cannot
// prevent startup); every method maps failures to *state.BloomUnavailable so
// callers fall back to MySQL.
type UploadBloomBackend struct {
	settings state.UploadBloomSettings

	mutex  sync.Mutex
	redis  *redis.Client
	closed bool
}

// Compile-time interface check.
var _ state.UploadBloomBackend = (*UploadBloomBackend)(nil)

// NewUploadBloomBackend builds the Redis bitmap backend from explicit
// settings. It validates the settings and never dials.
func NewUploadBloomBackend(settings state.UploadBloomSettings, options ...Option) (*UploadBloomBackend, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	resolved := clientOptions{}
	for _, option := range options {
		option(&resolved)
	}
	backend := &UploadBloomBackend{settings: settings}
	if resolved.redisClient != nil {
		backend.redis = resolved.redisClient
	}
	return backend, nil
}

// Enabled reports whether a backend is configured. A URL or an injected client
// enables it; the RAG_UPLOAD_BLOOM_MODE=off mode never constructs one.
func (b *UploadBloomBackend) Enabled() bool { return b != nil && b.settings.URL != "" }

// Close releases the Redis client; idempotent.
func (b *UploadBloomBackend) Close() error {
	b.mutex.Lock()
	b.closed = true
	connection := b.redis
	b.redis = nil
	b.mutex.Unlock()
	if connection == nil {
		return nil
	}
	if err := connection.Close(); err != nil {
		return state.NewBloomUnavailable()
	}
	return nil
}

// Ping checks Redis connectivity.
func (b *UploadBloomBackend) Ping(ctx context.Context) error {
	connection, err := b.connection()
	if err != nil {
		return err
	}
	if err := connection.Ping(ctx).Err(); err != nil {
		return state.NewBloomUnavailable()
	}
	return nil
}

// connection lazily creates the go-redis client, mirroring Client.connection.
func (b *UploadBloomBackend) connection() (*redis.Client, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	if b.closed {
		return nil, state.NewBloomUnavailable()
	}
	if b.redis != nil {
		return b.redis, nil
	}
	if b.settings.URL == "" {
		return nil, state.NewBloomUnavailable()
	}
	options, err := redisOptionsFromURL(b.settings.URL, b.settings.TimeoutSeconds)
	if err != nil {
		return nil, state.NewBloomUnavailable()
	}
	b.redis = redis.NewClient(options)
	return b.redis, nil
}

func (b *UploadBloomBackend) metaKey() string      { return b.settings.Prefix + ":meta" }
func (b *UploadBloomBackend) buildingKey() string  { return b.settings.Prefix + ":building" }
func (b *UploadBloomBackend) disabledKey() string   { return b.settings.Prefix + ":disabled" }
func (b *UploadBloomBackend) shardKey(generation int64, shard int) string {
	return fmt.Sprintf("%s:g%d:%d", b.settings.Prefix, generation, shard)
}

// decodeGeneration parses one meta/building JSON payload.
func decodeUploadBloomGeneration(raw string) (*state.UploadBloomGeneration, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var generation state.UploadBloomGeneration
	if err := json.Unmarshal([]byte(raw), &generation); err != nil {
		return nil, state.NewBloomUnavailable()
	}
	if !state.UploadBloomParametersValid(generation.Params) {
		return nil, state.NewBloomUnavailable()
	}
	return &generation, nil
}

// State reads the kill switch, active generation and building generation.
func (b *UploadBloomBackend) State(ctx context.Context) (state.UploadBloomState, error) {
	connection, err := b.connection()
	if err != nil {
		return state.UploadBloomState{}, err
	}
	values, err := connection.MGet(ctx, b.metaKey(), b.buildingKey(), b.disabledKey()).Result()
	if err != nil {
		return state.UploadBloomState{}, state.NewBloomUnavailable()
	}
	result := state.UploadBloomState{}
	if raw, ok := values[0].(string); ok {
		active, decodeErr := decodeUploadBloomGeneration(raw)
		if decodeErr != nil {
			return state.UploadBloomState{}, decodeErr
		}
		result.Active = active
	}
	if raw, ok := values[1].(string); ok {
		building, decodeErr := decodeUploadBloomGeneration(raw)
		if decodeErr != nil {
			return state.UploadBloomState{}, decodeErr
		}
		result.Building = building
	}
	if raw, ok := values[2].(string); ok && raw == uploadBloomDisabledValue {
		result.Disabled = true
	}
	return result, nil
}

// encodeGeneration renders one meta/building JSON payload.
func encodeUploadBloomGeneration(generation state.UploadBloomGeneration) (string, error) {
	encoded, err := json.Marshal(generation)
	if err != nil {
		return "", state.NewBloomUnavailable()
	}
	return string(encoded), nil
}

// SetBits registers IDs in one generation's bitmap.
func (b *UploadBloomBackend) SetBits(ctx context.Context, generation state.UploadBloomGeneration, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	connection, err := b.connection()
	if err != nil {
		return err
	}
	perShard := make(map[int][]int64)
	for _, id := range ids {
		for _, position := range state.UploadBloomBitPositions(generation.Params, id) {
			shard := int(position / generation.Params.BitsPerShard())
			offset := position % generation.Params.BitsPerShard()
			perShard[shard] = append(perShard[shard], offset)
		}
	}
	pipe := connection.Pipeline()
	for shard, offsets := range perShard {
		key := b.shardKey(generation.Generation, shard)
		for _, offset := range offsets {
			pipe.SetBit(ctx, key, offset, 1)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return state.NewBloomUnavailable()
	}
	return nil
}

// Probe reports whether the ID is present. A shard whose bitmap key is
// missing (lost or never written) is reported as shardMissing so the reader
// falls back to MySQL instead of trusting an all-zero bitmap: SETBIT on a
// missing key creates it, but a key can still vanish (manual deletion, an
// old restore, a failed first build), and a GetBit against a missing key
// returns 0 exactly like a genuinely absent ID.
func (b *UploadBloomBackend) Probe(ctx context.Context, generation state.UploadBloomGeneration, id string) (bool, bool, error) {
	connection, err := b.connection()
	if err != nil {
		return false, false, err
	}
	positions := state.UploadBloomBitPositions(generation.Params, id)
	perShard := make(map[int][]int64)
	for _, position := range positions {
		shard := int(position / generation.Params.BitsPerShard())
		offset := position % generation.Params.BitsPerShard()
		perShard[shard] = append(perShard[shard], offset)
	}
	pipe := connection.Pipeline()
	bits := make(map[int]*redis.IntCmd, len(perShard))
	exists := make(map[int]*redis.IntCmd, len(perShard))
	for shard := range perShard {
		key := b.shardKey(generation.Generation, shard)
		exists[shard] = pipe.Exists(ctx, key)
		for _, offset := range perShard[shard] {
			bits[shard] = pipe.GetBit(ctx, key, offset)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return false, false, state.NewBloomUnavailable()
	}
	for _, command := range exists {
		count, err := command.Result()
		if err != nil {
			return false, false, state.NewBloomUnavailable()
		}
		if count == 0 {
			// The shard bitmap itself is gone: neither "present" nor "absent"
			// is provable.
			return false, true, nil
		}
	}
	for _, command := range bits {
		value, err := command.Result()
		if err != nil {
			return false, false, state.NewBloomUnavailable()
		}
		if value == 0 {
			return false, false, nil
		}
	}
	return true, false, nil
}

// AcquireBuild pins the generation's parameters and takes the build lease.
// An existing building payload wins over the requested parameters so
// concurrent or restarted builders agree on one geometry.
func (b *UploadBloomBackend) AcquireBuild(ctx context.Context, generation int64, params state.UploadBloomParameters, ttl time.Duration) (state.UploadBloomGeneration, bool, error) {
	connection, err := b.connection()
	if err != nil {
		return state.UploadBloomGeneration{}, false, err
	}
	buildingKey := b.buildingKey()
	for attempt := 0; attempt < 2; attempt++ {
		encoded, err := encodeUploadBloomGeneration(state.UploadBloomGeneration{Generation: generation, Params: params})
		if err != nil {
			return state.UploadBloomGeneration{}, false, err
		}
		acquired, err := connection.SetNX(ctx, buildingKey, encoded, ttl).Result()
		if err != nil {
			return state.UploadBloomGeneration{}, false, state.NewBloomUnavailable()
		}
		if acquired {
			return state.UploadBloomGeneration{Generation: generation, Params: params}, true, nil
		}
		raw, err := connection.Get(ctx, buildingKey).Result()
		if err != nil {
			return state.UploadBloomGeneration{}, false, state.NewBloomUnavailable()
		}
		existing, decodeErr := decodeUploadBloomGeneration(raw)
		if decodeErr != nil {
			return state.UploadBloomGeneration{}, false, decodeErr
		}
		if existing == nil {
			// The lease expired between SetNX and Get; retry the acquisition.
			continue
		}
		return *existing, false, nil
	}
	return state.UploadBloomGeneration{}, false, state.NewBloomUnavailable()
}

// RenewBuild extends the build lease.
func (b *UploadBloomBackend) RenewBuild(ctx context.Context, generation int64, ttl time.Duration) error {
	connection, err := b.connection()
	if err != nil {
		return err
	}
	ok, err := connection.Expire(ctx, b.buildingKey(), ttl).Result()
	if err != nil {
		return state.NewBloomUnavailable()
	}
	if !ok {
		return state.NewBloomUnavailable()
	}
	return nil
}

// ReleaseBuild drops the build lease.
func (b *UploadBloomBackend) ReleaseBuild(ctx context.Context, generation int64) error {
	connection, err := b.connection()
	if err != nil {
		return err
	}
	if _, err := connection.Del(ctx, b.buildingKey()).Result(); err != nil {
		return state.NewBloomUnavailable()
	}
	return nil
}

// Activate publishes the new generation atomically and clears the kill
// switch. Order matters: meta first (the switch and the bitmap are already
// consistent), kill switch last so a crash between the two steps leaves the
// filter stopped rather than answering from stale data.
func (b *UploadBloomBackend) Activate(ctx context.Context, generation state.UploadBloomGeneration) error {
	connection, err := b.connection()
	if err != nil {
		return err
	}
	encoded, err := encodeUploadBloomGeneration(generation)
	if err != nil {
		return err
	}
	if _, err := connection.Set(ctx, b.metaKey(), encoded, 0).Result(); err != nil {
		return state.NewBloomUnavailable()
	}
	if _, err := connection.Del(ctx, b.disabledKey()).Result(); err != nil {
		return state.NewBloomUnavailable()
	}
	return nil
}

// Disable sets the global kill switch and verifies it is visible: the write
// is a SET (no TTL), and the verification re-reads the key. A failure or an
// invisible switch returns confirmed=false.
func (b *UploadBloomBackend) Disable(ctx context.Context) (bool, error) {
	connection, err := b.connection()
	if err != nil {
		return false, err
	}
	if _, err := connection.Set(ctx, b.disabledKey(), uploadBloomDisabledValue, 0).Result(); err != nil {
		return false, state.NewBloomUnavailable()
	}
	value, err := connection.Get(ctx, b.disabledKey()).Result()
	if err != nil {
		return false, state.NewBloomUnavailable()
	}
	return value == uploadBloomDisabledValue, nil
}

// ExpireGeneration retires a superseded generation's shards after the grace
// TTL (in-flight readers that fetched the old meta still probe it).
func (b *UploadBloomBackend) ExpireGeneration(ctx context.Context, generation state.UploadBloomGeneration, ttl time.Duration) error {
	connection, err := b.connection()
	if err != nil {
		return err
	}
	for shard := 0; shard < generation.Params.Shards; shard++ {
		key := b.shardKey(generation.Generation, shard)
		if err := connection.Expire(ctx, key, ttl).Err(); err != nil {
			return state.NewBloomUnavailable()
		}
	}
	return nil
}
