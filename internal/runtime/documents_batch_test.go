package runtime

import (
	"context"
	"testing"
)

// TestSubmitDocumentBatchAdmitsAndClassifies verifies the batch admission
// fan-out: distinct names are accepted in parallel, a duplicate name inside the
// same batch is reported as skipped instead of failing the whole batch, and a
// retried batch with the same idempotency key reuses the original record.
func TestSubmitDocumentBatchAdmitsAndClassifies(t *testing.T) {
	service, stores, broker := newAsyncService(t)
	ctx := context.Background()
	files := []UploadFile{
		{Name: "one.md", Content: []byte("# one")},
		{Name: "two.md", Content: []byte("# two")},
		{Name: "one.md", Content: []byte("# one again")},
	}
	payload, err := service.SubmitDocumentBatch(ctx, "alpha", files, "batch-key-1", "")
	if err != nil {
		t.Fatalf("SubmitDocumentBatch() error = %v", err)
	}
	if payload["total"] != 3 || payload["accepted"] != 2 || payload["skipped"] != 1 || payload["failed"] != 0 {
		t.Fatalf("payload = %v", payload)
	}
	items, ok := payload["items"].([]map[string]any)
	if !ok || len(items) != 3 {
		t.Fatalf("items = %v", payload["items"])
	}
	// Which of the two same-name files wins the admission race is arbitrary;
	// what matters is exactly one accepted item and one queued skip.
	oneNameStatuses := make([]any, 0, 2)
	for _, item := range items {
		switch item["name"] {
		case "two.md":
			if item["status"] != "accepted" || item["artifact_id"] == nil {
				t.Fatalf("accepted item = %v", item)
			}
			if _, found := item["job"]; !found {
				t.Fatalf("accepted item missing job projection: %v", item)
			}
		case "one.md":
			oneNameStatuses = append(oneNameStatuses, item["status"])
			if item["status"] == "accepted" && item["artifact_id"] == nil {
				t.Fatalf("accepted item without artifact id = %v", item)
			}
			if item["status"] == "skipped" && item["reason"] != "queued" {
				t.Fatalf("skipped item reason = %v", item["reason"])
			}
		}
	}
	if len(oneNameStatuses) != 2 || oneNameStatuses[0] == oneNameStatuses[1] {
		t.Fatalf("same-name outcomes = %v", oneNameStatuses)
	}
	if got := broker.documentNotificationCount(); got != 2 {
		t.Fatalf("document notifications = %d, want 2", got)
	}
	var jobs int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM document_jobs WHERE namespace = ?", "alpha").Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 2 {
		t.Fatalf("document jobs = %d, want 2", jobs)
	}

	replayed, err := service.SubmitDocumentBatch(ctx, "alpha", files, "batch-key-1", "")
	if err != nil {
		t.Fatalf("replayed SubmitDocumentBatch() error = %v", err)
	}
	if replayed["batch_id"] != payload["batch_id"] {
		t.Fatalf("replayed batch_id = %v, want %v", replayed["batch_id"], payload["batch_id"])
	}
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM document_jobs WHERE namespace = ?", "alpha").Scan(&jobs); err != nil {
		t.Fatalf("count jobs after replay: %v", err)
	}
	if jobs != 2 {
		t.Fatalf("document jobs after replay = %d, want 2", jobs)
	}

	active, err := service.ListDocumentBatches(ctx, "alpha", "", 10)
	if err != nil {
		t.Fatalf("ListDocumentBatches() error = %v", err)
	}
	batches, ok := active["batches"].([]map[string]any)
	if !ok || len(batches) != 1 {
		t.Fatalf("batches = %v", active["batches"])
	}
	batchID, _ := payload["batch_id"].(string)
	detail, err := service.GetDocumentBatch(ctx, "alpha", batchID, "")
	if err != nil || detail == nil {
		t.Fatalf("GetDocumentBatch() = %v, %v", detail, err)
	}
	missing, err := service.GetDocumentBatch(ctx, "alpha", "ffffffffffffffffffffffffffffffff", "")
	if err != nil || missing != nil {
		t.Fatalf("GetDocumentBatch(unknown) = %v, %v", missing, err)
	}
}

// TestSubmitDocumentBatchRejectsOversizedInput keeps the batch contract at the
// same ten-file limit as POST /v1/documents.
func TestSubmitDocumentBatchRejectsOversizedInput(t *testing.T) {
	service, _, _ := newAsyncService(t)
	files := make([]UploadFile, 11)
	for index := range files {
		files[index] = UploadFile{Name: string(rune('a'+index)) + ".md", Content: []byte("x")}
	}
	if _, err := service.SubmitDocumentBatch(context.Background(), "alpha", files, "", ""); err == nil {
		t.Fatal("SubmitDocumentBatch() accepted more than ten files")
	}
}
