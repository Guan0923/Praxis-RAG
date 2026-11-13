package runtime

import (
	"context"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// Admission and background maintenance leave the committed event for Debezium.
func TestDocumentAdmissionLeavesOutboxForDebezium(t *testing.T) {
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

	payload, err := service.addDocumentAsync(ctx, "alpha", "bridge.md", []byte("# Bridge\n\nCDC pipeline."), "")
	if err != nil {
		t.Fatalf("addDocumentAsync() error = %v", err)
	}
	artifactID, _ := payload["artifact_id"].(string)
	if !canonicalUUIDPattern.MatchString(artifactID) {
		t.Fatalf("artifact_id = %v, want canonical UUID", payload["artifact_id"])
	}
	service.maintenancePass(ctx)
	if count := broker.documentNotificationCount(); count != 0 {
		t.Fatalf("application published %d document notifications", count)
	}
	events, err := stores.DocumentJobs.PendingOutbox(ctx, 10)
	if err != nil || len(events) != 1 || events[0].ArtifactID != artifactID || events[0].PublishedAt != nil {
		t.Fatalf("committed outbox for Debezium = %+v, %v", events, err)
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
