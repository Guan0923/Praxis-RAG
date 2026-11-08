package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/documentworker"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/searchworker"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// TestDocumentDeadLetterExhaustion drives a document delivery through the real
// mq.PullConsumer retry budget: the handler keeps nacking, OnExhausted records
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
	notification, err := state.DocumentJobEnvelope(job)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope() error = %v", err)
	}
	body, _, err := mq.EncodeDocumentNotification(notification)
	if err != nil {
		t.Fatalf("EncodeDocumentNotification() error = %v", err)
	}

	// A dead-letter-only processor cannot consume the job, so the delivery keeps
	// failing until the broker retry budget is exhausted.
	worker, err := documentworker.New(documentworker.Options{
		Jobs:   h.Stores.DocumentJobs,
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("documentworker.New() error = %v", err)
	}
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
	cancel()
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("RunConsumer() error = %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("dead-letter consumer did not stop")
	}
	if err := worker.Close(); err != nil {
		t.Fatalf("worker.Close() error = %v", err)
	}
	if transport.ackCount() != 1 {
		t.Fatalf("ack count = %d, want 1 (ack only after the durable outcome)", transport.ackCount())
	}

	current, err := h.Stores.DocumentJobs.Get(ctx, artifactID, nil)
	if err != nil || current == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", current, err)
	}
	if current.Status != state.DocumentJobFailed || current.Error == nil || *current.Error != "Message retry limit reached" {
		t.Fatalf("dead-lettered job = %+v", current)
	}
	status, err := h.Service.GetDocumentStatus(ctx, "alpha", artifactID)
	if err != nil {
		t.Fatalf("GetDocumentStatus() error = %v", err)
	}
	if status["status"] != state.DocumentJobFailed || status["error"] != "Message retry limit reached" {
		t.Fatalf("document status = %v", status)
	}
}

// TestSearchDeadLetterExhaustion is the queued-search counterpart: the retry
// budget is exhausted, the task becomes durably failed, and the Python-safe
// failure message is exposed by the result read.
func TestSearchDeadLetterExhaustion(t *testing.T) {
	h := newHarness(t, "")
	h.startBackgroundPublisher(t)
	ctx := context.Background()

	resultID, err := h.Service.SubmitSearch(ctx, "alpha", contracts.SearchRequest{
		Query:      "E03 供水不足",
		Retrieval:  contracts.RetrievalHybrid,
		Rerank:     contracts.RerankCrossEncoder,
		CandidateK: 10,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("SubmitSearch() error = %v", err)
	}
	envelope, ok := h.Broker.lastSearchEnvelope()
	if !ok {
		t.Fatalf("no search envelope reached the broker")
	}
	body, _, err := mq.EncodeSearchEnvelope(envelope)
	if err != nil {
		t.Fatalf("EncodeSearchEnvelope() error = %v", err)
	}

	worker, err := searchworker.New(searchworker.Options{
		Tasks:  h.Stores.SearchTasks,
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("searchworker.New() error = %v", err)
	}
	transport := &fakePullTransport{}
	transport.push(mq.ReceivedMessage{Topic: "praxis_search", Body: body, DeliveryAttempt: 1})
	consumerConfig := consumerConfig("praxis_search", "praxis_search_workers")
	consumerConfig.MaxAttempts = 1
	consumer := mq.NewPullConsumer(consumerConfig, transport, worker.MessageHandler())
	consumer.OnExhausted = worker.ExhaustedHandler()
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.RunConsumer(runCtx, consumer) }()

	waitFor(t, 10*time.Second, "search task failed by dead-letter", func() bool {
		terminal, terminalErr := h.Stores.SearchTasks.Terminal(context.Background(), resultID)
		return terminalErr == nil && terminal
	})
	cancel()
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("RunConsumer() error = %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("dead-letter consumer did not stop")
	}
	if err := worker.Close(); err != nil {
		t.Fatalf("worker.Close() error = %v", err)
	}
	if transport.ackCount() != 1 {
		t.Fatalf("ack count = %d, want 1 (ack only after the durable outcome)", transport.ackCount())
	}

	_, err = h.Service.GetSearchResult(ctx, "alpha", resultID, 1, 10)
	var unavailable *apierrors.SnapshotUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("GetSearchResult() error = %v, want SnapshotUnavailable", err)
	}
	if unavailable.Message != "Search failed; submit a new search" {
		t.Fatalf("failure message = %q", unavailable.Message)
	}
}
