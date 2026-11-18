package runtime

import (
	"context"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// Admission leaves the committed document_jobs row for Debezium; the
// application never publishes the notification itself and no outbox row exists.
func TestDocumentAdmissionLeavesJobForDebezium(t *testing.T) {
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx,
		testKnowledgeBaseSettings(t, "alpha", "http://127.0.0.1:1", "knowledge_test")); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	if err := service.StartBackground(ctx,
		WithMaintenanceInterval(0),
		WithInitialMaintenancePass(false),
	); err != nil {
		t.Fatalf("StartBackground() error = %v", err)
	}
	t.Cleanup(service.StopBackground)

	payload, err := service.addDocumentAsync(ctx, "alpha", "bridge.md", []byte("# Bridge\\n\\nCDC pipeline."), "")
	if err != nil {
		t.Fatalf("addDocumentAsync() error = %v", err)
	}
	artifactID, _ := payload["artifact_id"].(string)
	if !canonicalUUIDPattern.MatchString(artifactID) {
		t.Fatalf("artifact_id = %v, want canonical UUID", payload["artifact_id"])
	}
	service.maintenancePass(ctx)

	namespace := "alpha"
	job, err := stores.DocumentJobs.Get(ctx, artifactID, &namespace)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", job, err)
	}
	if job.Status != state.DocumentJobQueued {
		t.Fatalf("job status = %q, want queued", job.Status)
	}
	if job.ScheduleVersion != 1 {
		t.Fatalf("schedule_version = %d, want 1", job.ScheduleVersion)
	}
	// The retired outbox table is not created by the schema anymore.
	var outboxTables int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables "+
			"WHERE table_schema = DATABASE() AND table_name = 'document_job_outbox'").Scan(&outboxTables); err != nil {
		t.Fatalf("count retired table: %v", err)
	}
	if outboxTables != 0 {
		t.Fatalf("document_job_outbox still exists")
	}
}
