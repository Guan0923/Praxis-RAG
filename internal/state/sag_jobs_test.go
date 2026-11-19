package state

import (
	"context"
	"database/sql"
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

// TestSagExtractionJobsStopRetryingAfterMaxFailures pins the retry cap: once a
// chunk has failed sagExtractionMaxFailures times, ready_at becomes NULL, the
// job is never claimed again, and last_error stays observable.
func TestSagExtractionJobsStopRetryingAfterMaxFailures(t *testing.T) {
	stores := newTestStores(t)
	repo := stores.SagIndex
	ctx := context.Background()
	const namespace, document, version = "rag-notes-cap", "retry-cap", "v1"
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
		Name: "retry-cap.md", DocumentID: document, Collection: "rag-docs", ChunkCount: 1,
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
		if err := repo.QueueExtraction(ctx, tx, namespace, document, version, []string{"chunkCap"}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	queue(version)
	// Fail the same chunk until the retry cap is exhausted. Each attempt sets a
	// finite ready_at until the last one, which must leave it NULL.
	for attempt := 1; attempt <= sagExtractionMaxFailures; attempt++ {
		// A failed finish moves ready_at into the future (the lease window),
		// so force eligibility before each claim like the production sweeper does.
		exec("UPDATE sag_extraction_jobs SET ready_at = 0 WHERE chunk_id = 'chunkCap'")
		job, err := repo.ClaimExtraction(ctx)
		if err != nil || job == nil || job.ChunkID != "chunkCap" {
			t.Fatalf("attempt %d claim = %+v, %v", attempt, job, err)
		}
		if err := repo.FinishExtraction(ctx, job, nil, "model_extraction_failed"); err != nil {
			t.Fatalf("attempt %d FinishExtraction error = %v", attempt, err)
		}
		var readyAt sql.NullFloat64
		if err := stores.DB.QueryRowContext(ctx,
			"SELECT ready_at FROM sag_extraction_jobs WHERE chunk_id = 'chunkCap'").Scan(&readyAt); err != nil {
			t.Fatalf("attempt %d read ready_at error = %v", attempt, err)
		}
		if attempt < sagExtractionMaxFailures {
			if !readyAt.Valid || readyAt.Float64 <= 0 {
				t.Fatalf("attempt %d ready_at = %v, want a future retry timestamp", attempt, readyAt)
			}
		} else if readyAt.Valid {
			t.Fatalf("final failure ready_at = %v, want NULL so the job stops retrying", readyAt)
		}
	}

	// The capped job is never claimed again, even after forcing eligibility of
	// every other row: NULL never satisfies ready_at <= NOW.
	exec("UPDATE sag_extraction_jobs SET ready_at = 0 WHERE chunk_id <> 'chunkCap' AND ready_at IS NOT NULL")
	if job, err := repo.ClaimExtraction(ctx); err != nil || job != nil {
		t.Fatalf("capped job was claimed again: %+v, %v", job, err)
	}

	var failures int
	var lastError sql.NullString
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT failures, last_error FROM sag_extraction_jobs WHERE chunk_id = 'chunkCap'").
		Scan(&failures, &lastError); err != nil {
		t.Fatalf("read final job state error = %v", err)
	}
	if failures != sagExtractionMaxFailures || !lastError.Valid || lastError.String != "model_extraction_failed" {
		t.Fatalf("final job state = %d failures, last_error = %v, want %d failures with the recorded error",
			failures, lastError, sagExtractionMaxFailures)
	}
}
