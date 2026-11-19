package state

import (
	"context"
	"testing"
)

func TestSagExtractionJobsRetryAndFence(t *testing.T) {
	stores := newTestStores(t)
	repo := stores.SagIndex
	ctx := context.Background()
	const namespace, document, version = "rag-notes", "agentic-rag", "v1"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := stores.DB.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stores.ModelProfiles.Create(ctx, ModelProfile{
		Kind: "extraction", Name: "sag", BaseURL: "http://model.local/v1", Model: "extractor",
	}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Documents.Upsert(ctx, namespace, DocumentRecord{
		Name: "agentic-rag.md", DocumentID: document, Collection: "rag-docs", ChunkCount: 2,
	}); err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO document_heads (namespace, document_id, version, updated_at) VALUES (?, ?, ?, 0)", namespace, document, version)
	queue := func(version string) {
		t.Helper()
		tx, err := stores.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := repo.QueueExtraction(ctx, tx, namespace, document, version, []string{"chunkA", "chunkB"}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(want string) *SagExtractionJob {
		t.Helper()
		job, err := repo.ClaimExtraction(ctx)
		if err != nil || job == nil || job.ChunkID != want {
			t.Fatalf("claim = %+v, %v, want %s", job, err, want)
		}
		return job
	}
	finish := func(job *SagExtractionJob, result *SagExtraction, failure string) {
		t.Helper()
		if err := repo.FinishExtraction(ctx, job, result, failure); err != nil {
			t.Fatal(err)
		}
	}

	queue(version)
	chunkA := claim("chunkA")
	chunkB := claim("chunkB")
	finish(chunkA, &SagExtraction{ChunkID: "chunkA", Event: "Agent 决定是否再次检索", Entities: []string{"Agent"}}, "")
	finish(chunkB, nil, "model_extraction_failed")
	if job, err := repo.ClaimExtraction(ctx); err != nil || job != nil {
		t.Fatalf("running or delayed jobs should not be claimed: %+v, %v", job, err)
	}
	exec("UPDATE sag_extraction_jobs SET ready_at = 0 WHERE chunk_id = 'chunkB'")
	retry := claim("chunkB")
	extract := &SagExtraction{ChunkID: "chunkB", Event: "检索返回可引用证据", Entities: []string{"证据"}}
	finish(chunkB, extract, "") // an old worker cannot complete the new lease
	if count, err := repo.DocumentEventCount(ctx, namespace, document); err != nil || count != 1 {
		t.Fatalf("old attempt changed event count: %d, %v", count, err)
	}
	finish(retry, extract, "")
	if count, err := repo.DocumentEventCount(ctx, namespace, document); err != nil || count != 2 {
		t.Fatalf("retry lost the successful fragment: %d, %v", count, err)
	}

	exec("UPDATE document_heads SET version = 'v2' WHERE namespace = ? AND document_id = ?", namespace, document)
	queue("v2")
	old := claim("chunkA")
	exec("UPDATE document_heads SET version = 'v3' WHERE namespace = ? AND document_id = ?", namespace, document)
	queue("v3")
	finish(old, &SagExtraction{ChunkID: "chunkA", Event: "旧版文档中的事件"}, "")
	if count, err := repo.DocumentEventCount(ctx, namespace, document); err != nil || count != 0 {
		t.Fatalf("old document version repopulated the index: %d, %v", count, err)
	}
}
