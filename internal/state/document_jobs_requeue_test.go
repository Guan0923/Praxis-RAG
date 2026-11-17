package state

import (
	"context"
	"testing"
)

// TestDocumentJobRequeueDueNowIgnoresQuietWindowAndExpiresLeases covers the
// operator-triggered immediate requeue behind the Console "立即重投" action:
//
//   - a due retry_wait row is requeued even when its updated_at is fresh
//     (the periodic Sweep may still wait for its quiet window);
//   - a processing row whose lease expired is requeued immediately;
//   - a merely queued row and a live lease are still left alone;
//   - the schedule_version/attempt semantics stay identical to Sweep.
func TestDocumentJobRequeueDueNowIgnoresQuietWindowAndExpiresLeases(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	clock := unixSeconds()
	stores.DocumentJobs.Clock = func() float64 { return clock }

	// A merely queued row must stay untouched: "waiting for a worker" is not a
	// retry and must not consume an execution attempt.
	stranded := submitTestDocument(t, stores, "manuals", "stranded.txt", []byte("x"), nil)
	// A live lease (a worker is actively processing) must not be stolen.
	live := submitTestDocument(t, stores, "manuals", "live.txt", []byte("l"), nil)
	if _, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, live.ArtifactID)); err != nil {
		t.Fatalf("Claim(live) error = %v", err)
	}

	// A retry_wait row whose retry_at has arrived but whose updated_at is
	// fresh (inside the quiet window): RequeueDueNow must act now.
	due := submitTestDocument(t, stores, "manuals", "due.txt", []byte("d"), nil)
	task, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, due.ArtifactID))
	if err != nil || task == nil {
		t.Fatalf("Claim(due) = %+v, %v", task, err)
	}
	reason := "Document processing failed: attempt 1/3: parse timeout (2m)"
	if err := stores.DocumentJobs.Fail(ctx, task, true, &reason); err != nil {
		t.Fatalf("Fail(due) error = %v", err)
	}
	job, err := stores.DocumentJobs.Get(ctx, due.ArtifactID, nil)
	if err != nil || job.Stage != "retry_wait" {
		t.Fatalf("job after retry = %+v, %v", job, err)
	}
	clock = job.RetryAt + 1 // retry_at is due; updated_at is still fresh.
	// The periodic sweep keeps waiting for the quiet window on freshly updated
	// rows; here updated_at moved with retry_at in the same Fail() write, so
	// whether Sweep sees it depends on the window length. Assert the operator
	// path directly instead of depending on that timing.
	if requeued, err := stores.DocumentJobs.RequeueDueNow(ctx, 50); err != nil || requeued != 1 {
		t.Fatalf("RequeueDueNow(due) = %d, %v", requeued, err)
	}
	job, err = stores.DocumentJobs.Get(ctx, due.ArtifactID, nil)
	if err != nil || job.Status != DocumentJobQueued || job.ScheduleVersion != 2 ||
		job.RetryAt != 0 || job.Attempts != 1 {
		t.Fatalf("requeued job = %+v, %v", job, err)
	}
	// The replacement generation is claimable and starts attempt 2.
	second, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, due.ArtifactID))
	if err != nil || second == nil || second.Attempts != 2 {
		t.Fatalf("replacement Claim() = %+v, %v", second, err)
	}

	// A processing row whose lease expired (worker crashed) is requeued now.
	crashed := submitTestDocument(t, stores, "manuals", "crashed.txt", []byte("z"), nil)
	if _, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, crashed.ArtifactID)); err != nil {
		t.Fatalf("Claim(crashed) error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET lease_until = 0 WHERE artifact_id = ?", crashed.ArtifactID); err != nil {
		t.Fatalf("lease expiry update error = %v", err)
	}
	if requeued, err := stores.DocumentJobs.RequeueDueNow(ctx, 50); err != nil || requeued != 1 {
		t.Fatalf("RequeueDueNow(expired) = %d, %v", requeued, err)
	}
	replacement, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, crashed.ArtifactID))
	if err != nil || replacement == nil || replacement.Attempts != 2 {
		t.Fatalf("replacement Claim(crashed) = %+v, %v", replacement, err)
	}

	// Untouched rows: the merely queued row and the live lease.
	for name, artifactID := range map[string]string{
		"stranded": stranded.ArtifactID,
		"live":     live.ArtifactID,
	} {
		job, err := stores.DocumentJobs.Get(ctx, artifactID, nil)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", name, err)
		}
		if job.ScheduleVersion != 1 {
			t.Fatalf("%s schedule_version = %d, want 1", name, job.ScheduleVersion)
		}
	}
}
