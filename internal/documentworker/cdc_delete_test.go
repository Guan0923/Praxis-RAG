package documentworker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// documentCDCRow renders one document_job_outbox row image the way Debezium
// emits it (flattened JSON, schemas.enable=false).
func documentCDCRow(notification state.DocumentNotification) map[string]any {
	return map[string]any{
		"id":          "0123456789abcdef0123456789abcdef",
		"artifact_id": notification.ArtifactID,
		"namespace":   notification.Namespace,
		"name":        notification.Name,
		"status":      "PENDING",
		"attempts":    float64(0),
		"retry_at":    notification.CreatedAt,
		"created_at":  notification.CreatedAt,
	}
}

func documentCDCEvent(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("json.Marshal(%+v): %v", fields, err)
	}
	return body
}

// outboxDeleteEvent is the CDC event produced when GCOutbox deletes the
// outbox row of notification.
func outboxDeleteEvent(t *testing.T, notification state.DocumentNotification) []byte {
	t.Helper()
	return documentCDCEvent(t, map[string]any{
		"before": documentCDCRow(notification),
		"after":  nil,
		"op":     "d",
	})
}

// outboxCreateEvent is the CDC event produced when the row is inserted.
func outboxCreateEvent(t *testing.T, notification state.DocumentNotification) []byte {
	t.Helper()
	return documentCDCEvent(t, map[string]any{
		"before": nil,
		"after":  documentCDCRow(notification),
		"op":     "c",
	})
}

func encodedNativeEvent(t *testing.T, notification state.DocumentNotification) []byte {
	t.Helper()
	body, _, err := mq.EncodeDocumentNotification(notification)
	if err != nil {
		t.Fatalf("EncodeDocumentNotification: %v", err)
	}
	return body
}

func TestMessageHandlerAcksOutboxDeletion(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	ingester := &fakeIngester{}
	processor := newProcessor(t, stores, ingester, Options{})
	handler := processor.MessageHandler()
	ctx := context.Background()

	if acked := handler(ctx, mq.ReceivedMessage{
		Topic: "doc-topic", Partition: 2, Offset: 42, Body: outboxDeleteEvent(t, notification),
	}); !acked {
		t.Fatalf("CDC delete event must be acknowledged")
	}
	if acked := handler(ctx, mq.ReceivedMessage{Body: nil}); !acked {
		t.Fatalf("tombstone (null value) must be acknowledged")
	}
	if acked := handler(ctx, mq.ReceivedMessage{Body: []byte("null")}); !acked {
		t.Fatalf("JSON null tombstone must be acknowledged")
	}
	if calls := ingester.callCount(); calls != 0 {
		t.Fatalf("ingester calls = %d, want 0 for outbox deletions", calls)
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	if job == nil || job.Status != state.DocumentJobQueued || job.Attempts != 0 {
		t.Fatalf("job changed by outbox deletion: %+v", job)
	}

	// Malformed and invalid payloads keep the retry/DLQ behaviour.
	if acked := handler(ctx, mq.ReceivedMessage{Body: []byte("{not json")}); acked {
		t.Fatalf("malformed payload must stay retryable")
	}
	if acked := handler(ctx, mq.ReceivedMessage{Body: []byte(`{"schema":3}`)}); acked {
		t.Fatalf("invalid schema-3 payload must stay retryable")
	}
}

func TestOutboxDeletionLogIsStableAndBounded(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	var logs bytes.Buffer
	processor := newProcessor(t, stores, &fakeIngester{}, Options{
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	handler := processor.MessageHandler()
	ctx := context.Background()
	if acked := handler(ctx, mq.ReceivedMessage{
		Topic: "doc-topic", Partition: 1, Offset: 7, Body: outboxDeleteEvent(t, notification),
	}); !acked {
		t.Fatalf("CDC delete event must be acknowledged")
	}
	if acked := handler(ctx, mq.ReceivedMessage{Body: nil}); !acked {
		t.Fatalf("tombstone must be acknowledged")
	}
	output := logs.String()
	if !strings.Contains(output, "outbox cleanup event ignored") {
		t.Fatalf("log output = %q, want stable substring %q", output, "outbox cleanup event ignored")
	}
	if strings.Contains(output, "malformed document message ignored") {
		t.Fatalf("deletion hit the malformed path: %q", output)
	}
	for _, field := range []string{"topic=doc-topic", "partition=1", "offset=7"} {
		if !strings.Contains(output, field) {
			t.Fatalf("log output = %q, want bounded field %q", output, field)
		}
	}
	if strings.Contains(output, notification.ArtifactID) || strings.Contains(output, "manual.txt") {
		t.Fatalf("log output leaked payload values: %q", output)
	}
}

func TestMessageHandlerIngestsNativeAndCDCCreate(t *testing.T) {
	cases := []struct {
		name string
		body func(t *testing.T, notification state.DocumentNotification) []byte
	}{
		{name: "native schema-3", body: encodedNativeEvent},
		{name: "cdc create", body: outboxCreateEvent},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stores := newTestStores(t)
			configureNamespace(t, stores)
			notification := submitJob(t, stores, "manual.txt", []byte("original"))
			ingester := &fakeIngester{}
			ingester.setRun(func(_ context.Context, payload Payload) error {
				commitPublication(t, stores, payload)
				return nil
			})
			processor := newProcessor(t, stores, ingester, Options{})
			if acked := processor.MessageHandler()(context.Background(), mq.ReceivedMessage{
				Topic: "doc-topic", Body: testCase.body(t, notification),
			}); !acked {
				t.Fatalf("job notification must be acknowledged")
			}
			if calls := ingester.callCount(); calls != 1 {
				t.Fatalf("ingester calls = %d, want 1", calls)
			}
			job := jobStatus(t, stores, notification.ArtifactID)
			if job == nil || job.Status != state.DocumentJobSucceeded {
				t.Fatalf("job = %+v, want succeeded", job)
			}
		})
	}
}

func TestOutboxDeletionAfterGCSucceededJobKeepsJobSucceeded(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	ingester := &fakeIngester{}
	ingester.setRun(func(_ context.Context, payload Payload) error {
		commitPublication(t, stores, payload)
		return nil
	})
	processor := newProcessor(t, stores, ingester, Options{})
	ctx := context.Background()
	if acked := processor.MessageHandler()(ctx, mq.ReceivedMessage{
		Topic: "doc-topic", Body: encodedNativeEvent(t, notification),
	}); !acked {
		t.Fatalf("notification must be acknowledged")
	}
	if job := jobStatus(t, stores, notification.ArtifactID); job == nil ||
		job.Status != state.DocumentJobSucceeded {
		t.Fatalf("job = %+v, want succeeded", job)
	}

	// Routine retention cleanup deletes the outbox row; a negative max age puts
	// the cutoff in the future so the test does not depend on the retention
	// setting or clock granularity.
	future := -1.0
	removed, err := stores.DocumentJobs.GCOutbox(ctx, &future)
	if err != nil {
		t.Fatalf("GCOutbox: %v", err)
	}
	if removed == 0 {
		t.Fatalf("GCOutbox removed no outbox rows")
	}

	// The later Debezium delete event must ack and leave the succeeded job alone.
	if acked := processor.MessageHandler()(ctx, mq.ReceivedMessage{
		Topic: "doc-topic", Partition: 0, Offset: 9, Body: outboxDeleteEvent(t, notification),
	}); !acked {
		t.Fatalf("outbox delete event must be acknowledged")
	}
	complete, err := processor.DeadLetterMessage(ctx, mq.ReceivedMessage{
		Body: outboxDeleteEvent(t, notification),
	})
	if err != nil || !complete {
		t.Fatalf("DeadLetterMessage(outbox delete) = %v, %v; want (true, nil)", complete, err)
	}
	if calls := ingester.callCount(); calls != 1 {
		t.Fatalf("ingester calls = %d, want 1", calls)
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	if job == nil || job.Status != state.DocumentJobSucceeded {
		t.Fatalf("job = %+v, want still succeeded", job)
	}
}

func TestDeadLetterMessageOutboxDeletionIsAlreadyComplete(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	processor := newProcessor(t, stores, &fakeIngester{}, Options{})
	ctx := context.Background()
	deleteBody := outboxDeleteEvent(t, notification)

	complete, err := processor.DeadLetterMessage(ctx, mq.ReceivedMessage{Body: deleteBody})
	if err != nil || !complete {
		t.Fatalf("DeadLetterMessage(outbox delete) = %v, %v; want (true, nil)", complete, err)
	}
	if acked := processor.DeadLetterHandler()(ctx, mq.ReceivedMessage{Body: deleteBody}); !acked {
		t.Fatalf("DeadLetterHandler(outbox delete) must acknowledge")
	}
	if err := processor.ExhaustedHandler()(ctx, mq.ReceivedMessage{Body: deleteBody}); err != nil {
		t.Fatalf("ExhaustedHandler(outbox delete) error = %v", err)
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	if job == nil || job.Status != state.DocumentJobQueued || job.Error != nil {
		t.Fatalf("job changed by dead-lettered outbox deletion: %+v", job)
	}
}

// documentPullTransport is a minimal mq.PullTransport double: it delivers the
// queued batches then blocks until the consumer context is cancelled, and it
// records every committed record.
type documentPullTransport struct {
	mutex   sync.Mutex
	batches [][]mq.ReceivedMessage
	acked   []mq.ReceivedMessage
}

func (f *documentPullTransport) Start() error { return nil }
func (f *documentPullTransport) Stop() error  { return nil }

func (f *documentPullTransport) Receive(ctx context.Context, _ int32, _ time.Duration) ([]mq.ReceivedMessage, error) {
	f.mutex.Lock()
	if len(f.batches) > 0 {
		batch := f.batches[0]
		f.batches = f.batches[1:]
		f.mutex.Unlock()
		return batch, nil
	}
	f.mutex.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *documentPullTransport) Ack(_ context.Context, message mq.ReceivedMessage) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.acked = append(f.acked, message)
	return nil
}

func (f *documentPullTransport) ChangeInvisibleDuration(_ context.Context, _ mq.ReceivedMessage, _ time.Duration) error {
	return nil
}

func (f *documentPullTransport) ackedCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return len(f.acked)
}

// documentDLQPublisher records dead letters so the test can prove none happen.
type documentDLQPublisher struct {
	mutex     sync.Mutex
	published [][]byte
}

func (f *documentDLQPublisher) Publish(_ context.Context, _ string, _ string, body []byte) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.published = append(f.published, append([]byte(nil), body...))
	return nil
}

func (f *documentDLQPublisher) Close() error { return nil }

func (f *documentDLQPublisher) count() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return len(f.published)
}

// TestConsumerCommitsOutboxDeletionWithoutRetryOrDLQ wires the real
// Processor.MessageHandler into mq.PullConsumer: a routine outbox-retention
// delete must be committed on the first delivery, never retried in-process and
// never published to the document DLQ.
func TestConsumerCommitsOutboxDeletionWithoutRetryOrDLQ(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	ingester := &fakeIngester{}
	processor := newProcessor(t, stores, ingester, Options{})

	transport := &documentPullTransport{batches: [][]mq.ReceivedMessage{{
		{Topic: "doc-topic", Key: notification.ArtifactID, Body: outboxDeleteEvent(t, notification)},
	}}}
	dlq := &documentDLQPublisher{}
	config := mq.DefaultConsumerConfig("doc-topic", "doc-group")
	config.PollInterval = time.Millisecond
	config.RetryBaseDelay = time.Millisecond
	config.RetryMaxDelay = 5 * time.Millisecond
	config.MaxAttempts = 3
	config.DLQTopic = "doc-dlq"
	consumer := mq.NewPullConsumer(config, transport, processor.MessageHandler())
	consumer.DLQ = dlq
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("consumer.Start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && transport.ackedCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("consumer.Stop: %v", err)
	}
	if acked := transport.ackedCount(); acked != 1 {
		t.Fatalf("committed records = %d, want 1", acked)
	}
	if published := dlq.count(); published != 0 {
		t.Fatalf("DLQ publishes = %d, want 0", published)
	}
	if calls := ingester.callCount(); calls != 0 {
		t.Fatalf("ingester calls = %d, want 0", calls)
	}
	if err := consumer.LastError(); err != nil {
		t.Fatalf("consumer.LastError = %v", err)
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	if job == nil || job.Status != state.DocumentJobQueued {
		t.Fatalf("job = %+v, want unchanged queued job", job)
	}
}
