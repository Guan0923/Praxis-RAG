package runtime

import (
	"context"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
)

// TestRestrictedSearchRequestWidensInternalBounds pins the internal pool: a
// document-restricted search widens candidate_k and the post-rerank selection to
// restrictedCandidateK while leaving the caller's request untouched.
func TestRestrictedSearchRequestWidensInternalBounds(t *testing.T) {
	request := contracts.SearchRequest{
		Query:       "q",
		Retrieval:   contracts.RetrievalEmbedding,
		Rerank:      contracts.RerankNone,
		CandidateK:  5,
		TopK:        2,
		DocumentIDs: []string{"doc-e03"},
	}
	restricted := restrictedSearchRequest(request)
	if restricted.CandidateK != restrictedCandidateK {
		t.Errorf("CandidateK = %d, want %d", restricted.CandidateK, restrictedCandidateK)
	}
	if restricted.TopK != restrictedCandidateK {
		t.Errorf("TopK = %d, want %d", restricted.TopK, restrictedCandidateK)
	}
	if request.CandidateK != 5 || request.TopK != 2 {
		t.Fatalf("restrictedSearchRequest mutated the caller request: %+v", request)
	}

	wide := contracts.SearchRequest{CandidateK: 100, TopK: 10}
	if got := restrictedSearchRequest(wide); got.CandidateK != 100 || got.TopK != restrictedCandidateK {
		t.Errorf("wide request = %+v", got)
	}
}

// TestFilterSearchPayloadKeepsVisibleResultsAndRenumbersRanks pins the
// post-orchestrator filter: only allowed document ids survive, ranks restart at
// 1, the caller-visible bounds are restored and the counts/evidence fields are
// recomputed.
func TestFilterSearchPayloadKeepsVisibleResultsAndRenumbersRanks(t *testing.T) {
	payload := map[string]any{
		"query":           "q",
		"candidate_k":     restrictedCandidateK,
		"top_k":           restrictedCandidateK,
		"evidence_status": "found",
		"answer_guidance": evidenceFoundGuidance,
		"results": []any{
			map[string]any{"rank": 1, "document_id": "doc-a", "chunk_id": "c1"},
			map[string]any{"rank": 2, "document_id": "doc-b", "chunk_id": "c2"},
			map[string]any{"rank": 3, "document_id": "doc-c", "chunk_id": "c3"},
			map[string]any{"rank": 4, "document_id": "doc-a", "chunk_id": "c4"},
		},
		"debug": map[string]any{
			"candidate_count": 7,
			"returned_count":  restrictedCandidateK,
		},
	}
	request := contracts.SearchRequest{
		CandidateK:  5,
		TopK:        2,
		DocumentIDs: []string{"doc-c", "doc-a"},
	}
	filtered := filterSearchPayload(payload, request)
	results := payloadResults(t, filtered)
	if len(results) != 2 {
		t.Fatalf("results = %v", results)
	}
	if results[0]["document_id"] != "doc-a" || results[0]["rank"] != 1 {
		t.Errorf("first result = %v", results[0])
	}
	if results[1]["document_id"] != "doc-c" || results[1]["rank"] != 2 {
		t.Errorf("second result = %v", results[1])
	}
	if filtered["evidence_status"] != "found" || filtered["answer_guidance"] != evidenceFoundGuidance {
		t.Errorf("evidence = %v/%v", filtered["evidence_status"], filtered["answer_guidance"])
	}
	if filtered["candidate_k"] != 5 || filtered["top_k"] != 2 {
		t.Errorf("visible bounds = %v/%v, want 5/2", filtered["candidate_k"], filtered["top_k"])
	}
	debug := payloadDebug(t, filtered)
	if debug["returned_count"] != 2 || debug["candidate_count"] != 7 {
		t.Errorf("debug = %v", debug)
	}

	// The original payload is not mutated (rank renumbering works on copies).
	original := payloadResults(t, payload)
	if original[0]["rank"] != 1 || original[3]["rank"] != 4 {
		t.Errorf("filter mutated the input payload: %v", original)
	}
}

// TestFilterSearchPayloadHonorsTopK truncates to the caller's top_k after
// filtering, even though the internal selection was wider.
func TestFilterSearchPayloadHonorsTopK(t *testing.T) {
	payload := map[string]any{
		"candidate_k": restrictedCandidateK,
		"top_k":       restrictedCandidateK,
		"results": []any{
			map[string]any{"rank": 1, "document_id": "doc-a"},
			map[string]any{"rank": 2, "document_id": "doc-b"},
			map[string]any{"rank": 3, "document_id": "doc-a"},
		},
		"debug": map[string]any{"candidate_count": 3, "returned_count": 3},
	}
	request := contracts.SearchRequest{CandidateK: 10, TopK: 1, DocumentIDs: []string{"doc-a", "doc-b"}}
	filtered := filterSearchPayload(payload, request)
	results := payloadResults(t, filtered)
	if len(results) != 1 || results[0]["document_id"] != "doc-a" {
		t.Fatalf("results = %v", results)
	}
}

// TestFilterSearchPayloadEmptyRestrictionKeepsNoEvidence pins the empty
// semantics: an empty (but non-nil) restriction yields an empty results list and
// the existing no-evidence status/guidance.
func TestFilterSearchPayloadEmptyRestrictionKeepsNoEvidence(t *testing.T) {
	payload := map[string]any{
		"candidate_k": restrictedCandidateK,
		"top_k":       restrictedCandidateK,
		"results": []any{
			map[string]any{"rank": 1, "document_id": "doc-a"},
		},
		"debug": map[string]any{"candidate_count": 1, "returned_count": 1},
	}
	filtered := filterSearchPayload(payload, contracts.SearchRequest{
		CandidateK: 5, TopK: 2, DocumentIDs: []string{},
	})
	results, ok := filtered["results"].([]any)
	if !ok || results == nil || len(results) != 0 {
		t.Fatalf("results = %#v, want non-nil empty []any", filtered["results"])
	}
	if filtered["evidence_status"] != "no_evidence" {
		t.Errorf("evidence_status = %v", filtered["evidence_status"])
	}
	if filtered["answer_guidance"] != evidenceMissingGuidance {
		t.Errorf("answer_guidance = %v", filtered["answer_guidance"])
	}
	if debug := payloadDebug(t, filtered); debug["returned_count"] != 0 {
		t.Errorf("returned_count = %v", debug["returned_count"])
	}
	if filtered["top_k"] != 2 || filtered["candidate_k"] != 5 {
		t.Errorf("bounds = %v/%v", filtered["candidate_k"], filtered["top_k"])
	}
}

// TestFilterSearchPayloadAcceptsTypedResults covers the un-normalized
// []map[string]any orchestrator shape.
func TestFilterSearchPayloadAcceptsTypedResults(t *testing.T) {
	payload := map[string]any{
		"results": []map[string]any{
			{"rank": 1, "document_id": "doc-a"},
			{"rank": 2, "document_id": "doc-b"},
		},
	}
	filtered := filterSearchPayload(payload, contracts.SearchRequest{
		CandidateK: 2, TopK: 5, DocumentIDs: []string{"doc-b"},
	})
	results := payloadResults(t, filtered)
	if len(results) != 1 || results[0]["document_id"] != "doc-b" || results[0]["rank"] != 1 {
		t.Fatalf("results = %v", results)
	}
}

// TestSearchHonorsDocumentIDs drives the full runtime path against the fake
// Qdrant/model APIs: a restricted request raises the internal candidate pool,
// filters the payload and keeps the caller-visible bounds.
func TestSearchHonorsDocumentIDs(t *testing.T) {
	qdrantURL, qdrantCalls := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, _ := newFakeModelAPI(t, nil)
	engine, _ := newSearchEngineForTest(t, qdrantURL, modelURL)

	request := mustSearchRequest(t, map[string]any{
		"query": "证据", "retrieval": "embedding", "rerank": "none", "candidate_k": 5, "top_k": 2,
	})
	request.DocumentIDs = []string{"doc-e03"}
	payload, err := engine.Search(context.Background(), "alpha", request)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	results := payloadResults(t, payload)
	if len(results) != 1 || results[0]["document_id"] != "doc-e03" || results[0]["rank"] != 1 {
		t.Fatalf("results = %v", results)
	}
	if payload["candidate_k"] != 5 || payload["top_k"] != 2 {
		t.Errorf("visible bounds = %v/%v", payload["candidate_k"], payload["top_k"])
	}
	if payload["evidence_status"] != "found" {
		t.Errorf("evidence_status = %v", payload["evidence_status"])
	}
	if debug := payloadDebug(t, payload); debug["returned_count"] != 1 {
		t.Errorf("returned_count = %v", debug["returned_count"])
	}
	// The widened candidate pool reached the retriever.
	if limit := lastQueryBody(t, qdrantCalls)["limit"]; limit != float64(restrictedCandidateK) {
		t.Errorf("Qdrant limit = %v, want %d", limit, restrictedCandidateK)
	}

	// A restriction excluding every document returns empty evidence.
	empty := mustSearchRequest(t, map[string]any{
		"query": "证据", "retrieval": "embedding", "rerank": "none", "candidate_k": 5, "top_k": 2,
	})
	empty.DocumentIDs = []string{}
	emptyPayload, err := engine.Search(context.Background(), "alpha", empty)
	if err != nil {
		t.Fatalf("Search(empty restriction) error = %v", err)
	}
	emptyResults := payloadResults(t, emptyPayload)
	if len(emptyResults) != 0 {
		t.Fatalf("empty restriction results = %v", emptyResults)
	}
	if emptyPayload["evidence_status"] != "no_evidence" || emptyPayload["answer_guidance"] != evidenceMissingGuidance {
		t.Errorf("empty restriction evidence = %v/%v", emptyPayload["evidence_status"], emptyPayload["answer_guidance"])
	}
}

// TestSearchRawHonorsDocumentIDs pins the worker path (no reference capture)
// applies the same visibility filter.
func TestSearchRawHonorsDocumentIDs(t *testing.T) {
	qdrantURL, _ := newFakeQdrantAPI(t, testQueryPoints)
	modelURL, _ := newFakeModelAPI(t, nil)
	engine, _ := newSearchEngineForTest(t, qdrantURL, modelURL)

	request := mustSearchRequest(t, map[string]any{
		"query": "证据", "retrieval": "embedding", "rerank": "none", "candidate_k": 5, "top_k": 2,
	})
	request.DocumentIDs = []string{"doc-temp"}
	payload, err := engine.SearchRaw(context.Background(), "alpha", request)
	if err != nil {
		t.Fatalf("SearchRaw() error = %v", err)
	}
	results := payloadResults(t, payload)
	if len(results) != 1 || results[0]["document_id"] != "doc-temp" || results[0]["rank"] != 1 {
		t.Fatalf("results = %v", results)
	}
	if payload["top_k"] != 2 || payload["candidate_k"] != 5 {
		t.Errorf("bounds = %v/%v", payload["candidate_k"], payload["top_k"])
	}
}
