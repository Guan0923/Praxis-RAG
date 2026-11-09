package retrieval

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
)

func TestMockRetrieverMatchesRRFQuery(t *testing.T) {
	results, err := NewMockRetriever().Search(context.Background(), "RRF 怎么融合 BM25 和 Dense？", 10, nil)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected results")
	}
	first := results[0]
	if first.DocumentID != "doc-rrf" || first.ChunkID != "doc-rrf:1" || first.Source != "knowledge/rrf.md" {
		t.Errorf("first candidate = %+v", first)
	}
	if math.Abs(first.Score-1.0) > 1e-12 {
		t.Errorf("score = %v, want 1.0", first.Score)
	}
	wantTerms := []string{"bm25", "dense", "rrf", "融合"}
	if !reflect.DeepEqual(first.MatchedTerms, wantTerms) {
		t.Errorf("matched terms = %v, want %v", first.MatchedTerms, wantTerms)
	}
	if first.Metadata["phase"] != "mock" || first.Metadata["topic"] != "retrieval" {
		t.Errorf("metadata = %v", first.Metadata)
	}
}

func TestMockRetrieverFilterIsAppliedBeforeRanking(t *testing.T) {
	results, err := NewMockRetriever().Search(context.Background(), "Agent Qdrant RRF", 10,
		map[string]any{"document_id": "doc-qdrant"})
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 1 || results[0].DocumentID != "doc-qdrant" {
		t.Fatalf("results = %+v", results)
	}
}

func TestMockRetrieverLimitAndNoMatch(t *testing.T) {
	results, err := NewMockRetriever().Search(context.Background(), "Agentic RAG", 1, nil)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 1 || results[0].DocumentID != "doc-agentic-rag" {
		t.Fatalf("results = %+v", results)
	}
	missing, err := NewMockRetriever().Search(context.Background(), "量子纠缠", 10, nil)
	if err != nil || len(missing) != 0 {
		t.Fatalf("no-match results = %+v, %v", missing, err)
	}
}

func TestMockRetrieverFilterTypeMismatchExcludes(t *testing.T) {
	results, err := NewMockRetriever().Search(context.Background(), "RRF", 10,
		map[string]any{"document_id": float64(1)})
	if err != nil || len(results) != 0 {
		t.Fatalf("type-mismatched filter results = %+v, %v", results, err)
	}
}

func TestMockRerankerSortsDeterministically(t *testing.T) {
	candidates := []contracts.Candidate{
		{DocumentID: "b", ChunkID: "chunk-b", Score: 0.5},
		{DocumentID: "a", ChunkID: "chunk-a", Score: 0.5},
		{DocumentID: "c", ChunkID: "chunk-c", Score: 0.9},
	}
	ordered, err := NewMockReranker("cross_encoder").Rerank(context.Background(), "ignored", candidates)
	if err != nil {
		t.Fatalf("Rerank() error: %v", err)
	}
	want := []string{"c", "a", "b"}
	got := make([]string, len(ordered))
	for index, candidate := range ordered {
		got[index] = candidate.DocumentID
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	// The input slice must not be mutated.
	if candidates[0].DocumentID != "b" {
		t.Errorf("input slice was mutated: %+v", candidates)
	}
}
