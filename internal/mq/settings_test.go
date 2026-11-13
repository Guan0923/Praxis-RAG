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
		settings.SearchTopic != DefaultSearchTopic || settings.SearchGroup != DefaultSearchGroup ||
		settings.CacheTopic != DefaultCacheTopic || settings.CacheGroup != DefaultCacheGroup ||
		settings.DocumentDLQTopic != DefaultDocumentDLQTopic ||
		settings.SearchDLQTopic != DefaultSearchDLQTopic ||
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
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "Search topic/group must be separate") {
		t.Fatalf("search separation error = %v", err)
	}
	isolateEnv(t)
	t.Setenv("RAG_SEARCH_GROUP", "")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("empty search group error = %v", err)
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
	t.Setenv("RAG_SEARCH_DLQ_TOPIC", "praxis_search")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "dead-letter topic must be separate") {
		t.Fatalf("DLQ collision error = %v", err)
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
// the topic -> DLQ mapping for all three business queues.
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
		{Settings{SearchTopic: "search", SearchDLQTopic: "search-dlq"}, "search", "search-dlq"},
		{Settings{CacheTopic: "cache", CacheDLQTopic: "cache-dlq"}, "cache", "cache-dlq"},
		{testSettings(true), "other", ""},
	}
	for _, item := range cases {
		if topic := item.settings.DLQTopicFor(item.topic); topic != item.want {
			t.Fatalf("DLQTopicFor(%s) = %q, want %q", item.topic, topic, item.want)
		}
	}
	missing := testSettings(true)
	missing.SearchTopic = ""
	if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "Search topic and consumer group must not be empty") {
		t.Fatalf("missing search topic error = %v", err)
	}
	cacheClash := testSettings(true)
	cacheClash.CacheGroup = "search-group"
	if err := cacheClash.Validate(); err == nil || !strings.Contains(err.Error(), "Cache topic/group must be separate") {
		t.Fatalf("cache separation error = %v", err)
	}
	sameDLQ := testSettings(true)
	sameDLQ.DocumentDLQTopic = "search-topic"
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
