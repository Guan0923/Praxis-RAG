package e2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// TestSearchPipelineEndToEnd drives the queued-search path through the real
// runtime and the search worker handler:
//
//	MySQL-only admission (no broker acceptance) -> schema-3 notification (what
//	Debezium emits) -> claim + retrieval + reference capture + snapshot
//	completion -> stored result readable through the service.
func TestSearchPipelineEndToEnd(t *testing.T) {
	h := newHarness(t, searchQueryPoints)
	ctx := context.Background()

	request := contracts.SearchRequest{
		Query:      "E03 供水不足",
		Retrieval:  contracts.RetrievalHybrid,
		Rerank:     contracts.RerankCrossEncoder,
		CandidateK: 10,
		TopK:       5,
	}
	// Admission is a MySQL commit: it must succeed with no broker configured and
	// no background loop running.
	resultID, err := h.Service.SubmitSearch(ctx, "alpha", request)
	if err != nil {
		t.Fatalf("SubmitSearch() error = %v", err)
	}
	if len(resultID) != 36 {
		t.Fatalf("SubmitSearch() result_id = %q", resultID)
	}
	view, err := h.Stores.SearchTasks.Get(ctx, "alpha", resultID, 1, 10)
	if err != nil || view == nil || view.Status != state.SearchTaskQueued {
		t.Fatalf("admitted search task = %+v, %v", view, err)
	}

	// Debezium delivers only the locating identifiers; the worker reads the
	// query arguments and deadline back from search_tasks.
	worker := newSearchProcessor(t, h)
	if acked := worker.Handle(ctx, searchNotification(resultID, "alpha")); !acked {
		t.Fatalf("Handle() = false, want an ACK once the durable terminal outcome is saved")
	}
	defer func() {
		if closeErr := worker.Close(); closeErr != nil {
			t.Fatalf("worker.Close() error = %v", closeErr)
		}
	}()

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

	// The result snapshot is a MySQL fact: evicting the whole Redis cache must
	// still yield the identical result by falling back to search_result_snapshots
	// (and the read path repairs the cache).
	if keys := h.Redis.Keys(); len(keys) == 0 {
		t.Fatalf("completed search left the Redis result cache empty")
	}
	// FlushAll evicts every Redis key; the read path must rebuild the envelope
	// from search_result_snapshots.
	h.Redis.FlushAll()
	restored, err := h.Service.GetSearchResult(ctx, "alpha", resultID, 1, 10)
	if err != nil {
		t.Fatalf("GetSearchResult() after cache loss error = %v", err)
	}
	if restored["result_id"] != resultID || restored["snapshot_available"] != true {
		t.Fatalf("restored snapshot = %v", restored)
	}
	restoredResults, ok := restored["results"].([]any)
	if !ok || len(restoredResults) != len(results) {
		t.Fatalf("restored results = %#v, want %d items", restored["results"], len(results))
	}
	if keys := h.Redis.Keys(); len(keys) == 0 {
		t.Fatalf("cache was not repaired from the MySQL snapshot")
	}
}

// TestSearchStaleNotificationIsAcknowledged pins the at-least-once delivery
// contract: a repeated schema-3 notification for an already-succeeded task must
// be ACKed without re-running retrieval.
func TestSearchStaleNotificationIsAcknowledged(t *testing.T) {
	h := newHarness(t, searchQueryPoints)
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
	worker := newSearchProcessor(t, h)
	defer func() { _ = worker.Close() }()
	notification := searchNotification(resultID, "alpha")
	if acked := worker.Handle(ctx, notification); !acked {
		t.Fatalf("Handle() = false, want ACK")
	}
	queriesBefore := h.Qdrant.count(http.MethodPost, "/points/query")
	// The task is now terminal, so the duplicate delivery must be skipped and
	// ACKed without a second retrieval.
	if acked := worker.Handle(ctx, notification); !acked {
		t.Fatalf("replayed Handle() = false, want a safe ACK")
	}
	if queriesAfter := h.Qdrant.count(http.MethodPost, "/points/query"); queriesAfter != queriesBefore {
		t.Fatalf("duplicate delivery re-ran retrieval (%d -> %d queries)", queriesBefore, queriesAfter)
	}
}
