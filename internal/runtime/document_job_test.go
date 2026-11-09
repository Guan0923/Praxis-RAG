package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/config"

	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/documentworker"
	"github.com/Guan0923/xinghe_rag/internal/sparse"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// newDocumentJobService reuses the upload harness: a committed knowledge-base
// row, fake model provider and fake Qdrant backend.
func newDocumentJobService(t *testing.T) (*Service, *state.Stores, *fakeKBQdrant) {
	t.Helper()
	service, stores, qdrantFake, _ := newUploadService(t)
	return service, stores, qdrantFake
}

// claimAdmittedJob submits one async admission and claims it like the worker
// would before calling the ingester.
func claimAdmittedJob(t *testing.T, stores *state.Stores, name, content string) (*state.DocumentJobTask, documentworker.Payload) {
	t.Helper()
	ctx := context.Background()
	admission, err := stores.DocumentJobs.Submit(ctx, "alpha", name, []byte(content), nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Submit() error = %v", err)
	}
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", job, err)
	}
	notification, err := state.DocumentJobEnvelope(job)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope() error = %v", err)
	}
	task, err := stores.DocumentJobs.Claim(ctx, notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %v, %v", task, err)
	}
	return task, documentworker.PayloadFromTask(task)
}

func TestProcessDocumentJobSucceeds(t *testing.T) {
	service, stores, qdrantFake := newDocumentJobService(t)
	ctx := context.Background()
	content := repeatedDocumentText(4)
	task, payload := claimAdmittedJob(t, stores, "async.md", content)

	result, err := service.ProcessDocumentJob(ctx, payload)
	if err != nil {
		t.Fatalf("ProcessDocumentJob() error = %v", err)
	}
	if result["sha256"] == "" || result["sha256"] == nil {
		t.Errorf("result sha256 = %v", result["sha256"])
	}
	chunkCount, _ := result["chunk_count"].(int)
	if chunkCount < 2 {
		t.Errorf("result chunk_count = %v", result["chunk_count"])
	}
	configurationVersion, _ := result["task_configuration_version"].(int64)
	if configurationVersion != task.ConfigurationVersion {
		t.Errorf("task_configuration_version = %v, want %d",
			result["task_configuration_version"], task.ConfigurationVersion)
	}
	if operationID, _ := result["operation_id"].(string); operationID == "" {
		t.Errorf("operation_id = %v", result["operation_id"])
	}

	stored, err := stores.Documents.Get(ctx, "alpha", "async.md")
	if err != nil || stored == nil {
		t.Fatalf("Documents.Get() = %v, %v", stored, err)
	}
	if stored.ChunkCount != chunkCount {
		t.Errorf("stored chunk count = %d, want %d", stored.ChunkCount, chunkCount)
	}
	if blob, found, err := stores.Blobs.Get(ctx, "alpha", state.SourceBucket, "async.md"); err != nil || !found || string(blob) != content {
		t.Errorf("source blob missing or different (found=%v err=%v)", found, err)
	}
	upserts := qdrantFake.find("PUT", "/points")
	if len(upserts) != 1 {
		t.Fatalf("Qdrant staged versions = %d, want 1", len(upserts))
	}
	if points, _ := upserts[0].Body["points"].([]any); len(points) != chunkCount {
		t.Errorf("staged points = %d, want %d", len(points), chunkCount)
	}

	job, err := stores.DocumentJobs.Get(ctx, task.ArtifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", job, err)
	}
	if job.Status != state.DocumentJobSucceeded || job.Stage != "completed" {
		t.Errorf("job = %+v", job)
	}
	if job.OperationID == nil || *job.OperationID == "" {
		t.Errorf("job operation_id = %v", job.OperationID)
	}
}

func TestProcessDocumentJobRedeliveryIsIdempotent(t *testing.T) {
	service, stores, qdrantFake := newDocumentJobService(t)
	ctx := context.Background()
	content := repeatedDocumentText(3)
	task, payload := claimAdmittedJob(t, stores, "replay.md", content)
	if _, err := service.ProcessDocumentJob(ctx, payload); err != nil {
		t.Fatalf("first ProcessDocumentJob() error = %v", err)
	}
	firstUpserts := len(qdrantFake.find("PUT", "/points"))
	if firstUpserts != 1 {
		t.Fatalf("first staged versions = %d", firstUpserts)
	}

	// A redelivery after the committed publication reports success without
	// indexing again.
	replay, err := service.ProcessDocumentJob(ctx, payload)
	if err != nil {
		t.Fatalf("replayed ProcessDocumentJob() error = %v", err)
	}
	if replay["artifact_id"] != task.ArtifactID || replay["status"] != state.DocumentJobSucceeded {
		t.Errorf("replay result = %v", replay)
	}
	if staged := len(qdrantFake.find("PUT", "/points")); staged != firstUpserts {
		t.Errorf("replay staged %d new versions", staged-firstUpserts)
	}
	if documents, _ := stores.Documents.List(ctx, "alpha"); len(documents) != 1 {
		t.Errorf("documents after replay = %+v", documents)
	}
}

func TestProcessDocumentJobTerminalFailureOnConfigurationChange(t *testing.T) {
	service, stores, qdrantFake := newDocumentJobService(t)
	ctx := context.Background()
	admission, err := stores.DocumentJobs.Submit(ctx, "alpha", "stale.md", []byte(repeatedDocumentText(3)), nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Submit() error = %v", err)
	}
	// The knowledge-base configuration changes after admission.
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE knowledge_bases SET version = version + 1 WHERE namespace = ?", "alpha"); err != nil {
		t.Fatalf("bump KB version: %v", err)
	}

	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", job, err)
	}
	notification, err := state.DocumentJobEnvelope(job)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope() error = %v", err)
	}
	processor, err := documentworker.New(documentworker.Options{
		Jobs:     stores.DocumentJobs,
		Ingester: service,
	})
	if err != nil {
		t.Fatalf("documentworker.New() error = %v", err)
	}
	if acked := processor.Handle(ctx, notification); !acked {
		t.Fatalf("terminal failure must be acknowledged")
	}
	failed, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || failed == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", failed, err)
	}
	if failed.Status != state.DocumentJobFailed || failed.Stage != "failed" {
		t.Fatalf("failed job = %+v", failed)
	}
	if failed.Error == nil || *failed.Error != "Invalid document or changed knowledge-base configuration" {
		t.Errorf("failed error = %v", failed.Error)
	}
	if len(qdrantFake.find("PUT", "/points")) != 0 {
		t.Errorf("stale job staged Qdrant points")
	}
	if records, _ := stores.Documents.List(ctx, "alpha"); len(records) != 0 {
		t.Errorf("stale job published documents: %+v", records)
	}
}

// TestProcessDocumentJobValidatesPublicationInTransaction proves the
// ValidatePublication hook runs inside the journal publication transaction: the
// admitted configuration version is current when processing starts, then the
// knowledge base changes while embeddings are computed, so only the in-tx check
// can reject the commit.
func TestProcessDocumentJobValidatesPublicationInTransaction(t *testing.T) {
	stores := newSearchTestStores(t)
	var bumpOnce sync.Once
	modelURL, _ := newFakeModelAPI(t, func(path string, body map[string]any) (int, string) {
		if strings.HasSuffix(path, "/embeddings") {
			bumpOnce.Do(func() {
				_, _ = stores.DB.Exec(
					"UPDATE knowledge_bases SET version = version + 1 WHERE namespace = ?", "alpha")
			})
		}
		return defaultModelHandler(path, body)
	})
	qdrantFake, qdrantURL := newFakeKBQdrant(t, "")
	service, err := New(config.RagConfig{
		Backend:    config.BackendQdrant,
		QdrantURL:  qdrantURL,
		Collection: "mini_agent_knowledge_pdf_assets_v1",
		VectorSize: 1024,
	}, stores, t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx,
		testKnowledgeBaseSettings(t, "alpha", modelURL, "knowledge_test")); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	admission, err := stores.DocumentJobs.Submit(ctx, "alpha", "race.md", []byte(repeatedDocumentText(3)), nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Submit() error = %v", err)
	}
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", job, err)
	}
	notification, err := state.DocumentJobEnvelope(job)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope() error = %v", err)
	}
	processor, err := documentworker.New(documentworker.Options{
		Jobs:     stores.DocumentJobs,
		Ingester: service,
	})
	if err != nil {
		t.Fatalf("documentworker.New() error = %v", err)
	}
	if acked := processor.Handle(ctx, notification); !acked {
		t.Fatalf("publication rejection must be terminal and acknowledged")
	}
	failed, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || failed == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", failed, err)
	}
	if failed.Status != state.DocumentJobFailed || failed.Error == nil ||
		*failed.Error != "Invalid document or changed knowledge-base configuration" {
		t.Fatalf("failed job = %+v (error=%v)", failed, failed.Error)
	}
	if records, _ := stores.Documents.List(ctx, "alpha"); len(records) != 0 {
		t.Errorf("rejected publication wrote documents: %+v", records)
	}
	if len(qdrantFake.find("PUT", "/points")) == 0 {
		t.Logf("staged points were compensated after the rejected publication")
	}
}

func TestProcessDocumentJobModelFailureIsRetryable(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, unauthorizedModelHandler)
	qdrantFake, qdrantURL := newFakeKBQdrant(t, "")
	service, stores := newKBService(t, qdrantURL)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx,
		testKnowledgeBaseSettings(t, "alpha", modelURL, "knowledge_test")); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	admission, err := stores.DocumentJobs.Submit(ctx, "alpha", "model-fail.md", []byte(repeatedDocumentText(3)), nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Submit() error = %v", err)
	}
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", job, err)
	}
	notification, err := state.DocumentJobEnvelope(job)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope() error = %v", err)
	}
	processor, err := documentworker.New(documentworker.Options{
		Jobs:     stores.DocumentJobs,
		Ingester: service,
	})
	if err != nil {
		t.Fatalf("documentworker.New() error = %v", err)
	}
	if acked := processor.Handle(ctx, notification); acked {
		t.Fatalf("model failure must stay retryable")
	}
	retried, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || retried == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", retried, err)
	}
	if retried.Status != state.DocumentJobQueued || retried.Stage != "retry_wait" || retried.Attempts != 1 {
		t.Fatalf("retried job = %+v", retried)
	}
	if retried.Error != nil {
		t.Errorf("retryable failure stored an error message: %v", retried.Error)
	}
	if len(qdrantFake.find("PUT", "/points")) != 0 {
		t.Errorf("failed model call staged Qdrant points")
	}
	if records, _ := stores.Documents.List(ctx, "alpha"); len(records) != 0 {
		t.Errorf("failed model call published documents: %+v", records)
	}
}

func TestRecoverDocumentUploadsRollsBackOrphan(t *testing.T) {
	service, stores, _ := newDocumentJobService(t)
	ctx := context.Background()
	journal, err := state.NewUploadJournal(stores, state.WithLeaseSeconds(0.02), state.WithAutoHeartbeat(false))
	if err != nil {
		t.Fatalf("NewUploadJournal() error = %v", err)
	}
	documentID := chunking.StableID("doc", sparse.CaseFold("orphan.txt"))
	operation, err := journal.Begin(ctx, "alpha", "orphan.txt", documentID, "knowledge_test", state.BeginOptions{})
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	workspace := filepath.Join(os.TempDir(), "rag-txn-"+operation.ID)
	if _, err := journal.RegisterWorkspace(ctx, operation, workspace, nil); err != nil {
		t.Fatalf("RegisterWorkspace() error = %v", err)
	}
	if err := journal.Intent(ctx, operation, "stage_qdrant", map[string]any{"version": operation.ID}); err != nil {
		t.Fatalf("Intent() error = %v", err)
	}
	time.Sleep(120 * time.Millisecond)

	if err := service.RecoverDocumentUploads(ctx); err != nil {
		t.Fatalf("RecoverDocumentUploads() error = %v", err)
	}
	recovered, err := stores.Uploads.Get(ctx, operation.ID)
	if err != nil || recovered == nil {
		t.Fatalf("Uploads.Get() = %v, %v", recovered, err)
	}
	if recovered.Status != state.UploadRolledBack && recovered.Status != state.UploadRollbackFailed {
		t.Errorf("orphan upload status = %q", recovered.Status)
	}
	if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
		t.Errorf("orphan workspace was not reclaimed: %v", statErr)
	}
}
