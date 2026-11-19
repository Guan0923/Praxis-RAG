package mq

import (
	"strings"
	"testing"
	"time"
)

// TestSettingsFromEnvDefaultsAndValidation resolves the shared Kafka env
// contract, including the new cache dead-letter topic/group.
func TestSettingsFromEnvDefaultsAndValidation(t *testing.T) {
	isolateEnv(t)
	settings, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("SettingsFromEnv() error = %v", err)
	}
	if settings.Configured() {
		t.Fatalf("expected brokerless settings: %+v", settings)
	}
	if settings.DocumentTopic != DefaultDocumentTopic || settings.DocumentGroup != DefaultDocumentGroup ||
		settings.CacheTopic != DefaultCacheTopic || settings.CacheGroup != DefaultCacheGroup ||
		settings.DocumentDLQTopic != DefaultDocumentDLQTopic ||
		settings.CacheDLQTopic != DefaultCacheDLQTopic ||
		settings.CacheDLQGroup != DefaultCacheDLQGroup {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
	if settings.ClientID != DefaultClientID || settings.SecurityProtocol != DefaultSecurityProtocol ||
		settings.PublishTimeout != DefaultPublishTimeout {
		t.Fatalf("unexpected security/timeout defaults: %+v", settings)
	}

	isolateEnv(t)
	t.Setenv("RAG_KAFKA_BOOTSTRAP_SERVERS", "broker-1:9092, broker-2:9093 ,")
	t.Setenv("RAG_KAFKA_CLIENT_ID", "worker-1")
	t.Setenv("RAG_KAFKA_PUBLISH_TIMEOUT_SECONDS", "2.5")
	t.Setenv("RAG_CACHE_DLQ_TOPIC", "cache-dlq")
	t.Setenv("RAG_CACHE_DLQ_GROUP", "cache-dlq-group")
	settings, err = SettingsFromEnv()
	if err != nil {
		t.Fatalf("configured SettingsFromEnv() error = %v", err)
	}
	if !settings.Configured() || len(settings.BootstrapServers) != 2 ||
		settings.BootstrapServers[0] != "broker-1:9092" || settings.BootstrapServers[1] != "broker-2:9093" {
		t.Fatalf("bootstrap servers = %+v", settings.BootstrapServers)
	}
	if settings.ClientID != "worker-1" || settings.PublishTimeout != 2500*time.Millisecond {
		t.Fatalf("configured settings = %+v", settings)
	}
	if settings.CacheDLQTopic != "cache-dlq" || settings.CacheDLQGroup != "cache-dlq-group" {
		t.Fatalf("cache DLQ settings = %+v", settings)
	}

	isolateEnv(t)
	t.Setenv("RAG_SEARCH_TOPIC", "praxis_document_ingest")
	t.Setenv("RAG_SEARCH_GROUP", "")
	t.Setenv("RAG_SEARCH_DLQ_TOPIC", "praxis_search_dead_letters")
	t.Setenv("RAG_SEARCH_DLQ_GROUP", "praxis_search_dead_letters")
	legacy, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("legacy RAG_SEARCH_* keys must be ignored, error = %v", err)
	}
	if legacy.DocumentTopic != DefaultDocumentTopic || legacy.DocumentDLQTopic != DefaultDocumentDLQTopic {
		t.Fatalf("legacy RAG_SEARCH_* keys leaked into settings: %+v", legacy)
	}
	isolateEnv(t)
	t.Setenv("RAG_KAFKA_PUBLISH_TIMEOUT_SECONDS", "0")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("invalid publish timeout error = %v", err)
	}
	isolateEnv(t)
	t.Setenv("RAG_DOCUMENT_TOPIC", "praxis_state_cache_refresh")
	if _, err := SettingsFromEnv(); err == nil {
		t.Fatal("document topic equal to cache topic accepted")
	}
	isolateEnv(t)
	t.Setenv("RAG_KAFKA_SECURITY_PROTOCOL", "KERBEROS")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_KAFKA_SECURITY_PROTOCOL") {
		t.Fatalf("invalid security protocol error = %v", err)
	}
	isolateEnv(t)
	t.Setenv("RAG_KAFKA_SASL_MECHANISM", "GSSAPI")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_KAFKA_SASL_MECHANISM") {
		t.Fatalf("invalid SASL mechanism error = %v", err)
	}
	isolateEnv(t)
	t.Setenv("RAG_CACHE_DLQ_TOPIC", "praxis_state_cache_refresh")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "Cache dead-letter topic") {
		t.Fatalf("cache DLQ collision error = %v", err)
	}
	isolateEnv(t)
	t.Setenv("RAG_CACHE_DLQ_GROUP", "praxis_state_cache_refresh_workers")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "Cache dead-letter group") {
		t.Fatalf("cache DLQ group collision error = %v", err)
	}
}

// TestSettingsValidateAndDLQTopicFor pins the topic/group separation rules and
// the topic -> DLQ mapping for the document and cache queues.
func TestSettingsValidateAndDLQTopicFor(t *testing.T) {
	settings := testSettings(true)
	if err := settings.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	cases := []struct {
		settings Settings
		topic    string
		want     string
	}{
		{Settings{DocumentTopic: "doc", DocumentDLQTopic: "doc-dlq"}, "doc", "doc-dlq"},
		{Settings{CacheTopic: "cache", CacheDLQTopic: "cache-dlq"}, "cache", "cache-dlq"},
		{testSettings(true), "other", ""},
	}
	for _, item := range cases {
		if topic := item.settings.DLQTopicFor(item.topic); topic != item.want {
			t.Fatalf("DLQTopicFor(%s) = %q, want %q", item.topic, topic, item.want)
		}
	}
	missing := testSettings(true)
	missing.DocumentTopic = ""
	if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "Document topic and consumer group must not be empty") {
		t.Fatalf("missing document topic error = %v", err)
	}
	cacheClash := testSettings(true)
	cacheClash.CacheGroup = cacheClash.DocumentGroup
	if err := cacheClash.Validate(); err == nil || !strings.Contains(err.Error(), "Cache topic/group must be separate") {
		t.Fatalf("cache separation error = %v", err)
	}
	sameDLQ := testSettings(true)
	sameDLQ.DocumentDLQTopic = sameDLQ.CacheTopic
	if err := sameDLQ.Validate(); err == nil || !strings.Contains(err.Error(), "Document dead-letter topic") {
		t.Fatalf("DLQ validation error = %v", err)
	}
	cacheDLQClash := testSettings(true)
	cacheDLQClash.CacheDLQTopic = "cache-topic"
	if err := cacheDLQClash.Validate(); err == nil || !strings.Contains(err.Error(), "Cache dead-letter topic") {
		t.Fatalf("cache DLQ validation error = %v", err)
	}
	cacheDLQGroupClash := testSettings(true)
	cacheDLQGroupClash.CacheDLQGroup = "cache-group"
	if err := cacheDLQGroupClash.Validate(); err == nil || !strings.Contains(err.Error(), "Cache dead-letter group") {
		t.Fatalf("cache DLQ group validation error = %v", err)
	}
}

// TestSettingsValidateDeleteAndReembedSeparation pins the delete and
// re-embedding queue contracts: a half-configured pair is rejected, and each
// topic/group must stay separate from every other queue's topic/group.
func TestSettingsValidateDeleteAndReembedSeparation(t *testing.T) {
	deleteMissingTopic := testSettings(true)
	deleteMissingTopic.DeleteGroup = "delete-group"
	if err := deleteMissingTopic.Validate(); err == nil ||
		!strings.Contains(err.Error(), "Delete topic and group must not be empty") {
		t.Fatalf("half-configured delete error = %v", err)
	}
	deleteMissingGroup := testSettings(true)
	deleteMissingGroup.DeleteTopic = "delete-topic"
	if err := deleteMissingGroup.Validate(); err == nil ||
		!strings.Contains(err.Error(), "Delete topic and group must not be empty") {
		t.Fatalf("half-configured delete error = %v", err)
	}

	deleteTopicClashes := []string{"doc-topic", "cache-topic", "doc-dlq", "cache-dlq"}
	for _, clash := range deleteTopicClashes {
		settings := testSettings(true)
		settings.DeleteTopic = clash
		settings.DeleteGroup = "delete-group"
		if err := settings.Validate(); err == nil ||
			!strings.Contains(err.Error(), "Delete topic must be separate") {
			t.Fatalf("delete topic clash %q error = %v", clash, err)
		}
	}
	deleteGroupClashes := []string{"doc-group", "cache-group", "cache-dlq-group"}
	for _, clash := range deleteGroupClashes {
		settings := testSettings(true)
		settings.DeleteTopic = "delete-topic"
		settings.DeleteGroup = clash
		if err := settings.Validate(); err == nil ||
			!strings.Contains(err.Error(), "Delete group must be separate") {
			t.Fatalf("delete group clash %q error = %v", clash, err)
		}
	}

	reembedMissingTopic := testSettings(true)
	reembedMissingTopic.ReembedGroup = "reembed-group"
	if err := reembedMissingTopic.Validate(); err == nil ||
		!strings.Contains(err.Error(), "Re-embedding topic and group must not be empty") {
		t.Fatalf("half-configured reembed error = %v", err)
	}
	reembedMissingGroup := testSettings(true)
	reembedMissingGroup.ReembedTopic = "reembed-topic"
	if err := reembedMissingGroup.Validate(); err == nil ||
		!strings.Contains(err.Error(), "Re-embedding topic and group must not be empty") {
		t.Fatalf("half-configured reembed error = %v", err)
	}

	reembedTopicClashes := []string{"doc-topic", "cache-topic", "doc-dlq", "cache-dlq"}
	for _, clash := range reembedTopicClashes {
		settings := testSettings(true)
		settings.ReembedTopic = clash
		settings.ReembedGroup = "reembed-group"
		if err := settings.Validate(); err == nil ||
			!strings.Contains(err.Error(), "Re-embedding topic must be separate") {
			t.Fatalf("reembed topic clash %q error = %v", clash, err)
		}
	}
	reembedGroupClashes := []string{"doc-group", "cache-group", "cache-dlq-group"}
	for _, clash := range reembedGroupClashes {
		settings := testSettings(true)
		settings.ReembedTopic = "reembed-topic"
		settings.ReembedGroup = clash
		if err := settings.Validate(); err == nil ||
			!strings.Contains(err.Error(), "Re-embedding group must be separate") {
			t.Fatalf("reembed group clash %q error = %v", clash, err)
		}
	}

	// Fully separate delete/reembed queues validate cleanly.
	both := testSettings(true)
	both.DeleteTopic = "delete-topic"
	both.DeleteGroup = "delete-group"
	both.ReembedTopic = "reembed-topic"
	both.ReembedGroup = "reembed-group"
	if err := both.Validate(); err != nil {
		t.Fatalf("separate delete/reembed queues rejected: %v", err)
	}
}
