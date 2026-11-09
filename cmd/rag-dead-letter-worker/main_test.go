package main

import (
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/mq"
)

func testKafkaSettings() mq.Settings {
	return mq.Settings{
		BootstrapServers: []string{"localhost:9092"},
		ClientID:         mq.DefaultClientID,
		SecurityProtocol: mq.DefaultSecurityProtocol,
		DocumentTopic:    "doc-topic",
		DocumentGroup:    "doc-group",
		SearchTopic:      "search-topic",
		SearchGroup:      "search-group",
		CacheTopic:       "cache-topic",
		CacheGroup:       "cache-group",
		DocumentDLQTopic: "praxis_document_ingest_dead_letters",
		SearchDLQTopic:   "praxis_search_dead_letters",
	}
}

func TestParseKind(t *testing.T) {
	kind, err := parseKind([]string{"--kind", "search"})
	if err != nil || kind != "search" {
		t.Fatalf("parseKind(search) = %q, %v", kind, err)
	}
	kind, err = parseKind([]string{"--kind=DOCUMENT"})
	if err != nil || kind != "document" {
		t.Fatalf("parseKind(DOCUMENT) = %q, %v", kind, err)
	}
	if _, err := parseKind(nil); err == nil || !strings.Contains(err.Error(), `--kind is required`) {
		t.Fatalf("missing kind error = %v", err)
	}
	if _, err := parseKind([]string{"--kind", "cache"}); err == nil {
		t.Fatal("invalid kind accepted")
	}
	if _, err := parseKind([]string{"--kind", "search", "extra"}); err == nil {
		t.Fatal("unexpected argument accepted")
	}
}

func TestDeadLetterTargetUsesExplicitKafkaTopics(t *testing.T) {
	settings := testKafkaSettings()
	t.Setenv("RAG_DOCUMENT_DLQ_GROUP", "praxis_document_dead_letters")
	t.Setenv("RAG_SEARCH_DLQ_GROUP", "praxis_search_dead_letters")
	target, err := deadLetterTarget("search", settings)
	if err != nil {
		t.Fatalf("deadLetterTarget(search) error = %v", err)
	}
	if target.topic != settings.SearchDLQTopic || target.group != "praxis_search_dead_letters" ||
		target.sourceGroup != settings.SearchGroup {
		t.Fatalf("search target = %+v", target)
	}
	target, err = deadLetterTarget("document", settings)
	if err != nil {
		t.Fatalf("deadLetterTarget(document) error = %v", err)
	}
	if target.topic != settings.DocumentDLQTopic || target.group != "praxis_document_dead_letters" ||
		target.sourceGroup != settings.DocumentGroup {
		t.Fatalf("document target = %+v", target)
	}
}

func TestDeadLetterTargetValidation(t *testing.T) {
	settings := testKafkaSettings()
	t.Setenv("RAG_DOCUMENT_DLQ_GROUP", "bad group")
	if _, err := deadLetterTarget("document", settings); err == nil ||
		!strings.Contains(err.Error(), "valid, independent groups") {
		t.Fatalf("invalid group error = %v", err)
	}
	t.Setenv("RAG_DOCUMENT_DLQ_GROUP", "praxis_document_dead_letters")
	t.Setenv("RAG_SEARCH_DLQ_GROUP", "doc-group")
	if _, err := deadLetterTarget("search", settings); err == nil ||
		!strings.Contains(err.Error(), "valid, independent groups") {
		t.Fatalf("reserved group error = %v", err)
	}
	t.Setenv("RAG_SEARCH_DLQ_GROUP", "praxis_search_dead_letters")
	t.Setenv("RAG_DOCUMENT_DLQ_GROUP", "same-group")
	t.Setenv("RAG_SEARCH_DLQ_GROUP", "same-group")
	if _, err := deadLetterTarget("search", settings); err == nil ||
		!strings.Contains(err.Error(), "valid, independent groups") {
		t.Fatalf("duplicate group error = %v", err)
	}
	t.Setenv("RAG_DOCUMENT_DLQ_GROUP", "praxis_document_dead_letters")
	t.Setenv("RAG_SEARCH_DLQ_GROUP", "praxis_search_dead_letters")
	emptyTopic := settings
	emptyTopic.SearchDLQTopic = ""
	if _, err := deadLetterTarget("search", emptyTopic); err == nil ||
		!strings.Contains(err.Error(), "RAG_SEARCH_DLQ_TOPIC must not be empty") {
		t.Fatalf("empty DLQ topic error = %v", err)
	}
}
