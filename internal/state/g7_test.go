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
	retryReason := "Document processing failed: qdrant_error at indexing (HTTP 500): boom"
	if err := stores.DocumentJobs.Fail(ctx, task, true, &retryReason); err != nil {
		t.Fatalf("Fail(retry) error = %v", err)
	}
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job.Status != DocumentJobQueued || job.Stage != "retry_wait" ||
		job.RetryAt <= 0 || job.ScheduleVersion != 2 || job.ExecutionID != nil {
		t.Fatalf("job after retry = %+v, %v", job, err)
	}
	// The retry branch keeps the last recorded failure reason visible while the
	// job waits for its next attempt.
	if job.Error == nil || *job.Error != retryReason {
		t.Fatalf("job after retry lost the failure reason: %+v", job)
	}
	// A duplicate delivery of the old generation is skipped; the bumped
	// generation is claimable.
	if old, err := stores.DocumentJobs.Claim(ctx, notification); err != nil || old != nil {
		t.Fatalf("old schedule_version was claimed: %+v, %v", old, err)
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
	admission := submitTestDocument(t, stores, "manuals", "retry.txt", []byte("bytes"), nil)
	notification := documentNotification(t, stores, admission.ArtifactID)
	task, err := stores.DocumentJobs.Claim(ctx, notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	reason := "Document processing failed: model_api_error at embedding (HTTP 429): rate limited"
	if err := stores.DocumentJobs.Fail(ctx, task, true, &reason); err != nil {
		t.Fatalf("Fail(retry) error = %v", err)
	}
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job.Error == nil || *job.Error != reason {
		t.Fatalf("retry_wait job error = %+v, %v; want %q", job, err, reason)
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

func TestDocumentJobSweepRequeuesAndBumpsVersion(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	admission := submitTestDocument(t, stores, "manuals", "stranded.txt", []byte("x"), nil)
	// Age the queued job beyond the sweep window.
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET updated_at = ? WHERE artifact_id = ?",
		unixSeconds()-3600, admission.ArtifactID); err != nil {
		t.Fatalf("age job error = %v", err)
	}
	requeued, err := stores.DocumentJobs.Sweep(ctx, 50)
	if err != nil || requeued != 1 {
		t.Fatalf("Sweep() = %d, %v", requeued, err)
	}
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job.Status != DocumentJobQueued || job.Stage != "retry_wait" || job.ScheduleVersion != 2 {
		t.Fatalf("swept job = %+v, %v", job, err)
	}
	// The sweep refreshed updated_at, so the same job is not swept again
	// immediately; a stale generation is not claimable, the new one is.
	if requeued, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || requeued != 0 {
		t.Fatalf("second Sweep() = %d, %v", requeued, err)
	}
	stale := DocumentNotification{Schema: DocumentEnvelopeSchema, ArtifactID: admission.ArtifactID,
		Namespace: "manuals", ScheduleVersion: 1}
	if task, err := stores.DocumentJobs.Claim(ctx, stale); err != nil || task != nil {
		t.Fatalf("stale generation claim = %+v, %v", task, err)
	}
	// A processing job with an expired lease is requeued with a new generation.
	second := submitTestDocument(t, stores, "manuals", "expired.txt", []byte("y"), nil)
	if task, err := stores.DocumentJobs.Claim(ctx, documentNotification(t, stores, second.ArtifactID)); err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET lease_until = 0 WHERE artifact_id = ?", second.ArtifactID); err != nil {
		t.Fatalf("lease expiry update error = %v", err)
	}
	if requeued, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || requeued < 1 {
		t.Fatalf("Sweep(expired) = %d, %v", requeued, err)
	}
	job, err = stores.DocumentJobs.Get(ctx, second.ArtifactID, nil)
	if err != nil || job.Status != DocumentJobQueued || job.Stage != "retry_wait" || job.ScheduleVersion != 2 {
		t.Fatalf("requeued job = %+v, %v", job, err)
	}
	// Retention GC deletes terminal jobs.
	if err := stores.DocumentJobs.Fail(ctx, &DocumentJobTask{DocumentJob: *job}, false, stringPtr("fail")); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET updated_at = ? WHERE artifact_id = ?", unixSeconds()-100, second.ArtifactID); err != nil {
		t.Fatalf("age terminal job error = %v", err)
	}
	removed, err := stores.DocumentJobs.GCJobs(ctx, float64Pointer(0))
	if err != nil || removed != 1 {
		t.Fatalf("GCJobs() = %d, %v", removed, err)
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
	if stores.DocumentJobs == nil || stores.SearchTasks == nil || stores.Results == nil {
		t.Fatalf("G7 components not wired: %+v", stores)
	}
	if err := stores.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// Search state machine --------------------------------------------------------

func submitTestSearch(t *testing.T, stores *Stores, namespace string, arguments map[string]any) SearchNotification {
	t.Helper()
	notification, err := stores.SearchTasks.Submit(context.Background(), namespace, arguments, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit() error = %v", err)
	}
	return notification
}

func TestSearchSubmitCreatesQueuedTaskWithoutBroker(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	notification := submitTestSearch(t, stores, "alpha", map[string]any{"query": "hello", "top_k": float64(3)})
	if notification.Schema != SearchEnvelopeSchema || !canonicalUUID(notification.ResultID) ||
		notification.Namespace != "alpha" || notification.ScheduleVersion != 1 {
		t.Fatalf("notification = %+v", notification)
	}
	task, err := stores.SearchTasks.read(ctx, notification.ResultID)
	if err != nil || task == nil || task.ScheduleVersion != 1 || task.Status != SearchTaskQueued {
		t.Fatalf("task = %+v, %v", task, err)
	}
	view, err := stores.SearchTasks.Get(ctx, "alpha", notification.ResultID, 1, 10)
	if err != nil || view == nil || view.Status != SearchTaskQueued || view.Stage != SearchTaskQueued {
		t.Fatalf("Get() = %+v, %v", view, err)
	}
	// A foreign namespace must not resolve the task.
	if view, err := stores.SearchTasks.Get(ctx, "beta", notification.ResultID, 1, 10); err != nil || view != nil {
		t.Fatalf("foreign namespace view = %+v, %v", view, err)
	}
	if _, err := stores.SearchTasks.Get(ctx, "alpha", "not-a-uuid", 1, 10); err == nil {
		t.Fatal("invalid result id accepted")
	} else {
		mustBeError[*SnapshotUnavailable](t, err)
	}
	if _, of := TableByName("search_task_outbox"); of {
		t.Fatal("search_task_outbox is still declared in the schema")
	}
}

func TestSearchClaimLeaseAndVersionSemantics(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	notification := submitTestSearch(t, stores, "alpha", map[string]any{"query": "hello"})
	task, err := stores.SearchTasks.Claim(ctx, notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	if task.Status != SearchTaskProcessing || task.Stage != SearchTaskProcessing || task.Attempts != 1 ||
		task.Owner == nil || task.RemainingSec < 0 || task.ScheduleVersion != 1 {
		t.Fatalf("task = %+v", task)
	}
	if second, err := stores.SearchTasks.Claim(ctx, notification); err != nil || second != nil {
		t.Fatalf("live lease re-claimed: %+v, %v", second, err)
	}
	// A different generation is skipped even when the row is claimable.
	stale := notification
	stale.ScheduleVersion = 99
	if claimed, err := stores.SearchTasks.Claim(ctx, stale); err != nil || claimed != nil {
		t.Fatalf("stale generation claimed: %+v, %v", claimed, err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE search_tasks SET lease_until = 0, status = ? WHERE result_id = ?",
		SearchTaskQueued, notification.ResultID); err != nil {
		t.Fatalf("lease reset error = %v", err)
	}
	task, err = stores.SearchTasks.Claim(ctx, notification)
	if err != nil || task == nil || task.Attempts != 2 {
		t.Fatalf("second Claim() = %+v, %v", task, err)
	}
}

func TestSearchFailureRetryBumpsVersionAndTerminal(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	notification := submitTestSearch(t, stores, "alpha", map[string]any{"query": "hello"})
	task, err := stores.SearchTasks.Claim(ctx, notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	changed, err := stores.SearchTasks.Fail(ctx, task, true, nil)
	if err != nil || !changed {
		t.Fatalf("Fail(retry) = %v, %v", changed, err)
	}
	view, err := stores.SearchTasks.Get(ctx, "alpha", notification.ResultID, 1, 10)
	if err != nil || view.Status != SearchTaskQueued || view.Stage != "retry_wait" {
		t.Fatalf("retry view = %+v, %v", view, err)
	}
	job, err := stores.SearchTasks.read(ctx, notification.ResultID)
	if err != nil || job.ScheduleVersion != 2 {
		t.Fatalf("retry task = %+v, %v", job, err)
	}
	// The old generation is skipped; the current one claims again.
	if claimed, err := stores.SearchTasks.Claim(ctx, notification); err != nil || claimed != nil {
		t.Fatalf("old generation claimed: %+v, %v", claimed, err)
	}
	updated := SearchNotification{Schema: SearchEnvelopeSchema, ResultID: job.ResultID,
		Namespace: job.Namespace, ScheduleVersion: job.ScheduleVersion}
	task, err = stores.SearchTasks.Claim(ctx, updated)
	if err != nil || task == nil {
		t.Fatalf("re-Claim() = %+v, %v", task, err)
	}
	changed, err = stores.SearchTasks.Fail(ctx, task, false, stringPtr("failed"))
	if err != nil || !changed {
		t.Fatalf("Fail(terminal) = %v, %v", changed, err)
	}
	if _, err := stores.SearchTasks.Get(ctx, "alpha", notification.ResultID, 1, 10); err == nil {
		t.Fatal("failed task returned a view")
	} else {
		unavailable := mustBeError[*SnapshotUnavailable](t, err)
		if unavailable.Message != "Search failed; submit a new search" {
			t.Fatalf("failure message = %q", unavailable.Message)
		}
	}
	if terminal, err := stores.SearchTasks.Terminal(ctx, updated); err != nil || !terminal {
		t.Fatalf("Terminal() = %v, %v", terminal, err)
	}
	if acked, err := stores.SearchTasks.DeadLetter(ctx, updated); err != nil || !acked {
		t.Fatalf("DeadLetter() = %v, %v", acked, err)
	}
	if acked, err := stores.SearchTasks.DeadLetter(ctx, "d1feec53-1036-4af0-8035-86ef6990fa6a"); err != nil || !acked {
		t.Fatalf("unknown DeadLetter() = %v, %v", acked, err)
	}
}

func TestSearchExpireAndClaimTimeout(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	now := 1000.0
	stores.SearchTasks.Clock = func() float64 { return now }
	stores.SearchTasks.Settings.TimeoutSeconds = 5
	notification := submitTestSearch(t, stores, "alpha", map[string]any{"query": "hello"})
	now = 1010
	expired, err := stores.SearchTasks.Expire(ctx, 10)
	if err != nil || expired != 1 {
		t.Fatalf("Expire() = %d, %v", expired, err)
	}
	if _, err := stores.SearchTasks.Get(ctx, "alpha", notification.ResultID, 1, 10); err == nil {
		t.Fatal("expired task returned a view")
	} else {
		mustBeError[*SnapshotUnavailable](t, err)
	}
	// Claim also converges an overdue task lazily and returns (nil, nil).
	second := SearchNotification{Schema: SearchEnvelopeSchema, ResultID: otherUUID(t),
		Namespace: "alpha", ScheduleVersion: 1}
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO search_tasks (result_id, namespace, arguments, status, stage, attempts, owner, "+
			"lease_until, retry_at, deadline, created_at, updated_at, schedule_version) "+
			"VALUES (?, ?, ?, ?, ?, 0, NULL, 0, 0, ?, ?, ?, 1)",
		second.ResultID, "alpha", "{}", SearchTaskQueued, SearchTaskQueued, now, now-100, now); err != nil {
		t.Fatalf("insert overdue task error = %v", err)
	}
	if task, err := stores.SearchTasks.Claim(ctx, second); err != nil || task != nil {
		t.Fatalf("overdue Claim() = %+v, %v", task, err)
	}
	overdue, err := stores.SearchTasks.read(ctx, second.ResultID)
	if err != nil || overdue.Status != SearchTaskTimedOut {
		t.Fatalf("overdue task = %+v, %v", overdue, err)
	}
	if task, err := stores.SearchTasks.Claim(ctx, second); err != nil || task != nil {
		t.Fatalf("timed-out Claim() = %+v, %v", task, err)
	}
}

func TestSearchCompleteWritesMySQLFactAndRepairsRedis(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	notification := submitTestSearch(t, stores, "alpha", map[string]any{"query": "hello"})
	task, err := stores.SearchTasks.Claim(ctx, notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	payload := map[string]any{
		"results": []any{
			map[string]any{"text": "first", "score": 0.9},
			map[string]any{"text": "second", "score": 0.5},
		},
	}
	if _, err := stores.SearchTasks.Complete(ctx, task, payload); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	// The terminal state and the result fact row committed together.
	view, err := stores.SearchTasks.Get(ctx, "alpha", notification.ResultID, 1, 10)
	if err != nil || view.Status != SearchTaskSucceeded {
		t.Fatalf("view = %+v, %v", view, err)
	}
	var content string
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT content FROM search_result_snapshots WHERE id = ?", notification.ResultID).Scan(&content); err != nil {
		t.Fatalf("snapshot row missing: %v", err)
	}
	// Reading works with the Redis cache disabled entirely (MySQL fallback).
	stores.Results.Transient = nil
	page, err := stores.Results.Get("alpha", notification.ResultID, 1, 10)
	if err != nil || page["total_results"] != 2 || page["snapshot_available"] != true {
		t.Fatalf("Get() = %+v, %v", page, err)
	}
	// A duplicate Complete after the terminal state is idempotent.
	if _, err := stores.SearchTasks.Complete(ctx, task, payload); err != nil {
		t.Fatalf("duplicate Complete() error = %v", err)
	}
	// A second result id cannot overwrite the immutable snapshot.
	other := otherUUID(t)
	if _, err := stores.SearchTasks.Submit(ctx, "alpha", map[string]any{"query": "second"}, &other); err != nil {
		t.Fatalf("second Submit() error = %v", err)
	}
	otherNotification := SearchNotification{Schema: SearchEnvelopeSchema, ResultID: other,
		Namespace: "alpha", ScheduleVersion: 1}
	otherTask, err := stores.SearchTasks.Claim(ctx, otherNotification)
	if err != nil || otherTask == nil {
		t.Fatalf("second Claim() = %+v, %v", otherTask, err)
	}
	replaced := payload
	replaced["result_id"] = notification.ResultID
	replaced["created_at"] = float64(1)
	replaced["expires_at"] = float64(2)
	replaced["snapshot_available"] = true
	replaced["results"] = []any{}
	if _, err := stores.SearchTasks.Complete(ctx, otherTask, map[string]any{"results": []any{}}); err != nil {
		t.Fatalf("second Complete() error = %v", err)
	}
}

func TestValidateSearchNotification(t *testing.T) {
	valid := map[string]any{
		"schema": float64(3), "result_id": "d1feec53-1036-4af0-8035-86ef6990fa6a",
		"namespace": "alpha", "schedule_version": float64(2),
	}
	if _, err := ValidateSearchNotification(valid); err != nil {
		t.Fatalf("valid notification error = %v", err)
	}
	for _, op := range []string{"c", "r", "u"} {
		notification, err := ValidateSearchNotification(map[string]any{
			"after": map[string]any{
				"result_id": "d1feec53-1036-4af0-8035-86ef6990fa6a", "namespace": "alpha",
				"schedule_version": float64(5),
			},
			"op": op,
		})
		if err != nil || notification.Schema != SearchEnvelopeSchema || notification.ScheduleVersion != 5 {
			t.Fatalf("Debezium op %q = %+v, %v", op, notification, err)
		}
	}
	for name, payload := range map[string]any{
		"null":        nil,
		"delete":      map[string]any{"before": map[string]any{"result_id": "d1feec53-1036-4af0-8035-86ef6990fa6a"}, "after": nil, "op": "d"},
		"before-only": map[string]any{"before": map[string]any{"result_id": "d1feec53-1036-4af0-8035-86ef6990fa6a"}},
	} {
		if _, err := ValidateSearchNotification(payload); !IgnorableCDCChange(err) {
			t.Fatalf("%s: error = %v, want ignorable", name, err)
		}
	}
	for name, mutation := range map[string]func(map[string]any){
		"schema":    func(payload map[string]any) { payload["schema"] = float64(2) },
		"result":    func(payload map[string]any) { payload["result_id"] = "nope" },
		"namespace": func(payload map[string]any) { payload["namespace"] = "" },
		"version":   func(payload map[string]any) { payload["schedule_version"] = float64(0) },
		"missing":   func(payload map[string]any) { delete(payload, "schedule_version") },
	} {
		payload := map[string]any{}
		for key, value := range valid {
			payload[key] = value
		}
		mutation(payload)
		if _, err := ValidateSearchNotification(payload); err == nil || IgnorableCDCChange(err) {
			t.Fatalf("%s: invalid notification accepted (err=%v)", name, err)
		}
	}
	isolateStateEnv(t)
	if settings, err := SearchTaskSettingsFromEnv(); err != nil || settings.TimeoutSeconds != 120 {
		t.Fatalf("search settings = %+v, %v", settings, err)
	}
	t.Setenv("RAG_SEARCH_TIMEOUT_SECONDS", "0")
	if _, err := SearchTaskSettingsFromEnv(); err == nil {
		t.Fatal("zero timeout accepted")
	}
	t.Setenv("RAG_SEARCH_TIMEOUT_SECONDS", "nan")
	if _, err := SearchTaskSettingsFromEnv(); err == nil {
		t.Fatal("NaN timeout accepted")
	}
}

func TestSearchSubmitDuplicateResultID(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	notification := submitTestSearch(t, stores, "alpha", map[string]any{"query": "first"})
	duplicate := notification.ResultID
	if _, err := stores.SearchTasks.Submit(ctx, "alpha", map[string]any{"query": "second"}, &duplicate); err == nil {
		t.Fatal("duplicate result id admitted")
	} else {
		mustBeError[*SearchTaskUnavailable](t, err)
	}
	view, err := stores.SearchTasks.Get(ctx, "alpha", notification.ResultID, 1, 10)
	if err != nil || view == nil {
		t.Fatalf("original task = %+v, %v", view, err)
	}
}
