package state

import (
	"context"
	"strings"
)

// DocumentNotificationPublisher publishes identifier-only schema-3 document
// notifications (Kafka in production; fakes in tests).
type DocumentNotificationPublisher interface {
	SendDocumentNotification(ctx context.Context, notification DocumentNotification) error
	Close() error
}

// DocumentQueueSettings mirrors document_queue.document_queue_settings(), using
// the Kafka broker/security settings shared with BrokerSettings.
type DocumentQueueSettings struct {
	BootstrapServers string
	ClientID         string
	SecurityProtocol string
	SASLMechanism    string
	SASLUsername     string
	SASLPassword     string
	TLSCA            string
	TLSInsecure      bool
	Topic            string
	Group            string
}

// DocumentQueueSettingsFromEnv validates the document topic/group and that they
// are separate from the cache refresh queue.
func DocumentQueueSettingsFromEnv() (DocumentQueueSettings, error) {
	cache := BrokerSettingsFromEnv()
	settings := DocumentQueueSettings{
		BootstrapServers: cache.BootstrapServers,
		ClientID:         cache.ClientID,
		SecurityProtocol: cache.SecurityProtocol,
		SASLMechanism:    cache.SASLMechanism,
		SASLUsername:     cache.SASLUsername,
		SASLPassword:     cache.SASLPassword,
		TLSCA:            cache.TLSCA,
		TLSInsecure:      cache.TLSInsecure,
		Topic:            strings.TrimSpace(envOrDefault("RAG_DOCUMENT_TOPIC", "praxis_document_ingest")),
		Group:            strings.TrimSpace(envOrDefault("RAG_DOCUMENT_GROUP", "praxis_document_ingest_workers")),
	}
	if settings.Topic == "" || settings.Group == "" {
		return DocumentQueueSettings{}, newValueError("Document topic and consumer group must not be empty")
	}
	if settings.Topic == cache.CacheTopic || settings.Group == cache.CacheGroup {
		return DocumentQueueSettings{}, newValueError("Document topic/group must be separate from cache refresh")
	}
	return settings, nil
}
