package state

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"
)

// CacheUnavailable is a sanitized backend failure. Callers fall back to the
// authoritative database only when the cache is disabled; while the cache is
// enabled the synchronizer maps an unavailable backend to *CacheRetryable
// instead (see CacheSynchronizer.GetOrLoad). It mirrors
// rag_mcp.state.cache.CacheUnavailable.
type CacheUnavailable struct{ Message string }

// Error implements error.
func (e *CacheUnavailable) Error() string { return e.Message }

func newCacheUnavailable(message string) *CacheUnavailable {
	return &CacheUnavailable{Message: message}
}

// CacheRetryable is a sanitized, retryable read-through cache failure: the
// caller should retry the read later. The synchronizer returns it for a
// backfill wait timeout, a lost backfill lease, or a backend outage that
// leaves lock ownership unverifiable while the cache is enabled. It
// deliberately replaces the legacy behavior of falling back to MySQL on a
// Redis outage.
type CacheRetryable struct{ Message string }

// Error implements error.
func (e *CacheRetryable) Error() string { return e.Message }

func newCacheRetryable(message string) *CacheRetryable {
	return &CacheRetryable{Message: message}
}

// Sanitized, stable messages for retryable read-through failures. They never
// carry backend details, tokens or credentials.
const (
	cacheReadRetryableMessage  = "State cache read is unavailable; retry later"
	cacheLockRetryableMessage  = "State cache backfill lock is unavailable; retry later"
	cacheWaitRetryableMessage  = "State cache backfill wait timed out; retry later"
	cacheLeaseRetryableMessage = "State cache backfill lease was lost; retry later"
)

// StateCache is the read-through snapshot backend. In production this is the
// Redis implementation of rag_mcp.state.cache.RedisStateCache; the Go port keeps
// it behind this interface so the state layer compiles and is testable without a
// Redis dependency. Values must be JSON-marshalable.
//
// The backfill lock methods serialize MySQL backfill across instances for one
// cache key: AcquireBackfillLock is a SET NX PX with a caller-supplied token,
// RenewBackfillLock extends the lease only for that token, and
// ReleaseBackfillLock deletes the lock only when the token still owns it.
// A backend outage is reported as *CacheUnavailable and leaves ownership
// unknown to the caller.
type StateCache interface {
	Enabled() bool
	Get(ctx context.Context, kind, scope string) (bool, any, error)
	Put(ctx context.Context, kind, scope string, revision int64, value any, deleted bool) (bool, error)
	AcquireBackfillLock(ctx context.Context, kind, scope, token string, ttl time.Duration) (bool, error)
	RenewBackfillLock(ctx context.Context, kind, scope, token string, ttl time.Duration) (bool, error)
	ReleaseBackfillLock(ctx context.Context, kind, scope, token string) error
	Close() error
}

// DisabledStateCache is the backend used when no cache client is configured.
type DisabledStateCache struct{}

// Enabled reports that the cache is disabled.
func (DisabledStateCache) Enabled() bool { return false }

// Get always misses.
func (DisabledStateCache) Get(context.Context, string, string) (bool, any, error) {
	return false, nil, nil
}

// Put is a no-op.
func (DisabledStateCache) Put(context.Context, string, string, int64, any, bool) (bool, error) {
	return false, nil
}

// AcquireBackfillLock reports a free lock (there is no cache to guard).
func (DisabledStateCache) AcquireBackfillLock(context.Context, string, string, string, time.Duration) (bool, error) {
	return true, nil
}

// RenewBackfillLock reports a held lock.
func (DisabledStateCache) RenewBackfillLock(context.Context, string, string, string, time.Duration) (bool, error) {
	return true, nil
}

// ReleaseBackfillLock is a no-op.
func (DisabledStateCache) ReleaseBackfillLock(context.Context, string, string, string) error {
	return nil
}

// Close is a no-op.
func (DisabledStateCache) Close() error { return nil }

// CacheSettings mirrors rag_mcp.state.cache.StateCacheSettings.
type CacheSettings struct {
	URL            string
	Prefix         string
	TTLSeconds     int
	TimeoutSeconds float64
	// Enabled explicitly overrides the URL-derived default when non-nil.
	Enabled *bool
}

// DefaultCacheSettings returns the dataclass defaults.
func DefaultCacheSettings() CacheSettings {
	return CacheSettings{Prefix: "rag:state:v1", TTLSeconds: 60, TimeoutSeconds: 0.3}
}

// RandomTTLSeconds draws the randomized payload TTL for one display-cache
// write: an integer number of seconds drawn uniformly from
// [T-floor(T/5), T], where T is the RAG_REDIS_TTL_SECONDS upper bound.
// With the default T=60 the range is 48~60 seconds. Randomizing the expiry
// staggers concurrent revalidations of one key so synchronized expiry no
// longer turns one miss into a thundering herd of MySQL backfills. The
// revision watermark stays un-expiring, so an older refill can never
// resurrect deleted data after the payload lapses.
func RandomTTLSeconds(ttlSeconds int) int {
	if ttlSeconds <= 0 {
		return ttlSeconds
	}
	minimum := ttlSeconds - ttlSeconds/5
	return minimum + rand.IntN(ttlSeconds-minimum+1)
}

// CacheSettingsFromEnv mirrors StateCacheSettings.from_env, including its
// validation order and error messages.
func CacheSettingsFromEnv() (CacheSettings, error) {
	settings := DefaultCacheSettings()
	rawEnabled := strings.ToLower(strings.TrimSpace(os.Getenv("RAG_REDIS_ENABLED")))
	if rawEnabled != "" {
		switch rawEnabled {
		case "1", "true", "yes", "on", "0", "false", "no", "off":
			enabled := rawEnabled == "1" || rawEnabled == "true" || rawEnabled == "yes" || rawEnabled == "on"
			settings.Enabled = &enabled
		default:
			return CacheSettings{}, fmt.Errorf("RAG_REDIS_ENABLED must be a boolean")
		}
	}
	ttl, err := strconv.Atoi(strings.TrimSpace(envOrDefault("RAG_REDIS_TTL_SECONDS", "60")))
	if err != nil {
		return CacheSettings{}, fmt.Errorf("RAG_REDIS_TTL_SECONDS must be a positive integer")
	}
	timeout, err := strconv.ParseFloat(strings.TrimSpace(envOrDefault("RAG_REDIS_TIMEOUT_SECONDS", "0.3")), 64)
	if err != nil {
		return CacheSettings{}, fmt.Errorf("RAG_REDIS_TIMEOUT_SECONDS must be a positive finite number")
	}
	settings.URL = strings.TrimSpace(os.Getenv("RAG_REDIS_URL"))
	settings.Prefix = strings.TrimSpace(envOrDefault("RAG_REDIS_PREFIX", "rag:state:v1"))
	settings.TTLSeconds = ttl
	settings.TimeoutSeconds = timeout
	if err := settings.Validate(); err != nil {
		return CacheSettings{}, err
	}
	return settings, nil
}

func envOrDefault(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

// Validate mirrors StateCacheSettings.__post_init__.
func (s CacheSettings) Validate() error {
	if s.TTLSeconds <= 0 {
		return fmt.Errorf("RAG_REDIS_TTL_SECONDS must be a positive integer")
	}
	if math.IsNaN(s.TimeoutSeconds) || math.IsInf(s.TimeoutSeconds, 0) || s.TimeoutSeconds <= 0 {
		return fmt.Errorf("RAG_REDIS_TIMEOUT_SECONDS must be a positive finite number")
	}
	if s.Prefix == "" {
		return fmt.Errorf("RAG_REDIS_PREFIX must not be empty")
	}
	return nil
}

// backendEnabled mirrors RedisStateCache.enabled without an injected client:
// an explicit setting wins, otherwise a configured URL enables the cache.
func (s CacheSettings) backendEnabled() bool {
	if s.Enabled != nil {
		return *s.Enabled
	}
	return s.URL != ""
}

// The cache refresh queue uses Kafka (see BrokerSettingsFromEnv).
//
// BrokerSettings mirrors the Kafka broker/queue settings shared with the
// transport packages (rag_mcp.state.cache_queue). An empty BootstrapServers
// means the process runs brokerless and events stay in the durable outbox.
type BrokerSettings struct {
	BootstrapServers string
	CacheTopic       string
	CacheGroup       string
	ClientID         string
	SecurityProtocol string
	SASLMechanism    string
	SASLUsername     string
	SASLPassword     string
	TLSCA            string
	TLSInsecure      bool
}

// BrokerSettingsFromEnv reads the RAG_KAFKA_*/RAG_CACHE_* contract. The cache
// topic/group defaults match the Python queue settings; every string setting is
// trimmed like the Python parser.
func BrokerSettingsFromEnv() BrokerSettings {
	return BrokerSettings{
		BootstrapServers: strings.TrimSpace(os.Getenv("RAG_KAFKA_BOOTSTRAP_SERVERS")),
		CacheTopic:       strings.TrimSpace(envOrDefault("RAG_CACHE_TOPIC", "praxis_state_cache_refresh")),
		CacheGroup:       strings.TrimSpace(envOrDefault("RAG_CACHE_GROUP", "praxis_state_cache_refresh_workers")),
		ClientID:         strings.TrimSpace(envOrDefault("RAG_KAFKA_CLIENT_ID", "praxis-rag")),
		SecurityProtocol: strings.TrimSpace(envOrDefault("RAG_KAFKA_SECURITY_PROTOCOL", "PLAINTEXT")),
		SASLMechanism:    strings.TrimSpace(os.Getenv("RAG_KAFKA_SASL_MECHANISM")),
		SASLUsername:     os.Getenv("RAG_KAFKA_SASL_USERNAME"),
		SASLPassword:     os.Getenv("RAG_KAFKA_SASL_PASSWORD"),
		TLSCA:            strings.TrimSpace(os.Getenv("RAG_KAFKA_TLS_CA")),
		TLSInsecure:      isTruthyValue(os.Getenv("RAG_KAFKA_TLS_INSECURE")),
	}
}

// isTruthyValue mirrors the Python truthy spellings used for queue settings.
func isTruthyValue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// ValidateCacheEventID mirrors the cache queue send's event id guard.
func ValidateCacheEventID(eventID string) bool {
	if len(eventID) != 32 {
		return false
	}
	for index := 0; index < len(eventID); index++ {
		char := eventID[index]
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
