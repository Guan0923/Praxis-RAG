// Package mq is the RocketMQ transport for the G7 async pipeline. It adapts the
// official Go client (apache/rocketmq-clients/golang/v5) to the narrow publisher
// and consumer seams used by internal/state and the future worker binaries.
//
// There is no broker in CI: the adapters are unit-tested through fake transports
// and the SDK-backed transports are only constructed when an endpoint is
// configured.
package mq

import (
	"strconv"
	"strings"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// Defaults mirror the Python queue settings.
const (
	DefaultSearchTopic       = "praxis_search"
	DefaultSearchGroup       = "praxis_search_workers"
	DefaultPublishTimeout    = 10 * time.Second
	DefaultVisibilityTimeout = 60 * time.Second
	DefaultAwaitDuration     = 5 * time.Second
	DefaultBatchSize         = int32(16)
	DefaultPollInterval      = 500 * time.Millisecond
)

// ConfigError is a validation failure in mq-level settings (the document and
// cache settings reuse state's Python-equivalent validation).
type ConfigError struct{ Message string }

// Error implements error.
func (e *ConfigError) Error() string { return e.Message }

func newConfigError(message string) *ConfigError { return &ConfigError{Message: message} }

// Settings is the resolved RocketMQ configuration for one process.
type Settings struct {
	Endpoint  string
	Namespace string
	TLS       bool
	AccessKey string
	SecretKey string

	DocumentTopic string
	DocumentGroup string
	SearchTopic   string
	SearchGroup   string

	PublishTimeout    time.Duration
	VisibilityTimeout time.Duration
	AwaitDuration     time.Duration
}

// SettingsFromEnv resolves the queue settings with the same validation and
// messages as the Python cache/document/search queue settings. Cache and
// document validation is reused from internal/state; only the search queue
// separation is defined here (matching rag_mcp.state.search_queue).
func SettingsFromEnv() (Settings, error) {
	cache := state.RocketMQSettingsFromEnv()
	document, err := state.DocumentQueueSettingsFromEnv()
	if err != nil {
		return Settings{}, err
	}
	settings := Settings{
		Endpoint:      cache.Endpoint,
		Namespace:     cache.Namespace,
		TLS:           cache.TLS,
		AccessKey:     cache.AccessKey,
		SecretKey:     cache.SecretKey,
		DocumentTopic: document.Topic,
		DocumentGroup: document.Group,
		SearchTopic:   strings.TrimSpace(envOrDefault("RAG_SEARCH_TOPIC", DefaultSearchTopic)),
		SearchGroup:   strings.TrimSpace(envOrDefault("RAG_SEARCH_GROUP", DefaultSearchGroup)),
	}
	if settings.SearchTopic == "" || settings.SearchGroup == "" {
		return Settings{}, newConfigError("Search topic and consumer group must not be empty")
	}
	if settings.SearchTopic == cache.Topic || settings.SearchTopic == document.Topic ||
		settings.SearchGroup == cache.Group || settings.SearchGroup == document.Group {
		return Settings{}, newConfigError("Search topic/group must be separate from cache and document queues")
	}
	if settings.PublishTimeout, err = durationEnv("RAG_ROCKETMQ_PUBLISH_TIMEOUT_SECONDS", DefaultPublishTimeout); err != nil {
		return Settings{}, err
	}
	if settings.VisibilityTimeout, err = durationEnv("RAG_ROCKETMQ_VISIBILITY_SECONDS", DefaultVisibilityTimeout); err != nil {
		return Settings{}, err
	}
	if settings.AwaitDuration, err = durationEnv("RAG_ROCKETMQ_AWAIT_SECONDS", DefaultAwaitDuration); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

func envOrDefault(name, fallback string) string {
	if value, ok := lookupEnv(name); ok {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(envOrDefault(name, ""))
	if raw == "" {
		return fallback, nil
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds <= 0 {
		return 0, newConfigError(name + " must be positive")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// Configured reports whether a broker endpoint is set.
func (s Settings) Configured() bool { return strings.TrimSpace(s.Endpoint) != "" }

// Validate re-checks the settings that a hand-built Settings value can get
// wrong. Endpoint absence is reported later as ErrNotConfigured so callers can
// run the state layer without a broker; zero durations fall back to defaults in
// the publisher/consumer constructors.
func (s Settings) Validate() error {
	if strings.TrimSpace(s.DocumentTopic) == "" || strings.TrimSpace(s.DocumentGroup) == "" {
		return newConfigError("Document topic and consumer group must not be empty")
	}
	if strings.TrimSpace(s.SearchTopic) == "" || strings.TrimSpace(s.SearchGroup) == "" {
		return newConfigError("Search topic and consumer group must not be empty")
	}
	if s.SearchTopic == s.DocumentTopic || s.SearchGroup == s.DocumentGroup {
		return newConfigError("Search topic/group must be separate from cache and document queues")
	}
	return nil
}
