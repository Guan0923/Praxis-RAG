package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/mq"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// TestDocumentDeadLetterExhaustion drives a document delivery through the real
// mq.PullConsumer retry budget. The dead-letter worker has no ingester, so the
// handler keeps nacking; OnExhausted decodes the same Debezium event and records
// the durable terminal state, and only then is the message acked.
func TestDocumentDeadLetterExhaustion(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()

	admission, err := h.Service.AddDocument(ctx, "alpha", "poison.md", []byte(documentText), "")
	if err != nil {
		t.Fatalf("AddDocument() error = %v", err)
	}
	artifactID, _ := admission["artifact_id"].(string)
	job, err := h.Stores.DocumentJobs.Get(ctx, artifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", job, err)
	}

	// The dead-letter topic carries the raw Debezium change event (schema
	// stripped, op/source present), not the application-native envelope.
	body, err := json.Marshal(map[string]any{
		"before": nil,
		"after": map[string]any{
			"artifact_id":      job.ArtifactID,
			"namespace":        job.Namespace,
			"schedule_version": job.ScheduleVersion,
		},
		"source": map[string]any{"table": "document_jobs"},
		"op":     "c",
		"ts_ms":  time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("marshal Debezium event: %v", err)
	}

	worker := newDocumentDeadLetterProcessor(t, h)
	transport := &fakePullTransport{}
	transport.push(mq.ReceivedMessage{Topic: "praxis_document_ingest", Body: body, DeliveryAttempt: 1})
	consumerConfig := consumerConfig("praxis_document_ingest", "praxis_document_ingest_workers")
	consumerConfig.MaxAttempts = 1
	consumer := mq.NewPullConsumer(consumerConfig, transport, worker.MessageHandler())
	consumer.OnExhausted = worker.ExhaustedHandler()
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.RunConsumer(runCtx, consumer) }()

	waitFor(t, 10*time.Second, "document job failed by dead-letter", func() bool {
		current, readErr := h.Stores.DocumentJobs.Get(context.Background(), artifactID, nil)
		return readErr == nil && current != nil && current.Status == state.DocumentJobFailed
	})
	waitFor(t, 5*time.Second, "document dead-letter committed", func() bool { return transport.ackCount() == 1 })
	cancel()
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("RunConsumer() error = %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("dead-letter consumer did not stop")
	}
	if transport.ackCount() != 1 {
		t.Fatalf("ack count = %d, want 1 (ack only after the durable outcome)", transport.ackCount())
	}

	current, err := h.Stores.DocumentJobs.Get(ctx, artifactID, nil)
	if err != nil || current == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", current, err)
	}
	// The dead-lettered job keeps the documented prefix; when a prior attempt
	// recorded a failure, DeadLetter appends that last error after it.
	if current.Status != state.DocumentJobFailed || current.Error == nil ||
		!strings.HasPrefix(*current.Error, "Message retry limit reached") {
		t.Fatalf("dead-lettered job = %+v", current)
	}
	status, err := h.Service.GetDocumentStatus(ctx, "alpha", artifactID)
	if err != nil {
		t.Fatalf("GetDocumentStatus() error = %v", err)
	}
	if status["status"] != state.DocumentJobFailed || status["error"] != *current.Error {
		t.Fatalf("document status = %v (job error %q)", status, *current.Error)
	}
}
