package state

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// CacheUnavailable is a sanitized cache failure; callers must fall back to the
// authoritative database. It mirrors rag_mcp.state.cache.CacheUnavailable.
type CacheUnavailable struct{ Message string }

// Error implements error.
func (e *CacheUnavailable) Error() string { return e.Message }

func newCacheUnavailable(message string) *CacheUnavailable {
	return &CacheUnavailable{Message: message}
}

// StateCache is the read-through snapshot backend. In production this is the
// Redis implementation of rag_mcp.state.cache.RedisStateCache; the Go port keeps
// it behind this interface so the state layer compiles and is testable without a
// Redis dependency. Values must be JSON-marshalable.
type StateCache interface {
	Enabled() bool
	Get(ctx context.Context, kind, scope string) (bool, any, error)
	Put(ctx context.Context, kind, scope string, revision int64, value any, deleted bool) (bool, error)
	Close() error
}

// EventPublisher is the durable retry transport for cache refresh events
// (RocketMQ in production). Only 32-char lowercase event ids cross the queue.
type EventPublisher interface {
	Send(eventID string) error
	ConsumeOnce(callback func(eventID string) bool)
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

// RocketMQSettings mirrors rag_mcp.state.cache_queue.RocketMQSettings.
type RocketMQSettings struct {
	Endpoint  string
	Topic     string
	Group     string
	Namespace string
	AccessKey string
	SecretKey string
	TLS       bool
}

// RocketMQSettingsFromEnv mirrors RocketMQSettings.from_env.
func RocketMQSettingsFromEnv() RocketMQSettings {
	return RocketMQSettings{
		Endpoint:  strings.TrimSpace(os.Getenv("RAG_ROCKETMQ_ENDPOINT")),
		Topic:     strings.TrimSpace(envOrDefault("RAG_ROCKETMQ_TOPIC", "praxis_state_cache_refresh")),
		Group:     strings.TrimSpace(envOrDefault("RAG_ROCKETMQ_GROUP", "praxis_state_cache_refresh_workers")),
		Namespace: strings.TrimSpace(os.Getenv("RAG_ROCKETMQ_NAMESPACE")),
		AccessKey: os.Getenv("RAG_ROCKETMQ_ACCESS_KEY"),
		SecretKey: os.Getenv("RAG_ROCKETMQ_SECRET_KEY"),
		TLS:       isTruthyValue(os.Getenv("RAG_ROCKETMQ_TLS")),
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

// ValidateCacheEventID mirrors RocketMQCacheQueue.send's event id guard.
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
