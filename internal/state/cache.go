package state

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"
)

// CacheUnavailable is a sanitized cache failure; callers fall back to the
// authoritative database. It mirrors rag_mcp.state.cache.CacheUnavailable.
//
// Reason keeps the internal cause distinguishable after the Redis client folded
// network outages and single-entry corruption into one error type: a corrupt
// list only affects itself, a caller cancellation is not a Redis fault, and only
// availability failures (connection errors, timeouts) feed the display-cache
// circuit breaker. The zero Reason keeps the historical "backend unavailable"
// meaning.
type CacheUnavailable struct {
	Message string
	Reason  CacheFailureReason
}

// CacheFailureReason classifies one *CacheUnavailable.
type CacheFailureReason string

const (
	// CacheFailureNetwork marks connectivity/timeout failures that make the
	// backend unusable for every key. These are the failures the display-cache
	// circuit breaker counts.
	CacheFailureNetwork CacheFailureReason = "network"
	// CacheFailureCorrupt marks one cached payload that cannot be decoded. The
	// backend itself answered, so only this entry is affected: it is repaired
	// by the MySQL fallback of the very request that hit it.
	CacheFailureCorrupt CacheFailureReason = "corrupt"
	// CacheFailureCanceled marks a read aborted because the caller canceled the
	// request. It says nothing about backend health.
	CacheFailureCanceled CacheFailureReason = "canceled"
)

// Error implements error.
func (e *CacheUnavailable) Error() string { return e.Message }

// AvailabilityFailure reports whether this failure is a backend availability
// fault (network outage, or the legacy unclassified failures) as opposed to a
// single corrupt entry or a caller cancellation.
func (e *CacheUnavailable) AvailabilityFailure() bool {
	return e.Reason == "" || e.Reason == CacheFailureNetwork
}

func newCacheUnavailable(message string) *CacheUnavailable {
	return &CacheUnavailable{Message: message}
}

// CacheRetryable is a sanitized, retryable read-through cache failure: the
// caller should retry the read later. The synchronizer returns it for a lost
// backfill lease, and for a backfill wait timeout. It never replaces the
// documented outage contract (Redis failures fall back to MySQL); see
// CacheSynchronizer.GetOrLoad.
type CacheRetryable struct{ Message string }

// Error implements error.
func (e *CacheRetryable) Error() string { return e.Message }

func newCacheRetryable(message string) *CacheRetryable {
	return &CacheRetryable{Message: message}
}

// Sanitized, stable messages for retryable read-through failures. They never
// carry backend details, tokens or credentials.
const (
	cacheWaitRetryableMessage  = "State cache backfill wait timed out; retry later"
	cacheLeaseRetryableMessage = "State cache backfill lease was lost; retry later"
)

// ErrCacheBackfillLeaseLost means the snapshot was not published because the
// caller no longer owns the backfill lock.
var ErrCacheBackfillLeaseLost = errors.New(cacheLeaseRetryableMessage)

// NewCacheUnavailableNetwork builds an availability failure (connection error,
// timeout, closed backend).
func NewCacheUnavailableNetwork(message string) *CacheUnavailable {
	return &CacheUnavailable{Message: message, Reason: CacheFailureNetwork}
}

// NewCacheUnavailableCorrupt builds a single-entry decode failure.
func NewCacheUnavailableCorrupt(message string) *CacheUnavailable {
	return &CacheUnavailable{Message: message, Reason: CacheFailureCorrupt}
}

// NewCacheUnavailableCanceled builds a failure caused by the caller canceling
// the request.
func NewCacheUnavailableCanceled(message string) *CacheUnavailable {
	return &CacheUnavailable{Message: message, Reason: CacheFailureCanceled}
}

// IsCacheAvailabilityFailure reports whether err is a *CacheUnavailable that
// makes the backend unusable (network outage or the legacy unclassified kind).
func IsCacheAvailabilityFailure(err error) bool {
	var unavailable *CacheUnavailable
	return errors.As(err, &unavailable) && unavailable.AvailabilityFailure()
}

// ViewBackpressureMessage is the user-visible "fallback budget exhausted"
// detail served by REST (503) and MCP tool errors.
const ViewBackpressureMessage = "服务繁忙，请稍后重试"

// ViewBackpressureError reports that the instance-level MySQL fallback budget
// for display read views is exhausted. It is the only state-layer error the
// runtime must not retry against the authoritative repository: the request was
// rejected precisely to protect that database, so the error travels unchanged
// through the cached views and the runtime to REST/MCP.
type ViewBackpressureError struct{ Message string }

// Error implements error.
func (e *ViewBackpressureError) Error() string { return e.Message }

// NewViewBackpressureError builds the display-query fallback rejection.
func NewViewBackpressureError() *ViewBackpressureError {
	return &ViewBackpressureError{Message: ViewBackpressureMessage}
}

// IsViewBackpressure reports whether err is a *ViewBackpressureError.
func IsViewBackpressure(err error) bool {
	var backpressure *ViewBackpressureError
	return errors.As(err, &backpressure)
}

// StateCache is the read-through snapshot backend. In production this is the
// Redis implementation of rag_mcp.state.cache.RedisStateCache; the Go port keeps
// it behind this interface so the state layer compiles and is testable without a
// Redis dependency. Values must be JSON-marshalable.
//
// The backfill lock methods serialize MySQL backfill across instances for one
// cache key: AcquireBackfillLock is a SET NX PX with a caller-supplied token,
// RenewBackfillLock extends the lease only for that token, and
// ReleaseBackfillLock deletes the lock only when the token still owns it.
// Put verifies the same token atomically with publication. Lease loss returns
// ErrCacheBackfillLeaseLost; a rejected revision returns (false, nil).
// A backend outage is reported as *CacheUnavailable and leaves ownership
// unknown to the caller.
type StateCache interface {
	Enabled() bool
	Get(ctx context.Context, kind, scope string) (bool, any, error)
	Put(ctx context.Context, kind, scope, token string, revision int64, value any, deleted bool) (bool, error)
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
func (DisabledStateCache) Put(context.Context, string, string, string, int64, any, bool) (bool, error) {
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
	// BreakerThreshold is the number of consecutive backend availability
	// failures that trips the display-cache circuit breaker.
	BreakerThreshold int
	// BreakerCooldownSeconds is how long a tripped breaker stays open before
	// one half-open probe request is allowed through.
	BreakerCooldownSeconds int
	// FallbackConcurrency bounds the concurrent display-query MySQL fallbacks
	// (cache misses and outage fallbacks) of one service instance. Waiting for
	// a shared result does not consume a slot.
	FallbackConcurrency int
}

// Display-cache circuit defaults: five consecutive availability failures trip
// the breaker, it stays open for ten seconds before one probe, and one instance
// keeps at most four concurrent display fallbacks against MySQL. Multi-replica
// deployments therefore land at roughly replicas x 4 total fallback queries and
// must budget for uploads/publications sharing the same database.
const (
	defaultBreakerThreshold        = 5
	defaultBreakerCooldownSeconds  = 10
	defaultFallbackConcurrency     = 4
	breakerThresholdEnvName        = "RAG_REDIS_BREAKER_THRESHOLD"
	breakerCooldownEnvName         = "RAG_REDIS_BREAKER_COOLDOWN_SECONDS"
	fallbackConcurrencyEnvName     = "RAG_CACHE_FALLBACK_CONCURRENCY"
)

// DefaultCacheSettings returns the dataclass defaults.
func DefaultCacheSettings() CacheSettings {
	return CacheSettings{
		Prefix:                 "rag:state:v1",
		TTLSeconds:            60,
		TimeoutSeconds:         0.3,
		BreakerThreshold:       defaultBreakerThreshold,
		BreakerCooldownSeconds: defaultBreakerCooldownSeconds,
		FallbackConcurrency:    defaultFallbackConcurrency,
	}
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
	settings.BreakerThreshold, err = positiveIntEnv(breakerThresholdEnvName,
		defaultBreakerThreshold)
	if err != nil {
		return CacheSettings{}, err
	}
	settings.BreakerCooldownSeconds, err = positiveIntEnv(breakerCooldownEnvName,
		defaultBreakerCooldownSeconds)
	if err != nil {
		return CacheSettings{}, err
	}
	settings.FallbackConcurrency, err = positiveIntEnv(fallbackConcurrencyEnvName,
		defaultFallbackConcurrency)
	if err != nil {
		return CacheSettings{}, err
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

// positiveIntEnv reads one strictly positive integer setting.
func positiveIntEnv(name string, fallback int) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(envOrDefault(name, strconv.Itoa(fallback))))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
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
	if s.BreakerThreshold <= 0 {
		return fmt.Errorf("%s must be a positive integer", breakerThresholdEnvName)
	}
	if s.BreakerCooldownSeconds <= 0 {
		return fmt.Errorf("%s must be a positive integer", breakerCooldownEnvName)
	}
	if s.FallbackConcurrency <= 0 {
		return fmt.Errorf("%s must be a positive integer", fallbackConcurrencyEnvName)
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
