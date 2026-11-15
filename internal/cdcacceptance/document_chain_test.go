package cdcacceptance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// TestDocumentChainSucceedsWithoutOutbox covers the happy path of the document
// acceptance row: Submit writes only document_jobs at schedule_version 1, the
// raw Debezium insert event drives the worker to a succeeded terminal state
// (single journal publication), and replays are ACKed without re-execution.
func TestDocumentChainSucceedsWithoutOutbox(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()
	namespace := uniqueNamespace(t, "acc-doc-ok")
	createKnowledgeBase(t, stores, namespace, "acceptance-collection")
	ingester := &journalIngester{stores: stores}
	worker := newDocumentProcessor(t, stores, ingester)

	content := []byte("hello CDC acceptance")
	admission, err := stores.DocumentJobs.Submit(ctx, namespace, "guide.txt", content, nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Submit() error = %v", err)
	}
	job := requireDocumentJob(t, stores, admission.ArtifactID, state.DocumentJobQueued, "queued", 1)
	if len(job.Content) != len(content) {
		t.Fatalf("queued job content = %d bytes, want %d", len(job.Content), len(content))
	}
	// Submit must not write any retired outbox row or declare an outbox table.
	for _, retired := range []string{"document_job_outbox", "search_task_outbox", "cache_outbox", "document_task_outbox"} {
		if schemaDeclaresTable(retired) {
			t.Fatalf("retired outbox table %q is still declared in schemaTables", retired)
		}
	}

	// Debezium delivers the committed INSERT (op=c) as identifier-only JSON.
	if acked := deliver(t, worker.MessageHandler(), "praxis_document_ingest",
		rawDocumentEvent(t, admission.ArtifactID, namespace, 1, "c")); !acked {
		t.Fatal("initial document delivery was not acknowledged")
	}
	job = requireDocumentJob(t, stores, admission.ArtifactID, state.DocumentJobSucceeded, "completed", 1)
	if len(job.Content) != 0 {
		t.Fatalf("succeeded job still stores %d content bytes", len(job.Content))
	}
	if ingester.callCount() != 1 {
		t.Fatalf("ingester calls = %d, want 1", ingester.callCount())
	}
	published, err := stores.Documents.Get(ctx, namespace, "guide.txt")
	if err != nil || published == nil {
		t.Fatalf("Documents.Get() = %+v, %v; want the committed document", published, err)
	}
	if published.SHA256 != job.SHA256 {
		t.Fatalf("document sha256 = %q, want job sha %q", published.SHA256, job.SHA256)
	}
	// The journal publication the ingester committed is what Reconcile adopted.
	if countRows(t, stores,
		"SELECT COUNT(*) FROM upload_operations WHERE namespace = ? AND status = ?",
		namespace, state.UploadCommitted) != 1 {
		t.Fatal("succeeded job is missing its committed journal publication")
	}

	// A duplicate initial-snapshot/insert delivery is ACKed and must not run the
	// ingester again (terminal row, safe skip).
	for attempt := 0; attempt < 2; attempt++ {
		if acked := deliver(t, worker.MessageHandler(), "praxis_document_ingest",
			rawDocumentEvent(t, admission.ArtifactID, namespace, 1, "r")); !acked {
			t.Fatalf("duplicate delivery %d was not acknowledged", attempt)
		}
	}
	if ingester.callCount() != 1 {
		t.Fatalf("duplicate deliveries re-ran ingestion: calls = %d", ingester.callCount())
	}
	status, err := stores.DocumentJobs.Status(ctx, namespace, admission.ArtifactID)
	if err != nil || status.Status != state.DocumentJobSucceeded || status.Stage != "completed" {
		t.Fatalf("Status() = %+v, %v", status, err)
	}
	// The application-native schema-4 envelope (the shape other Go producers
	// publish) is also ACKed directly by Handle for a terminal job.
	direct := state.DocumentNotification{
		Schema: state.DocumentEnvelopeSchema, ArtifactID: admission.ArtifactID,
		Namespace: namespace, ScheduleVersion: 1,
	}
	if direct.Schema != 4 {
		t.Fatalf("DocumentEnvelopeSchema = %d, want 4", direct.Schema)
	}
	if acked := worker.Handle(ctx, direct); !acked {
		t.Fatal("terminal native envelope was not acknowledged")
	}
	if ingester.callCount() != 1 {
		t.Fatalf("direct terminal delivery re-ran ingestion: calls = %d", ingester.callCount())
	}
}

// TestDocumentChainRetryableFailureWaitsForRetryAt covers the retry row: an
// infrastructure failure records the reason and retry_at without bumping the
// generation; a duplicate before the deadline is skipped; the sweep emits the
// replacement generation when the retry is due, and that generation completes.
func TestDocumentChainRetryableFailureWaitsForRetryAt(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()
	namespace := uniqueNamespace(t, "acc-doc-retry")
	createKnowledgeBase(t, stores, namespace, "acceptance-collection")
	ingester := &journalIngester{
		stores:   stores,
		failures: []error{errors.New("qdrant unavailable: connect timeout")},
	}
	worker := newDocumentProcessor(t, stores, ingester)
	clock := float64(time.Now().Unix())
	stores.DocumentJobs.Clock = func() float64 { return clock }
	admission, err := stores.DocumentJobs.Submit(ctx, namespace, "retry.txt", []byte("retry me"), nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Submit() error = %v", err)
	}

	// The first delivery fails retryably: the failure is durable before ACK.
	if acked := deliver(t, worker.MessageHandler(), "praxis_document_ingest",
		rawDocumentEvent(t, admission.ArtifactID, namespace, 1, "c")); !acked {
		t.Fatal("retryable failure was not acknowledged")
	}
	job := requireDocumentJob(t, stores, admission.ArtifactID, state.DocumentJobQueued, "retry_wait", 1)
	if job.Error == nil || !strings.Contains(*job.Error, "Document processing failed") {
		t.Fatalf("job error = %v, want a recorded failure message", job.Error)
	}
	if job.Owner != nil || job.ExecutionID != nil || job.LeaseUntil != 0 {
		t.Fatalf("retry_wait job still holds execution state: %+v", job)
	}
	if job.RetryAt <= clock {
		t.Fatalf("retry_at = %v, want a future deadline", job.RetryAt)
	}
	if ingester.callCount() != 1 {
		t.Fatalf("ingester calls = %d, want 1", ingester.callCount())
	}

	// A duplicate before retry_at is skipped, not executed.
	if acked := deliver(t, worker.MessageHandler(), "praxis_document_ingest",
		rawDocumentEvent(t, admission.ArtifactID, namespace, 1, "u")); !acked {
		t.Fatal("early retry delivery was not acknowledged")
	}
	if ingester.callCount() != 1 {
		t.Fatalf("early retry delivery re-ran ingestion: calls = %d", ingester.callCount())
	}

	// The sweep emits the replacement generation only when retry_at is due.
	clock = job.RetryAt + 1
	if swept, err := stores.DocumentJobs.Sweep(ctx, 10); err != nil || swept != 1 {
		t.Fatalf("Sweep(due) = %d, %v; want 1", swept, err)
	}
	requireDocumentJob(t, stores, admission.ArtifactID, state.DocumentJobQueued, "retry_wait", 2)
	// The old generation is now stale.
	if acked := deliver(t, worker.MessageHandler(), "praxis_document_ingest",
		rawDocumentEvent(t, admission.ArtifactID, namespace, 1, "u")); !acked {
		t.Fatal("stale generation was not acknowledged")
	}
	if ingester.callCount() != 1 {
		t.Fatalf("stale generation re-ran ingestion: calls = %d", ingester.callCount())
	}
	// The replacement generation completes.
	if acked := deliver(t, worker.MessageHandler(), "praxis_document_ingest",
		rawDocumentEvent(t, admission.ArtifactID, namespace, 2, "u")); !acked {
		t.Fatal("retry generation was not acknowledged")
	}
	requireDocumentJob(t, stores, admission.ArtifactID, state.DocumentJobSucceeded, "completed", 2)
	if ingester.callCount() != 2 {
		t.Fatalf("ingester calls = %d, want 2", ingester.callCount())
	}
}

// TestDocumentChainPausedDebeziumKeepsQueuedGeneration covers the broker-outage
// row: the notification is not delivered while the connector is paused, and a
// merely queued job is deliberately not re-emitted by Sweep. The original
// generation is preserved and completes after resume.
func TestDocumentChainPausedDebeziumKeepsQueuedGeneration(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()
	namespace := uniqueNamespace(t, "acc-doc-sweep")
	createKnowledgeBase(t, stores, namespace, "acceptance-collection")
	ingester := &journalIngester{stores: stores}
	worker := newDocumentProcessor(t, stores, ingester)
	admission, err := stores.DocumentJobs.Submit(ctx, namespace, "paused.txt", []byte("paused"), nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Submit() error = %v", err)
	}
	// No delivery happens: Debezium is paused. A merely queued job must keep its
	// generation; its Kafka notification is retained and delivered on resume.
	stores.DocumentJobs.Settings.SweepAfterSeconds = -1
	if swept, err := stores.DocumentJobs.Sweep(ctx, 10); err != nil || swept != 0 {
		t.Fatalf("Sweep(paused) = %d, %v; want 0", swept, err)
	}
	requireDocumentJob(t, stores, admission.ArtifactID, state.DocumentJobQueued, "queued", 1)

	// After Debezium resumes, the original notification completes the job.
	if acked := deliver(t, worker.MessageHandler(), "praxis_document_ingest",
		rawDocumentEvent(t, admission.ArtifactID, namespace, 1, "c")); !acked {
		t.Fatal("post-resume delivery was not acknowledged")
	}
	requireDocumentJob(t, stores, admission.ArtifactID, state.DocumentJobSucceeded, "completed", 1)
	if ingester.callCount() != 1 {
		t.Fatalf("ingester calls = %d, want 1", ingester.callCount())
	}
}

// TestDocumentChainCrashRecoveryViaLeaseExpiry covers the worker-crash row: the
// lease expires, Sweep requeues with a new generation, and the fresh delivery
// completes while the crashed generation is ignored.
func TestDocumentChainCrashRecoveryViaLeaseExpiry(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()
	namespace := uniqueNamespace(t, "acc-doc-crash")
	createKnowledgeBase(t, stores, namespace, "acceptance-collection")
	ingester := &journalIngester{stores: stores}
	worker := newDocumentProcessor(t, stores, ingester)

	base := time.Now().Unix()
	offset := 0.0
	stores.DocumentJobs.Clock = func() float64 { return float64(base) + offset }
	admission, err := stores.DocumentJobs.Submit(ctx, namespace, "crash.txt", []byte("crash"), nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Submit() error = %v", err)
	}
	notification := state.DocumentNotification{
		Schema: state.DocumentEnvelopeSchema, ArtifactID: admission.ArtifactID,
		Namespace: namespace, ScheduleVersion: 1,
	}
	task, err := stores.DocumentJobs.Claim(ctx, notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	// The worker dies after claiming: no heartbeat, so the lease runs out.
	offset = task.LeaseUntil - float64(base) + 1
	swept, err := stores.DocumentJobs.Sweep(ctx, 10)
	if err != nil || swept != 1 {
		t.Fatalf("Sweep() after crash = %d, %v; want 1", swept, err)
	}
	requeued := requireDocumentJob(t, stores, admission.ArtifactID, state.DocumentJobQueued, "retry_wait", 2)
	if requeued.Owner != nil || requeued.ExecutionID != nil || requeued.LeaseUntil != 0 {
		t.Fatalf("crashed execution state was not cleared: %+v", requeued)
	}
	// The crashed generation cannot complete; the requeued generation can.
	if acked := deliver(t, worker.MessageHandler(), "praxis_document_ingest",
		rawDocumentEvent(t, admission.ArtifactID, namespace, 1, "c")); !acked {
		t.Fatal("crashed generation was not acknowledged")
	}
	if ingester.callCount() != 0 {
		t.Fatalf("crashed generation executed the job: calls = %d", ingester.callCount())
	}
	if acked := deliver(t, worker.MessageHandler(), "praxis_document_ingest",
		rawDocumentEvent(t, admission.ArtifactID, namespace, 2, "u")); !acked {
		t.Fatal("recovered generation was not acknowledged")
	}
	requireDocumentJob(t, stores, admission.ArtifactID, state.DocumentJobSucceeded, "completed", 2)
	if ingester.callCount() != 1 {
		t.Fatalf("ingester calls = %d, want 1", ingester.callCount())
	}
}
