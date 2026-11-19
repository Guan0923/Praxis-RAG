package retrieval

import (
	"context"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
)

// countingReranker records every candidate it received.
type countingReranker struct {
	seen []contracts.Candidate
}

func (r *countingReranker) Rerank(_ context.Context, _ string, candidates []contracts.Candidate) ([]contracts.Candidate, error) {
	r.seen = append(r.seen, candidates...)
	return candidates, nil
}

// TestVisibilityFiltersApplyBeforeRecall pins the enforcement point: a
// server-side DocumentIDs restriction becomes a document_id recall condition,
// so the retriever — and therefore the reranker behind it — only ever
// receives allowed candidates. A denied document never reaches rerank (or JEV
// in SAG2, which reads the same filters through RejectedKeys).
func TestVisibilityFiltersApplyBeforeRecall(t *testing.T) {
	allowed := []string{"doc-a", "doc-b"}
	merged := visibilityFilters(map[string]any{}, allowed)
	list, ok := merged["document_id"].([]string)
	if !ok || len(list) != 2 || list[0] != "doc-a" || list[1] != "doc-b" {
		t.Fatalf("document_id condition = %#v", merged["document_id"])
	}

	// Nothing visible: recall still carries the key but matches no document.
	empty := visibilityFilters(map[string]any{}, []string{})
	if _, ok := empty["document_id"]; !ok {
		t.Fatal("an empty restriction must keep the document_id key")
	}
	if list, _ := empty["document_id"].([]string); len(list) != 1 || list[0] == "doc-a" {
		t.Fatalf("empty restriction = %#v", empty["document_id"])
	}

	// An existing caller filter survives the merge.
	withFilter := visibilityFilters(map[string]any{"source": "s.pdf"}, []string{"doc-a"})
	if withFilter["source"] != "s.pdf" {
		t.Fatalf("caller filter lost: %#v", withFilter)
	}

	// End to end through the orchestrator: the mock retriever applies the
	// allow-list, the counting reranker never sees the denied document.
	retriever := NewMockRetrieverWithChunks([]KnowledgeChunk{
		{DocumentID: "doc-a", ChunkID: "c1", Title: "A", Source: "a.md", Text: "alpha mock"},
		{DocumentID: "doc-b", ChunkID: "c2", Title: "B", Source: "b.md", Text: "gamma mock"},
	})
	// deny "doc-b" by allowing only "doc-a"
	restrictor := NewOrchestrator(
		map[contracts.RetrievalMode]Retriever{contracts.RetrievalEmbedding: retriever},
		map[contracts.RerankMode]Reranker{contracts.RerankCrossEncoder: &countingReranker{}},
		"mock-v1", nil, nil, nil,
	)
	request := contracts.SearchRequest{
		Query: "mock", Retrieval: contracts.RetrievalEmbedding,
		Rerank: contracts.RerankCrossEncoder, CandidateK: 10, TopK: 10,
		DocumentIDs: []string{"doc-a"},
	}
	payload, err := restrictor.Search(context.Background(), request)
	if err != nil {
		t.Fatalf("restricted search: %v", err)
	}
	items, _ := payload["results"].([]map[string]any)
	if len(items) != 1 {
		t.Fatalf("restricted results = %#v, want exactly the allowed document", items)
	}
	if id, _ := items[0]["document_id"].(string); id != "doc-a" {
		t.Fatalf("non-allowed document leaked into results: %#v", items[0])
	}
	reranker := restrictor.rerankers[contracts.RerankCrossEncoder].(*countingReranker)
	if len(reranker.seen) == 0 {
		t.Fatal("the allowed document never reached the reranker")
	}
	for _, candidate := range reranker.seen {
		if candidate.DocumentID != "doc-a" {
			t.Fatalf("denied document reached the reranker: %+v", candidate)
		}
	}
}

// TestVisibilityFiltersSAG2LexicalIndex pins the SAG2 side: the lexical index
// rejects every passage whose document is outside the allow-list, so neither
// the initial recall nor any expansion round can surface it (and the JEV
// scorer only receives allowed passages).
func TestVisibilityFiltersSAG2LexicalIndex(t *testing.T) {
	corpus := []LexicalDoc{
		{DocumentID: "doc-a", Source: "a.md", Title: "A", Text: "alpha beta"},
		{DocumentID: "doc-b", Source: "b.md", Title: "B", Text: "gamma delta"},
	}
	index := NewLexicalIndex(corpus)
	filters := visibilityFilters(map[string]any{}, []string{"doc-a"})
	rejected := index.RejectedKeys(filters)
	if len(rejected) == 0 {
		t.Fatal("the denied document must be rejected by the lexical index")
	}
	hits := index.Search([]string{"alpha", "gamma"}, rejected, 10)
	if len(hits) == 0 {
		t.Fatal("the allowed document must still be recalled")
	}
	for _, hit := range hits {
		if hit.DocumentID != "doc-a" {
			t.Fatalf("denied document leaked through the lexical index: %+v", hit)
		}
	}
}
