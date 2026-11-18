package retrieval

// This file implements the SAG retriever: the query-time half of the SAG
// architecture (Zleap-AI/SAG, "SQL-Retrieval Augmented Generation with
// Query-Time Dynamic Hyperedges"). The offline half (one event + normalized
// entities per chunk) is persisted by the runtime's extraction stage into
// MySQL (internal/state/sag.go); this retriever consumes it:
//
//  1. Seed recall: the query's dense+sparse signals recall chunk candidates
//     from Qdrant (the existing HybridSearch path), then the SAG index maps
//     those chunks to their events and entities.
//  2. Dynamic hyperedge: SQL joins expand the seed entities into the events
//     that share them (EventsByEntities), instantiating only this query's
//     hyperedges — never a global graph traversal.
//  3. Selection: the strongest event candidates and the direct chunk
//     candidates are deduplicated and returned as the original evidence
//     chunks (the output boundary stays the source chunk).
//
// Divergences from the SAG reference (zleap-sag full_expand), deliberate:
//   - Seed recall reuses the Qdrant hybrid search instead of separate event
//     and entity vector indexes; MySQL is the only source of truth for the
//     event-entity data, so no second vector store is populated.
//   - The LLM reranking/jev stage of the reference's "Precise" mode is not
//     run; the mode orders by seed recall score fused with the hyperedge
//     expansion evidence (shared-entity count). JEV reranking stays available
//     through the existing sag2 mode.
//   - Entity alias merging beyond casefold normalization is not attempted
//     (SAG itself does no global entity resolution).
import (
	"context"
	"fmt"
	"sort"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/sparse"
)

// SagIndex is the read-only port this retriever needs from the persisted
// event-entity index. The runtime implements it over state.SagRepository.
type SagIndex interface {
	// EventsByChunkIDs maps chunk ids to their events.
	EventsByChunkIDs(ctx context.Context, namespace string, chunkIDs []string) (map[string]SagEvent, error)
	// EntitiesByEvents returns the entities of the given event ids.
	EntitiesByEvents(ctx context.Context, namespace string, eventIDs []string) ([]SagEntity, error)
	// EventsByEntities expands entity ids into their associated events (the
	// dynamic hyperedge join).
	EventsByEntities(ctx context.Context, namespace string, entityIDs []string, limit int) ([]SagEvent, error)
}

// SagEvent mirrors state.SagEvent for the retrieval layer (decoupled so the
// retriever tests need no MySQL).
type SagEvent struct {
	ID         string
	Namespace  string
	DocumentID string
	ChunkID     string
	Event      string
	Version    string
}

// SagEntity mirrors state.SagEntity.
type SagEntity struct {
	ID            string
	Namespace     string
	NormalizedKey string
	EntityKey     string
	DisplayName   string
}

// SagOptions tunes the retrieval rounds. Zero values fall back to the
// defaults (8 seed entities / 32 hyperedge events per entity / 0.5 seed
// weight).
type SagOptions struct {
	SeedEntities     int
	HyperedgeLimit   int
	SeedWeight       float64
	HyperedgeWeight  float64
}

// SagRetriever implements Retriever with the SAG event-entity flow. It holds
// no mutable state: every search resolves the corpus from the index ports.
type SagRetriever struct {
	namespace string
	index     SagIndex
	embedder  Embedder
	encoder   *sparse.Encoder
	store     SagStore
	options   SagOptions
}

// SagStore is the vector-store slice the SAG retriever needs: hybrid chunk
// recall plus evidence-text hydration for hyperedge candidates that the
// recall did not return. The runtime implements it over *qdrant.Store.
type SagStore interface {
	HybridSearch(ctx context.Context, dense []float64, sparseVector sparse.SparseEmbedding, limit int, filters map[string]any) ([]SagPoint, error)
	HydrateChunks(ctx context.Context, chunkIDs []string) ([]SagPoint, error)
}

// SagPoint is one Qdrant point projected for the seed recall step.
type SagPoint struct {
	DocumentID string
	ChunkID    string
	Title      string
	Source     string
	Text       string
	Score      float64
	Metadata   map[string]any
}

var _ Retriever = (*SagRetriever)(nil)

// NewSagRetriever wires the retriever. The store parameter implements the
// hybrid recall and hydration ports (the runtime's sagStoreAdapter).
func NewSagRetriever(
	namespace string,
	index SagIndex,
	embedder Embedder,
	encoder *sparse.Encoder,
	store SagStore,
	options SagOptions,
) *SagRetriever {
	return &SagRetriever{
		namespace: namespace,
		index:     index,
		embedder:  embedder,
		encoder:   encoder,
		store:     store,
		options:   normalizeSagOptions(options),
	}
}

func normalizeSagOptions(options SagOptions) SagOptions {
	if options.SeedEntities <= 0 {
		options.SeedEntities = 8
	}
	if options.HyperedgeLimit <= 0 {
		options.HyperedgeLimit = 32
	}
	if options.SeedWeight <= 0 {
		options.SeedWeight = 0.5
	}
	if options.HyperedgeWeight <= 0 {
		options.HyperedgeWeight = 0.5
	}
	return options
}

// Search runs the SAG retrieval procedure:
//
//  1. hybrid seed recall over the chunk points;
//  2. chunk → event → entity mapping through the SAG index;
//  3. entity → event expansion (the dynamic hyperedge);
//  4. fusion of direct candidates and hyperedge candidates, deduplicated by
//     chunk id and bounded by limit.
func (r *SagRetriever) Search(
	ctx context.Context,
	query string,
	limit int,
	filters map[string]any,
) ([]contracts.Candidate, error) {
	if limit <= 0 {
		return []contracts.Candidate{}, nil
	}
	if r == nil || r.index == nil {
		return nil, contracts.NewRagOperationError("SAG 检索失败：事件-实体索引不可用")
	}
	dense, err := r.embedder.EmbedQuery(ctx, query)
	if err != nil {
		return nil, sagRetrievalFailure("向量检索失败", err)
	}
	var sparseVector sparse.SparseEmbedding
	if r.encoder != nil {
		encoded, err := r.encoder.EncodeQuery(query)
		if err != nil {
			return nil, sagRetrievalFailure("词法检索失败", err)
		}
		sparseVector = encoded
	}
	points, err := r.store.HybridSearch(ctx, dense, sparseVector, limit*2, filters)
	if err != nil {
		return nil, sagRetrievalFailure("混合检索失败", err)
	}
	if len(points) == 0 {
		return []contracts.Candidate{}, nil
	}
	chunkIDs := make([]string, 0, len(points))
	direct := make(map[string]SagPoint, len(points))
	for _, point := range points {
		if point.ChunkID == "" {
			continue
		}
		if _, duplicate := direct[point.ChunkID]; duplicate {
			continue
		}
		direct[point.ChunkID] = point
		chunkIDs = append(chunkIDs, point.ChunkID)
	}
	// Seed events: only chunks whose extraction produced an event participate
	// in the hyperedge expansion.
	events, err := r.index.EventsByChunkIDs(ctx, r.namespace, chunkIDs)
	if err != nil {
		return nil, sagRetrievalFailure("事件索引读取失败", err)
	}
	if len(events) == 0 {
		// No SAG index for these chunks: the direct chunk candidates are the
		// whole answer (the mode degrades to hybrid recall, never to silence).
		return r.projectDirect(points, limit), nil
	}
	seedEventIDs := make([]string, 0, len(events))
	for _, event := range events {
		seedEventIDs = append(seedEventIDs, event.ID)
	}
	entities, err := r.index.EntitiesByEvents(ctx, r.namespace, seedEventIDs)
	if err != nil {
		return nil, sagRetrievalFailure("实体索引读取失败", err)
	}
	sort.Slice(entities, func(i, j int) bool {
		return entities[i].NormalizedKey < entities[j].NormalizedKey
	})
	if len(entities) > r.options.SeedEntities {
		entities = entities[:r.options.SeedEntities]
	}
	// Dynamic hyperedge: expand the seed entities into associated events.
	entityIDs := make([]string, 0, len(entities))
	for _, entity := range entities {
		entityIDs = append(entityIDs, entity.ID)
	}
	expanded, err := r.index.EventsByEntities(ctx, r.namespace, entityIDs, r.options.HyperedgeLimit)
	if err != nil {
		return nil, sagRetrievalFailure("超边扩展失败", err)
	}
	// Fusion: direct candidates carry the seed weight; hyperedge events carry
	// the hyperedge weight plus the number of shared entities they were
	// reached through.
	fused := make(map[string]*sagScoredCandidate, len(direct)+len(expanded))
	for _, point := range points {
		if point.ChunkID == "" {
			continue
		}
		if _, duplicate := fused[point.ChunkID]; duplicate {
			continue
		}
		fused[point.ChunkID] = &sagScoredCandidate{
			documentID: point.DocumentID,
			chunkID:    point.ChunkID,
			title:      point.Title,
			source:     point.Source,
			text:       point.Text,
			metadata:   point.Metadata,
			score:      point.Score * r.options.SeedWeight,
			eventHit:   events[point.ChunkID].Event,
		}
	}
	sharedEntityCounts := make(map[string]int, len(expanded))
	for _, event := range expanded {
		sharedEntityCounts[event.ChunkID]++
	}
	for _, event := range expanded {
		candidate, ok := fused[event.ChunkID]
		if !ok {
			candidate = &sagScoredCandidate{
				documentID: event.DocumentID,
				chunkID:    event.ChunkID,
				eventHit:   event.Event,
			}
			fused[event.ChunkID] = candidate
		}
		candidate.hyperedgeHits++
	}
	for chunkID := range sharedEntityCounts {
		if candidate, ok := fused[chunkID]; ok {
			candidate.score += float64(sharedEntityCounts[chunkID]) * r.options.HyperedgeWeight
		}
	}
	// Hyperedge-only chunks were not returned by the vector recall, so they
	// carry no evidence text yet. Hydrate them from the store: the output
	// boundary of this mode is the original chunk, so a candidate that cannot
	// be hydrated is dropped rather than returned textless.
	missing := make([]string, 0, len(fused))
	for _, candidate := range fused {
		if candidate.text == "" {
			missing = append(missing, candidate.chunkID)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		hydrated, err := r.store.HydrateChunks(ctx, missing)
		if err != nil {
			return nil, sagRetrievalFailure("证据补水失败", err)
		}
		for _, point := range hydrated {
			if candidate, ok := fused[point.ChunkID]; ok {
				candidate.documentID = point.DocumentID
				candidate.title = point.Title
				candidate.source = point.Source
				candidate.text = point.Text
				candidate.metadata = point.Metadata
			}
		}
		for _, chunkID := range missing {
			if candidate, ok := fused[chunkID]; ok && candidate.text == "" {
				delete(fused, chunkID)
			}
		}
	}
	ranked := make([]*sagScoredCandidate, 0, len(fused))
	for _, candidate := range fused {
		ranked = append(ranked, candidate)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if ranked[i].hyperedgeHits != ranked[j].hyperedgeHits {
			return ranked[i].hyperedgeHits > ranked[j].hyperedgeHits
		}
		return ranked[i].chunkID < ranked[j].chunkID
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	results := make([]contracts.Candidate, 0, len(ranked))
	for _, candidate := range ranked {
		metadata := candidate.metadata
		if metadata == nil {
			metadata = map[string]any{}
		}
		if candidate.eventHit != "" {
			metadata["sag_event"] = candidate.eventHit
		}
		results = append(results, contracts.Candidate{
			DocumentID: candidate.documentID,
			ChunkID:    candidate.chunkID,
			Title:      candidate.title,
			Source:     candidate.source,
			Text:       candidate.text,
			Score:      candidate.score,
			Metadata:   metadata,
		})
	}
	return results, nil
}

// projectDirect converts recall points to candidates when no SAG index exists
// (extraction disabled or not yet run).
func (r *SagRetriever) projectDirect(points []SagPoint, limit int) []contracts.Candidate {
	results := make([]contracts.Candidate, 0, limit)
	for _, point := range points {
		if len(results) == limit {
			break
		}
		metadata := point.Metadata
		if metadata == nil {
			metadata = map[string]any{}
		}
		results = append(results, contracts.Candidate{
			DocumentID: point.DocumentID,
			ChunkID:    point.ChunkID,
			Title:      point.Title,
			Source:     point.Source,
			Text:       point.Text,
			Score:      point.Score,
			Metadata:   metadata,
		})
	}
	return results
}

// sagScoredCandidate is one fused candidate carried to the final ranking.
type sagScoredCandidate struct {
	documentID    string
	chunkID       string
	title         string
	source        string
	text          string
	metadata      map[string]any
	score         float64
	eventHit      string
	hyperedgeHits int
}

func sagRetrievalFailure(stage string, err error) error {
	return contracts.NewRagOperationError(fmt.Sprintf("SAG 检索失败：%s（%s）", stage, modelapi.SafeErrorName(err)))
}
