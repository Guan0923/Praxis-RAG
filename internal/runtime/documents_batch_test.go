package runtime

import (
	"context"
	"strings"
	"testing"
)

// TestSubmitDocumentBatchRejectsHTMLWithoutDocling pins the per-item config
// failure: when RAG_DOCLING_SERVICE_URL is not configured the HTML file fails
// with reason "config" and never reaches document_jobs, so nothing retries.
func TestSubmitDocumentBatchRejectsHTMLWithoutDocling(t *testing.T) {
	service, stores := newAsyncService(t)
	ctx := context.Background()
	files := []UploadFile{
		{Name: "page.html", Content: []byte("<html></html>")},
		{Name: "note.md", Content: []byte("# ok")},
	}
	payload, err := service.SubmitDocumentBatch(ctx, "alpha", files, "batch-html", "")
	if err != nil {
		t.Fatalf("SubmitDocumentBatch() error = %v", err)
	}
	if payload["accepted"] != 1 || payload["failed"] != 1 || payload["skipped"] != 0 {
		t.Fatalf("payload = %v", payload)
	}
	items, ok := payload["items"].([]map[string]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %v", payload["items"])
	}
	var htmlItem map[string]any
	for _, item := range items {
		if item["name"] == "page.html" {
			htmlItem = item
		}
	}
	if htmlItem == nil {
		t.Fatalf("HTML item missing from payload: %v", items)
	}
	if htmlItem["status"] != "failed" || htmlItem["reason"] != "config" {
		t.Fatalf("HTML item = %v, want failed/config", htmlItem)
	}
	if text, _ := htmlItem["error"].(string); !strings.Contains(text, doclingServiceRequiredMessage) {
		t.Fatalf("HTML item error = %q", text)
	}
	var jobs int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM document_jobs WHERE namespace = ? AND name = ?", "alpha", "page.html").Scan(&jobs); err != nil {
		t.Fatalf("count document_jobs error = %v", err)
	}
	if jobs != 0 {
		t.Fatalf("document_jobs has %d HTML rows after a rejected batch admission", jobs)
	}
}

// TestSubmitDocumentBatchAdmitsAndClassifies verifies the batch admission
// fan-out: distinct names are accepted in parallel, a duplicate name inside the
// same batch is reported as skipped instead of failing the whole batch, and a
// retried batch with the same idempotency key reuses the original record.
func TestSubmitDocumentBatchAdmitsAndClassifies(t *testing.T) {
	service, stores := newAsyncService(t)
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
	var jobs int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM document_jobs WHERE namespace = ?", "alpha").Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 2 {
		t.Fatalf("document jobs = %d, want 2", jobs)
	}
	var queuedVersions int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM document_jobs WHERE namespace = ? AND schedule_version = 1", "alpha").Scan(&queuedVersions); err != nil {
		t.Fatalf("count scheduled jobs: %v", err)
	}
	if queuedVersions != 2 {
		t.Fatalf("jobs with initial schedule_version = %d, want 2", queuedVersions)
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
// ten-file limit of the batch upload endpoint.
func TestSubmitDocumentBatchRejectsOversizedInput(t *testing.T) {
	service, _ := newAsyncService(t)
	files := make([]UploadFile, 11)
	for index := range files {
		files[index] = UploadFile{Name: string(rune('a'+index)) + ".md", Content: []byte("x")}
	}
	if _, err := service.SubmitDocumentBatch(context.Background(), "alpha", files, "", ""); err == nil {
		t.Fatal("SubmitDocumentBatch() accepted more than ten files")
	}
}
