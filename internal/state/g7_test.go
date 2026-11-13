package state

import (
	"context"
	"strings"
	"sync"
	"testing"
)

type PublisherError struct{ message string }

func (e *PublisherError) Error() string { return e.message }

type fakeSearchPublisher struct {
	mutex  sync.Mutex
	sent   []SearchEnvelope
	err    error
	closed bool
}

func (f *fakeSearchPublisher) SendSearchEnvelope(_ context.Context, envelope SearchEnvelope) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, envelope)
	return nil
}

func (f *fakeSearchPublisher) Close() error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.closed = true
	return nil
}

func (f *fakeSearchPublisher) messages() []SearchEnvelope {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]SearchEnvelope(nil), f.sent...)
}

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

func TestDocumentJobSubmitCommitsJobAndPendingOutbox(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	content := []byte("original")
	admission := submitTestDocument(t, stores, "manuals", "manual.txt", content, nil)
	if len(admission.ArtifactID) != 36 || admission.Status != DocumentJobQueued || admission.Reused || admission.EventID == nil {
		t.Fatalf("admission = %+v", admission)
	}
	job, err := stores.DocumentJobs.Get(ctx, admission.ArtifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("Get() = %+v, %v", job, err)
	}
	if job.Status != DocumentJobQueued || job.Stage != DocumentJobQueued || string(job.Content) != "original" ||
		job.Size != int64(len(content)) || job.SHA256 != sha256Sum(content) || job.ConfigurationVersion != 1 {
		t.Fatalf("job = %+v", job)
	}
	events, err := stores.DocumentJobs.PendingOutbox(ctx, 10)
	if err != nil || len(events) != 1 || events[0].ArtifactID != admission.ArtifactID || events[0].Status != "PENDING" {
		t.Fatalf("PendingOutbox() = %+v, %v", events, err)
	}
	notification, err := DocumentJobEnvelope(job)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope() error = %v", err)
	}
	if notification.Schema != DocumentEnvelopeSchema || notification.ArtifactID != admission.ArtifactID ||
		notification.Namespace != "manuals" || notification.Name != "manual.txt" {
		t.Fatalf("notification = %+v", notification)
	}
	encoded, err := marshalJSON(notification)
	if err != nil || strings.Contains(encoded, "original") || strings.Contains(encoded, "content") {
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
	if !replay.Reused || replay.ArtifactID != first.ArtifactID || replay.EventID != nil {
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

func TestDocumentJobClaimHeartbeatProgressAndTerminal(t *testing.T) {
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
	if err != nil || job.Status != DocumentJobQueued || job.Stage != "retry_wait" || job.RetryAt <= 0 {
		t.Fatalf("job after retry = %+v, %v", job, err)
	}
	// The retry branch keeps the last recorded failure reason visible while the
	// job waits for its next attempt.
	if job.Error == nil || *job.Error != retryReason {
		t.Fatalf("job after retry lost the failure reason: %+v", job)
	}
	task, err = stores.DocumentJobs.Claim(ctx, notification)
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
	task, err = stores.DocumentJobs.Claim(ctx, notification)
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

func TestDocumentJobDeadLetterAndValidationEnvelope(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	admission := submitTestDocument(t, stores, "manuals", "a.txt", []byte("x"), nil)
	notification := documentNotification(t, stores, admission.ArtifactID)
	// Debezium change events are accepted and normalized.
	debezium, err := ValidateDocumentNotification(map[string]any{
		"after": map[string]any{
			"artifact_id": admission.ArtifactID, "namespace": "manuals", "name": "a.txt", "created_at": 1.5,
		},
		"op": "c",
	})
	if err != nil || debezium.Schema != DocumentEnvelopeSchema || debezium.CreatedAt != 1.5 {
		t.Fatalf("Debezium envelope = %+v, %v", debezium, err)
	}
	for name, payload := range map[string]any{
		"not a map":      "x",
		"unknown schema": map[string]any{"schema": 99},
		"bad artifact":   map[string]any{"schema": 3, "artifact_id": "nope", "namespace": "manuals", "name": "a.txt"},
		"bad namespace":  map[string]any{"schema": 3, "artifact_id": admission.ArtifactID, "namespace": "-bad", "name": "a.txt"},
		"bad name":       map[string]any{"schema": 3, "artifact_id": admission.ArtifactID, "namespace": "manuals", "name": "a.exe"},
		"bad timestamp":  map[string]any{"schema": 3, "artifact_id": admission.ArtifactID, "namespace": "manuals", "name": "a.txt", "created_at": -1.0},
	} {
		if _, err := ValidateDocumentNotification(payload); err == nil {
			t.Fatalf("%s: invalid notification accepted", name)
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
	if acked, err := stores.DocumentJobs.DeadLetter(ctx, notification); err != nil || !acked {
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

func TestDocumentJobSweepAndGC(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	configureTestNamespace(t, stores, "manuals")
	admission := submitTestDocument(t, stores, "manuals", "stranded.txt", []byte("x"), nil)
	if err := stores.DocumentJobs.MarkPublished(ctx, *admission.EventID, nil); err != nil {
		t.Fatalf("MarkPublished() error = %v", err)
	}
	// Age the job and its outbox row beyond the sweep window.
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET updated_at = ? WHERE artifact_id = ?",
		unixSeconds()-3600, admission.ArtifactID); err != nil {
		t.Fatalf("age job error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_job_outbox SET created_at = ? WHERE artifact_id = ?",
		unixSeconds()-8*24*3600, admission.ArtifactID); err != nil {
		t.Fatalf("age outbox error = %v", err)
	}
	reemitted, err := stores.DocumentJobs.Sweep(ctx, 50)
	if err != nil || reemitted != 1 {
		t.Fatalf("Sweep() = %d, %v", reemitted, err)
	}
	if events, err := stores.DocumentJobs.PendingOutbox(ctx, 10); err != nil || len(events) != 1 {
		t.Fatalf("PendingOutbox() = %+v, %v", events, err)
	}
	if reemitted, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || reemitted != 0 {
		t.Fatalf("second Sweep() = %d, %v", reemitted, err)
	}
	// A processing job with an expired lease is requeued and re-emitted.
	second := submitTestDocument(t, stores, "manuals", "expired.txt", []byte("y"), nil)
	notification := documentNotification(t, stores, second.ArtifactID)
	if task, err := stores.DocumentJobs.Claim(ctx, notification); err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET lease_until = 0 WHERE artifact_id = ?", second.ArtifactID); err != nil {
		t.Fatalf("lease expiry update error = %v", err)
	}
	if reemitted, err := stores.DocumentJobs.Sweep(ctx, 50); err != nil || reemitted < 1 {
		t.Fatalf("Sweep(expired) = %d, %v", reemitted, err)
	}
	job, err := stores.DocumentJobs.Get(ctx, second.ArtifactID, nil)
	if err != nil || job.Status != DocumentJobQueued || job.Stage != "retry_wait" {
		t.Fatalf("requeued job = %+v, %v", job, err)
	}
	// Retention GC deletes terminal jobs and old outbox rows.
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
	if removed, err := stores.DocumentJobs.GCOutbox(ctx, float64Pointer(0)); err != nil || removed == 0 {
		t.Fatalf("GCOutbox() = %d, %v", removed, err)
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
	if stores.DocumentJobs == nil ||
		stores.SearchTasks == nil || stores.SearchDispatcher == nil {
		t.Fatalf("G7 components not wired: %+v", stores)
	}
	if err := stores.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// Search state machine --------------------------------------------------------

func submitTestSearch(t *testing.T, stores *Stores, namespace string, arguments map[string]any) SearchEnvelope {
	t.Helper()
	envelope, err := stores.SearchTasks.Submit(context.Background(), namespace, arguments, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit() error = %v", err)
	}
	return envelope
}

func TestSearchSubmitCreatesTaskAndOutbox(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	envelope := submitTestSearch(t, stores, "alpha", map[string]any{"query": "hello", "top_k": float64(3)})
	if envelope.Schema != SearchEnvelopeSchema || !canonicalUUID(envelope.ResultID) ||
		envelope.Namespace != "alpha" || !(envelope.CreatedAt < envelope.Deadline && envelope.Deadline <= envelope.ExpiresAt) {
		t.Fatalf("envelope = %+v", envelope)
	}
	view, err := stores.SearchTasks.Get(ctx, "alpha", envelope.ResultID, 1, 10)
	if err != nil || view == nil || view.Status != SearchTaskQueued || view.Stage != SearchTaskQueued {
		t.Fatalf("Get() = %+v, %v", view, err)
	}
	if _, err := stores.SearchTasks.Get(ctx, "beta", envelope.ResultID, 1, 10); err != nil {
		t.Fatalf("foreign Get() error = %v", err)
	} else {
		// A foreign namespace must not resolve the task.
		if view, _ := stores.SearchTasks.Get(ctx, "beta", envelope.ResultID, 1, 10); view != nil {
			t.Fatalf("foreign namespace view = %+v", view)
		}
	}
	var pending int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM search_task_outbox WHERE pending = 1").Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending outbox rows = %d, %v", pending, err)
	}
	if _, err := stores.SearchTasks.Get(ctx, "alpha", "not-a-uuid", 1, 10); err == nil {
		t.Fatal("invalid result id accepted")
	} else {
		mustBeError[*SnapshotUnavailable](t, err)
	}
}

func TestSearchClaimLeaseAndAttempts(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	envelope := submitTestSearch(t, stores, "alpha", map[string]any{"query": "hello"})
	task, err := stores.SearchTasks.Claim(ctx, envelope)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	if task.Status != SearchTaskProcessing || task.Stage != SearchTaskProcessing || task.Attempts != 1 ||
		task.Owner == nil || task.RemainingSec < 0 {
		t.Fatalf("task = %+v", task)
	}
	if second, err := stores.SearchTasks.Claim(ctx, envelope); err != nil || second != nil {
		t.Fatalf("live lease re-claimed: %+v, %v", second, err)
	}
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE search_tasks SET lease_until = 0, status = ? WHERE result_id = ?",
		SearchTaskQueued, envelope.ResultID); err != nil {
		t.Fatalf("lease reset error = %v", err)
	}
	task, err = stores.SearchTasks.Claim(ctx, envelope)
	if err != nil || task == nil || task.Attempts != 2 {
		t.Fatalf("second Claim() = %+v, %v", task, err)
	}
	// A forged envelope (different arguments) must not claim the stored task.
	forged := envelope
	forged.Arguments = map[string]any{"query": "forged"}
	if forgedTask, err := stores.SearchTasks.Claim(ctx, forged); err != nil || forgedTask != nil {
		t.Fatalf("forged claim = %+v, %v", forgedTask, err)
	}
}

func TestSearchFailureRetryAndTerminal(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	envelope := submitTestSearch(t, stores, "alpha", map[string]any{"query": "hello"})
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE search_tasks SET status = ?, owner = ? WHERE result_id = ?",
		SearchTaskProcessing, "owner-token", envelope.ResultID); err != nil {
		t.Fatalf("status seed error = %v", err)
	}
	task, err := stores.SearchTasks.Claim(ctx, envelope)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	changed, err := stores.SearchTasks.Fail(ctx, task, true, nil)
	if err != nil || !changed {
		t.Fatalf("Fail(retry) = %v, %v", changed, err)
	}
	view, err := stores.SearchTasks.Get(ctx, "alpha", envelope.ResultID, 1, 10)
	if err != nil || view.Status != SearchTaskQueued || view.Stage != "retry_wait" {
		t.Fatalf("retry view = %+v, %v", view, err)
	}
	task, err = stores.SearchTasks.Claim(ctx, envelope)
	if err != nil || task == nil {
		t.Fatalf("re-Claim() = %+v, %v", task, err)
	}
	changed, err = stores.SearchTasks.Fail(ctx, task, false, stringPtr("failed"))
	if err != nil || !changed {
		t.Fatalf("Fail(terminal) = %v, %v", changed, err)
	}
	if _, err := stores.SearchTasks.Get(ctx, "alpha", envelope.ResultID, 1, 10); err == nil {
		t.Fatal("failed task returned a view")
	} else {
		unavailable := mustBeError[*SnapshotUnavailable](t, err)
		if unavailable.Message != "Search failed; submit a new search" {
			t.Fatalf("failure message = %q", unavailable.Message)
		}
	}
	if terminal, err := stores.SearchTasks.Terminal(ctx, envelope.ResultID); err != nil || !terminal {
		t.Fatalf("Terminal() = %v, %v", terminal, err)
	}
	if acked, err := stores.SearchTasks.DeadLetter(ctx, envelope); err != nil || !acked {
		t.Fatalf("DeadLetter() = %v, %v", acked, err)
	}
	if acked, err := stores.SearchTasks.DeadLetter(ctx, "d1feec53-1036-4af0-8035-86ef6990fa6a"); err != nil || !acked {
		t.Fatalf("unknown DeadLetter() = %v, %v", acked, err)
	}
}

func TestSearchExpireAndTimeout(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	now := 1000.0
	stores.SearchTasks.Clock = func() float64 { return now }
	stores.SearchTasks.Settings.TimeoutSeconds = 5
	envelope := submitTestSearch(t, stores, "alpha", map[string]any{"query": "hello"})
	now = 1010
	expired, err := stores.SearchTasks.Expire(ctx, 10)
	if err != nil || expired != 1 {
		t.Fatalf("Expire() = %d, %v", expired, err)
	}
	if _, err := stores.SearchTasks.Get(ctx, "alpha", envelope.ResultID, 1, 10); err == nil {
		t.Fatal("expired task returned a view")
	} else {
		mustBeError[*SnapshotUnavailable](t, err)
	}
}

func TestSearchDispatchPendingAndAdmission(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	publisher := &fakeSearchPublisher{}
	dispatcher := NewSearchTaskDispatcher(stores, publisher)
	envelope, err := dispatcher.Submit(ctx, "alpha", map[string]any{"query": "hello"})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	messages := publisher.messages()
	if len(messages) != 1 || messages[0].ResultID != envelope.ResultID || messages[0].Schema != SearchEnvelopeSchema {
		t.Fatalf("messages = %+v", messages)
	}
	if remaining, err := dispatcher.RunOnce(ctx); err != nil || remaining != 0 {
		t.Fatalf("RunOnce() = %d, %v", remaining, err)
	}
	// No publisher means admission is refused without creating a task.
	offline := NewSearchTaskDispatcher(stores, nil)
	if _, err := offline.Submit(ctx, "alpha", map[string]any{"query": "offline"}); err == nil {
		t.Fatal("offline Submit() succeeded")
	} else if unavailable := mustBeError[*SearchTaskUnavailable](t, err); unavailable.Message != "Search broker unavailable" {
		t.Fatalf("offline message = %q", unavailable.Message)
	}
	// A broker failure terminates the admitted task as failed.
	failing := &fakeSearchPublisher{err: &PublisherError{message: "broker down"}}
	badDispatcher := NewSearchTaskDispatcher(stores, failing)
	if _, err := badDispatcher.Submit(ctx, "alpha", map[string]any{"query": "failing"}); err == nil {
		t.Fatal("failing Submit() succeeded")
	} else {
		mustBeError[*SearchTaskUnavailable](t, err)
	}
	// Failure keeps the task durable but not queued; the successful admission
	// remains queued until a worker claims it.
	var failedTasks int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM search_tasks WHERE status = ?", SearchTaskFailed).Scan(&failedTasks); err != nil || failedTasks != 1 {
		t.Fatalf("failed tasks = %d, %v", failedTasks, err)
	}
}

func TestValidateSearchEnvelopeAndSettings(t *testing.T) {
	valid := map[string]any{
		"schema": float64(2), "result_id": "d1feec53-1036-4af0-8035-86ef6990fa6a", "namespace": "alpha",
		"arguments": map[string]any{"query": "hello"}, "created_at": 1.0, "expires_at": 100.0, "deadline": 50.0,
	}
	if _, err := ValidateSearchEnvelope(valid); err != nil {
		t.Fatalf("valid envelope error = %v", err)
	}
	for name, mutation := range map[string]func(map[string]any){
		"schema":    func(payload map[string]any) { payload["schema"] = float64(3) },
		"extra key": func(payload map[string]any) { payload["extra"] = true },
		"result":    func(payload map[string]any) { payload["result_id"] = "nope" },
		"namespace": func(payload map[string]any) { payload["namespace"] = "" },
		"arguments": func(payload map[string]any) { payload["arguments"] = "x" },
		"deadline":  func(payload map[string]any) { payload["deadline"] = 200.0 },
		"time":      func(payload map[string]any) { payload["created_at"] = math_NaN() },
	} {
		payload := map[string]any{}
		for key, value := range valid {
			payload[key] = value
		}
		mutation(payload)
		if _, err := ValidateSearchEnvelope(payload); err == nil {
			t.Fatalf("%s: invalid envelope accepted", name)
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

func math_NaN() float64 {
	value := 0.0
	return value / value
}

func TestSearchSubmitDuplicateResultID(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	envelope := submitTestSearch(t, stores, "alpha", map[string]any{"query": "first"})
	duplicate := envelope.ResultID
	if _, err := stores.SearchTasks.Submit(ctx, "alpha", map[string]any{"query": "second"}, &duplicate); err == nil {
		t.Fatal("duplicate result id admitted")
	} else {
		mustBeError[*SearchTaskUnavailable](t, err)
	}
	view, err := stores.SearchTasks.Get(ctx, "alpha", envelope.ResultID, 1, 10)
	if err != nil || view == nil {
		t.Fatalf("original task = %+v, %v", view, err)
	}
}
