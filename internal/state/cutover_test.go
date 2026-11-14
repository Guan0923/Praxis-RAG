package state

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCutoverStateRoundTrip(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if value, ok, err := stores.CutoverStateGet(ctx, CutoverKeyPhase); err != nil || ok || value != "" {
		t.Fatalf("missing CutoverStateGet() = %q, %v, %v", value, ok, err)
	}
	if err := stores.CutoverStateSet(ctx, CutoverKeyPhase, "drain"); err != nil {
		t.Fatalf("CutoverStateSet() error = %v", err)
	}
	if value, ok, err := stores.CutoverStateGet(ctx, CutoverKeyPhase); err != nil || !ok || value != "drain" {
		t.Fatalf("CutoverStateGet() = %q, %v, %v", value, ok, err)
	}
	if err := stores.CutoverStateSet(ctx, CutoverKeyPhase, "cutover"); err != nil {
		t.Fatalf("CutoverStateSet(update) error = %v", err)
	}
	if err := stores.CutoverStateSet(ctx, CutoverKeyCDCStartFile, "binlog.000042"); err != nil {
		t.Fatalf("CutoverStateSet(file) error = %v", err)
	}
	if err := stores.CutoverStateSet(ctx, CutoverKeyCDCStartPos, "77123"); err != nil {
		t.Fatalf("CutoverStateSet(pos) error = %v", err)
	}
	values, err := stores.CutoverStateAll(ctx)
	if err != nil {
		t.Fatalf("CutoverStateAll() error = %v", err)
	}
	if values[CutoverKeyPhase] != "cutover" || values[CutoverKeyCDCStartFile] != "binlog.000042" ||
		values[CutoverKeyCDCStartPos] != "77123" || len(values) != 3 {
		t.Fatalf("CutoverStateAll() = %v", values)
	}
	for _, key := range []string{"", strings.Repeat("k", 65)} {
		if _, _, err := stores.CutoverStateGet(ctx, key); err == nil {
			t.Fatalf("CutoverStateGet(%q) succeeded", key)
		}
		if err := stores.CutoverStateSet(ctx, key, "v"); err == nil {
			t.Fatalf("CutoverStateSet(%q) succeeded", key)
		}
	}
}

func TestCutoverInventoryReportsUnfinishedWork(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	submitTestDocument(t, stores, "manuals", "queued.txt", []byte("x"), nil)
	documentProcessing := submitTestDocument(t, stores, "manuals", "processing.txt", []byte("y"), nil)
	if task, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, documentProcessing.ArtifactID)); err != nil || task == nil {
		t.Fatalf("document Claim() = %+v, %v", task, err)
	}
	cacheKeys, err := stores.monitorCount(ctx, "SELECT COUNT(*) FROM cache_revisions")
	if err != nil {
		t.Fatalf("cache key count error = %v", err)
	}
	dirtyKeys, err := stores.monitorCount(ctx,
		"SELECT COUNT(*) FROM cache_revisions WHERE refreshed_revision < revision")
	if err != nil {
		t.Fatalf("dirty key count error = %v", err)
	}
	inventory, err := stores.CutoverInventory(ctx, time.Now())
	if err != nil || inventory == nil {
		t.Fatalf("CutoverInventory() = %+v, %v", inventory, err)
	}
	if inventory.DocumentJobs.Active != 2 || inventory.DocumentJobs.ByStatus[DocumentJobQueued] != 1 ||
		inventory.DocumentJobs.ByStatus[DocumentJobProcessing] != 1 {
		t.Fatalf("document inventory = %+v", inventory.DocumentJobs)
	}
	if inventory.DocumentJobs.MinActiveScheduleVersion != 1 || inventory.DocumentJobs.OldestActiveSeconds < 0 {
		t.Fatalf("document version/age = %+v", inventory.DocumentJobs)
	}
	if inventory.CacheKeys != cacheKeys || inventory.CacheDirtyKeys != dirtyKeys {
		t.Fatalf("cache inventory = keys %d/%d, dirty %d/%d",
			inventory.CacheKeys, cacheKeys, inventory.CacheDirtyKeys, dirtyKeys)
	}
	keys, err := stores.CutoverCacheRevisions(ctx)
	if err != nil || len(keys) != cacheKeys {
		t.Fatalf("CutoverCacheRevisions() = %d rows, %v; want %d", len(keys), err, cacheKeys)
	}
	dirty := 0
	for _, key := range keys {
		if key.RefreshedRevision < key.Revision {
			dirty++
		}
	}
	if dirty != dirtyKeys {
		t.Fatalf("CutoverCacheRevisions() dirty rows = %d, want %d", dirty, dirtyKeys)
	}
}

func TestRequeueActiveDocumentJobsBumpsBaselineIdempotently(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	first := submitTestDocument(t, stores, "manuals", "first.txt", []byte("x"), nil)
	second := submitTestDocument(t, stores, "manuals", "second.txt", []byte("y"), nil)
	// Make the second job active-but-processing so the backfill must reset the
	// execution fencing as well.
	if task, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, second.ArtifactID)); err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	requeued, err := stores.DocumentJobs.RequeueActive(ctx, 10, 3)
	if err != nil || requeued != 2 {
		t.Fatalf("RequeueActive() = %d, %v", requeued, err)
	}
	for _, artifactID := range []string{first.ArtifactID, second.ArtifactID} {
		job, err := stores.DocumentJobs.Get(ctx, artifactID, nil)
		if err != nil || job == nil {
			t.Fatalf("Get(%s) = %+v, %v", artifactID, job, err)
		}
		// GREATEST(schedule_version, baseline) + 1
		if job.ScheduleVersion != 4 || job.Status != DocumentJobQueued || job.Stage != "retry_wait" ||
			job.Owner != nil || job.ExecutionID != nil || job.LeaseUntil != 0 || job.RetryAt <= 0 {
			t.Fatalf("requeued job = %+v", job)
		}
	}
	// Idempotent: a second run with the same baseline touches nothing.
	if requeued, err := stores.DocumentJobs.RequeueActive(ctx, 10, 3); err != nil || requeued != 0 {
		t.Fatalf("second RequeueActive() = %d, %v", requeued, err)
	}
	inventory, err := stores.CutoverInventory(ctx, time.Now())
	if err != nil || inventory.DocumentJobs.MinActiveScheduleVersion != 4 {
		t.Fatalf("inventory after backfill = %+v, %v", inventory, err)
	}
	// The pre-cutover generation is skipped; the backfilled generation claims.
	stale := DocumentNotification{Schema: DocumentEnvelopeSchema, ArtifactID: first.ArtifactID,
		Namespace: "manuals", ScheduleVersion: 1}
	if task, err := stores.DocumentJobs.Claim(ctx, stale); err != nil || task != nil {
		t.Fatalf("stale generation Claim() = %+v, %v", task, err)
	}
	backfilled := DocumentNotification{Schema: DocumentEnvelopeSchema, ArtifactID: first.ArtifactID,
		Namespace: "manuals", ScheduleVersion: 4}
	if task, err := stores.DocumentJobs.Claim(ctx, backfilled); err != nil || task == nil {
		t.Fatalf("backfilled Claim() = %+v, %v", task, err)
	}
	// A higher baseline bumps again, and the batch limit is honoured.
	if requeued, err := stores.DocumentJobs.RequeueActive(ctx, 1, 5); err != nil || requeued != 1 {
		t.Fatalf("limited RequeueActive() = %d, %v", requeued, err)
	}
	if requeued, err := stores.DocumentJobs.RequeueActive(ctx, 10, 5); err != nil || requeued != 1 {
		t.Fatalf("rest RequeueActive() = %d, %v", requeued, err)
	}
	if requeued, err := stores.DocumentJobs.RequeueActive(ctx, 10, 5); err != nil || requeued != 0 {
		t.Fatalf("third RequeueActive() = %d, %v", requeued, err)
	}
	// Rows newer than the baseline are never rewritten.
	if requeued, err := stores.DocumentJobs.RequeueActive(ctx, 10, 2); err != nil || requeued != 0 {
		t.Fatalf("stale-baseline RequeueActive() = %d, %v", requeued, err)
	}
}
