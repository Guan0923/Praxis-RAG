package runtime

// This file wires the SAG retrieval mode (event-entity index + query-time
// dynamic hyperedges) into the service runtime. The adapter projects the
// state.SagRepository reads into the retrieval layer's SagIndex port; the
// retriever itself lives in internal/retrieval/sag.go.
import (
	"context"

	"github.com/Guan0923/Praxis-RAG/internal/qdrant"
	"github.com/Guan0923/Praxis-RAG/internal/retrieval"
	"github.com/Guan0923/Praxis-RAG/internal/sparse"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// sagIndexAdapter implements retrieval.SagIndex over the MySQL repository.
// The methods are read-only; MySQL is the source of truth for the
// event-entity data (the extraction stage wrote it during publish).
type sagIndexAdapter struct {
	repository *state.SagRepository
}

var _ retrieval.SagIndex = (*sagIndexAdapter)(nil)

func newSagIndexAdapter(repository *state.SagRepository) *sagIndexAdapter {
	return &sagIndexAdapter{repository: repository}
}

func (a *sagIndexAdapter) EventsByChunkIDs(ctx context.Context, namespace string, chunkIDs []string) (map[string]retrieval.SagEvent, error) {
	events, err := a.repository.EventsByChunkIDs(ctx, namespace, chunkIDs)
	if err != nil {
		return nil, err
	}
	result := make(map[string]retrieval.SagEvent, len(events))
	for chunkID, event := range events {
		result[chunkID] = sagEventProjection(event)
	}
	return result, nil
}

func (a *sagIndexAdapter) EntitiesByEvents(ctx context.Context, namespace string, eventIDs []string) ([]retrieval.SagEntity, error) {
	entities, err := a.repository.EntitiesByEvents(ctx, namespace, eventIDs)
	if err != nil {
		return nil, err
	}
	result := make([]retrieval.SagEntity, 0, len(entities))
	for _, entity := range entities {
		result = append(result, retrieval.SagEntity{
			ID:            entity.ID,
			Namespace:     entity.Namespace,
			NormalizedKey: entity.NormalizedKey,
			EntityKey:     entity.EntityKey,
			DisplayName:   entity.DisplayName,
		})
	}
	return result, nil
}

func (a *sagIndexAdapter) EventsByEntities(ctx context.Context, namespace string, entityIDs []string, limit int) ([]retrieval.SagEvent, error) {
	events, err := a.repository.EventsByEntities(ctx, namespace, entityIDs, limit)
	if err != nil {
		return nil, err
	}
	result := make([]retrieval.SagEvent, 0, len(events))
	for _, event := range events {
		result = append(result, sagEventProjection(event))
	}
	return result, nil
}

func sagEventProjection(event state.SagEvent) retrieval.SagEvent {
	return retrieval.SagEvent{
		ID:         event.ID,
		Namespace:  event.Namespace,
		DocumentID: event.DocumentID,
		ChunkID:    event.ChunkID,
		Event:      event.Event,
		Version:    event.Version,
	}
}

// sagStoreAdapter projects the qdrant store's hybrid search and hydration
// into the retrieval layer's point shape. The dense+sparse chunk recall is
// shared with the hybrid mode; only the fusion downstream differs.
type sagStoreAdapter struct {
	store *qdrant.Store
}

func (a *sagStoreAdapter) HybridSearch(ctx context.Context, dense []float64, sparseVector sparse.SparseEmbedding, limit int, filters map[string]any) ([]retrieval.SagPoint, error) {
	points, err := a.store.HybridSearch(ctx, dense, sparseVector, limit, filters)
	if err != nil {
		return nil, err
	}
	return sagPointProjection(points), nil
}

func (a *sagStoreAdapter) HydrateChunks(ctx context.Context, chunkIDs []string) ([]retrieval.SagPoint, error) {
	points, err := a.store.HydrateChunks(ctx, chunkIDs)
	if err != nil {
		return nil, err
	}
	return sagPointProjection(points), nil
}

func sagPointProjection(points []qdrant.Point) []retrieval.SagPoint {
	result := make([]retrieval.SagPoint, 0, len(points))
	for _, point := range points {
		payload := point.Payload
		if payload == nil {
			payload = map[string]any{}
		}
		chunkID, _ := payload["chunk_id"].(string)
		documentID, _ := payload["document_id"].(string)
		title, _ := payload["title"].(string)
		source, _ := payload["source"].(string)
		text, _ := payload["text"].(string)
		metadata := map[string]any{}
		if raw, ok := payload["metadata"].(map[string]any); ok {
			metadata = raw
		}
		result = append(result, retrieval.SagPoint{
			DocumentID: documentID,
			ChunkID:    chunkID,
			Title:      title,
			Source:     source,
			Text:       text,
			Score:      point.Score,
			Metadata:   metadata,
		})
	}
	return result
}
