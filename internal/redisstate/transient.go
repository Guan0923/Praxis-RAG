package redisstate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// Transient is the expiring key/value store used for result snapshots and the
// queued-search task state. It is a port of rag_mcp.state.transient.TransientStore.
//
// It implements state.TransientStore (Get/Put/Delete) and additionally the
// compare-and-set, lock and unlock helpers the search-task port needs. Values
// must be JSON objects.
type Transient struct {
	client     *Client
	defaultTTL float64
}

// Compile-time interface check.
var _ state.TransientStore = (*Transient)(nil)

func newTransient(client *Client, defaultTTL float64) *Transient {
	return &Transient{client: client, defaultTTL: defaultTTL}
}

// Key returns the Redis key of one transient entry (tests/diagnostics).
func (t *Transient) Key(kind, identity string) string {
	return transientKey(t.prefix(), kind, identity)
}

func (t *Transient) prefix() string {
	if t.client == nil {
		return state.DefaultCacheSettings().Prefix
	}
	return t.client.settings.Prefix
}

// connection mirrors TransientStore._connection: a missing/disabled backend is
// "Redis is required for queued operations"; connection creation failures are
// "Redis task storage unavailable".
func (t *Transient) connection() (*redis.Client, error) {
	if t.client == nil || !t.client.Enabled() {
		return nil, newTransientUnavailable("Redis is required for queued operations")
	}
	connection, err := t.client.connection()
	if err != nil {
		return nil, newTransientUnavailable("Redis task storage unavailable")
	}
	return connection, nil
}

// Get returns (nil, false, nil) on a miss.
func (t *Transient) Get(kind, identity string) (any, bool, error) {
	connection, err := t.connection()
	if err != nil {
		return nil, false, err
	}
	raw, err := connection.Get(context.Background(), t.Key(kind, identity)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, newTransientUnavailable("Redis task read unavailable")
	}
	value, err := decodeTransientState(raw)
	if err != nil {
		return nil, false, newTransientUnavailable("Redis task read unavailable")
	}
	return value, true, nil
}

// Put stores one JSON object with the Python ceil/max(1) TTL semantics. nx
// refuses to replace an existing entry.
func (t *Transient) Put(kind, identity string, value any, ttlSeconds float64, nx bool) (bool, error) {
	connection, err := t.connection()
	if err != nil {
		return false, err
	}
	encoded, err := encodeTransientState(value)
	if err != nil {
		return false, err
	}
	ttl, err := ceilTTL(ttlSeconds, t.defaultTTL)
	if err != nil {
		return false, err
	}
	args := redis.SetArgs{TTL: ttl}
	if nx {
		args.Mode = "NX"
	}
	result, err := connection.SetArgs(context.Background(), t.Key(kind, identity), encoded, args).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, newTransientUnavailable("Redis task write unavailable")
	}
	return result == "OK", nil
}

// Delete removes one entry and returns the number of deleted keys.
func (t *Transient) Delete(kind, identity string) (int64, error) {
	connection, err := t.connection()
	if err != nil {
		return 0, err
	}
	deleted, err := connection.Del(context.Background(), t.Key(kind, identity)).Result()
	if err != nil {
		return 0, newTransientUnavailable("Redis task delete unavailable")
	}
	return deleted, nil
}

// Update applies callback atomically using WATCH/MULTI optimistic locking, like
// TransientStore.update. A nil result aborts the update without writing; a
// callback error is preserved, and backend failures are sanitized.
func (t *Transient) Update(
	kind, identity string,
	callback func(current map[string]any) (map[string]any, error),
	ttlSeconds *float64,
) (map[string]any, error) {
	connection, err := t.connection()
	if err != nil {
		return nil, err
	}
	ttl := t.defaultTTL
	if ttlSeconds != nil {
		ttl = *ttlSeconds
	}
	expiration, err := ceilTTL(ttl, t.defaultTTL)
	if err != nil {
		return nil, err
	}
	key := t.Key(kind, identity)
	for attempt := 0; attempt < 32; attempt++ {
		var updated map[string]any
		var callbackError error
		watchErr := connection.Watch(context.Background(), func(transaction *redis.Tx) error {
			raw, err := transaction.Get(context.Background(), key).Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				return err
			}
			var current map[string]any
			if err == nil {
				current, err = decodeTransientState(raw)
				if err != nil {
					return err
				}
			}
			updated, callbackError = callback(current)
			if callbackError != nil {
				return callbackError
			}
			if updated == nil {
				// Nothing to write: return without MULTI/EXEC.
				return nil
			}
			encoded, encodeErr := encodeTransientState(updated)
			if encodeErr != nil {
				return encodeErr
			}
			_, err = transaction.TxPipelined(context.Background(), func(pipe redis.Pipeliner) error {
				pipe.Set(context.Background(), key, encoded, expiration)
				return nil
			})
			return err
		}, key)
		if callbackError != nil {
			return nil, callbackError
		}
		if errors.Is(watchErr, redis.TxFailedErr) {
			continue
		}
		if watchErr != nil {
			return nil, newTransientUnavailable("Redis task update unavailable")
		}
		return updated, nil
	}
	return nil, newTransientUnavailable("Redis task update contention")
}

// Lock acquires a mutually-exclusive expiring lock and returns its token. The
// boolean result is false when another owner holds the lock.
func (t *Transient) Lock(kind, identity string, ttlSeconds float64) (string, bool, error) {
	connection, err := t.connection()
	if err != nil {
		return "", false, err
	}
	expiration, err := ceilTTL(ttlSeconds, t.defaultTTL)
	if err != nil {
		return "", false, err
	}
	token, err := randomToken()
	if err != nil {
		return "", false, newTransientUnavailable("Redis task lock unavailable")
	}
	acquired, err := connection.SetArgs(context.Background(), t.Key("lock:"+kind, identity), token,
		redis.SetArgs{Mode: "NX", TTL: expiration}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return "", false, newTransientUnavailable("Redis task lock unavailable")
	}
	if acquired != "OK" {
		return "", false, nil
	}
	return token, true, nil
}

// Unlock releases a lock only when token still owns it.
func (t *Transient) Unlock(kind, identity, token string) (bool, error) {
	connection, err := t.connection()
	if err != nil {
		return false, err
	}
	key := t.Key("lock:"+kind, identity)
	for attempt := 0; attempt < 32; attempt++ {
		released := false
		watchErr := connection.Watch(context.Background(), func(transaction *redis.Tx) error {
			current, err := transaction.Get(context.Background(), key).Result()
			if errors.Is(err, redis.Nil) {
				return nil
			}
			if err != nil {
				return err
			}
			if current != token {
				return nil
			}
			released = true
			_, err = transaction.TxPipelined(context.Background(), func(pipe redis.Pipeliner) error {
				pipe.Del(context.Background(), key)
				return nil
			})
			return err
		}, key)
		if errors.Is(watchErr, redis.TxFailedErr) {
			continue
		}
		if watchErr != nil {
			return false, newTransientUnavailable("Redis task unlock unavailable")
		}
		return released, nil
	}
	return false, newTransientUnavailable("Redis task unlock contention")
}

// ---- JSON helpers ----------------------------------------------------------

// encodeTransientState encodes one JSON object with the same escaping rules as
// TransientStore._encode (UTF-8 preserved, no HTML escaping, NaN rejected).
func encodeTransientState(value any) (string, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("Redis task state must be an object")
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(object); err != nil {
		return "", &state.ValueError{Message: "Redis task state must be a finite JSON object"}
	}
	return strings.TrimRight(buffer.String(), "\n"), nil
}

// decodeTransientState decodes one JSON object, mirroring TransientStore._decode.
func decodeTransientState(raw string) (map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("Invalid Redis task state")
	}
	return object, nil
}

// randomToken mirrors uuid4().hex for lock tokens.
func randomToken() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
