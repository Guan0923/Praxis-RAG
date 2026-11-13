package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/documentworker"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// TestDocumentPipelineEndToEnd drives the MCP add path through the real runtime
// ingester and the document worker consumer: admission -> broker delivery ->
// parse/chunk/embed/upsert + journal commit -> durable succeeded -> documents
// list/status readable.
func TestDocumentPipelineEndToEnd(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()

	admission, err := h.Service.AddDocument(ctx, "alpha", "manual.md", []byte(documentText), "")
	if err != nil {
		t.Fatalf("AddDocument() error = %v", err)
	}
	artifactID, _ := admission["artifact_id"].(string)
	if len(artifactID) != 36 {
		t.Fatalf("AddDocument() artifact_id = %v", admission)
	}
	job, err := h.Stores.DocumentJobs.Get(ctx, artifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", job, err)
	}
	if job.Status != state.DocumentJobQueued {
		t.Fatalf("admitted job status = %q", job.Status)
	}
	notification, err := state.DocumentJobEnvelope(job)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope() error = %v", err)
	}
	body, _, err := mq.EncodeDocumentNotification(notification)
	if err != nil {
		t.Fatalf("EncodeDocumentNotification() error = %v", err)
	}

	worker, err := documentworker.New(documentworker.Options{
		Jobs:            h.Stores.DocumentJobs,
		Ingester:        h.Service,
		JournalRecovery: h.Service,
		Logger:          testLogger(),
	})
	if err != nil {
		t.Fatalf("documentworker.New() error = %v", err)
	}
	transport := &fakePullTransport{}
	transport.push(mq.ReceivedMessage{Topic: "praxis_document_ingest", Body: body, DeliveryAttempt: 1})
	consumer := mq.NewPullConsumer(
		consumerConfig("praxis_document_ingest", "praxis_document_ingest_workers"),
		transport, worker.MessageHandler(),
	)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.RunConsumer(runCtx, consumer) }()

	waitFor(t, 90*time.Second, "document job succeeded", func() bool {
		current, readErr := h.Stores.DocumentJobs.Get(context.Background(), artifactID, nil)
		return readErr == nil && current != nil && current.Status == state.DocumentJobSucceeded
	})
	// The durable job becomes succeeded inside Handle; the commit/ack happens
	// right after it returns, so wait for the delivery to be acknowledged too.
	waitFor(t, 90*time.Second, "document delivery acked", func() bool { return transport.ackCount() == 1 })
	cancel()
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("RunConsumer() error = %v", runErr)
		}
	case <-time.After(90 * time.Second):
		t.Fatalf("document consumer did not stop")
	}
	if transport.ackCount() != 1 || transport.nackCount() != 0 {
		t.Fatalf("delivery ack/nack = %d/%d, want 1/0", transport.ackCount(), transport.nackCount())
	}

	// Durable job state and the client-facing status projection.
	current, err := h.Stores.DocumentJobs.Get(ctx, artifactID, nil)
	if err != nil || current == nil || current.Status != state.DocumentJobSucceeded || current.Stage != "completed" {
		t.Fatalf("terminal job = %+v, %v", current, err)
	}
	if current.OperationID == nil {
		t.Fatalf("terminal job publication operation_id missing: %+v", current)
	}
	status, err := h.Service.GetDocumentStatus(ctx, "alpha", artifactID)
	if err != nil {
		t.Fatalf("GetDocumentStatus() error = %v", err)
	}
	if status["status"] != state.DocumentJobSucceeded || status["stage"] != "completed" {
		t.Fatalf("document status = %v", status)
	}

	// The document list shows the assetless markdown record.
	documents, err := h.Service.ListDocuments(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListDocuments() error = %v", err)
	}
	if len(documents) != 1 {
		t.Fatalf("ListDocuments() = %d records, want 1: %v", len(documents), documents)
	}
	record := documents[0]
	if record["name"] != "manual.md" {
		t.Fatalf("document name = %v", record["name"])
	}
	if record["collection"] != "knowledge_test" {
		t.Fatalf("document collection = %v", record["collection"])
	}
	if chunkCount, _ := record["chunk_count"].(float64); chunkCount <= 0 {
		t.Fatalf("document chunk_count = %v", record["chunk_count"])
	}
	if record["sha256"] == "" {
		t.Fatalf("document record has no sha256: %v", record)
	}
	if _, hasAssets := record["assets"]; hasAssets {
		t.Fatalf("markdown document must stay assetless: %v", record)
	}
	chunking, _ := record["chunking"].(map[string]any)
	if chunking == nil || chunking["embedding_model"] != "embed-a" {
		t.Fatalf("document chunking = %v", record["chunking"])
	}

	// The real ingestion path hit the fake embedding server and upserted points.
	if embeddings := h.Model.count(http.MethodPost, "/embeddings"); embeddings == 0 {
		t.Fatalf("no embedding requests reached the model server")
	}
	upserts := 0
	for _, request := range h.Qdrant.all() {
		if request.Method == http.MethodPut && strings.HasSuffix(request.Path, "/points") &&
			strings.Contains(request.Path, "/collections/knowledge_test/") {
			upserts++
		}
	}
	if upserts == 0 {
		t.Fatalf("no Qdrant point upserts were issued to knowledge_test")
	}
}
