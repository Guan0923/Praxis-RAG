package documentworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/statetest"
)

const testNamespace = "manuals"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestStores(t *testing.T) *state.Stores {
	t.Helper()
	url, key := statetest.Connection(t)
	stores, err := state.New(state.StateSettings{
		URL:           url,
		EncryptionKey: key,
		AutoSchema:    true,
		PoolSize:      10,
		MaxOverflow:   20,
		PoolRecycle:   1800,
		PoolTimeout:   30,
	}, true)
	if err != nil {
		t.Fatalf("state.New(MySQL) error = %v", err)
	}
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	return stores
}

func configureNamespace(t *testing.T, stores *state.Stores) {
	t.Helper()
	if err := stores.KnowledgeBases.Create(context.Background(), map[string]any{
		"namespace": testNamespace, "collection": "test-collection",
	}); err != nil {
		t.Fatalf("KnowledgeBases.Create: %v", err)
	}
}

// submitJob admits one document job and returns the schema-4 notification
// Debezium would publish for the committed row (schedule_version = 1).
func submitJob(t *testing.T, stores *state.Stores, name string, content []byte) state.DocumentNotification {
	t.Helper()
	admission, err := stores.DocumentJobs.Submit(context.Background(), testNamespace, name, content, nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Submit: %v", err)
	}
	job, err := stores.DocumentJobs.Get(context.Background(), admission.ArtifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get: %v %v", job, err)
	}
	return notificationFor(t, job)
}

// notificationFor rebuilds the identifier-only schema-4 notification of a job,
// optionally with a different scheduling generation (stale/new deliveries).
func notificationFor(t *testing.T, job *state.DocumentJob) state.DocumentNotification {
	t.Helper()
	notification, err := state.DocumentJobEnvelope(job)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope: %v", err)
	}
	return notification
}

// notificationAtVersion rewrites the scheduling generation of a notification to
// model a stale or replacement delivery.
func notificationAtVersion(notification state.DocumentNotification, version int64) state.DocumentNotification {
	notification.ScheduleVersion = version
	return notification
}

// nativeBody encodes the application-native schema-4 envelope.
func nativeBody(t *testing.T, notification state.DocumentNotification) []byte {
	t.Helper()
	body, err := json.Marshal(notification)
	if err != nil {
		t.Fatalf("json.Marshal(DocumentNotification): %v", err)
	}
	return body
}

func sha256Hex(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

// commitPublication simulates the runtime's successful publish: the journal
// operation is committed with the task-identifying result fields, which is what
// Reconcile adopts to make the job terminal.
func commitPublication(t *testing.T, stores *state.Stores, payload Payload) {
	t.Helper()
	ctx := context.Background()
	idempotencyKey := "document-task:" + payload.ArtifactID + ":run"
	operation, err := stores.Uploads.Begin(ctx, payload.Namespace, payload.Name, "doc-1", "test-collection",
		state.BeginOptions{IdempotencyKey: &idempotencyKey})
	if err != nil {
		t.Fatalf("Uploads.Begin: %v", err)
	}
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := stores.Documents.UpsertTx(ctx, transaction, payload.Namespace, state.DocumentRecord{
		Name: payload.Name, DocumentID: "doc-1", Collection: "test-collection",
	}); err != nil {
		t.Fatalf("Documents.UpsertTx: %v", err)
	}
	if _, err := stores.Uploads.Commit(ctx, operation, map[string]any{
		"sha256":                     sha256Hex(payload.Content),
		"size":                       float64(payload.Size),
		"task_configuration_version": float64(payload.ConfigurationVersion),
	}, transaction); err != nil {
		t.Fatalf("Uploads.Commit: %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}
}

type fakeIngester struct {
	mutex    sync.Mutex
	calls    int
	payloads []Payload
	run      func(ctx context.Context, payload Payload) error
}

func (f *fakeIngester) ProcessDocumentJob(ctx context.Context, payload Payload) (map[string]any, error) {
	f.mutex.Lock()
	f.calls++
	f.payloads = append(f.payloads, payload)
	run := f.run
	f.mutex.Unlock()
	if run != nil {
		if err := run(ctx, payload); err != nil {
			return nil, err
		}
	}
	return map[string]any{"document": map[string]any{"name": payload.Name}}, nil
}

func (f *fakeIngester) setRun(run func(ctx context.Context, payload Payload) error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.run = run
}

func (f *fakeIngester) callCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.calls
}

// successIngester publishes the job it is given, so the processor can reconcile
// it into the succeeded terminal state.
func successIngester(t *testing.T, stores *state.Stores) *fakeIngester {
	t.Helper()
	ingester := &fakeIngester{}
	ingester.setRun(func(_ context.Context, payload Payload) error {
		commitPublication(t, stores, payload)
		return nil
	})
	return ingester
}

// fakeJournalRecovery records the optional upload-journal recovery pass.
type fakeJournalRecovery struct {
	mutex sync.Mutex
	calls int
	err   error
}

func (f *fakeJournalRecovery) RecoverDocumentUploads(context.Context) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls++
	return f.err
}

func (f *fakeJournalRecovery) callCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.calls
}

func newProcessor(t *testing.T, stores *state.Stores, ingester Ingester, options Options) *Processor {
	t.Helper()
	options.Jobs = stores.DocumentJobs
	options.Ingester = ingester
	if options.Logger == nil {
		options.Logger = testLogger()
	}
	processor, err := New(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return processor
}

func jobStatus(t *testing.T, stores *state.Stores, artifactID string) *state.DocumentJob {
	t.Helper()
	job, err := stores.DocumentJobs.Get(context.Background(), artifactID, nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Get: %v", err)
	}
	return job
}

func TestHandleSuccess(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	ingester := successIngester(t, stores)
	processor := newProcessor(t, stores, ingester, Options{})

	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("Handle() must acknowledge a committed publication")
	}
	if ingester.callCount() != 1 {
		t.Fatalf("ingester calls = %d", ingester.callCount())
	}
	payload := ingester.payloads[0]
	if payload.ArtifactID != notification.ArtifactID || payload.Namespace != testNamespace ||
		payload.Name != "manual.txt" || string(payload.Content) != "original" {
		t.Fatalf("ingester payload = %+v", payload)
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	if job.Status != state.DocumentJobSucceeded || job.Stage != "completed" || job.OperationID == nil {
		t.Fatalf("terminal job = %+v", job)
	}
	// Redelivery of a terminal job is acknowledged without re-ingesting.
	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("redelivery must be acknowledged")
	}
	if ingester.callCount() != 1 {
		t.Fatalf("redelivery re-ran ingestion")
	}
}

// TestHandleRetryableFailureRequeuesWithNewScheduleVersion covers the retry
// branch of docs/cdc-migration.md §4.1: Fail(retry=true) increments the
// schedule_version in the same UPDATE, so exactly one replacement notification
// exists and the current delivery can be acknowledged.
func TestHandleRetryableFailureRequeuesWithNewScheduleVersion(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	ingester := &fakeIngester{}
	ingester.setRun(func(context.Context, Payload) error {
		return errors.New("pdf process crashed")
	})
	processor := newProcessor(t, stores, ingester, Options{})

	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("a recorded retry must be acknowledged (the sweep re-emits the new version)")
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	if job.Status != state.DocumentJobQueued || job.Stage != "retry_wait" || job.Attempts != 1 {
		t.Fatalf("retried job = %+v", job)
	}
	if job.ScheduleVersion != 2 {
		t.Fatalf("schedule_version = %d, want 2 (Fail(retry=true) increments)", job.ScheduleVersion)
	}
	if job.Error == nil || !strings.Contains(*job.Error, "Document processing failed") {
		t.Fatalf("retry did not persist the safe failure message: %+v", job.Error)
	}

	// The replacement generation notification is claimable and runs the job.
	ingester.setRun(func(_ context.Context, payload Payload) error {
		commitPublication(t, stores, payload)
		return nil
	})
	retryNotification := notificationAtVersion(notification, job.ScheduleVersion)
	if acked := processor.Handle(context.Background(), retryNotification); !acked {
		t.Fatalf("retry delivery must be acknowledged after success")
	}
	if job := jobStatus(t, stores, notification.ArtifactID); job.Status != state.DocumentJobSucceeded || job.Attempts != 2 {
		t.Fatalf("job = %+v", job)
	}
	if ingester.callCount() != 2 {
		t.Fatalf("ingester calls = %d, want 2", ingester.callCount())
	}
}

func TestHandleValidationFailureIsTerminal(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	ingester := &fakeIngester{}
	ingester.setRun(func(context.Context, Payload) error {
		return &apierrors.KnowledgeValidationError{Message: "Invalid document"}
	})
	processor := newProcessor(t, stores, ingester, Options{})

	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("validation failure must be acknowledged as terminal")
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	// The persisted terminal text keeps the documented prefix and appends the
	// structured failure summary (category/stage/status/detail).
	const terminalPrefix = "Invalid document or changed knowledge-base configuration"
	if job.Status != state.DocumentJobFailed || job.Error == nil || !strings.HasPrefix(*job.Error, terminalPrefix) {
		t.Fatalf("failed job = %+v", job)
	}
	if len(*job.Error) <= len(terminalPrefix) {
		t.Fatalf("failed job error lost the structured summary: %q", *job.Error)
	}
	if job.ScheduleVersion != 1 {
		t.Fatalf("terminal failure must not increment schedule_version: %d", job.ScheduleVersion)
	}
	if terminal, err := stores.DocumentJobs.Terminal(context.Background(), notification.ArtifactID); err != nil || !terminal {
		t.Fatalf("Terminal() = %v, %v", terminal, err)
	}
	// A terminal failed job is acknowledged without re-running ingestion.
	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("delivery of a failed job must be acknowledged")
	}
	if ingester.callCount() != 1 {
		t.Fatalf("terminal redelivery re-ran ingestion: %d calls", ingester.callCount())
	}
}

// TestHandleSkipsLiveLeaseAndReclaimsAfterExpiry covers the "another owner holds
// a live lease" case: the notification is safely skipped and ACKed, and once the
// lease expires the same generation is claimable by a new execution.
func TestHandleSkipsLiveLeaseAndReclaimsAfterExpiry(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	clock := float64(time.Now().Unix())
	stores.DocumentJobs.Clock = func() float64 { return clock }
	notification := submitJob(t, stores, "manual.txt", []byte("original"))

	// Another live worker holds the lease.
	task, err := stores.DocumentJobs.Claim(context.Background(), notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	ingester := successIngester(t, stores)
	processor := newProcessor(t, stores, ingester, Options{})
	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("delivery under a live lease must be safely acknowledged")
	}
	if ingester.callCount() != 0 {
		t.Fatalf("ingester ran under a live lease")
	}

	// After the lease expires another worker takes over (fencing the old one).
	clock += stores.DocumentJobs.Settings.LeaseSeconds + 10
	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("expired lease must be reclaimable")
	}
	if ingester.callCount() != 1 {
		t.Fatalf("ingester calls = %d", ingester.callCount())
	}
	if job := jobStatus(t, stores, notification.ArtifactID); job.Status != state.DocumentJobSucceeded || job.Attempts != 2 {
		t.Fatalf("job = %+v", job)
	}
	var conflict *state.UploadConflict
	if err := stores.DocumentJobs.Progress(context.Background(), task, "stale-stage"); !errors.As(err, &conflict) {
		t.Fatalf("stale execution Progress() error = %v, want UploadConflict", err)
	}
}

// TestStaleScheduleVersionIsAcknowledged covers the crash-after-claim recovery
// path: the sweep bumps schedule_version, the old delivery is skipped and ACKed,
// and the replacement generation runs. No transactional outbox row is involved.
func TestStaleScheduleVersionIsAcknowledged(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	clock := float64(time.Now().Unix())
	stores.DocumentJobs.Clock = func() float64 { return clock }
	notification := submitJob(t, stores, "manual.txt", []byte("original"))

	// The worker claimed the job and crashed before finishing it.
	if task, err := stores.DocumentJobs.Claim(context.Background(), notification); err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	clock += stores.DocumentJobs.Settings.LeaseSeconds + 10
	ingester := successIngester(t, stores)
	processor := newProcessor(t, stores, ingester, Options{})
	if err := processor.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	if job.ScheduleVersion != 2 || job.Status != state.DocumentJobQueued || job.Stage != "retry_wait" {
		t.Fatalf("swept job = %+v", job)
	}

	// The old generation's delivery is stale: ACK without running.
	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("stale schedule_version delivery must be acknowledged")
	}
	if ingester.callCount() != 0 {
		t.Fatalf("stale delivery ran ingestion")
	}

	// The replacement generation is claimable.
	replacement := notificationAtVersion(notification, job.ScheduleVersion)
	if acked := processor.Handle(context.Background(), replacement); !acked {
		t.Fatalf("current schedule_version delivery must be acknowledged")
	}
	if ingester.callCount() != 1 {
		t.Fatalf("ingester calls = %d, want 1", ingester.callCount())
	}
	if job := jobStatus(t, stores, notification.ArtifactID); job.Status != state.DocumentJobSucceeded {
		t.Fatalf("job = %+v", job)
	}
}

// TestMessageHandlerDecodesNativeAndDebeziumEnvelopes proves both accepted wire
// shapes execute the committed job: the application-native schema-4 envelope and
// the raw Debezium change event (schemas.enable=false, op c/r/u).
func TestMessageHandlerDecodesNativeAndDebeziumEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		body func(t *testing.T, notification state.DocumentNotification) []byte
	}{
		{name: "native schema-4", body: nativeBody},
		{name: "debezium create", body: func(t *testing.T, notification state.DocumentNotification) []byte {
			return documentCDCEvent(t, nil, documentCDCRow(notification), "c")
		}},
		{name: "debezium snapshot read", body: func(t *testing.T, notification state.DocumentNotification) []byte {
			return documentCDCEvent(t, nil, documentCDCRow(notification), "r")
		}},
		{name: "debezium schedule bump", body: func(t *testing.T, notification state.DocumentNotification) []byte {
			return documentCDCEvent(t, documentCDCRow(notification), documentCDCRow(notification), "u")
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stores := newTestStores(t)
			configureNamespace(t, stores)
			notification := submitJob(t, stores, "manual.txt", []byte("original"))
			ingester := successIngester(t, stores)
			processor := newProcessor(t, stores, ingester, Options{})
			if acked := processor.MessageHandler()(context.Background(), mq.ReceivedMessage{
				Topic: "praxis_document_ingest", Body: testCase.body(t, notification),
			}); !acked {
				t.Fatalf("valid notification must be acknowledged")
			}
			if ingester.callCount() != 1 {
				t.Fatalf("ingester calls = %d, want 1", ingester.callCount())
			}
			if job := jobStatus(t, stores, notification.ArtifactID); job.Status != state.DocumentJobSucceeded {
				t.Fatalf("job = %+v, want succeeded", job)
			}
		})
	}
}

func TestMessageHandlerRejectsMalformedEnvelopes(t *testing.T) {
	stores := newTestStores(t)
	processor := newProcessor(t, stores, &fakeIngester{}, Options{})
	handler := processor.MessageHandler()
	for _, body := range [][]byte{
		[]byte("{not json"),
		// Document schema 4 requires schedule_version; 3 is the retired document
		// schema and 1 is the cache schema.
		[]byte(`{"schema":3,"artifact_id":"3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a","namespace":"manuals"}`),
		[]byte(`{"schema":1}`),
		[]byte(`{"schema":4,"artifact_id":"not-a-uuid","namespace":"manuals","schedule_version":1}`),
		[]byte(`{"schema":4,"artifact_id":"3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a","namespace":"manuals"}`),
		[]byte(`{"schema":4,"artifact_id":"3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a","namespace":"manuals","schedule_version":0}`),
		[]byte(`{"schema":4,"artifact_id":"3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a","namespace":"manuals","schedule_version":1} trailing`),
	} {
		if acked := handler(context.Background(), mq.ReceivedMessage{Body: body}); acked {
			t.Fatalf("poison payload %q must be nacked for broker retry/DLQ", body)
		}
	}
}

func TestHandleKeepsLeaseAliveDuringSlowIngestion(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	started := make(chan struct{})
	release := make(chan struct{})
	ingester := &fakeIngester{}
	committed := make(chan struct{}, 1)
	ingester.setRun(func(_ context.Context, payload Payload) error {
		close(started)
		<-release
		commitPublication(t, stores, payload)
		committed <- struct{}{}
		return nil
	})
	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuffer, nil))
	processor := newProcessor(t, stores, ingester, Options{HeartbeatInterval: 10 * time.Millisecond, Logger: logger})

	done := make(chan bool, 1)
	go func() { done <- processor.Handle(context.Background(), notification) }()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatalf("ingester did not start")
	}
	before := jobStatus(t, stores, notification.ArtifactID).LeaseUntil
	time.Sleep(80 * time.Millisecond)
	after := jobStatus(t, stores, notification.ArtifactID).LeaseUntil
	if after <= before {
		t.Fatalf("heartbeat did not renew the lease: %v -> %v", before, after)
	}
	close(release)
	select {
	case <-committed:
	case <-time.After(30 * time.Second):
		t.Fatalf("ingester did not commit")
	}
	select {
	case acked := <-done:
		if !acked {
			t.Fatalf("slow ingestion must be acknowledged after success: job=%+v logs=%s",
				jobStatus(t, stores, notification.ArtifactID), logBuffer.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("Handle did not return")
	}
	if job := jobStatus(t, stores, notification.ArtifactID); job.Status != state.DocumentJobSucceeded {
		t.Fatalf("job = %+v", job)
	}
}

func TestHandleReconcilesCommittedPublicationAfterCrash(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("crashed"))
	task, err := stores.DocumentJobs.Claim(context.Background(), notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	// The runtime had committed the publication, but the worker crashed before
	// acknowledging and recording the terminal state.
	commitPublication(t, stores, PayloadFromTask(task))

	ingester := &fakeIngester{}
	processor := newProcessor(t, stores, ingester, Options{})
	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("a committed publication must be reconciled and acknowledged")
	}
	if ingester.callCount() != 0 {
		t.Fatalf("reconciled delivery must not re-ingest")
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	if job.Status != state.DocumentJobSucceeded || job.Stage != "completed" {
		t.Fatalf("job = %+v", job)
	}
}

func TestDeadLetterAndExhaustion(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	ingester := &fakeIngester{}
	ingester.setRun(func(context.Context, Payload) error {
		return errors.New("permanent broker failure")
	})
	processor := newProcessor(t, stores, ingester, Options{})
	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("parser failure must be recorded and acknowledged for redelivery")
	}

	complete, err := processor.DeadLetter(context.Background(), notification)
	if err != nil || !complete {
		t.Fatalf("DeadLetter() = %v, %v", complete, err)
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	// DeadLetter keeps the documented prefix and, when the retry branch recorded
	// a failure, appends that last error so the operator sees the real cause.
	if job.Status != state.DocumentJobFailed || job.Error == nil ||
		!strings.HasPrefix(*job.Error, "Message retry limit reached") {
		t.Fatalf("dead-lettered job = %+v", job)
	}
	if !strings.Contains(*job.Error, "last error:") ||
		!strings.Contains(*job.Error, "Document processing failed") {
		t.Fatalf("dead-lettered job lost the last recorded failure: %q", *job.Error)
	}

	body := nativeBody(t, notification)
	if acked := processor.DeadLetterHandler()(context.Background(), mq.ReceivedMessage{Body: body}); !acked {
		t.Fatalf("dead-letter handler must acknowledge a terminal job")
	}
	if complete, err := processor.DeadLetterMessage(context.Background(), mq.ReceivedMessage{Body: []byte("bad")}); err != nil || complete {
		t.Fatalf("poison dead-letter message = %v, %v", complete, err)
	}
	if err := processor.ExhaustedHandler()(context.Background(), mq.ReceivedMessage{Body: body}); err != nil {
		t.Fatalf("ExhaustedHandler: %v", err)
	}
}

func TestDeadLetterWhileLiveLeaseIsNotAcknowledged(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	if task, err := stores.DocumentJobs.Claim(context.Background(), notification); err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	processor := newProcessor(t, stores, &fakeIngester{}, Options{})
	complete, err := processor.DeadLetter(context.Background(), notification)
	if err != nil || complete {
		t.Fatalf("DeadLetter() under a live lease = %v, %v; want (false, nil)", complete, err)
	}
}

// TestRecoverSweepsGarbageCollectsAndRecoversJournal covers the maintenance pass:
// Recover only requeues stranded/expired jobs (bumping schedule_version), GCs old
// terminal rows and runs the optional upload-journal recovery. No outbox table is
// read or written.
func TestRecoverSweepsGarbageCollectsAndRecoversJournal(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	clock := float64(time.Now().Unix())
	stores.DocumentJobs.Clock = func() float64 { return clock }
	recovery := &fakeJournalRecovery{}
	ingester := successIngester(t, stores)
	processor := newProcessor(t, stores, ingester, Options{JournalRecovery: recovery})

	// A terminal job older than the retention window is garbage collected.
	terminal := submitJob(t, stores, "terminal.txt", []byte("old"))
	if acked := processor.Handle(context.Background(), terminal); !acked {
		t.Fatalf("terminal job must be processed")
	}
	// A queued job whose notification was lost is re-emitted by the sweep.
	stranded := submitJob(t, stores, "stranded.txt", []byte("lost"))
	// A processing job whose worker crashed is requeued when its lease expires.
	crashed := submitJob(t, stores, "crashed.txt", []byte("lease"))
	if task, err := stores.DocumentJobs.Claim(context.Background(), crashed); err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}

	clock += stores.DocumentJobs.Settings.LeaseSeconds +
		stores.DocumentJobs.Settings.SweepAfterSeconds +
		stores.DocumentJobs.Settings.TerminalRetentionSeconds + 10
	if err := processor.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if recovery.callCount() != 1 {
		t.Fatalf("journal recovery calls = %d, want 1", recovery.callCount())
	}
	if job, err := stores.DocumentJobs.Get(context.Background(), terminal.ArtifactID, nil); err != nil || job != nil {
		t.Fatalf("terminal job after Recover = %+v, %v; want GC'd", job, err)
	}
	for _, notification := range []state.DocumentNotification{stranded, crashed} {
		job := jobStatus(t, stores, notification.ArtifactID)
		if job.Status != state.DocumentJobQueued || job.Stage != "retry_wait" || job.ScheduleVersion != 2 {
			t.Fatalf("recovered job = %+v", job)
		}
	}
	// The replacement generation is consumable.
	replacement := notificationAtVersion(stranded, 2)
	if acked := processor.Handle(context.Background(), replacement); !acked {
		t.Fatalf("swept notification must be acknowledged")
	}
	if job := jobStatus(t, stores, stranded.ArtifactID); job.Status != state.DocumentJobSucceeded {
		t.Fatalf("stranded job = %+v", job)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	stores := newTestStores(t)
	if _, err := New(Options{}); err == nil {
		t.Fatalf("missing Jobs must fail")
	}
	// A dead-letter-only worker needs no ingester; Handle stays retryable.
	deadLetterProcessor, err := New(Options{Jobs: stores.DocumentJobs, Logger: testLogger()})
	if err != nil {
		t.Fatalf("dead-letter-only New: %v", err)
	}
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	if acked := deadLetterProcessor.Handle(context.Background(), notification); acked {
		t.Fatalf("dead-letter-only Handle must be retryable")
	}
	if complete, err := deadLetterProcessor.DeadLetter(context.Background(), notification); err != nil || !complete {
		t.Fatalf("dead-letter-only DeadLetter() = %v, %v", complete, err)
	}
}

func TestUnknownJobIsAcknowledged(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	processor := newProcessor(t, stores, &fakeIngester{}, Options{})
	notification := state.DocumentNotification{
		Schema:          state.DocumentEnvelopeSchema,
		ArtifactID:      "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a",
		Namespace:       testNamespace,
		ScheduleVersion: 1,
	}
	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("unknown job delivery must be acknowledged")
	}
}

// fakeConsumer is a minimal mq.Consumer that feeds one channel through the
// configured handler, covering the production RunConsumer lifecycle.
type fakeConsumer struct {
	handler  mq.Handler
	messages chan mq.ReceivedMessage

	mutex   sync.Mutex
	started bool
	stopped bool
}

func (c *fakeConsumer) Start(ctx context.Context) error {
	c.mutex.Lock()
	c.started = true
	c.mutex.Unlock()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-c.messages:
				c.handler(ctx, message)
			}
		}
	}()
	return nil
}

func (c *fakeConsumer) Stop() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.stopped = true
	return nil
}

func (c *fakeConsumer) state() (bool, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.started, c.stopped
}

func TestRunConsumerLifecycle(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	body := nativeBody(t, notification)
	ingester := successIngester(t, stores)
	processor := newProcessor(t, stores, ingester, Options{})
	consumer := &fakeConsumer{handler: processor.MessageHandler(), messages: make(chan mq.ReceivedMessage, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- processor.RunConsumer(ctx, consumer) }()

	consumer.messages <- mq.ReceivedMessage{Body: body}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if job := jobStatus(t, stores, notification.ArtifactID); job.Status == state.DocumentJobSucceeded {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if job := jobStatus(t, stores, notification.ArtifactID); job.Status != state.DocumentJobSucceeded {
		t.Fatalf("consumer handler did not finish the job: %+v", job)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunConsumer() = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("RunConsumer did not stop")
	}
	started, stopped := consumer.state()
	if !started || !stopped {
		t.Fatalf("consumer lifecycle started=%v stopped=%v", started, stopped)
	}
	if err := processor.RunConsumer(context.Background(), nil); err == nil {
		t.Fatalf("RunConsumer(nil) must fail")
	}
}
