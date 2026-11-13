package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// TestDocumentPipelineEndToEnd drives the asynchronous upload path through the
// real runtime ingester and the document worker handler:
//
//	MySQL admission -> schema-4 notification (what Debezium emits) ->
//	parse/chunk/embed/upsert + journal commit -> durable succeeded -> documents
//	list/status readable.
//
// There is no transactional outbox and no broker acceptance step: admission is a
// committed document_jobs row, and the identifier-only notification drives the
// worker.
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
	// Admission is the only write: MySQL is the fact source and Debezium starts
	// watching the task at schedule_version 1.
	if job.ScheduleVersion != 1 {
		t.Fatalf("admitted job schedule_version = %d, want 1", job.ScheduleVersion)
	}

	// Debezium delivers the identifier-only schema-4 notification; the worker
	// reads every parameter back from the committed row.
	notification := documentNotification(t, h, artifactID)
	worker := newDocumentProcessor(t, h)
	if acked := worker.Handle(ctx, notification); !acked {
		t.Fatalf("Handle() = false, want an ACK once the durable outcome is saved")
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

	// A redelivery of the same notification (at-least-once delivery) is safe to
	// ACK and must not change the terminal row.
	if acked := worker.Handle(ctx, notification); !acked {
		t.Fatalf("replayed Handle() = false, want a safe ACK for the terminal job")
	}
	replayed, err := h.Stores.DocumentJobs.Get(ctx, artifactID, nil)
	if err != nil || replayed == nil || replayed.Status != state.DocumentJobSucceeded ||
		replayed.Stage != current.Stage || replayed.Attempts != current.Attempts {
		t.Fatalf("replayed job = %+v, %v", replayed, err)
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
