package state

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func stringPointerForBatch(value string) *string { return &value }

func TestDocumentBatchCreateFinishAndRead(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	owner := "user-1"
	names := []string{"a.md", "b.md"}
	batch, reused, err := stores.DocumentBatches.Create(ctx, "manuals", &owner, "batch-key", names)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if reused {
		t.Fatal("Create() unexpectedly reused a batch")
	}
	if batch.Total != 2 || batch.Status != DocumentBatchSaving || len(batch.Items) != 2 {
		t.Fatalf("batch = %+v", batch)
	}
	if batch.Items[0].Status != DocumentBatchItemSaving || batch.Items[1].Name != "b.md" {
		t.Fatalf("items = %+v", batch.Items)
	}

	same, reusedAgain, err := stores.DocumentBatches.Create(ctx, "manuals", &owner, "batch-key", names)
	if err != nil {
		t.Fatalf("Create(repeat) error = %v", err)
	}
	if !reusedAgain || same.ID != batch.ID {
		t.Fatalf("repeat = %+v reused=%v", same, reusedAgain)
	}

	admission := submitTestDocument(t, stores, "manuals", "a.md", []byte("x"), nil)
	results := []DocumentBatchItemResult{
		{Position: 0, Status: DocumentBatchItemAccepted, ArtifactID: admission.ArtifactID},
		{Position: 1, Status: DocumentBatchItemSkipped, Reason: DocumentBatchReasonExists},
	}
	if err := stores.DocumentBatches.Finish(ctx, "manuals", batch.ID, results); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	loaded, err := stores.DocumentBatches.Get(ctx, "manuals", batch.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("Get() returned nil")
	}
	if loaded.Accepted != 1 || loaded.Skipped != 1 || loaded.Failed != 0 {
		t.Fatalf("counters = %+v", loaded)
	}
	if loaded.Status != DocumentBatchProcessing {
		t.Fatalf("status = %q, want processing", loaded.Status)
	}
	if loaded.Items[0].ArtifactID == nil || *loaded.Items[0].ArtifactID != admission.ArtifactID {
		t.Fatalf("item[0] = %+v", loaded.Items[0])
	}
	if loaded.Items[0].Job == nil || loaded.Items[0].Job.Status != DocumentJobQueued {
		t.Fatalf("item[0].job = %+v", loaded.Items[0].Job)
	}
	if loaded.Items[1].Reason == nil || *loaded.Items[1].Reason != DocumentBatchReasonExists {
		t.Fatalf("item[1] = %+v", loaded.Items[1])
	}

	if other, err := stores.DocumentBatches.Get(ctx, "elsewhere", batch.ID); err != nil || other != nil {
		t.Fatalf("Get(other namespace) = %+v, %v", other, err)
	}
	active, err := stores.DocumentBatches.ListActive(ctx, "manuals", &owner, 10)
	if err != nil || len(active) != 1 {
		t.Fatalf("ListActive(owner) = %d, %v", len(active), err)
	}
	others, err := stores.DocumentBatches.ListActive(ctx, "manuals", stringPointerForBatch("user-2"), 10)
	if err != nil || len(others) != 0 {
		t.Fatalf("ListActive(other owner) = %d, %v", len(others), err)
	}
}

func TestDocumentBatchListActiveLoadsItemsForSelectedBatches(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	var now float64 = 1000
	stores.DocumentBatches.Clock = func() float64 { return now }
	owner := "user-1"

	old, _, err := stores.DocumentBatches.Create(ctx, "manuals", &owner, "", []string{"skipped.md", "current.md"})
	if err != nil {
		t.Fatal(err)
	}
	current := submitTestDocument(t, stores, "manuals", "current.md", []byte("current"), nil)
	if err := stores.DocumentBatches.Finish(ctx, "manuals", old.ID, []DocumentBatchItemResult{
		{Position: 0, Status: DocumentBatchItemSkipped, Reason: DocumentBatchReasonExists},
		{Position: 1, Status: DocumentBatchItemAccepted, ArtifactID: current.ArtifactID},
	}); err != nil {
		t.Fatal(err)
	}

	now++
	latest, _, err := stores.DocumentBatches.Create(ctx, "manuals", &owner, "", []string{"pruned.md"})
	if err != nil {
		t.Fatal(err)
	}
	pruned := submitTestDocument(t, stores, "manuals", "pruned.md", []byte("pruned"), nil)
	if err := stores.DocumentBatches.Finish(ctx, "manuals", latest.ID, []DocumentBatchItemResult{
		{Position: 0, Status: DocumentBatchItemAccepted, ArtifactID: pruned.ArtifactID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := stores.DB.ExecContext(ctx, "DELETE FROM document_jobs WHERE artifact_id = ?", pruned.ArtifactID); err != nil {
		t.Fatal(err)
	}

	batches, err := stores.DocumentBatches.ListActive(ctx, "manuals", &owner, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || batches[0].ID != latest.ID || batches[1].ID != old.ID {
		t.Fatalf("ListActive() = %+v, want latest then old", batches)
	}
	if items := batches[0].Items; len(items) != 1 || items[0].Name != "pruned.md" ||
		items[0].ArtifactID == nil || *items[0].ArtifactID != pruned.ArtifactID || items[0].Job != nil {
		t.Fatalf("latest items = %+v, want the accepted item with a missing job", items)
	}
	if items := batches[1].Items; len(items) != 2 || items[0].Name != "skipped.md" || items[0].Job != nil ||
		items[1].Name != "current.md" || items[1].Job == nil || items[1].Job.ArtifactID != current.ArtifactID ||
		items[1].Job.Status != DocumentJobQueued {
		t.Fatalf("old items = %+v, want skipped item then queued item", items)
	}
}

func TestDocumentBatchCreateRejectsEmptyNames(t *testing.T) {
	stores := newTestStores(t)
	_, reused, err := stores.DocumentBatches.Create(context.Background(), "manuals", nil, "batch-key", nil)
	if err == nil || reused {
		t.Fatalf("Create(no names) = reused %v, err %v; want rejected", reused, err)
	}
	var valueErr *ValueError
	if !errors.As(err, &valueErr) || valueErr.Message != "A document batch needs at least one file" {
		t.Fatalf("Create(no names) error = %v (%T)", err, err)
	}
}

func TestDocumentBatchPrune(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	batch, _, err := stores.DocumentBatches.Create(ctx, "manuals", nil, "", []string{"old.md"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_batches SET created_at = 0 WHERE id = ?", batch.ID); err != nil {
		t.Fatalf("age the batch: %v", err)
	}
	pruned, err := stores.DocumentBatches.Prune(ctx, 1)
	if err != nil || pruned != 1 {
		t.Fatalf("Prune() = %d, %v", pruned, err)
	}
	if remaining, err := stores.DocumentBatches.Get(ctx, "manuals", batch.ID); err != nil || remaining != nil {
		t.Fatalf("Get() after prune = %+v, %v", remaining, err)
	}
	var items int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM document_batch_items WHERE batch_id = ?", batch.ID).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 0 {
		t.Fatalf("items after prune = %d", items)
	}
}

func TestDocumentBatchDerivedStatus(t *testing.T) {
	repositories := &DocumentBatches{Clock: func() float64 { return 2000 }}
	saved := &DocumentBatch{Status: DocumentBatchSaving, UpdatedAt: 1000}
	if status := repositories.deriveStatus(saved); status != DocumentBatchInterrupted {
		t.Fatalf("stale saving status = %q", status)
	}
	fresh := &DocumentBatch{Status: DocumentBatchSaving, UpdatedAt: 1990}
	if status := repositories.deriveStatus(fresh); status != DocumentBatchSaving {
		t.Fatalf("fresh saving status = %q", status)
	}
	failed := &DocumentBatch{Status: DocumentBatchActive, Failed: 1, Total: 1}
	if status := repositories.deriveStatus(failed); status != DocumentBatchFailed {
		t.Fatalf("failed batch status = %q", status)
	}
	skippedOnly := &DocumentBatch{Status: DocumentBatchActive, Skipped: 1, Total: 1}
	if status := repositories.deriveStatus(skippedOnly); status != DocumentBatchCompleted {
		t.Fatalf("all-skipped batch status = %q", status)
	}
	processing := &DocumentBatch{
		Status: DocumentBatchActive, Accepted: 1,
		Items: []DocumentBatchItem{{Status: DocumentBatchItemAccepted, Job: &DocumentJobStatus{Status: DocumentJobQueued}}},
	}
	if status := repositories.deriveStatus(processing); status != DocumentBatchProcessing {
		t.Fatalf("processing status = %q", status)
	}
	completed := &DocumentBatch{
		Status: DocumentBatchActive, Accepted: 2,
		Items: []DocumentBatchItem{
			{Status: DocumentBatchItemAccepted, Job: &DocumentJobStatus{Status: DocumentJobSucceeded}},
			{Status: DocumentBatchItemSkipped},
			{Status: DocumentBatchItemAccepted, Job: &DocumentJobStatus{Status: DocumentJobFailed}},
		},
	}
	if status := repositories.deriveStatus(completed); status != DocumentBatchCompleted {
		t.Fatalf("completed status = %q", status)
	}
}

// TestConcurrentSameNameAdmissionsSerialize proves the per-name lease keeps the
// check-then-insert race out of the async admission path: exactly one of the
// concurrent same-name uploads is queued, the others are rejected as queued
// duplicates (DocumentQueuedConflict).
func TestConcurrentSameNameAdmissionsSerialize(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	var waitGroup sync.WaitGroup
	failures := make([]error, 4)
	for index := range failures {
		waitGroup.Add(1)
		go func(position int) {
			defer waitGroup.Done()
			_, failures[position] = stores.DocumentJobs.Submit(
				ctx, "manuals", "same.txt", []byte("content"), nil)
		}(index)
	}
	waitGroup.Wait()
	accepted, queued := 0, 0
	for _, err := range failures {
		if err == nil {
			accepted++
			continue
		}
		var conflict *DocumentQueuedConflict
		if errors.As(err, &conflict) {
			queued++
			continue
		}
		t.Fatalf("unexpected admission error: %v", err)
	}
	if accepted != 1 || queued != len(failures)-1 {
		t.Fatalf("accepted=%d queued=%d errors=%v", accepted, queued, failures)
	}
}

// TestConcurrentDistinctNameAdmissionsProceed proves different file names no
// longer share a namespace-wide lock: every concurrent admission succeeds.
func TestConcurrentDistinctNameAdmissionsProceed(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	names := []string{"a.md", "b.md", "c.md", "d.md", "e.md", "f.md", "g.md", "h.md"}
	failures := make([]error, len(names))
	var waitGroup sync.WaitGroup
	for index, name := range names {
		waitGroup.Add(1)
		go func(position int, fileName string) {
			defer waitGroup.Done()
			key := fmt.Sprintf("distinct-key-%d", position)
			_, failures[position] = stores.DocumentJobs.Submit(
				ctx, "manuals", fileName, []byte("content"), &key)
		}(index, name)
	}
	waitGroup.Wait()
	for index, err := range failures {
		if err != nil {
			t.Fatalf("admission %q failed: %v", names[index], err)
		}
	}
}
