package state

import (
	"context"
	"strings"
	"testing"
)

func configureTestNamespace(t *testing.T, stores *Stores, namespace string) {
	t.Helper()
	if err := stores.KnowledgeBases.Create(context.Background(), map[string]any{
		"namespace": namespace, "collection": "test-collection",
	}); err != nil {
		t.Fatalf("KnowledgeBases.Create(%q) error = %v", namespace, err)
	}
}

func submitTestDocument(t *testing.T, stores *Stores, namespace, name string, content []byte, key *string) DocumentAdmission {
	t.Helper()
	admission, err := stores.DocumentJobs.Submit(context.Background(), namespace, name, content, key)
	if err != nil {
		t.Fatalf("DocumentJobs.Submit() error = %v", err)
	}
	return admission
}

func documentNotification(t *testing.T, stores *Stores, artifactID string) DocumentNotification {
	t.Helper()
	job, err := stores.DocumentJobs.Get(context.Background(), artifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get() = %+v, %v", job, err)
	}
	notification, err := DocumentJobEnvelope(job)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope() error = %v", err)
	}
	return notification
}

func TestDocumentJobSubmitPersistsQueuedJobWithoutOutbox(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	content := []byte("original")
	admission := submitTestDocument(t, stores, "manuals", "manual.txt", content, nil)
	if len(admission.ArtifactID) != 36 || admission.Status != DocumentJobQueued || admission.Reused {
		t.Fatalf("admission = %+v", admission)
	}
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("Get() = %+v, %v", job, err)
	}
	if job.Status != DocumentJobQueued || job.Stage != DocumentJobQueued || string(job.Content) != "original" ||
		job.Size != int64(len(content)) || job.SHA256 != sha256Sum(content) || job.ConfigurationVersion != 1 ||
		job.ScheduleVersion != 1 {
		t.Fatalf("job = %+v", job)
	}
	// The retired outbox tables are no longer part of the schema, and Submit
	// never writes anything besides document_jobs.
	for _, table := range []string{"document_job_outbox", "search_task_outbox", "cache_outbox", "document_task_outbox"} {
		if _, found := TableByName(table); found {
			t.Fatalf("retired table %q is still declared in the schema", table)
		}
	}
	notification, err := DocumentJobEnvelope(job)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope() error = %v", err)
	}
	if notification.Schema != DocumentEnvelopeSchema || notification.ArtifactID != admission.ArtifactID ||
		notification.Namespace != "manuals" || notification.ScheduleVersion != 1 {
		t.Fatalf("notification = %+v", notification)
	}
	encoded, err := marshalJSON(notification)
	if err != nil || strings.Contains(encoded, "original") || strings.Contains(encoded, "content") ||
		strings.Contains(encoded, `"name"`) {
		t.Fatalf("notification leaked content: %s, %v", encoded, err)
	}
}

func TestDocumentJobIdempotencyReplayAndConflicts(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	key := "same"
	first := submitTestDocument(t, stores, "manuals", "a.txt", []byte("same"), &key)
	replay := submitTestDocument(t, stores, "manuals", "a.txt", []byte("same"), &key)
	if !replay.Reused || replay.ArtifactID != first.ArtifactID {
		t.Fatalf("replay = %+v", replay)
	}
	if _, err := stores.DocumentJobs.Submit(ctx, "manuals", "a.txt", []byte("different"), &key); err == nil {
		t.Fatal("different file input with the same key succeeded")
	} else if conflict := mustBeError[*UploadConflict](t, err); conflict.Message != "Idempotency key has different file input" {
		t.Fatalf("conflict = %q", conflict.Message)
	}
	// A different filename under a new key conflicts with the queued name.
	otherKey := "other"
	_, err := stores.DocumentJobs.Submit(ctx, "manuals", "a.txt", []byte("same"), &otherKey)
	if conflict := mustBeError[*UploadConflict](t, err); conflict.Message != "A document with this filename is already queued" {
		t.Fatalf("queued conflict = %q", conflict.Message)
	}
	// A published document cannot be re-admitted by name.
	if err := stores.Documents.Upsert(ctx, "manuals", DocumentRecord{
		Name: "published.txt", DocumentID: "doc-published", Collection: "test-collection",
	}); err != nil {
		t.Fatalf("Documents.Upsert() error = %v", err)
	}
	if _, err := stores.DocumentJobs.Submit(ctx, "manuals", "published.txt", []byte("x"), nil); err == nil {
		t.Fatal("published document name was admitted")
	} else if conflict := mustBeError[*UploadConflict](t, err); conflict.Message != "Document already exists; use update_document" {
		t.Fatalf("published conflict = %q", conflict.Message)
	}
	// Validation happens before any database write.
	for name, content := range map[string][]byte{
		"bad.exe":     []byte("x"),
		"":            []byte("x"),
		"CON.txt":     []byte("x"),
		"../evil.txt": []byte("x"),
	} {
		if _, err := stores.DocumentJobs.Submit(ctx, "manuals", name, content, nil); err == nil {
			t.Fatalf("invalid filename %q was admitted", name)
		} else {
			mustBeError[*DocumentValidationError](t, err)
		}
	}
	if _, err := stores.DocumentJobs.Submit(ctx, "manuals", "empty.txt", nil, nil); err == nil {
		t.Fatal("empty file was admitted")
	} else {
		mustBeError[*DocumentValidationError](t, err)
	}
	longKey := strings.Repeat("k", 129)
	if _, err := stores.DocumentJobs.Submit(ctx, "manuals", "key.txt", []byte("x"), &longKey); err == nil {
		t.Fatal("oversized idempotency key was admitted")
	} else {
		mustBeError[*DocumentValidationError](t, err)
	}
	// Missing knowledge base configuration is rejected.
	if _, err := stores.DocumentJobs.Submit(ctx, "missing", "a.txt", []byte("x"), nil); err == nil {
		t.Fatal("unconfigured namespace was admitted")
	}
}

func TestDocumentJobClaimHeartbeatProgressAndVersionSemantics(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	clock := unixSeconds()
	stores.DocumentJobs.Clock = func() float64 { return clock }
	admission := submitTestDocument(t, stores, "manuals", "manual.txt", []byte("bytes"), nil)
	notification := documentNotification(t, stores, admission.ArtifactID)
	task, err := stores.DocumentJobs.Claim(ctx, notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	if task.Status != DocumentJobProcessing || task.Stage != "receiving" || task.Attempts != 1 ||
		string(task.Content) != "bytes" || task.Owner == nil || task.ExecutionID == nil || task.LeaseUntil <= 0 {
		t.Fatalf("task = %+v", task)
	}
	// Claim does not bump the scheduling generation, so the same notification
	// cannot be claimed twice while the lease is live.
	if second, err := stores.DocumentJobs.Claim(ctx, notification); err != nil || second != nil {
		t.Fatalf("live lease was re-claimed: %+v, %v", second, err)
	}
	if err := stores.DocumentJobs.Heartbeat(ctx, task); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if err := stores.DocumentJobs.Progress(ctx, task, "chunking"); err != nil {
		t.Fatalf("Progress() error = %v", err)
	}
	if task.Stage != "chunking" {
		t.Fatalf("task stage = %q", task.Stage)
	}
	stale := *task
	stale.ExecutionID = stringPtr("stale-execution")
	if err := stores.DocumentJobs.Progress(ctx, &stale, "nope"); err == nil {
		t.Fatal("stale execution progressed")
	} else {
		mustBeError[*UploadConflict](t, err)
	}
	retryReason := "Document processing failed: attempt 1/3: qdrant_error at indexing (HTTP 500): boom"
	if err := stores.DocumentJobs.Fail(ctx, task, true, &retryReason); err != nil {
		t.Fatalf("Fail(retry) error = %v", err)
	}
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job.Status != DocumentJobQueued || job.Stage != "retry_wait" ||
		job.RetryAt <= clock || job.ScheduleVersion != 1 || job.ExecutionID != nil {
		t.Fatalf("job after retry = %+v, %v", job, err)
	}
	// The retry branch keeps the last recorded failure reason visible while the
	// job waits for its next attempt.
	if job.Error == nil || *job.Error != retryReason {
		t.Fatalf("job after retry lost the failure reason: %+v", job)
	}
	// A duplicate delivery of the old generation is skipped before retry_at; the
	// periodic sweep is the only path that emits the replacement notification.
	if old, err := stores.DocumentJobs.Claim(ctx, notification); err != nil || old != nil {
		t.Fatalf("retry claimed before retry_at: %+v, %v", old, err)
	}
	if requeued, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || requeued != 0 {
		t.Fatalf("Sweep() before retry_at = %d, %v", requeued, err)
	}
	clock = job.RetryAt + 1
	if requeued, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || requeued != 1 {
		t.Fatalf("Sweep() at retry_at = %d, %v", requeued, err)
	}
	job, err = stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job.ScheduleVersion != 2 || job.RetryAt != 0 || job.Status != DocumentJobQueued {
		t.Fatalf("job after sweep = %+v, %v", job, err)
	}
	updated := documentNotification(t, stores, admission.ArtifactID)
	if updated.ScheduleVersion != 2 {
		t.Fatalf("notification after retry = %+v", updated)
	}
	task, err = stores.DocumentJobs.Claim(ctx, updated)
	if err != nil || task == nil || task.Attempts != 2 {
		t.Fatalf("second Claim() = %+v, %v", task, err)
	}
	if err := stores.DocumentJobs.Fail(ctx, task, false, stringPtr("Invalid document")); err != nil {
		t.Fatalf("Fail(terminal) error = %v", err)
	}
	status, err := stores.DocumentJobs.Status(ctx, "manuals", admission.ArtifactID)
	if err != nil || status.Status != DocumentJobFailed || status.Stage != "failed" ||
		status.Error == nil || *status.Error != "Invalid document" {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if terminal, err := stores.DocumentJobs.Terminal(ctx, admission.ArtifactID); err != nil || !terminal {
		t.Fatalf("Terminal() = %v, %v", terminal, err)
	}
	// Terminal rows are never reclaimed, even for the current generation.
	if reclaimed, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, admission.ArtifactID)); err != nil || reclaimed != nil {
		t.Fatalf("terminal job was reclaimed: %+v, %v", reclaimed, err)
	}
	if _, err := stores.DocumentJobs.Status(ctx, "manuals", "d1feec53-1036-4af0-8035-86ef6990fa6a"); err == nil {
		t.Fatal("missing status lookup succeeded")
	} else {
		mustBeError[*LookupError](t, err)
	}
	if _, err := stores.DocumentJobs.Get(ctx, "not-a-uuid", nil); err == nil {
		t.Fatal("noncanonical artifact id accepted")
	}
}

func TestDocumentJobRetryFailureReasonPersistsThenSuccessClears(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	clock := unixSeconds()
	stores.DocumentJobs.Clock = func() float64 { return clock }
	admission := submitTestDocument(t, stores, "manuals", "retry.txt", []byte("bytes"), nil)
	notification := documentNotification(t, stores, admission.ArtifactID)
	task, err := stores.DocumentJobs.Claim(ctx, notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	reason := "Document processing failed: attempt 1/3: model_api_error at embedding (HTTP 429): rate limited"
	if err := stores.DocumentJobs.Fail(ctx, task, true, &reason); err != nil {
		t.Fatalf("Fail(retry) error = %v", err)
	}
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job.Error == nil || *job.Error != reason {
		t.Fatalf("retry_wait job error = %+v, %v; want %q", job, err, reason)
	}
	clock = job.RetryAt + 1
	if _, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	task, err = stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, admission.ArtifactID))
	if err != nil || task == nil {
		t.Fatalf("second Claim() = %+v, %v", task, err)
	}
	// Column-level check of the success path: finish() is what Succeed() calls
	// after Reconcile proves the publication; a terminal success must not keep
	// the previous attempt's reason.
	if err := stores.DocumentJobs.finish(ctx, task, DocumentJobSucceeded, nil, nil); err != nil {
		t.Fatalf("finish(success) error = %v", err)
	}
	status, err := stores.DocumentJobs.Status(ctx, "manuals", admission.ArtifactID)
	if err != nil || status.Status != DocumentJobSucceeded || status.Stage != "completed" {
		t.Fatalf("status after success = %+v, %v", status, err)
	}
	if status.Error != nil {
		t.Fatalf("success kept the retry failure reason: %q", *status.Error)
	}
}

func TestDocumentJobDeadLetterAndNotificationValidation(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	admission := submitTestDocument(t, stores, "manuals", "a.txt", []byte("x"), nil)
	notification := documentNotification(t, stores, admission.ArtifactID)
	// Application-native schema 4 and Debezium change events are accepted.
	native, err := ValidateDocumentNotification(map[string]any{
		"schema": float64(4), "artifact_id": admission.ArtifactID, "namespace": "manuals",
		"schedule_version": float64(1),
	})
	if err != nil || native.Schema != DocumentEnvelopeSchema || native.ScheduleVersion != 1 {
		t.Fatalf("native envelope = %+v, %v", native, err)
	}
	for _, op := range []string{"c", "r", "u"} {
		debezium, err := ValidateDocumentNotification(map[string]any{
			"after": map[string]any{
				"artifact_id": admission.ArtifactID, "namespace": "manuals", "schedule_version": float64(3),
			},
			"op": op,
		})
		if err != nil || debezium.ScheduleVersion != 3 {
			t.Fatalf("Debezium op %q = %+v, %v", op, debezium, err)
		}
	}
	// Deletes, tombstones and before-only changes are recognisable non-tasks.
	for name, payload := range map[string]any{
		"null tombstone":       nil,
		"op delete":            map[string]any{"before": map[string]any{"artifact_id": admission.ArtifactID}, "after": nil, "op": "d"},
		"before-only":          map[string]any{"before": map[string]any{"artifact_id": admission.ArtifactID}},
		"delete without image": map[string]any{"after": nil, "op": "d"},
		"truncate without row": map[string]any{"op": "t"},
	} {
		if _, err := ValidateDocumentNotification(payload); !IgnorableCDCChange(err) {
			t.Fatalf("%s: error = %v, want ignorable", name, err)
		}
	}
	for name, payload := range map[string]any{
		"not a map":        "x",
		"old schema 3":     map[string]any{"schema": 3, "artifact_id": admission.ArtifactID, "namespace": "manuals", "name": "a.txt", "created_at": 1.5},
		"bad artifact":     map[string]any{"schema": 4, "artifact_id": "nope", "namespace": "manuals", "schedule_version": 1},
		"bad namespace":    map[string]any{"schema": 4, "artifact_id": admission.ArtifactID, "namespace": "", "schedule_version": 1},
		"missing version":  map[string]any{"schema": 4, "artifact_id": admission.ArtifactID, "namespace": "manuals"},
		"zero version":     map[string]any{"schema": 4, "artifact_id": admission.ArtifactID, "namespace": "manuals", "schedule_version": 0},
		"fraction version": map[string]any{"after": map[string]any{"artifact_id": admission.ArtifactID, "namespace": "manuals", "schedule_version": 1.5}, "op": "u"},
		"unknown op":       map[string]any{"after": map[string]any{"artifact_id": admission.ArtifactID, "namespace": "manuals", "schedule_version": 1}, "op": "x"},
	} {
		if _, err := ValidateDocumentNotification(payload); err == nil || IgnorableCDCChange(err) {
			t.Fatalf("%s: invalid notification accepted (err=%v)", name, err)
		}
	}
	task, err := stores.DocumentJobs.Claim(ctx, notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	if acked, err := stores.DocumentJobs.DeadLetter(ctx, notification); err != nil || acked {
		t.Fatalf("live DeadLetter() = %v, %v", acked, err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET lease_until = 0 WHERE artifact_id = ?", admission.ArtifactID); err != nil {
		t.Fatalf("lease expiry update error = %v", err)
	}
	lastFailure := "Document processing failed: pdf_parse_error at parsing (HTTP 502): upstream parse failed"
	if err := stores.DocumentJobs.Fail(ctx, task, true, &lastFailure); err != nil {
		t.Fatalf("Fail(retry) error = %v", err)
	}
	// The dead letter acts on the current generation; the old notification is
	// stale, so use the recomputed envelope.
	if acked, err := stores.DocumentJobs.DeadLetter(ctx, documentNotification(t, stores, admission.ArtifactID)); err != nil || !acked {
		t.Fatalf("DeadLetter() = %v, %v", acked, err)
	}
	status, err := stores.DocumentJobs.Status(ctx, "manuals", admission.ArtifactID)
	if err != nil || status.Status != DocumentJobFailed || status.Error == nil ||
		*status.Error != "Message retry limit reached; last error: "+lastFailure {
		t.Fatalf("status = %+v, %v", status, err)
	}
	unknown := notification
	unknown.ArtifactID = "d1feec53-1036-4af0-8035-86ef6990fa6a"
	if acked, err := stores.DocumentJobs.DeadLetter(ctx, unknown); err != nil || !acked {
		t.Fatalf("unknown DeadLetter() = %v, %v", acked, err)
	}
}

func TestDocumentJobSweepLeavesQueuedJobsAndEmitsDueRetries(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	clock := unixSeconds()
	stores.DocumentJobs.Clock = func() float64 { return clock }

	// A job that is merely queued (notification in flight or waiting for a
	// worker/parse slot) is not a retry: aging it must not bump the scheduling
	// generation or consume an execution attempt.
	stranded := submitTestDocument(t, stores, "manuals", "stranded.txt", []byte("x"), nil)
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET updated_at = ? WHERE artifact_id = ?",
		clock-3600, stranded.ArtifactID); err != nil {
		t.Fatalf("age job error = %v", err)
	}
	if requeued, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || requeued != 0 {
		t.Fatalf("Sweep(queued) = %d, %v", requeued, err)
	}
	job, err := stores.DocumentJobs.Get(ctx, stranded.ArtifactID, nil)
	if err != nil || job.Status != DocumentJobQueued || job.Stage != DocumentJobQueued ||
		job.ScheduleVersion != 1 || job.Attempts != 0 {
		t.Fatalf("merely queued job was changed: %+v, %v", job, err)
	}

	// A retryable failure records retry_at and the reason but does not emit a
	// notification immediately.
	retried := submitTestDocument(t, stores, "manuals", "retry.txt", []byte("y"), nil)
	task, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, retried.ArtifactID))
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	reason := "Document processing failed: attempt 1/3: parse timeout (2m)"
	if err := stores.DocumentJobs.Fail(ctx, task, true, &reason); err != nil {
		t.Fatalf("Fail(retry) error = %v", err)
	}
	job, err = stores.DocumentJobs.Get(ctx, retried.ArtifactID, nil)
	if err != nil || job.Status != DocumentJobQueued || job.Stage != "retry_wait" ||
		job.ScheduleVersion != 1 || job.Attempts != 1 || job.RetryAt <= clock {
		t.Fatalf("job after retry = %+v, %v", job, err)
	}
	if requeued, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || requeued != 0 {
		t.Fatalf("Sweep(before retry_at) = %d, %v", requeued, err)
	}
	clock = job.RetryAt + 1
	if requeued, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || requeued != 1 {
		t.Fatalf("Sweep(at retry_at) = %d, %v", requeued, err)
	}
	job, err = stores.DocumentJobs.Get(ctx, retried.ArtifactID, nil)
	if err != nil || job.ScheduleVersion != 2 || job.RetryAt != 0 || job.Status != DocumentJobQueued {
		t.Fatalf("job after sweep = %+v, %v", job, err)
	}
	// The pre-sweep generation is stale; the replacement generation claims the
	// second attempt.
	stale := DocumentNotification{Schema: DocumentEnvelopeSchema, ArtifactID: retried.ArtifactID,
		Namespace: "manuals", ScheduleVersion: 1}
	if task, err := stores.DocumentJobs.Claim(ctx, stale); err != nil || task != nil {
		t.Fatalf("stale generation claim = %+v, %v", task, err)
	}
	secondTask, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, retried.ArtifactID))
	if err != nil || secondTask == nil || secondTask.Attempts != 2 {
		t.Fatalf("replacement Claim() = %+v, %v", secondTask, err)
	}

	// A processing job whose worker crashed is requeued once its lease expires.
	crashed := submitTestDocument(t, stores, "manuals", "expired.txt", []byte("z"), nil)
	if task, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, crashed.ArtifactID)); err != nil || task == nil {
		t.Fatalf("Claim(crashed) = %+v, %v", task, err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET lease_until = 0 WHERE artifact_id = ?", crashed.ArtifactID); err != nil {
		t.Fatalf("lease expiry update error = %v", err)
	}
	// Keep the second retried execution's lease live so only the crashed job is
	// selected by the sweep.
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET lease_until = ? WHERE artifact_id = ?",
		clock+3600, secondTask.ArtifactID); err != nil {
		t.Fatalf("lease keepalive update error = %v", err)
	}
	clock += 500
	if requeued, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || requeued != 1 {
		t.Fatalf("Sweep(expired) = %d, %v", requeued, err)
	}
	job, err = stores.DocumentJobs.Get(ctx, crashed.ArtifactID, nil)
	if err != nil || job.Status != DocumentJobQueued || job.Stage != "retry_wait" ||
		job.ScheduleVersion != 2 || job.RetryAt != 0 {
		t.Fatalf("requeued job = %+v, %v", job, err)
	}
	replacement, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, crashed.ArtifactID))
	if err != nil || replacement == nil || replacement.Attempts != 2 {
		t.Fatalf("replacement Claim() = %+v, %v", replacement, err)
	}
	// Retention GC deletes terminal jobs.
	if err := stores.DocumentJobs.Fail(ctx, replacement, false, stringPtr("fail")); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET updated_at = ? WHERE artifact_id = ?", clock-100, crashed.ArtifactID); err != nil {
		t.Fatalf("age terminal job error = %v", err)
	}
	removed, err := stores.DocumentJobs.GCJobs(ctx, float64Pointer(0))
	if err != nil || removed != 1 {
		t.Fatalf("GCJobs() = %d, %v", removed, err)
	}
}

func TestDocumentJobAttemptLimitStopsFourthExecution(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	clock := unixSeconds()
	stores.DocumentJobs.Clock = func() float64 { return clock }
	admission := submitTestDocument(t, stores, "manuals", "timeout.pdf", []byte("timeout"), nil)

	for attempt := 1; attempt <= MaxDocumentJobAttempts; attempt++ {
		task, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, admission.ArtifactID))
		if err != nil || task == nil || task.Attempts != attempt {
			t.Fatalf("Claim(attempt %d) = %+v, %v", attempt, task, err)
		}
		reason := "Document processing failed: attempt 3/3: parse timeout (2m)"
		if err := stores.DocumentJobs.Fail(ctx, task, true, &reason); err != nil {
			t.Fatalf("Fail(attempt %d) error = %v", attempt, err)
		}
		job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
		if err != nil {
			t.Fatalf("Get(attempt %d) error = %v", attempt, err)
		}
		if attempt < MaxDocumentJobAttempts {
			if job.Status != DocumentJobQueued || job.Stage != "retry_wait" || job.Attempts != attempt {
				t.Fatalf("job after attempt %d = %+v", attempt, job)
			}
			clock = job.RetryAt + 1
			if requeued, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || requeued != 1 {
				t.Fatalf("Sweep(after attempt %d) = %d, %v", attempt, requeued, err)
			}
			continue
		}
		if job.Status != DocumentJobFailed || job.Stage != "failed" || job.Attempts != MaxDocumentJobAttempts {
			t.Fatalf("job after final failure = %+v", job)
		}
		if job.Error == nil || !strings.Contains(*job.Error, "parse timeout") {
			t.Fatalf("final failure lost the reason: %+v", job.Error)
		}
	}

	// Neither a duplicate notification nor the periodic sweep may start a
	// fourth execution.
	if task, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, admission.ArtifactID)); err != nil || task != nil {
		t.Fatalf("fourth Claim() = %+v, %v", task, err)
	}
	if requeued, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || requeued != 0 {
		t.Fatalf("Sweep(after failure) = %d, %v", requeued, err)
	}
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job.Attempts != MaxDocumentJobAttempts || job.Status != DocumentJobFailed {
		t.Fatalf("final job = %+v, %v", job, err)
	}
}

func TestDocumentJobExpiredLeaseAtAttemptLimitTerminalizes(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	clock := unixSeconds()
	stores.DocumentJobs.Clock = func() float64 { return clock }
	admission := submitTestDocument(t, stores, "manuals", "crash.pdf", []byte("crash"), nil)
	reason := "Document processing failed: attempt 1/3: parse timeout"

	for attempt := 1; attempt <= MaxDocumentJobAttempts; attempt++ {
		task, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, admission.ArtifactID))
		if err != nil || task == nil || task.Attempts != attempt {
			t.Fatalf("Claim(attempt %d) = %+v, %v", attempt, task, err)
		}
		if attempt == MaxDocumentJobAttempts {
			// The third execution crashed without recording a failure; the
			// expired lease must terminalize instead of starting a fourth.
			if _, err := stores.DB.ExecContext(ctx,
				"UPDATE document_jobs SET lease_until = 0 WHERE artifact_id = ?", admission.ArtifactID); err != nil {
				t.Fatalf("lease expiry update error = %v", err)
			}
			clock += 1
			if requeued, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || requeued != 0 {
				t.Fatalf("Sweep(at attempt limit) = %d, %v", requeued, err)
			}
			continue
		}
		if err := stores.DocumentJobs.Fail(ctx, task, true, &reason); err != nil {
			t.Fatalf("Fail(attempt %d) error = %v", attempt, err)
		}
		job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
		if err != nil {
			t.Fatalf("Get(attempt %d) error = %v", attempt, err)
		}
		clock = job.RetryAt + 1
		if requeued, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || requeued != 1 {
			t.Fatalf("Sweep(after attempt %d) = %d, %v", attempt, requeued, err)
		}
	}
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job.Status != DocumentJobFailed || job.Attempts != MaxDocumentJobAttempts {
		t.Fatalf("job after expired third lease = %+v, %v", job, err)
	}
	if job.Error == nil || !strings.Contains(*job.Error, "attempt limit reached") {
		t.Fatalf("terminal reason = %+v, want the attempt-limit message", job.Error)
	}
	if task, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, admission.ArtifactID)); err != nil || task != nil {
		t.Fatalf("claim after terminalized crash = %+v, %v", task, err)
	}
}

func TestDocumentJobReconcileCommittedPublication(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	content := []byte("committed")
	admission := submitTestDocument(t, stores, "manuals", "manual.txt", content, nil)
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("Get() = %+v, %v", job, err)
	}
	task := &DocumentJobTask{DocumentJob: *job}
	// No publication yet: reconcile is false and success is a conflict.
	if reconciled, err := stores.DocumentJobs.Reconcile(ctx, task); err != nil || reconciled {
		t.Fatalf("Reconcile() = %v, %v", reconciled, err)
	}
	if err := stores.DocumentJobs.Succeed(ctx, task); err == nil {
		t.Fatal("Succeed() without a publication succeeded")
	} else {
		mustBeError[*UploadConflict](t, err)
	}
	// An unfinished journal execution makes the task busy and undeliverable.
	idempotencyKey := "document-task:" + admission.ArtifactID + ":run"
	operation, err := stores.Uploads.Begin(ctx, "manuals", "manual.txt", "doc-1", "test-collection", BeginOptions{
		IdempotencyKey: &idempotencyKey,
	})
	if err != nil {
		t.Fatalf("Uploads.Begin() error = %v", err)
	}
	if busy, err := stores.DocumentJobs.ExecutionBusy(ctx, task); err != nil || !busy {
		t.Fatalf("ExecutionBusy() = %v, %v", busy, err)
	}
	if acked, err := stores.DocumentJobs.DeadLetter(ctx, documentNotification(t, stores, admission.ArtifactID)); err != nil || acked {
		t.Fatalf("DeadLetter() while busy = %v, %v", acked, err)
	}
	// Commit the journal publication with the task's identifying result fields,
	// then reconcile it into the terminal state.
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := stores.Documents.UpsertTx(ctx, transaction, "manuals", DocumentRecord{
		Name: "manual.txt", DocumentID: "doc-1", Collection: "test-collection",
	}); err != nil {
		t.Fatalf("Documents.UpsertTx() error = %v", err)
	}
	result := map[string]any{
		"sha256":                     sha256Sum(content),
		"size":                       float64(len(content)),
		"task_configuration_version": float64(job.ConfigurationVersion),
	}
	if _, err := stores.Uploads.Commit(ctx, operation, result, transaction); err != nil {
		t.Fatalf("Uploads.Commit() error = %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("Commit tx error = %v", err)
	}
	if err := stores.DocumentJobs.Succeed(ctx, task); err != nil {
		t.Fatalf("Succeed() error = %v", err)
	}
	current, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || current.Status != DocumentJobSucceeded || current.Stage != "completed" ||
		len(current.Content) != 0 || current.OperationID == nil || *current.OperationID != operation.ID {
		t.Fatalf("terminal job = %+v, %v", current, err)
	}
	if terminal, err := stores.DocumentJobs.Terminal(ctx, admission.ArtifactID); err != nil || !terminal {
		t.Fatalf("Terminal() = %v, %v", terminal, err)
	}
}

func TestDocumentQueueSettingsValidation(t *testing.T) {
	isolateStateEnv(t)
	settings, err := DocumentQueueSettingsFromEnv()
	if err != nil || settings.Topic != "praxis_document_ingest" || settings.Group != "praxis_document_ingest_workers" {
		t.Fatalf("queue settings = %+v, %v", settings, err)
	}
	t.Setenv("RAG_DOCUMENT_TOPIC", "praxis_state_cache_refresh")
	if _, err := DocumentQueueSettingsFromEnv(); err == nil {
		t.Fatal("document topic reused the cache topic")
	}
	isolateStateEnv(t)
	t.Setenv("RAG_DOCUMENT_GROUP", "")
	if _, err := DocumentQueueSettingsFromEnv(); err == nil {
		t.Fatal("empty document group accepted")
	}
}

func TestStoresWiresG7StateMachines(t *testing.T) {
	stores := newTestStores(t)
	if stores.DocumentJobs == nil {
		t.Fatalf("G7 document job state machine not wired: %+v", stores)
	}
	if err := stores.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}
