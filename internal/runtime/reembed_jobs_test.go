package runtime

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

func TestReembedJobPublishesAndRefreshesCachedIndex(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	qdrant, qdrantURL := newFakeKBQdrant(t, kbScrollPoints)
	service, stores := newKBService(t, qdrantURL)
	ctx := context.Background()
	created, err := service.CreateKnowledgeBase(ctx, testCreateInput("alpha", modelURL))
	if err != nil {
		t.Fatal(err)
	}
	oldCollection := created["collection"].(string)
	qdrant.addSource(oldCollection)
	seedReembedDocuments(t, stores)
	before, err := service.search.tenant(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}

	input := kbconfig.UpdateInput{Values: map[string]any{"embedding_model": "embed-b"}}
	job, err := service.SubmitReembed(ctx, "alpha", input)
	if err != nil || job.Status != state.ReembedQueued {
		t.Fatalf("SubmitReembed() = %v, %v", job, err)
	}
	var conflict *apierrors.KnowledgeValidationError
	if _, err := service.SubmitReembed(ctx, "alpha", input); !errors.As(err, &conflict) {
		t.Fatalf("duplicate active submission error = %v, want conflict", err)
	}
	staged, err := service.search.tenant(ctx, "alpha")
	if err != nil || staged != before {
		t.Fatalf("queued job modified the live index, tenant error = %v", err)
	}

	// Use a separate runtime for the worker so the warm search cache can only
	// follow the switch through the shared state.
	worker, err := New(service.Config(), stores, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Close(ctx) })
	for range 2 {
		if err := worker.ProcessReembed(ctx, "alpha", job.ID); err != nil {
			t.Fatal(err)
		}
	}
	finished, err := service.LatestReembed(ctx, "alpha")
	if err != nil || finished.Status != state.ReembedSucceeded {
		t.Fatalf("LatestReembed() = %v, %v", finished, err)
	}
	after, err := service.search.tenant(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if after.collection == oldCollection || after.embedding.Model != "embed-b" {
		t.Fatalf("search did not pick up the new collection and model: %s, %s", after.collection, after.embedding.Model)
	}
	if writes := qdrant.find(http.MethodPut, "/collections/"+after.collection+"/points"); len(writes) != 1 {
		t.Fatalf("rebuild writes = %d, want 1 after duplicate notification", len(writes))
	}
	if pending, err := stores.ReembedJobs.Pending(ctx, "alpha", job.ID); err != nil || pending != nil {
		t.Fatalf("terminal job still pending: %v, %v", pending, err)
	}
}

// TestReembedJobFailsWhenSourceChanged pins the failure path: when the
// knowledge-base snapshot moved on after the job was queued, the worker must
// fail the job with the source-changed message instead of rebuilding into a
// stale collection, and the failed job must leave the pending queue.
func TestReembedJobFailsWhenSourceChanged(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	_, qdrantURL := newFakeKBQdrant(t, kbScrollPoints)
	service, stores := newKBService(t, qdrantURL)
	ctx := context.Background()
	if _, err := service.CreateKnowledgeBase(ctx, testCreateInput("alpha", modelURL)); err != nil {
		t.Fatal(err)
	}
	seedReembedDocuments(t, stores)
	input := kbconfig.UpdateInput{Values: map[string]any{"embedding_model": "embed-b"}}
	job, err := service.SubmitReembed(ctx, "alpha", input)
	if err != nil || job.Status != state.ReembedQueued {
		t.Fatalf("SubmitReembed() = %v, %v", job, err)
	}

	// Move the snapshot forward behind the queue's back: the job's fenced
	// base_version no longer matches, so the worker must observe the change.
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE reembed_jobs SET base_version = base_version + 1 WHERE job_id = ?", job.ID); err != nil {
		t.Fatalf("corrupt base_version: %v", err)
	}

	worker, err := New(service.Config(), stores, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Close(ctx) })
	if err := worker.ProcessReembed(ctx, "alpha", job.ID); err != nil {
		t.Fatalf("ProcessReembed() error = %v", err)
	}

	finished, err := service.LatestReembed(ctx, "alpha")
	if err != nil || finished == nil || finished.Status != state.ReembedFailed {
		t.Fatalf("LatestReembed() = %v, %v, want %q", finished, err, state.ReembedFailed)
	}
	if finished.Error != state.ErrReembedSourceChanged.Error() {
		t.Fatalf("failed error = %q, want %q", finished.Error, state.ErrReembedSourceChanged.Error())
	}
	if pending, err := stores.ReembedJobs.Pending(ctx, "alpha", job.ID); err != nil || pending != nil {
		t.Fatalf("failed job still pending: %v, %v", pending, err)
	}
}
