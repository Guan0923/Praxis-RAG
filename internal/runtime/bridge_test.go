package runtime

import (
	"context"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// TestDebeziumBridgeNeverPublishesFromApp wires the real background dispatchers
// with RAG_OUTBOX_BRIDGE=debezium and proves the accepted upload leaves a PENDING
// outbox row without the application talking to the broker: the external
// Debezium consumer is the only publisher in that mode.
func TestDebeziumBridgeNeverPublishesFromApp(t *testing.T) {
	t.Setenv("RAG_OUTBOX_BRIDGE", "debezium")
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx,
		testKnowledgeBaseSettings(t, "alpha", "http://127.0.0.1:1", "knowledge_test")); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	broker := &fakeBroker{}
	if err := service.StartBackground(ctx,
		WithBackgroundPublisher(broker),
		WithMaintenanceInterval(0),
		WithInitialMaintenancePass(false),
	); err != nil {
		t.Fatalf("StartBackground() error = %v", err)
	}
	t.Cleanup(service.StopBackground)

	if stores.DocumentDispatcher == nil {
		t.Fatal("DocumentDispatcher is nil")
	}
	if stores.DocumentDispatcher.Mode != "debezium" {
		t.Fatalf("DocumentDispatcher.Mode = %q, want debezium", stores.DocumentDispatcher.Mode)
	}
	if stores.DocumentDispatcher.Relay != nil {
		t.Fatal("DocumentDispatcher.Relay must be nil in debezium mode")
	}

	payload, err := service.addDocumentAsync(ctx, "alpha", "bridge.md", []byte("# Bridge\n\nCDC pipeline."), "")
	if err != nil {
		t.Fatalf("addDocumentAsync() error = %v", err)
	}
	artifactID, _ := payload["artifact_id"].(string)
	if !canonicalUUIDPattern.MatchString(artifactID) {
		t.Fatalf("artifact_id = %v, want canonical UUID", payload["artifact_id"])
	}
	if count := broker.documentNotificationCount(); count != 0 {
		t.Fatalf("documentNotificationCount() = %d, want 0 (debezium mode must not publish)", count)
	}
	events, err := stores.DocumentJobs.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("PendingOutbox() error = %v", err)
	}
	if len(events) == 0 {
		t.Fatal("outbox must retain PENDING rows for Debezium")
	}
	namespace := "alpha"
	job, err := stores.DocumentJobs.Get(ctx, artifactID, &namespace)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", job, err)
	}
	if job.Status != state.DocumentJobQueued {
		t.Fatalf("job status = %q, want queued", job.Status)
	}
}

// TestRelayBridgePublishesFromApp is the counterpart: the default relay mode
// publishes one schema-3 notification for the same admission.
func TestRelayBridgePublishesFromApp(t *testing.T) {
	t.Setenv("RAG_OUTBOX_BRIDGE", "relay")
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx,
		testKnowledgeBaseSettings(t, "alpha", "http://127.0.0.1:1", "knowledge_test")); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	broker := &fakeBroker{}
	if err := service.StartBackground(ctx,
		WithBackgroundPublisher(broker),
		WithMaintenanceInterval(0),
		WithInitialMaintenancePass(false),
	); err != nil {
		t.Fatalf("StartBackground() error = %v", err)
	}
	t.Cleanup(service.StopBackground)

	if stores.DocumentDispatcher == nil || stores.DocumentDispatcher.Relay == nil {
		t.Fatal("relay mode must construct DocumentOutboxRelay")
	}
	if _, err := service.addDocumentAsync(ctx, "alpha", "relay.md", []byte("# Relay\n\nLocal pipeline."), ""); err != nil {
		t.Fatalf("addDocumentAsync() error = %v", err)
	}
	if count := broker.documentNotificationCount(); count != 1 {
		t.Fatalf("documentNotificationCount() = %d, want 1", count)
	}
}
