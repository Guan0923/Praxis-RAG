// Package mq is the Kafka transport for the G7 async pipeline. It adapts
// github.com/segmentio/kafka-go to the narrow publisher and consumer seams used
// by internal/state and the worker binaries.
//
// There is no broker in CI: the adapters are unit-tested through fake transports
// and the SDK-backed transports are only constructed when bootstrap servers are
// configured.
package mq

import (
	"strconv"
	"strings"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// Defaults mirror the shared Kafka queue contract (RAG_KAFKA_* + the queue
// topic/group envs). They are kept exported for workers, tests, and docs.
const (
	DefaultClientID         = "praxis-rag"
	DefaultSecurityProtocol = "PLAINTEXT"

	DefaultCacheTopic = "praxis_state_cache_refresh"
	DefaultCacheGroup = "praxis_state_cache_refresh_workers"

	DefaultDocumentTopic = "praxis_document_ingest"
	DefaultDocumentGroup = "praxis_document_ingest_workers"

	DefaultSearchTopic = "praxis_search"
	DefaultSearchGroup = "praxis_search_workers"

	DefaultDocumentDLQTopic = "praxis_document_ingest_dead_letters"
	DefaultSearchDLQTopic   = "praxis_search_dead_letters"
	DefaultCacheDLQTopic    = "praxis_state_cache_refresh_dead_letters"

	// DefaultCacheDLQGroup is the consumer group of the cache dead-letter
	// handler. It must stay distinct from every business group and from the
	// document/search DLQ groups.
	DefaultCacheDLQGroup = "praxis_state_cache_refresh_dead_letters"

	DefaultPublishTimeout    = 10 * time.Second
	DefaultVisibilityTimeout = 60 * time.Second
	DefaultAwaitDuration     = 5 * time.Second
	DefaultBatchSize         = int32(16)
	// MaxConsumerBatchSize bounds one transport receive and the local prefetch
	// queue depth of a consumer.
	MaxConsumerBatchSize = int32(32)
	// DefaultConsumerConcurrency keeps one handler goroutine per worker unless a
	// deployment explicitly raises it; each extra goroutine can hold one
	// document's parse in flight.
	DefaultConsumerConcurrency = 1
	// MaxConsumerConcurrency bounds the handler goroutines of one worker.
	MaxConsumerConcurrency = 64
	DefaultPollInterval    = 500 * time.Millisecond
	DefaultRetryBaseDelay  = 100 * time.Millisecond
	DefaultRetryMaxDelay   = 5 * time.Second
)

// ConfigError is a validation failure in mq-level settings (Kafka bootstrap,
// security, topic/group separation) or a hand-built ConsumerConfig.
type ConfigError struct{ Message string }

// Error implements error.
func (e *ConfigError) Error() string { return e.Message }

func newConfigError(message string) *ConfigError { return &ConfigError{Message: message} }

// Settings is the resolved Kafka configuration for one process.
type Settings struct {
	BootstrapServers []string
	ClientID         string
	SecurityProtocol string
	SASLMechanism    string
	SASLUsername     string
	SASLPassword     string
	TLSCA            string
	TLSInsecure      bool

	CacheTopic string
	CacheGroup string

	DocumentTopic string
	DocumentGroup string

	SearchTopic string
	SearchGroup string

	DocumentDLQTopic string
	SearchDLQTopic   string
	// CacheDLQTopic/CacheDLQGroup belong to the cache read-model consumer
	// (rag-cache-worker): a malformed cache event is dead-lettered there instead
	// of blocking the cache topic.
	CacheDLQTopic string
	CacheDLQGroup string

	PublishTimeout time.Duration
}

// SettingsFromEnv resolves the Kafka queue settings from the shared env contract.
// The broker/security/topic values come from internal/state (BrokerSettings and
// DocumentQueueSettings) so producer, consumer, and state agree; the search and
// DLQ topics/groups plus the publish timeout are mq-level settings.
func SettingsFromEnv() (Settings, error) {
	broker := state.BrokerSettingsFromEnv()
	document, err := state.DocumentQueueSettingsFromEnv()
	if err != nil {
		return Settings{}, err
	}
	publishTimeout, err := durationEnv("RAG_KAFKA_PUBLISH_TIMEOUT_SECONDS", DefaultPublishTimeout)
	if err != nil {
		return Settings{}, err
	}
	settings := Settings{
		BootstrapServers: splitBootstrapServers(broker.BootstrapServers),
		ClientID:         strings.TrimSpace(broker.ClientID),
		SecurityProtocol: strings.TrimSpace(broker.SecurityProtocol),
		SASLMechanism:    strings.TrimSpace(broker.SASLMechanism),
		SASLUsername:     broker.SASLUsername,
		SASLPassword:     broker.SASLPassword,
		TLSCA:            strings.TrimSpace(broker.TLSCA),
		TLSInsecure:      broker.TLSInsecure,
		CacheTopic:       broker.CacheTopic,
		CacheGroup:       broker.CacheGroup,
		DocumentTopic:    document.Topic,
		DocumentGroup:    document.Group,
		SearchTopic:      strings.TrimSpace(envOrDefault("RAG_SEARCH_TOPIC", DefaultSearchTopic)),
		SearchGroup:      strings.TrimSpace(envOrDefault("RAG_SEARCH_GROUP", DefaultSearchGroup)),
		DocumentDLQTopic: strings.TrimSpace(envOrDefault("RAG_DOCUMENT_DLQ_TOPIC", DefaultDocumentDLQTopic)),
		SearchDLQTopic:   strings.TrimSpace(envOrDefault("RAG_SEARCH_DLQ_TOPIC", DefaultSearchDLQTopic)),
		CacheDLQTopic:    strings.TrimSpace(envOrDefault("RAG_CACHE_DLQ_TOPIC", DefaultCacheDLQTopic)),
		CacheDLQGroup:    strings.TrimSpace(envOrDefault("RAG_CACHE_DLQ_GROUP", DefaultCacheDLQGroup)),
		PublishTimeout:   publishTimeout,
	}
	if settings.ClientID == "" {
		settings.ClientID = DefaultClientID
	}
	if settings.SecurityProtocol == "" {
		settings.SecurityProtocol = DefaultSecurityProtocol
	}
	if err := settings.Validate(); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

// splitBootstrapServers parses the comma-separated bootstrap list.
func splitBootstrapServers(raw string) []string {
	servers := make([]string, 0, 2)
	for _, item := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(item)
		if trimmed != "" {
			servers = append(servers, trimmed)
		}
	}
	return servers
}

// Configured reports whether Kafka bootstrap servers are set. An empty list
// means brokerless (the service runs fail-closed until a broker is configured).
func (s Settings) Configured() bool { return len(s.BootstrapServers) > 0 }

// Validate re-checks the Kafka settings a hand-built value can get wrong.
// Broker absence is reported later as ErrNotConfigured so the state layer can run
// without a broker; zero durations fall back to defaults in the constructors.
func (s Settings) Validate() error {
	if err := validateSecurity(s); err != nil {
		return err
	}
	if strings.TrimSpace(s.DocumentTopic) == "" || strings.TrimSpace(s.DocumentGroup) == "" {
		return newConfigError("Document topic and consumer group must not be empty")
	}
	if strings.TrimSpace(s.SearchTopic) == "" || strings.TrimSpace(s.SearchGroup) == "" {
		return newConfigError("Search topic and consumer group must not be empty")
	}
	if s.SearchTopic == s.DocumentTopic || s.SearchGroup == s.DocumentGroup {
		return newConfigError("Search topic/group must be separate from the cache and document queues")
	}
	if s.CacheTopic != "" || s.CacheGroup != "" {
		if s.CacheTopic == s.DocumentTopic || s.CacheTopic == s.SearchTopic ||
			s.CacheGroup == s.DocumentGroup || s.CacheGroup == s.SearchGroup {
			return newConfigError("Cache topic/group must be separate from the document and search queues")
		}
	}
	if s.DocumentDLQTopic != "" {
		if s.DocumentDLQTopic == s.DocumentTopic || s.DocumentDLQTopic == s.SearchTopic ||
			s.DocumentDLQTopic == s.CacheTopic {
			return newConfigError("Document dead-letter topic must be separate from the business topics")
		}
	}
	if s.SearchDLQTopic != "" {
		if s.SearchDLQTopic == s.DocumentTopic || s.SearchDLQTopic == s.SearchTopic ||
			s.SearchDLQTopic == s.CacheTopic || s.SearchDLQTopic == s.DocumentDLQTopic {
			return newConfigError("Search dead-letter topic must be separate from the business topics")
		}
	}
	if s.CacheDLQTopic != "" {
		if s.CacheDLQTopic == s.DocumentTopic || s.CacheDLQTopic == s.SearchTopic ||
			s.CacheDLQTopic == s.CacheTopic || s.CacheDLQTopic == s.DocumentDLQTopic ||
			s.CacheDLQTopic == s.SearchDLQTopic {
			return newConfigError("Cache dead-letter topic must be separate from the business topics")
		}
	}
	if s.CacheDLQGroup != "" {
		if s.CacheDLQGroup == s.DocumentGroup || s.CacheDLQGroup == s.SearchGroup ||
			s.CacheDLQGroup == s.CacheGroup {
			return newConfigError("Cache dead-letter group must be separate from the business consumer groups")
		}
	}
	return nil
}

// validateSecurity checks the Kafka security protocol/SASL/topic settings that a
// hand-built Settings value can get wrong.
func validateSecurity(settings Settings) error {
	switch strings.ToUpper(strings.TrimSpace(settings.SecurityProtocol)) {
	case "", "PLAINTEXT", "SSL", "SASL_PLAINTEXT", "SASL_SSL":
	default:
		return newConfigError("RAG_KAFKA_SECURITY_PROTOCOL must be PLAINTEXT, SSL, SASL_PLAINTEXT or SASL_SSL")
	}
	switch strings.ToUpper(strings.TrimSpace(settings.SASLMechanism)) {
	case "", "PLAIN", "SCRAM-SHA-256", "SCRAM-SHA-512":
	default:
		return newConfigError("RAG_KAFKA_SASL_MECHANISM must be PLAIN, SCRAM-SHA-256 or SCRAM-SHA-512")
	}
	return nil
}

// DLQTopicFor maps a business topic to its configured dead-letter topic; an
// unknown topic returns "" (no dead-lettering).
func (s Settings) DLQTopicFor(topic string) string {
	switch strings.TrimSpace(topic) {
	case "":
		return ""
	case s.DocumentTopic:
		return s.DocumentDLQTopic
	case s.SearchTopic:
		return s.SearchDLQTopic
	case s.CacheTopic:
		return s.CacheDLQTopic
	default:
		return ""
	}
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
