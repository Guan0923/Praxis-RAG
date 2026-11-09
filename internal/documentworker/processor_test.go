package documentworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

const testNamespace = "manuals"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestStores(t *testing.T) *state.Stores {
	t.Helper()
	settings := state.StateSettings{
		SQLitePath: filepath.Join(t.TempDir(), "state.sqlite3"),
		AutoSchema: true,
	}
	stores, err := state.New(settings, true)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
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
	notification, err := state.DocumentJobEnvelope(job)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope: %v", err)
	}
	return notification
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
	ingester := &fakeIngester{}
	ingester.setRun(func(_ context.Context, payload Payload) error {
		commitPublication(t, stores, payload)
		return nil
	})
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

func TestHandleRetriesParserFailures(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	ingester := &fakeIngester{}
	ingester.setRun(func(context.Context, Payload) error {
		return errors.New("docling process crashed")
	})
	processor := newProcessor(t, stores, ingester, Options{})

	if acked := processor.Handle(context.Background(), notification); acked {
		t.Fatalf("parser failure must not be acknowledged")
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	if job.Status != state.DocumentJobQueued || job.Stage != "retry_wait" || job.Attempts != 1 {
		t.Fatalf("retried job = %+v", job)
	}

	ingester.setRun(func(_ context.Context, payload Payload) error {
		commitPublication(t, stores, payload)
		return nil
	})
	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("retry delivery must be acknowledged after success")
	}
	if job := jobStatus(t, stores, notification.ArtifactID); job.Status != state.DocumentJobSucceeded || job.Attempts != 2 {
		t.Fatalf("job = %+v", job)
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
	if job.Status != state.DocumentJobFailed || job.Error == nil ||
		*job.Error != "Invalid document or changed knowledge-base configuration" {
		t.Fatalf("failed job = %+v", job)
	}
	if terminal, err := stores.DocumentJobs.Terminal(context.Background(), notification.ArtifactID); err != nil || !terminal {
		t.Fatalf("Terminal() = %v, %v", terminal, err)
	}
}

func TestHandleLeaseExpiryAndFencing(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	clock := float64(time.Now().Unix())
	stores.DocumentJobs.Clock = func() float64 { return clock }
	notification := submitJob(t, stores, "manual.txt", []byte("original"))

	// Another live worker holds the lease: this delivery must stay retryable.
	task, err := stores.DocumentJobs.Claim(context.Background(), notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	ingester := &fakeIngester{}
	ingester.setRun(func(_ context.Context, payload Payload) error {
		commitPublication(t, stores, payload)
		return nil
	})
	processor := newProcessor(t, stores, ingester, Options{})
	if acked := processor.Handle(context.Background(), notification); acked {
		t.Fatalf("delivery under a live lease must not be acknowledged")
	}
	if ingester.callCount() != 0 {
		t.Fatalf("ingester ran under a live lease")
	}

	// After the lease expires another worker takes over (fencing the old one).
	clock += 400
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
	case <-time.After(2 * time.Second):
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
	case <-time.After(2 * time.Second):
		t.Fatalf("ingester did not commit")
	}
	select {
	case acked := <-done:
		if !acked {
			t.Fatalf("slow ingestion must be acknowledged after success: job=%+v logs=%s", jobStatus(t, stores, notification.ArtifactID), logBuffer.String())
		}
	case <-time.After(3 * time.Second):
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
	if acked := processor.Handle(context.Background(), notification); acked {
		t.Fatalf("parser failure must stay retryable")
	}

	complete, err := processor.DeadLetter(context.Background(), notification)
	if err != nil || !complete {
		t.Fatalf("DeadLetter() = %v, %v", complete, err)
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	if job.Status != state.DocumentJobFailed || job.Error == nil || *job.Error != "Message retry limit reached" {
		t.Fatalf("dead-lettered job = %+v", job)
	}

	body, _, err := mq.EncodeDocumentNotification(notification)
	if err != nil {
		t.Fatalf("EncodeDocumentNotification: %v", err)
	}
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

func TestMessageHandlerDecodesNotification(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	ingester := &fakeIngester{}
	ingester.setRun(func(_ context.Context, payload Payload) error {
		commitPublication(t, stores, payload)
		return nil
	})
	processor := newProcessor(t, stores, ingester, Options{})
	body, _, err := mq.EncodeDocumentNotification(notification)
	if err != nil {
		t.Fatalf("EncodeDocumentNotification: %v", err)
	}
	handler := processor.MessageHandler()
	if acked := handler(context.Background(), mq.ReceivedMessage{Body: body}); !acked {
		t.Fatalf("valid notification must be acknowledged")
	}
	if acked := handler(context.Background(), mq.ReceivedMessage{Body: []byte("{not json")}); acked {
		t.Fatalf("poison payload must be nacked for broker retry/DLQ")
	}
	if acked := handler(context.Background(), mq.ReceivedMessage{Body: []byte(`{"schema":3}`)}); acked {
		t.Fatalf("invalid schema-3 payload must be nacked")
	}
}

func TestRecoverSweepsStrandedAndExpiredJobs(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	clock := float64(time.Now().Unix())
	stores.DocumentJobs.Clock = func() float64 { return clock }
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	processor := newProcessor(t, stores, &fakeIngester{}, Options{})

	// A queued job whose notification was lost is re-emitted after the sweep
	// window.
	clock += stores.DocumentJobs.Settings.SweepAfterSeconds + 10
	if err := processor.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	events, err := stores.DocumentJobs.PendingOutbox(context.Background(), 10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("sweep did not re-emit the stranded notification")
	}

	// An expired processing lease is requeued.
	task, err := stores.DocumentJobs.Claim(context.Background(), notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %+v, %v", task, err)
	}
	clock += stores.DocumentJobs.Settings.LeaseSeconds + 10
	if err := processor.Recover(context.Background()); err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	if job.Status != state.DocumentJobQueued || job.Stage != "retry_wait" {
		t.Fatalf("expired lease job = %+v", job)
	}
}

func TestOutboxReceiverLocalMode(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	first := submitJob(t, stores, "first.txt", []byte("first"))
	second := submitJob(t, stores, "second.txt", []byte("second"))
	ingester := &fakeIngester{}
	ingester.setRun(func(_ context.Context, payload Payload) error {
		commitPublication(t, stores, payload)
		return nil
	})
	receiver := NewOutboxReceiver(stores.DocumentJobs)
	processor := newProcessor(t, stores, ingester, Options{Receiver: receiver, BatchSize: 2})

	processed, err := processor.RunOnce(context.Background())
	if err != nil || processed != 2 {
		t.Fatalf("RunOnce() = %d, %v", processed, err)
	}
	for _, notification := range []state.DocumentNotification{first, second} {
		if job := jobStatus(t, stores, notification.ArtifactID); job.Status != state.DocumentJobSucceeded {
			t.Fatalf("job %s = %+v", notification.ArtifactID, job)
		}
	}
	pending, err := stores.DocumentJobs.PendingOutbox(context.Background(), 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("localhost outbox pending = %d, %v", len(pending), err)
	}
	if notification, ok, err := receiver.Receive(context.Background()); err != nil || ok {
		t.Fatalf("empty receiver = %+v, %v, %v", notification, ok, err)
	}
	if processed, err := processor.RunOnce(context.Background()); err != nil || processed != 0 {
		t.Fatalf("empty RunOnce() = %d, %v", processed, err)
	}
}

func TestRunLoopShutdownIsIdempotent(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	ingester := &fakeIngester{}
	ingester.setRun(func(_ context.Context, payload Payload) error {
		commitPublication(t, stores, payload)
		return nil
	})
	processor := newProcessor(t, stores, ingester, Options{
		Receiver:     NewOutboxReceiver(stores.DocumentJobs),
		PollInterval: 5 * time.Millisecond,
	})
	done := make(chan error, 1)
	go func() { done <- processor.Run(context.Background()) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if job := jobStatus(t, stores, notification.ArtifactID); job.Status == state.DocumentJobSucceeded {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if job := jobStatus(t, stores, notification.ArtifactID); job.Status != state.DocumentJobSucceeded {
		t.Fatalf("Run loop did not process the job: %+v", job)
	}
	processor.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not stop")
	}
	processor.Stop()
	if err := processor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := processor.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := processor.Run(context.Background()); err == nil {
		t.Fatalf("Run after Close must fail")
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
		Schema:     state.DocumentEnvelopeSchema,
		ArtifactID: "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a",
		Namespace:  testNamespace,
		Name:       "missing.txt",
		CreatedAt:  1,
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
	body, _, err := mq.EncodeDocumentNotification(notification)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	ingester := &fakeIngester{}
	ingester.setRun(func(_ context.Context, payload Payload) error {
		commitPublication(t, stores, payload)
		return nil
	})
	processor := newProcessor(t, stores, ingester, Options{})
	consumer := &fakeConsumer{handler: processor.MessageHandler(), messages: make(chan mq.ReceivedMessage, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- processor.RunConsumer(ctx, consumer) }()

	consumer.messages <- mq.ReceivedMessage{Body: body}
	deadline := time.Now().Add(2 * time.Second)
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
	case <-time.After(2 * time.Second):
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
