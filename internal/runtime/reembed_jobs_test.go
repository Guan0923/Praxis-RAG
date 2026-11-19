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
