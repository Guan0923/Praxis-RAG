// Package redisstate provides the concrete Redis backend behind the internal/state
// cache interface: a read-through display cache (StateCache) for the document,
// knowledge-base and catalog read models, plus the per-kind+scope backfill
// lock (SET NX PX with a caller-supplied token; renew and release verify the
// token) that serializes MySQL backfills across instances.
//
// It is a port of rag_mcp/state/cache.py. MySQL remains authoritative; cache
// failures are reported as sanitized *state.CacheUnavailable errors with a
// distinguishable cause (network, single-entry corruption, caller
// cancellation), and the state layer's read-through path falls back to MySQL
// on outage while the display-cache circuit breaker bounds the cost. Search
// results are synchronous and are not cached or stored here.
package redisstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

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

// Client is the Redis StateCache implementation.
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
}

// Compile-time interface checks.
var _ state.StateCache = (*Client)(nil)

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
// cache operations report "Redis is required for cached reads".
func NewDisabled() *Client {
	settings := state.DefaultCacheSettings()
	disabled := false
	settings.Enabled = &disabled
	return &Client{settings: settings, enabled: false}
}

// Settings returns the effective cache settings (secrets are not redacted here;
// callers must not expose the URL).
func (c *Client) Settings() state.CacheSettings { return c.settings }

// Enabled reports whether the cache is configured. It mirrors
// RedisStateCache.enabled: an explicit setting wins, otherwise a URL or an
// injected client enables it.
func (c *Client) Enabled() bool { return c.enabled }

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
//
// The returned *state.CacheUnavailable keeps the internal cause distinguishable:
// connection errors and timeouts carry the network reason (they feed the
// display-cache circuit breaker), an undecodable payload carries the corrupt
// reason (only this entry is affected), and a caller cancellation carries the
// canceled reason (not a Redis fault).
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
		return false, nil, classifyCacheUnavailable(ctx, err, "State cache read unavailable")
	}
	if len(values) < 2 || values[0] == nil {
		return false, nil, nil
	}
	rawPayload, ok := cacheText(values[0])
	if !ok {
		return false, nil, state.NewCacheUnavailableCorrupt("State cache read unavailable")
	}
	envelope, err := decodeCacheEnvelope(rawPayload)
	if err != nil {
		return false, nil, state.NewCacheUnavailableCorrupt("State cache read unavailable")
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

// Compare decimal revisions as strings to preserve the full int64 range in Lua.
var putSnapshotScript = redis.NewScript(`
local revision, deleted = ARGV[1], ARGV[2]
local current = redis.call("GET", KEYS[2])
if current then
    local previous, wasDeleted = string.match(current, "^([0-9]+):([01])$")
    if not previous then
        return redis.error_reply("invalid state cache watermark")
    end
    if #previous > #revision or (#previous == #revision and previous > revision) then
        return 0
    end
    if previous == revision and wasDeleted == "1" and deleted == "0" then
        return 0
    end
end
redis.call("SET", KEYS[1], ARGV[3], "EX", ARGV[4])
redis.call("SET", KEYS[2], revision .. ":" .. deleted)
return 1
`)

// Put atomically publishes a snapshot unless a newer revision already exists.
// Equal revisions can refill expired payloads but cannot undo a deletion; the
// deletion value is discarded. It mirrors RedisStateCache.put. Failures keep
// the same cause classification as Get: network outages, single-entry encoding
// problems and caller cancellations stay distinguishable. The payload TTL is
// randomized per write in [T-floor(T/5), T] (48~60s by default) so synchronized
// expiries stagger, while the revision watermark never expires.
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
		return false, state.NewCacheUnavailableCorrupt("State cache write unavailable")
	}
	newRevision := envelope.Revision
	deletedMarker := "0"
	if deleted {
		deletedMarker = "1"
	}
	payloadKey, revisionKey := KeyPair(c.settings.Prefix, kind, scope)
	ttl := state.RandomTTLSeconds(c.settings.TTLSeconds)
	stored, err := putSnapshotScript.Run(ctx, connection, []string{payloadKey, revisionKey},
		newRevision, deletedMarker, payload, ttl).Int64()
	if err != nil {
		return false, classifyCacheUnavailable(ctx, err, "State cache write unavailable")
	}
	return stored == 1, nil
}

// classifyCacheUnavailable wraps one backend transport failure, keeping the
// caller's active cancellation distinguishable from a backend availability
// fault: only the latter feeds the display-cache circuit breaker.
func classifyCacheUnavailable(ctx context.Context, cause error, message string) *state.CacheUnavailable {
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return state.NewCacheUnavailableCanceled(message)
	}
	return state.NewCacheUnavailableNetwork(message)
}

// renewBackfillLockScript and releaseBackfillLockScript keep the token check
// and the write in one atomic operation, mirroring the lease scripts' contract
// that a caller must never extend or delete a lock it no longer owns.
const renewBackfillLockScriptSource = `
local current = redis.call('GET', KEYS[1])
if not current or current ~= ARGV[1] then
  return 0
end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 1
`

const releaseBackfillLockScriptSource = `
local current = redis.call('GET', KEYS[1])
if current and current == ARGV[1] then
  redis.call('DEL', KEYS[1])
  return 1
end
return 0
`

var (
	renewBackfillLockScript   = redis.NewScript(renewBackfillLockScriptSource)
	releaseBackfillLockScript = redis.NewScript(releaseBackfillLockScriptSource)
)

// backfillLockKey returns the per-key backfill lock key. It reuses the
// identityDigest hash tag so payload, watermark and lock share one Redis
// Cluster hash slot.
func backfillLockKey(prefix, kind, scope string) string {
	return prefix + ":{" + identityDigest(kind, scope) + "}:lock"
}

// BackfillLockKey returns the backfill lock key used by this client
// (tests/diagnostics).
func (c *Client) BackfillLockKey(kind, scope string) string {
	return backfillLockKey(c.settings.Prefix, kind, scope)
}

// backfillTTL converts the lease duration to the millisecond string the Lua
// renew script compares/persists.
func backfillTTL(ttl time.Duration) string {
	return strconv.FormatInt(ttl.Milliseconds(), 10)
}

// AcquireBackfillLock takes the kind+scope backfill lock for token with one
// SET NX PX: it either wins atomically or observes that another instance holds
// the lock (false, nil). A backend failure returns *state.CacheUnavailable,
// which leaves ownership unknown; callers must not treat that as a free lock.
func (c *Client) AcquireBackfillLock(ctx context.Context, kind, scope, token string, ttl time.Duration) (bool, error) {
	if token == "" {
		return false, &state.ValueError{Message: "State cache backfill lock token must not be empty"}
	}
	if ttl <= 0 {
		return false, &state.ValueError{Message: "State cache backfill lock TTL must be positive"}
	}
	if !c.enabled {
		// A disabled cache has no payload to guard, so there is nothing to
		// serialize: report the lock as taken by this caller.
		return true, nil
	}
	connection, err := c.connection()
	if err != nil {
		return false, err
	}
	acquired, err := connection.SetNX(ctx, backfillLockKey(c.settings.Prefix, kind, scope), token, ttl).Result()
	if err != nil {
		return false, &state.CacheUnavailable{Message: "State cache backfill lock unavailable"}
	}
	return acquired, nil
}

// RenewBackfillLock extends the lock only when token still owns it. A false
// result with a nil error means the lease was lost (expired or taken over);
// a backend error returns *state.CacheUnavailable and leaves ownership
// unverifiable.
func (c *Client) RenewBackfillLock(ctx context.Context, kind, scope, token string, ttl time.Duration) (bool, error) {
	if token == "" {
		return false, &state.ValueError{Message: "State cache backfill lock token must not be empty"}
	}
	if ttl <= 0 {
		return false, &state.ValueError{Message: "State cache backfill lock TTL must be positive"}
	}
	if !c.enabled {
		return true, nil
	}
	connection, err := c.connection()
	if err != nil {
		return false, err
	}
	renewed, err := renewBackfillLockScript.Run(ctx, connection,
		[]string{backfillLockKey(c.settings.Prefix, kind, scope)},
		token, backfillTTL(ttl)).Int64()
	if err != nil {
		return false, &state.CacheUnavailable{Message: "State cache backfill lock unavailable"}
	}
	return renewed == 1, nil
}

// ReleaseBackfillLock deletes the lock only when token still owns it; it is
// idempotent. A backend error is reported, but the holder must already have
// given up filling when that happens, so the error only feeds diagnostics.
func (c *Client) ReleaseBackfillLock(ctx context.Context, kind, scope, token string) error {
	if token == "" {
		return &state.ValueError{Message: "State cache backfill lock token must not be empty"}
	}
	if !c.enabled {
		return nil
	}
	connection, err := c.connection()
	if err != nil {
		return err
	}
	if _, err := releaseBackfillLockScript.Run(ctx, connection,
		[]string{backfillLockKey(c.settings.Prefix, kind, scope)},
		token).Int64(); err != nil {
		return &state.CacheUnavailable{Message: "State cache backfill lock unavailable"}
	}
	return nil
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

// Attach wires the backend into the shared state stores: read-through display
// cache reads use it as the StateCache backend. It is the one-line integration
// point for cmd/rag-service after state.New, because internal/state cannot
// import this package. Search results are no longer cached here.
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
	return nil
}
