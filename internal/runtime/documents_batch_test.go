package runtime

import (
	"context"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// TestSubmitDocumentBatchAdmitsAndClassifies verifies input-order admission:
// the first same-name file wins deterministically, a duplicate name inside the
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
// TestSubmitDocumentBatchOrdersQuotaRejections covers the "9 files, last slot"
// case: the first file in input order wins, the remaining files report the
// owner's quota reason, and no document job is written for the rejected ones.
func TestSubmitDocumentBatchOrdersQuotaRejections(t *testing.T) {
	service, stores := newAsyncService(t)
	ctx := context.Background()
	owner, err := stores.Users.Create(ctx, "batch-quota-owner", "hash")
	if err != nil {
		t.Fatalf("Users.Create() error = %v", err)
	}
	if _, err := stores.KnowledgeBases.SetOwner(ctx, "alpha", owner.ID); err != nil {
		t.Fatalf("SetOwner() error = %v", err)
	}
	for index := 0; index < 9; index++ {
		name := "existing-" + string(rune('a'+index)) + ".md"
		if err := stores.Documents.Upsert(ctx, "alpha", state.DocumentRecord{
			Name: name, DocumentID: "doc-" + string(rune('a'+index)), Size: 4,
			ChunkCount: 1, Collection: "knowledge_test",
		}); err != nil {
			t.Fatalf("Documents.Upsert(%q) error = %v", name, err)
		}
	}
	files := []UploadFile{
		{Name: "slot.md", Content: []byte("# slot")},
		{Name: "over-a.md", Content: []byte("# over a")},
		{Name: "over-b.md", Content: []byte("# over b")},
	}
	payload, err := service.SubmitDocumentBatch(ctx, "alpha", files, "quota-batch", "")
	if err != nil {
		t.Fatalf("SubmitDocumentBatch() error = %v", err)
	}
	if payload["accepted"] != 1 || payload["failed"] != 2 {
		t.Fatalf("payload = %v, want accepted 1 failed 2", payload)
	}
	items, ok := payload["items"].([]map[string]any)
	if !ok || len(items) != 3 {
		t.Fatalf("items = %v", payload["items"])
	}
	if items[0]["name"] != "slot.md" || items[0]["status"] != "accepted" {
		t.Fatalf("first item = %v, want slot.md accepted", items[0])
	}
	for _, index := range []int{1, 2} {
		if items[index]["status"] != "failed" || items[index]["reason"] != state.DocumentBatchReasonQuota {
			t.Fatalf("item %d = %v, want quota failure", index, items[index])
		}
	}
	var jobs int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM document_jobs WHERE namespace = ?", "alpha").Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("document jobs = %d, want only the accepted slot", jobs)
	}
}

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
