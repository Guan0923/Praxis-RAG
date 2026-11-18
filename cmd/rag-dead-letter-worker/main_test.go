package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/mq"
)

func testKafkaSettings() mq.Settings {
	return mq.Settings{
		BootstrapServers: []string{"localhost:9092"},
		ClientID:         mq.DefaultClientID,
		SecurityProtocol: mq.DefaultSecurityProtocol,
		DocumentTopic:    "doc-topic",
		DocumentGroup:    "doc-group",
		CacheTopic:       "cache-topic",
		CacheGroup:       "cache-group",
		DocumentDLQTopic: "praxis_document_ingest_dead_letters",
		CacheDLQTopic:    "praxis_state_cache_refresh_dead_letters",
		CacheDLQGroup:    "praxis_state_cache_refresh_dead_letters",
	}
}

func TestParseKind(t *testing.T) {
	kind, err := parseKind([]string{"--kind", "document"})
	if err != nil || kind != "document" {
		t.Fatalf("parseKind(document) = %q, %v", kind, err)
	}
	kind, err = parseKind([]string{"--kind=DOCUMENT"})
	if err != nil || kind != "document" {
		t.Fatalf("parseKind(DOCUMENT) = %q, %v", kind, err)
	}
	kind, err = parseKind([]string{"--kind", "cache"})
	if err != nil || kind != "cache" {
		t.Fatalf("parseKind(cache) = %q, %v", kind, err)
	}
	if _, err := parseKind(nil); err == nil || !strings.Contains(err.Error(), `--kind is required`) {
		t.Fatalf("missing kind error = %v", err)
	}
	if _, err := parseKind([]string{"--kind", "ingest"}); err == nil {
		t.Fatal("invalid kind accepted")
	}
	// Search is synchronous now: there is no search dead-letter source.
	if _, err := parseKind([]string{"--kind", "search"}); err == nil ||
		!strings.Contains(err.Error(), `must be "document" or "cache"`) {
		t.Fatalf("search kind error = %v", err)
	}
	if _, err := parseKind([]string{"--kind", "document", "extra"}); err == nil {
		t.Fatal("unexpected argument accepted")
	}
}

func TestDeadLetterTargetUsesExplicitKafkaTopics(t *testing.T) {
	settings := testKafkaSettings()
	t.Setenv("RAG_DOCUMENT_DLQ_GROUP", "praxis_document_dead_letters")
	target, err := deadLetterTarget("document", settings)
	if err != nil {
		t.Fatalf("deadLetterTarget(document) error = %v", err)
	}
	if target.topic != settings.DocumentDLQTopic || target.group != "praxis_document_dead_letters" ||
		target.sourceGroup != settings.DocumentGroup {
		t.Fatalf("document target = %+v", target)
	}
	// Cache dead letters use the cache consumer's independent DLQ topic/group.
	target, err = deadLetterTarget("cache", settings)
	if err != nil {
		t.Fatalf("deadLetterTarget(cache) error = %v", err)
	}
	if target.topic != settings.CacheDLQTopic || target.group != settings.CacheDLQGroup ||
		target.sourceGroup != settings.CacheGroup {
		t.Fatalf("cache target = %+v", target)
	}
}

func TestDeadLetterTargetValidation(t *testing.T) {
	settings := testKafkaSettings()
	t.Setenv("RAG_DOCUMENT_DLQ_GROUP", "bad group")
	if _, err := deadLetterTarget("document", settings); err == nil ||
		!strings.Contains(err.Error(), "valid, independent groups") {
		t.Fatalf("invalid group error = %v", err)
	}
	t.Setenv("RAG_DOCUMENT_DLQ_GROUP", settings.CacheDLQGroup)
	if _, err := deadLetterTarget("document", settings); err == nil ||
		!strings.Contains(err.Error(), "valid, independent groups") {
		t.Fatalf("duplicate group error = %v", err)
	}
	t.Setenv("RAG_DOCUMENT_DLQ_GROUP", settings.DocumentGroup)
	if _, err := deadLetterTarget("document", settings); err == nil ||
		!strings.Contains(err.Error(), "valid, independent groups") {
		t.Fatalf("reserved group error = %v", err)
	}
	reservedCache := settings
	reservedCache.CacheDLQGroup = settings.CacheGroup
	if _, err := deadLetterTarget("cache", reservedCache); err == nil ||
		!strings.Contains(err.Error(), "valid, independent groups") {
		t.Fatalf("reserved cache DLQ group error = %v", err)
	}
	t.Setenv("RAG_DOCUMENT_DLQ_GROUP", "praxis_document_dead_letters")
	emptyTopic := settings
	emptyTopic.DocumentDLQTopic = ""
	if _, err := deadLetterTarget("document", emptyTopic); err == nil ||
		!strings.Contains(err.Error(), "RAG_DOCUMENT_DLQ_TOPIC must not be empty") {
		t.Fatalf("empty document DLQ topic error = %v", err)
	}
	emptyCacheTopic := settings
	emptyCacheTopic.CacheDLQTopic = ""
	if _, err := deadLetterTarget("cache", emptyCacheTopic); err == nil ||
		!strings.Contains(err.Error(), "RAG_CACHE_DLQ_TOPIC must not be empty") {
		t.Fatalf("empty cache DLQ topic error = %v", err)
	}
	// A removed kind is a usage error, not a silent no-op.
	if _, err := deadLetterTarget("search", settings); err == nil ||
		!strings.Contains(err.Error(), `must be "document" or "cache"`) {
		t.Fatalf("search kind error = %v", err)
	}
}

// TestCacheDeadLetterHandlerAlwaysAcknowledges pins the cache dead-letter
// contract: cache refreshes are derived state with their own MySQL dirty-key
// retry water line, so every dead letter — valid, tombstone or malformed — is
// recorded and acknowledged instead of looping, and no business state is touched.
func TestCacheDeadLetterHandlerAlwaysAcknowledges(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := cacheDeadLetterHandler(logger)
	payloads := [][]byte{
		// Application-native schema-1 envelope.
		[]byte(`{"schema":1,"table":"documents","op":"u","after":{"namespace":"alpha","name":"a.pdf"}}`),
		// Raw Debezium change event for a captured table.
		[]byte(`{"before":null,"after":{"namespace":"alpha"},"source":{"table":"documents"},"op":"c"}`),
		// A tombstone (null value) carries no cache key.
		nil,
		// A malformed payload must not keep re-delivering.
		[]byte(`{"op":"c","after":`),
	}
	for _, body := range payloads {
		if acked := handler(context.Background(), mq.ReceivedMessage{
			Topic: "praxis_state_cache_refresh_dead_letters", Body: body, DeliveryAttempt: 1,
		}); !acked {
			t.Fatalf("cache dead-letter handler did not acknowledge %q", body)
		}
	}
}
