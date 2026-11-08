package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/searchrefs"
	"github.com/Guan0923/xinghe_rag/internal/searchworker"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// TestSearchPipelineEndToEnd drives the queued-search path through the real
// runtime: broker-backed admission -> captured schema-2 envelope -> search
// worker retrieval + reference capture + snapshot completion -> stored result
// readable through the service.
func TestSearchPipelineEndToEnd(t *testing.T) {
	h := newHarness(t, searchQueryPoints)
	h.startBackgroundPublisher(t)
	ctx := context.Background()

	request := contracts.SearchRequest{
		Query:      "E03 供水不足",
		Retrieval:  contracts.RetrievalHybrid,
		Rerank:     contracts.RerankCrossEncoder,
		CandidateK: 10,
		TopK:       5,
	}
	resultID, err := h.Service.SubmitSearch(ctx, "alpha", request)
	if err != nil {
		t.Fatalf("SubmitSearch() error = %v", err)
	}
	if len(resultID) != 36 {
		t.Fatalf("SubmitSearch() result_id = %q", resultID)
	}
	envelope, ok := h.Broker.lastSearchEnvelope()
	if !ok {
		t.Fatalf("no search envelope reached the broker")
	}
	if envelope.ResultID != resultID || envelope.Namespace != "alpha" {
		t.Fatalf("published envelope = %+v", envelope)
	}
	if envelope.Arguments["query"] != request.Query || envelope.Arguments["retrieval"] != string(contracts.RetrievalHybrid) {
		t.Fatalf("envelope arguments = %v", envelope.Arguments)
	}
	body, _, err := mq.EncodeSearchEnvelope(envelope)
	if err != nil {
		t.Fatalf("EncodeSearchEnvelope() error = %v", err)
	}
	// The broker decoder must keep integer tokens as json.Number (Python json
	// preserves ints); float64 would make the worker reject candidate_k/top_k.
	decodedEnvelope, err := mq.DecodeSearchEnvelope(body)
	if err != nil {
		t.Fatalf("DecodeSearchEnvelope() error = %v", err)
	}
	if candidateK, ok := decodedEnvelope.Arguments["candidate_k"].(json.Number); !ok || candidateK.String() != "10" {
		t.Fatalf("decoded candidate_k = %#v (%T), want json.Number(\"10\")",
			decodedEnvelope.Arguments["candidate_k"], decodedEnvelope.Arguments["candidate_k"])
	}
	if topK, ok := decodedEnvelope.Arguments["top_k"].(json.Number); !ok || topK.String() != "5" {
		t.Fatalf("decoded top_k = %#v (%T), want json.Number(\"5\")",
			decodedEnvelope.Arguments["top_k"], decodedEnvelope.Arguments["top_k"])
	}

	worker, err := searchworker.New(searchworker.Options{
		Tasks:    h.Stores.SearchTasks,
		Searcher: h.Service,
		Capturer: searchrefs.New(h.Stores, testLogger()),
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("searchworker.New() error = %v", err)
	}
	transport := &fakePullTransport{}
	transport.push(mq.ReceivedMessage{Topic: "praxis_search", Body: body, DeliveryAttempt: 1})
	consumer := mq.NewPullConsumer(
		consumerConfig("praxis_search", "praxis_search_workers"),
		transport, worker.MessageHandler(),
	)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.RunConsumer(runCtx, consumer) }()

	waitFor(t, 15*time.Second, "search task succeeded", func() bool {
		view, readErr := h.Stores.SearchTasks.Get(context.Background(), "alpha", resultID, 1, 10)
		return readErr == nil && view != nil && view.Status == state.SearchTaskSucceeded
	})
	cancel()
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("RunConsumer() error = %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("search consumer did not stop")
	}
	if err := worker.Close(); err != nil {
		t.Fatalf("worker.Close() error = %v", err)
	}
	if transport.ackCount() != 1 || transport.nackCount() != 0 {
		t.Fatalf("delivery ack/nack = %d/%d, want 1/0", transport.ackCount(), transport.nackCount())
	}

	// The durable task is terminal and the immutable snapshot is readable.
	terminal, err := h.Stores.SearchTasks.Terminal(ctx, resultID)
	if err != nil || !terminal {
		t.Fatalf("Terminal() = %v, %v", terminal, err)
	}
	payload, err := h.Service.GetSearchResult(ctx, "alpha", resultID, 1, 10)
	if err != nil {
		t.Fatalf("GetSearchResult() error = %v", err)
	}
	if payload["result_id"] != resultID {
		t.Fatalf("stored result_id = %v", payload["result_id"])
	}
	if payload["snapshot_available"] != true {
		t.Fatalf("stored snapshot_available = %v", payload["snapshot_available"])
	}
	results, ok := payload["results"].([]any)
	if !ok || len(results) == 0 {
		t.Fatalf("stored results = %v", payload["results"])
	}
	if payload["evidence_status"] == nil {
		t.Fatalf("stored payload has no evidence_status: %v", payload)
	}

	// The retrieval stack really ran: embedding, Qdrant query and rerank.
	if embeddings := h.Model.count(http.MethodPost, "/embeddings"); embeddings == 0 {
		t.Fatalf("no embedding requests reached the model server")
	}
	if reranks := h.Model.count(http.MethodPost, "/rerank"); reranks == 0 {
		t.Fatalf("no rerank request reached the model server")
	}
	if queries := h.Qdrant.count(http.MethodPost, "/points/query"); queries == 0 {
		t.Fatalf("no Qdrant query was issued")
	}
}
