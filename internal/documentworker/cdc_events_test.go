package documentworker

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// documentCDCRow renders the document_jobs row image Debezium captures: only
// the locating columns of column.include.list, never file bytes or status.
func documentCDCRow(notification state.DocumentNotification) map[string]any {
	return map[string]any{
		"artifact_id":      notification.ArtifactID,
		"namespace":        notification.Namespace,
		"schedule_version": notification.ScheduleVersion,
	}
}

// documentCDCEvent renders one raw Debezium change event
// (debezium.format.value.schemas.enable=false).
func documentCDCEvent(t *testing.T, before, after any, op string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"before": before,
		"after":  after,
		"source": map[string]any{
			"schema": "praxis_rag",
			"table":  "document_jobs",
		},
		"op":    op,
		"ts_ms": float64(1700000000000),
	})
	if err != nil {
		t.Fatalf("json.Marshal(change event): %v", err)
	}
	return body
}

// deleteEvent is the change event the task connector would produce for a row
// removal (skipped.operations=d normally suppresses it, but a replayed external
// delete must still be ACKed).
func deleteEvent(t *testing.T, notification state.DocumentNotification) []byte {
	t.Helper()
	return documentCDCEvent(t, documentCDCRow(notification), nil, "d")
}

// TestMessageHandlerAcksNonTaskCDCEvents covers docs/cdc-migration.md §2.1: a
// delete (op=d), a tombstone (null value) and a before-only image are not task
// work. They must be acknowledged without retry, DLQ or touching the job row.
func TestMessageHandlerAcksNonTaskCDCEvents(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	ingester := &fakeIngester{}
	processor := newProcessor(t, stores, ingester, Options{})
	handler := processor.MessageHandler()
	ctx := context.Background()

	for name, body := range map[string][]byte{
		"delete":                 deleteEvent(t, notification),
		"tombstone (nil value)":  nil,
		"tombstone (JSON null)":  []byte("null"),
		"before-only, no op":     []byte(`{"before":{"artifact_id":"` + notification.ArtifactID + `"}}`),
		"unrelated table delete": []byte(`{"before":{"id":1},"after":null,"source":{"table":"cache_revisions"},"op":"d"}`),
	} {
		if acked := handler(ctx, mq.ReceivedMessage{Topic: "praxis_document_ingest", Body: body}); !acked {
			t.Fatalf("%s must be acknowledged without retry", name)
		}
	}
	if calls := ingester.callCount(); calls != 0 {
		t.Fatalf("ingester calls = %d, want 0 for non-task CDC events", calls)
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	if job == nil || job.Status != state.DocumentJobQueued || job.Attempts != 0 || job.ScheduleVersion != 1 {
		t.Fatalf("job changed by non-task CDC event: %+v", job)
	}
}

// TestDeleteEventAfterTerminalJobKeepsState replays a retention DELETE of an
// already-finished job row: the delete is ACKed and neither the terminal job nor
// the processed state changes.
func TestDeleteEventAfterTerminalJobKeepsState(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	ingester := successIngester(t, stores)
	processor := newProcessor(t, stores, ingester, Options{})
	ctx := context.Background()
	if acked := processor.MessageHandler()(ctx, mq.ReceivedMessage{
		Topic: "praxis_document_ingest", Body: nativeBody(t, notification),
	}); !acked {
		t.Fatalf("job notification must be acknowledged")
	}
	if job := jobStatus(t, stores, notification.ArtifactID); job == nil ||
		job.Status != state.DocumentJobSucceeded {
		t.Fatalf("job = %+v, want succeeded", job)
	}

	deleteBody := deleteEvent(t, notification)
	if acked := processor.MessageHandler()(ctx, mq.ReceivedMessage{
		Topic: "praxis_document_ingest", Partition: 0, Offset: 9, Body: deleteBody,
	}); !acked {
		t.Fatalf("delete event of a terminal job must be acknowledged")
	}
	if complete, err := processor.DeadLetterMessage(ctx, mq.ReceivedMessage{Body: deleteBody}); err != nil || !complete {
		t.Fatalf("DeadLetterMessage(delete) = %v, %v; want (true, nil)", complete, err)
	}
	if calls := ingester.callCount(); calls != 1 {
		t.Fatalf("ingester calls = %d, want 1", calls)
	}
	if job := jobStatus(t, stores, notification.ArtifactID); job == nil ||
		job.Status != state.DocumentJobSucceeded {
		t.Fatalf("job = %+v, want still succeeded", job)
	}
}

func TestDeadLetterMessageIgnoresNonTaskCDCEvents(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	processor := newProcessor(t, stores, &fakeIngester{}, Options{})
	ctx := context.Background()

	for name, body := range map[string][]byte{
		"delete":          deleteEvent(t, notification),
		"tombstone":       nil,
		"JSON null":       []byte("null"),
		"unrelated table": []byte(`{"after":null,"before":{"id":1},"source":{"table":"documents"},"op":"d"}`),
	} {
		complete, err := processor.DeadLetterMessage(ctx, mq.ReceivedMessage{Body: body})
		if err != nil || !complete {
			t.Fatalf("DeadLetterMessage(%s) = %v, %v; want (true, nil)", name, complete, err)
		}
		if acked := processor.DeadLetterHandler()(ctx, mq.ReceivedMessage{Body: body}); !acked {
			t.Fatalf("DeadLetterHandler(%s) must acknowledge", name)
		}
		if err := processor.ExhaustedHandler()(ctx, mq.ReceivedMessage{Body: body}); err != nil {
			t.Fatalf("ExhaustedHandler(%s) error = %v", name, err)
		}
	}

	// A malformed dead-letter payload is not a completed outcome: it must stay
	// retryable so a human/replay can inspect it.
	if complete, err := processor.DeadLetterMessage(ctx, mq.ReceivedMessage{Body: []byte("{not json")}); err != nil || complete {
		t.Fatalf("malformed dead-letter message = %v, %v; want (false, nil)", complete, err)
	}
	job := jobStatus(t, stores, notification.ArtifactID)
	if job == nil || job.Status != state.DocumentJobQueued || job.Error != nil {
		t.Fatalf("job changed by dead-lettered non-task event: %+v", job)
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

// TestConsumerCommitsNonTaskCDCEventWithoutRetryOrDLQ wires the real
// Processor.MessageHandler into mq.PullConsumer: a delete/tombstone must be
// committed on the first delivery, never retried in-process and never published
// to the document DLQ.
func TestConsumerCommitsNonTaskCDCEventWithoutRetryOrDLQ(t *testing.T) {
	stores := newTestStores(t)
	configureNamespace(t, stores)
	notification := submitJob(t, stores, "manual.txt", []byte("original"))
	ingester := &fakeIngester{}
	processor := newProcessor(t, stores, ingester, Options{})

	transport := &documentPullTransport{batches: [][]mq.ReceivedMessage{{
		{Topic: "praxis_document_ingest", Key: notification.ArtifactID, Partition: 0, Offset: 0, Body: deleteEvent(t, notification)},
		{Topic: "praxis_document_ingest", Partition: 0, Offset: 1, Body: nil},
	}}}
	dlq := &documentDLQPublisher{}
	config := mq.DefaultConsumerConfig("praxis_document_ingest", "praxis_document_ingest_workers")
	config.PollInterval = time.Millisecond
	config.RetryBaseDelay = time.Millisecond
	config.RetryMaxDelay = 5 * time.Millisecond
	config.MaxAttempts = 3
	config.DLQTopic = "praxis_document_ingest_dlq"
	consumer := mq.NewPullConsumer(config, transport, processor.MessageHandler())
	consumer.DLQ = dlq
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("consumer.Start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && transport.ackedCount() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("consumer.Stop: %v", err)
	}
	if acked := transport.ackedCount(); acked != 2 {
		t.Fatalf("committed records = %d, want 2", acked)
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
