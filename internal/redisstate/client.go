// Package redisstate provides the concrete Redis backends behind the
// internal/state cache interfaces: a read-through state cache (StateCache) and
// the expiring transient store used for search-result snapshots and queued
// search state.
//
// It is a port of rag_mcp/state/cache.py and rag_mcp/state/transient.py. MySQL
// remains authoritative; every cache failure is reported as
// *state.CacheUnavailable (or *TransientUnavailable for transient task state) so
// callers fall back to the database instead of failing a request.
package redisstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// TransientUnavailable reports that transient task state cannot be read or
// written. It mirrors rag_mcp.state.transient.TransientUnavailable; no
// credentials or request data are ever exposed.
type TransientUnavailable struct{ Message string }

// Error implements error.
func (e *TransientUnavailable) Error() string { return e.Message }

func newTransientUnavailable(message string) *TransientUnavailable {
	return &TransientUnavailable{Message: message}
}

// Option customizes a Client, primarily for tests that inject a client backed by
// an in-process Redis.
type Option func(*clientOptions)

type clientOptions struct {
	redisClient *redis.Client
}

// WithRedisClient injects an already-configured go-redis client. The injected
// client is used instead of a lazily created one and is closed by Close.
func WithRedisClient(client *redis.Client) Option {
	return func(options *clientOptions) { options.redisClient = client }
}

// Client is the Redis StateCache + TransientProvider implementation.
//
// Construction is lazy: unreachable Redis cannot prevent database startup, and
// the no-op mode (RAG_REDIS_ENABLED=false) never dials. All methods are safe for
// concurrent use.
type Client struct {
	settings state.CacheSettings
	enabled  bool

	mutex  sync.Mutex
	redis  *redis.Client
	closed bool

	transient *Transient
}

// Compile-time interface checks.
var (
	_ state.StateCache        = (*Client)(nil)
	_ state.TransientProvider = (*Client)(nil)
)

// New builds the Redis backend from explicit cache settings. It validates the
// settings exactly like RedisStateCache.__init__ and never dials.
func New(settings state.CacheSettings, options ...Option) (*Client, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	resolved := clientOptions{}
	for _, option := range options {
		option(&resolved)
	}
	enabled := settings.URL != "" || resolved.redisClient != nil
	if settings.Enabled != nil {
		enabled = *settings.Enabled
	}
	client := &Client{
		settings: settings,
		enabled:  enabled,
	}
	if enabled && resolved.redisClient != nil {
		client.redis = resolved.redisClient
	}
	client.transient = newTransient(client, defaultTransientTTL())
	return client, nil
}

// NewFromEnv reads RAG_REDIS_* settings and builds the backend. A disabled
// configuration returns the no-op client instead of dialing.
func NewFromEnv(options ...Option) (*Client, error) {
	settings, err := state.CacheSettingsFromEnv()
	if err != nil {
		return nil, err
	}
	return New(settings, options...)
}

// NewDisabled returns the no-op backend: reads miss, writes are refused and
// transient state reports "Redis is required for queued operations".
func NewDisabled() *Client {
	settings := state.DefaultCacheSettings()
	disabled := false
	settings.Enabled = &disabled
	client := &Client{settings: settings, enabled: false}
	client.transient = newTransient(client, defaultTransientTTL())
	return client
}

// Settings returns the effective cache settings (secrets are not redacted here;
// callers must not expose the URL).
func (c *Client) Settings() state.CacheSettings { return c.settings }

// Enabled reports whether the cache is configured. It mirrors
// RedisStateCache.enabled: an explicit setting wins, otherwise a URL or an
// injected client enables it.
func (c *Client) Enabled() bool { return c.enabled }

// Transient returns the expiring transient store view of this backend.
func (c *Client) Transient() *Transient { return c.transient }

// TransientStore implements state.TransientProvider.
func (c *Client) TransientStore() state.TransientStore { return c.transient }

// connection lazily creates the go-redis client.
func (c *Client) connection() (*redis.Client, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.closed {
		return nil, &state.CacheUnavailable{Message: "State cache is closed"}
	}
	if c.redis != nil {
		return c.redis, nil
	}
	if c.settings.URL == "" {
		return nil, &state.CacheUnavailable{Message: "State cache connection unavailable"}
	}
	options, err := redisOptionsFromURL(c.settings.URL, c.settings.TimeoutSeconds)
	if err != nil {
		return nil, &state.CacheUnavailable{Message: "State cache connection unavailable"}
	}
	c.redis = redis.NewClient(options)
	return c.redis, nil
}

// redisOptionsFromURL parses a redis:// (or rediss://) URL and applies the
// Python socket_timeout/socket_connect_timeout settings. Application retries are
// disabled so an unavailable cache fails fast like redis-py's default client;
// MySQL remains authoritative and the durable outbox owns retries.
func redisOptionsFromURL(rawURL string, timeoutSeconds float64) (*redis.Options, error) {
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	timeout := secondsToDuration(timeoutSeconds)
	options.DialTimeout = timeout
	options.ReadTimeout = timeout
	options.WriteTimeout = timeout
	options.PoolTimeout = timeout
	options.MaxRetries = -1
	return options, nil
}

func secondsToDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

// Ping checks Redis connectivity, used by readiness probes.
func (c *Client) Ping(ctx context.Context) error {
	connection, err := c.connection()
	if err != nil {
		return err
	}
	return connection.Ping(ctx).Err()
}

// Close releases the Redis client. It is idempotent and never masks a database
// shutdown failure with an untyped panic.
func (c *Client) Close() error {
	c.mutex.Lock()
	if c.closed {
		c.mutex.Unlock()
		return nil
	}
	c.closed = true
	connection := c.redis
	c.redis = nil
	c.mutex.Unlock()
	if connection == nil {
		return nil
	}
	if err := connection.Close(); err != nil {
		return &state.CacheUnavailable{Message: "State cache close unavailable"}
	}
	return nil
}

// KeyPair returns the payload and persistent watermark keys of one cache entry.
// identity is sha256(json([kind, scope], ensure_ascii=True, compact)) and the
// keys share one Redis Cluster hash slot.
func KeyPair(prefix, kind, scope string) (payload string, revision string) {
	digest := identityDigest(kind, scope)
	base := prefix + ":{" + digest + "}"
	return base + ":payload", base + ":revision"
}

// PayloadKey returns the payload key used by this client (tests/diagnostics).
func (c *Client) PayloadKey(kind, scope string) string {
	payload, _ := KeyPair(c.settings.Prefix, kind, scope)
	return payload
}

// RevisionKey returns the persistent watermark key used by this client.
func (c *Client) RevisionKey(kind, scope string) string {
	_, revision := KeyPair(c.settings.Prefix, kind, scope)
	return revision
}

// identityDigest mirrors json.dumps([kind, scope], ensure_ascii=True,
// separators=(",", ":")) followed by sha256. Cache kinds and scopes are ASCII
// in this service, so Go's compact []string encoding matches the Python bytes.
func identityDigest(kind, scope string) string {
	encoded, _ := json.Marshal([]string{kind, scope})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// cacheEnvelope is the stored payload envelope; field order matches the Python
// json.dumps output for easier inspection.
type cacheEnvelope struct {
	Schema   int    `json:"schema"`
	Revision string `json:"revision"`
	Deleted  bool   `json:"deleted"`
	Value    any    `json:"value"`
}

// Get returns (false, nil) on a miss; a cached deletion is (true, nil). A
// corrupt envelope, watermark mismatch or backend failure behaves exactly like
// RedisStateCache.get: corruption/unavailability is a CacheUnavailable error,
// while a watermark mismatch is reported as a miss.
func (c *Client) Get(ctx context.Context, kind, scope string) (bool, any, error) {
	if !c.enabled {
		return false, nil, nil
	}
	connection, err := c.connection()
	if err != nil {
		return false, nil, err
	}
	payloadKey, revisionKey := KeyPair(c.settings.Prefix, kind, scope)
	values, err := connection.MGet(ctx, payloadKey, revisionKey).Result()
	if err != nil {
		return false, nil, &state.CacheUnavailable{Message: "State cache read unavailable"}
	}
	if len(values) < 2 || values[0] == nil {
		return false, nil, nil
	}
	rawPayload, ok := cacheText(values[0])
	if !ok {
		return false, nil, &state.CacheUnavailable{Message: "State cache read unavailable"}
	}
	envelope, err := decodeCacheEnvelope(rawPayload)
	if err != nil {
		return false, nil, &state.CacheUnavailable{Message: "State cache read unavailable"}
	}
	watermark, _ := cacheText(values[1])
	deletedMarker := "0"
	if envelope.Deleted {
		deletedMarker = "1"
	}
	if watermark != envelope.Revision+":"+deletedMarker {
		return false, nil, nil
	}
	if envelope.Deleted {
		return true, nil, nil
	}
	return true, envelope.Value, nil
}

// decodeCacheEnvelope validates the stored envelope exactly like get().
func decodeCacheEnvelope(raw string) (*cacheEnvelope, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("invalid envelope")
	}
	schema, ok := object["schema"].(json.Number)
	if !ok || schema.String() != "1" {
		return nil, fmt.Errorf("invalid envelope")
	}
	revision, ok := object["revision"].(string)
	if !ok || !canonicalRevision(revision) {
		return nil, fmt.Errorf("invalid revision")
	}
	deleted, ok := object["deleted"].(bool)
	if !ok {
		return nil, fmt.Errorf("invalid envelope")
	}
	value, present := object["value"]
	if !present {
		return nil, fmt.Errorf("invalid envelope")
	}
	return &cacheEnvelope{Schema: 1, Revision: revision, Deleted: deleted, Value: value}, nil
}

// canonicalRevision reports whether value is a non-negative decimal integer
// without leading zeros (Python `revision == str(int(revision))`).
func canonicalRevision(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return len(value) == 1 || value[0] != '0'
}

// Put atomically publishes a snapshot unless a newer revision already exists.
// Equal revisions can refill expired payloads but cannot undo a deletion; the
// deletion value is discarded. It mirrors RedisStateCache.put.
func (c *Client) Put(ctx context.Context, kind, scope string, revision int64, value any, deleted bool) (bool, error) {
	if !c.enabled {
		return false, nil
	}
	if revision < 0 {
		return false, &state.ValueError{Message: "Cache revision must be a nonnegative integer"}
	}
	connection, err := c.connection()
	if err != nil {
		return false, err
	}
	envelope := cacheEnvelope{Schema: 1, Revision: strconv.FormatInt(revision, 10), Deleted: deleted}
	if !deleted {
		envelope.Value = value
	}
	payload, err := encodeCacheEnvelope(envelope)
	if err != nil {
		return false, &state.CacheUnavailable{Message: "State cache write unavailable"}
	}
	newRevision := envelope.Revision
	deletedMarker := "0"
	if deleted {
		deletedMarker = "1"
	}
	payloadKey, revisionKey := KeyPair(c.settings.Prefix, kind, scope)
	ttl := time.Duration(c.settings.TTLSeconds) * time.Second
	for range 32 {
		skip := false
		watchErr := connection.Watch(ctx, func(transaction *redis.Tx) error {
			current, err := transaction.Get(ctx, revisionKey).Result()
			if err != nil && err != redis.Nil {
				return err
			}
			if err == nil {
				oldRevision, oldDeleted, ok := parseWatermark(current)
				if !ok {
					return fmt.Errorf("invalid state cache watermark")
				}
				if len(oldRevision) > len(newRevision) ||
					(len(oldRevision) == len(newRevision) && oldRevision > newRevision) {
					skip = true
					return nil
				}
				if oldRevision == newRevision && oldDeleted && !deleted {
					skip = true
					return nil
				}
			}
			_, err = transaction.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, payloadKey, payload, ttl)
				pipe.Set(ctx, revisionKey, newRevision+":"+deletedMarker, 0)
				return nil
			})
			return err
		}, payloadKey, revisionKey)
		if watchErr == redis.TxFailedErr {
			continue
		}
		if watchErr != nil {
			return false, &state.CacheUnavailable{Message: "State cache write unavailable"}
		}
		return !skip, nil
	}
	return false, &state.CacheUnavailable{Message: "State cache write unavailable"}
}

// parseWatermark parses "<revision>:<0|1>" exactly like the Lua PUT script.
func parseWatermark(value string) (revision string, deleted bool, ok bool) {
	revision, marker, found := strings.Cut(value, ":")
	if !found || revision == "" {
		return "", false, false
	}
	for index := 0; index < len(revision); index++ {
		if revision[index] < '0' || revision[index] > '9' {
			return "", false, false
		}
	}
	switch marker {
	case "0":
		return revision, false, true
	case "1":
		return revision, true, true
	default:
		return "", false, false
	}
}

func cacheText(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case []byte:
		return string(typed), true
	default:
		return "", false
	}
}

func encodeCacheEnvelope(envelope cacheEnvelope) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(envelope); err != nil {
		return "", err
	}
	return strings.TrimRight(buffer.String(), "\n"), nil
}

// Attach wires the backend into the shared state stores: read-through snapshot
// reads use it as the StateCache backend and result snapshots use its expiring
// transient store. It is the one-line integration point for cmd/rag-service
// after state.New, because internal/state cannot import this package.
func Attach(stores *state.Stores, client *Client) error {
	if stores == nil {
		return fmt.Errorf("redisstate: state stores are required")
	}
	if client == nil {
		return fmt.Errorf("redisstate: client is required")
	}
	if stores.CacheSync != nil {
		stores.CacheSync.Backend = client
	}
	if stores.Results != nil {
		stores.Results.Transient = client.TransientStore()
	}
	return nil
}

// ---- transient key helpers -------------------------------------------------

// transientKey mirrors TransientStore._key: the prefix, the literal
// ":transient:" marker and sha256(json([kind, identity])).
func transientKey(prefix, kind, identity string) string {
	return prefix + ":transient:{" + identityDigest(kind, identity) + "}"
}

// ceilTTL mirrors TransientStore._ttl: a positive finite TTL rounded up to at
// least one second; a non-positive value selects the default snapshot TTL.
func ceilTTL(value float64, fallback float64) (time.Duration, error) {
	ttl := value
	if ttl == 0 {
		ttl = fallback
	}
	if math.IsNaN(ttl) || math.IsInf(ttl, 0) || ttl <= 0 {
		return 0, &state.ValueError{Message: "Redis task TTL must be positive"}
	}
	seconds := math.Ceil(ttl)
	if seconds < 1 {
		seconds = 1
	}
	return time.Duration(seconds) * time.Second, nil
}

// defaultTransientTTL mirrors TransientStore._ttl(None): the snapshot TTL from
// the environment, falling back to the dataclass default when invalid.
func defaultTransientTTL() float64 {
	settings, err := state.SnapshotSettingsFromEnv()
	if err != nil {
		return float64(state.DefaultSnapshotSettings().TTLSeconds)
	}
	return float64(settings.TTLSeconds)
}
