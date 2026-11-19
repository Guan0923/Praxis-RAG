package retrieval

// Tests for the SAG retriever: seed recall through the hybrid port, the
// chunk→event→entity mapping, the dynamic hyperedge expansion and the fusion
// ranking. The index and store are deterministic fakes; no MySQL or Qdrant is
// required.
import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/sparse"
)

// fakeSagIndex is an in-memory SagIndex.
type fakeSagIndex struct {
	eventsByChunk  map[string]SagEvent
	entitiesByEv   map[string][]SagEntity
	eventsByEntity map[string][]SagEvent
}

func (f *fakeSagIndex) EventsByChunkIDs(_ context.Context, _ string, chunkIDs []string) (map[string]SagEvent, error) {
	result := make(map[string]SagEvent, len(chunkIDs))
	for _, chunkID := range chunkIDs {
		if event, ok := f.eventsByChunk[chunkID]; ok {
			result[chunkID] = event
		}
	}
	return result, nil
}

func (f *fakeSagIndex) EntitiesByEvents(_ context.Context, _ string, eventIDs []string) ([]SagEntity, error) {
	result := make([]SagEntity, 0)
	seen := map[string]struct{}{}
	for _, eventID := range eventIDs {
		for _, entity := range f.entitiesByEv[eventID] {
			if _, duplicate := seen[entity.ID]; duplicate {
				continue
			}
			seen[entity.ID] = struct{}{}
			result = append(result, entity)
		}
	}
	return result, nil
}

func (f *fakeSagIndex) EventsByEntities(_ context.Context, _ string, entityIDs []string, _ int) ([]SagEvent, error) {
	result := make([]SagEvent, 0)
	seen := map[string]struct{}{}
	for _, entityID := range entityIDs {
		for _, event := range f.eventsByEntity[entityID] {
			if _, duplicate := seen[event.ID]; duplicate {
				continue
			}
			seen[event.ID] = struct{}{}
			result = append(result, event)
		}
	}
	return result, nil
}

// fakeSagStore returns fixed hybrid recall points and hydrates from a chunk
// map (the hyperedge-only candidates' evidence source).
type fakeSagStore struct {
	points     []SagPoint
	chunks     map[string]SagPoint
	hydrateErr error
}

func (f *fakeSagStore) HybridSearch(_ context.Context, _ []float64, _ sparse.SparseEmbedding, _ int, _ map[string]any) ([]SagPoint, error) {
	return f.points, nil
}

func (f *fakeSagStore) HydrateChunks(_ context.Context, chunkIDs []string) ([]SagPoint, error) {
	if f.hydrateErr != nil {
		return nil, f.hydrateErr
	}
	result := make([]SagPoint, 0, len(chunkIDs))
	for _, chunkID := range chunkIDs {
		if point, ok := f.chunks[chunkID]; ok {
			result = append(result, point)
		}
	}
	return result, nil
}

// fakeSagEmbedder returns a fixed dense vector.
type fakeSagEmbedder struct{}

func (fakeSagEmbedder) EmbedQuery(_ context.Context, _ string) ([]float64, error) {
	return []float64{0.1, 0.2}, nil
}

func newSagTestRetriever(index SagIndex, points []SagPoint, chunks map[string]SagPoint) *SagRetriever {
	return NewSagRetriever(
		"ns",
		index,
		fakeSagEmbedder{},
		nil,
		&fakeSagStore{points: points, chunks: chunks},
		SagOptions{},
	)
}

func TestSagRetrieverExpandsSharedEntities(t *testing.T) {
	index := &fakeSagIndex{
		eventsByChunk: map[string]SagEvent{
			// Seed chunk: recalled by hybrid search, carries an event that
			// mentions the shared entity.
			"chunk-1": {ID: "ev-1", ChunkID: "chunk-1", DocumentID: "doc-1", Event: "Project Atlas started in 2024."},
			"chunk-3": {ID: "ev-3", ChunkID: "chunk-3", DocumentID: "doc-2", Event: "Atlas released version 2."},
		},
		entitiesByEv: map[string][]SagEntity{
			"ev-1": {
				{ID: "ent-atlas", NormalizedKey: "atlas", DisplayName: "Atlas"},
			},
		},
		eventsByEntity: map[string][]SagEvent{
			// The dynamic hyperedge: the shared "atlas" entity links chunk-3's
			// event even though chunk-3 was NOT recalled by hybrid search.
			"ent-atlas": {
				{ID: "ev-3", ChunkID: "chunk-3", DocumentID: "doc-2", Event: "Atlas released version 2."},
			},
		},
	}
	points := []SagPoint{
		{DocumentID: "doc-1", ChunkID: "chunk-1", Title: "Doc One", Source: "one.md", Text: "Project Atlas started in 2024.", Score: 0.9, Metadata: map[string]any{}},
	}
	// chunk-3 is only reachable through the shared-entity hyperedge; the
	// store must still hydrate its original evidence text.
	chunks := map[string]SagPoint{
		"chunk-3": {DocumentID: "doc-2", ChunkID: "chunk-3", Title: "Doc Two", Source: "two.md", Text: "Atlas released version 2.", Metadata: map[string]any{}},
	}
	retriever := newSagTestRetriever(index, points, chunks)
	results, err := retriever.Search(context.Background(), "who released Atlas 2", 5, nil)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("Search() returned %d results, want both the direct chunk and the hyperedge chunk: %+v", len(results), results)
	}
	byChunk := map[string]contracts.Candidate{}
	for _, result := range results {
		byChunk[result.ChunkID] = result
	}
	direct, ok := byChunk["chunk-1"]
	if !ok || direct.Text == "" {
		t.Fatalf("direct chunk-1 missing or textless: %+v", direct)
	}
	hyper, ok := byChunk["chunk-3"]
	if !ok {
		t.Fatalf("hyperedge chunk-3 (not recalled by hybrid search) missing: %+v", results)
	}
	if hyper.Text == "" {
		t.Fatalf("hyperedge candidate must carry original evidence text, got empty text for chunk-3")
	}
	if hyper.Metadata["sag_event"] != "Atlas released version 2." {
		t.Fatalf("hyperedge candidate metadata.sag_event = %v", hyper.Metadata["sag_event"])
	}
}

func TestSagRetrieverDegradesWithoutIndex(t *testing.T) {
	index := &fakeSagIndex{eventsByChunk: map[string]SagEvent{}}
	points := []SagPoint{
		{DocumentID: "doc-1", ChunkID: "chunk-1", Title: "Doc One", Source: "one.md", Text: "text", Score: 0.8, Metadata: map[string]any{}},
		{DocumentID: "doc-1", ChunkID: "chunk-2", Title: "Doc One", Source: "one.md", Text: "more", Score: 0.7, Metadata: map[string]any{}},
	}
	retriever := newSagTestRetriever(index, points, nil)
	results, err := retriever.Search(context.Background(), "anything", 5, nil)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Search() = %d results, want the 2 direct candidates (degraded hybrid)", len(results))
	}
	if results[0].ChunkID != "chunk-1" || results[0].Score != 0.8 {
		t.Fatalf("degraded mode must keep recall order and scores, got %+v", results[0])
	}
}

func TestSagRetrieverRespectsLimit(t *testing.T) {
	index := &fakeSagIndex{eventsByChunk: map[string]SagEvent{}}
	points := []SagPoint{
		{DocumentID: "doc-1", ChunkID: "chunk-1", Text: "a", Score: 0.9, Metadata: map[string]any{}},
		{DocumentID: "doc-1", ChunkID: "chunk-2", Text: "b", Score: 0.8, Metadata: map[string]any{}},
		{DocumentID: "doc-1", ChunkID: "chunk-3", Text: "c", Score: 0.7, Metadata: map[string]any{}},
	}
	retriever := newSagTestRetriever(index, points, nil)
	results, err := retriever.Search(context.Background(), "q", 2, nil)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Search() = %d results, want 2 (limit)", len(results))
	}
}

func TestSagRetrieverZeroLimit(t *testing.T) {
	retriever := newSagTestRetriever(&fakeSagIndex{}, nil, nil)
	results, err := retriever.Search(context.Background(), "q", 0, nil)
	if err != nil || len(results) != 0 {
		t.Fatalf("Search(limit=0) = (%v, %v), want empty", results, err)
	}
}

// TestSagRetrieverRequiresIndex pins the misconfiguration guard: a nil index
// fails the search with the frozen unavailable-index message.
func TestSagRetrieverRequiresIndex(t *testing.T) {
	retriever := NewSagRetriever("ns", nil, fakeSagEmbedder{}, nil, &fakeSagStore{}, SagOptions{})
	_, err := retriever.Search(context.Background(), "q", 5, nil)
	if err == nil || !strings.Contains(err.Error(), "事件-实体索引不可用") {
		t.Fatalf("error = %v, want the unavailable-index message", err)
	}
	var operation *contracts.RagOperationError
	if !errors.As(err, &operation) {
		t.Fatalf("error = %v, want *contracts.RagOperationError", err)
	}
}

// TestSagRetrieverHydrationContract pins the evidence-hydration behaviour: a
// store failure fails the whole search with the frozen message, and a
// hyperedge candidate the store cannot hydrate is dropped from the output
// instead of surviving with empty evidence text.
func TestSagRetrieverHydrationContract(t *testing.T) {
	index := &fakeSagIndex{
		eventsByChunk: map[string]SagEvent{
			"chunk-1": {ID: "ev-1", ChunkID: "chunk-1", DocumentID: "doc-1", Event: "Project Atlas started in 2024."},
		},
		entitiesByEv: map[string][]SagEntity{
			"ev-1": {
				{ID: "ent-atlas", NormalizedKey: "atlas", DisplayName: "Atlas"},
			},
		},
		eventsByEntity: map[string][]SagEvent{
			// chunk-3 is only reachable through the shared-entity hyperedge,
			// so its evidence text must come from HydrateChunks.
			"ent-atlas": {
				{ID: "ev-3", ChunkID: "chunk-3", DocumentID: "doc-2", Event: "Atlas released version 2."},
			},
		},
	}
	points := []SagPoint{
		{DocumentID: "doc-1", ChunkID: "chunk-1", Title: "Doc One", Source: "one.md",
			Text: "Project Atlas started in 2024.", Score: 0.9, Metadata: map[string]any{}},
	}

	failing := &fakeSagStore{points: points, hydrateErr: errors.New("corpus scroll failed")}
	retriever := NewSagRetriever("ns", index, fakeSagEmbedder{}, nil, failing, SagOptions{})
	if _, err := retriever.Search(context.Background(), "who released Atlas 2", 5, nil); err == nil ||
		!strings.Contains(err.Error(), "证据补水失败") {
		t.Fatalf("hydrate failure error = %v, want the frozen hydration message", err)
	}

	unhydratable := &fakeSagStore{points: points, chunks: map[string]SagPoint{}}
	retriever = NewSagRetriever("ns", index, fakeSagEmbedder{}, nil, unhydratable, SagOptions{})
	results, err := retriever.Search(context.Background(), "who released Atlas 2", 5, nil)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 || results[0].ChunkID != "chunk-1" {
		t.Fatalf("Search() = %+v, want only the hydrated direct chunk (textless hyperedge dropped)", results)
	}
}
