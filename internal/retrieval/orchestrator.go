package retrieval

import (
	"context"
	"fmt"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/qdrant"
	"github.com/Guan0923/Praxis-RAG/internal/sparse"
)

// Orchestrator selects one Retriever and optional Reranker for a validated
// request and returns bounded evidence plus debug metadata.
type Orchestrator struct {
	retrievers     map[contracts.RetrievalMode]Retriever
	rerankers      map[contracts.RerankMode]Reranker
	implementation string
	limitations    []string
	warmup         func(context.Context) error
	projector      func([]contracts.Candidate) []map[string]any
}

// NewOrchestrator wires the retriever/reranker registries. A nil warmup or
// projector matches the Python optional arguments.
func NewOrchestrator(
	retrievers map[contracts.RetrievalMode]Retriever,
	rerankers map[contracts.RerankMode]Reranker,
	implementation string,
	limitations []string,
	warmup func(context.Context) error,
	projector func([]contracts.Candidate) []map[string]any,
) *Orchestrator {
	if limitations == nil {
		limitations = []string{}
	}
	return &Orchestrator{
		retrievers:     retrievers,
		rerankers:      rerankers,
		implementation: implementation,
		limitations:    limitations,
		warmup:         warmup,
		projector:      projector,
	}
}

// Warmup prepares optional heavyweight dependencies before serving requests.
func (o *Orchestrator) Warmup(ctx context.Context) error {
	if o.warmup == nil {
		return nil
	}
	return o.warmup(ctx)
}

// Search returns bounded evidence and debug metadata for one search request.
func (o *Orchestrator) Search(ctx context.Context, request contracts.SearchRequest) (map[string]any, error) {
	retriever, ok := o.retrievers[request.Retrieval]
	if !ok {
		return nil, fmt.Errorf("Unsupported retrieval mode: %s", request.Retrieval)
	}
	retrievalStarted := time.Now()
	candidates, err := retriever.Search(ctx, request.Query, request.CandidateK, asMap(request.Filters))
	if err != nil {
		return nil, err
	}
	retrievalMilliseconds := millisecondsSince(retrievalStarted)
	candidateCount := len(candidates)
	rerankMilliseconds := 0.0
	if request.Rerank != contracts.RerankNone {
		reranker, ok := o.rerankers[request.Rerank]
		if !ok {
			return nil, fmt.Errorf("Unsupported rerank mode: %s", request.Rerank)
		}
		rerankStarted := time.Now()
		candidates, err = reranker.Rerank(ctx, request.Query, candidates)
		if err != nil {
			return nil, err
		}
		rerankMilliseconds = millisecondsSince(rerankStarted)
	}
	selected := uniqueAssets(candidates, request.TopK)
	evidenceStatus := "no_evidence"
	answerGuidance := "No supporting local evidence was found; do not guess or present unsupported knowledge claims."
	if len(selected) > 0 {
		evidenceStatus = "found"
		answerGuidance = "Answer from the returned evidence and cite source plus chunk_id."
	}
	var results []map[string]any
	if o.projector != nil {
		results = o.projector(selected)
	} else {
		results = make([]map[string]any, 0, len(selected))
		for index, candidate := range selected {
			results = append(results, candidate.ToResult(index+1))
		}
	}
	return map[string]any{
		"query":           request.Query,
		"retrieval":       string(request.Retrieval),
		"rerank":          string(request.Rerank),
		"candidate_k":     request.CandidateK,
		"top_k":           request.TopK,
		"evidence_status": evidenceStatus,
		"answer_guidance": answerGuidance,
		"results":         results,
		"debug": map[string]any{
			"implementation":  o.implementation,
			"candidate_count": candidateCount,
			"returned_count":  len(results),
			"retrieval_ms":    roundScore(retrievalMilliseconds, 3),
			"rerank_ms":       roundScore(rerankMilliseconds, 3),
			"limitations":     o.limitations,
		},
	}, nil
}

func millisecondsSince(start time.Time) float64 {
	return float64(time.Since(start).Nanoseconds()) / 1e6
}

// uniqueAssets deduplicates asset-backed candidates by asset id while keeping
// chunk-backed candidates distinct by chunk id, bounded by limit.
func uniqueAssets(candidates []contracts.Candidate, limit int) []contracts.Candidate {
	selected := make([]contracts.Candidate, 0, len(candidates))
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		key := "chunk:" + candidate.ChunkID
		if assetID, ok := candidate.Metadata["asset_id"]; ok && truthy(assetID) {
			key = "asset:" + pythonString(assetID)
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		selected = append(selected, candidate)
		if len(selected) == limit {
			break
		}
	}
	return selected
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	default:
		return true
	}
}

// BuildDefaultOrchestrator reproduces the Python factory: the mock backend is
// deterministic, while the Qdrant backend needs explicit HTTP model
// connections in Go (Python loaded FlagEmbedding in-process).
func BuildDefaultOrchestrator(cfg config.RagConfig) (*Orchestrator, error) {
	if cfg.Backend == config.BackendQdrant {
		return nil, fmt.Errorf(
			"RAG_BACKEND=qdrant requires BuildQdrantOrchestrator with HTTP embedding and reranker connections",
		)
	}
	retriever := NewMockRetriever()
	return NewOrchestrator(
		map[contracts.RetrievalMode]Retriever{
			contracts.RetrievalBM25:      retriever,
			contracts.RetrievalEmbedding: retriever,
			contracts.RetrievalHybrid:    retriever,
			// The deterministic mock backend has no lexical corpus and no JEV
			// decision model, so sag2 reports its limitation instead of
			// silently returning mock evidence; sag likewise has no
			// event-entity index in the debug CLI stack.
			contracts.RetrievalSAG2: NewUnavailableRetriever(
				"retrieval sag2 requires the Qdrant backend with a configured JEV decision model",
			),
			contracts.RetrievalSAG: NewUnavailableRetriever(
				"retrieval sag requires the service runtime with the SAG event-entity index",
			),
		},
		map[contracts.RerankMode]Reranker{
			contracts.RerankCrossEncoder: NewMockReranker("cross_encoder"),
		},
		"mock-v1",
		[]string{"All retrieval modes and rerankers are deterministic Phase-1 mocks."},
		nil,
		nil,
	), nil
}

// QdrantDependencies is the Go replacement for build_qdrant_dependencies: the
// local FlagEmbedding model becomes an HTTP embedding connection.
type QdrantDependencies struct {
	Embedder *modelapi.HTTPEmbedder
	Encoder  *sparse.Encoder
	Store    *qdrant.Store
}

// BuildQdrantDependencies constructs the Dense, BM25 Sparse, and Qdrant stack.
func BuildQdrantDependencies(
	cfg config.RagConfig,
	embedding modelapi.ModelConnection,
) (QdrantDependencies, error) {
	store, err := qdrant.NewStore(qdrant.StoreConfig{
		Collection: cfg.Collection,
		URL:        cfg.QdrantURL,
		Path:       cfg.QdrantPath,
		VectorSize: cfg.VectorSize,
		Namespace:  cfg.Namespace,
	})
	if err != nil {
		return QdrantDependencies{}, err
	}
	encoder, err := sparse.NewEncoder(sparse.DefaultEncoderOptions())
	if err != nil {
		return QdrantDependencies{}, err
	}
	return QdrantDependencies{
		Embedder: modelapi.NewHTTPEmbedder(embedding, modelapi.DefaultTimeout),
		Encoder:  encoder,
		Store:    store,
	}, nil
}

// BuildQdrantOrchestrator wires the HTTP model API into the Qdrant retrieval
// stack and uses the production implementation string.
func BuildQdrantOrchestrator(
	cfg config.RagConfig,
	embedding modelapi.ModelConnection,
	reranker modelapi.ModelConnection,
) (*Orchestrator, error) {
	dependencies, err := BuildQdrantDependencies(cfg, embedding)
	if err != nil {
		return nil, err
	}
	encoder := dependencies.Encoder
	store := dependencies.Store
	dense := NewDenseRetriever(dependencies.Embedder, store)
	bm25 := NewBm25Retriever(encoder, store)
	hybrid := NewHybridRetriever(dependencies.Embedder, encoder, store)
	crossEncoder, err := NewHTTPCrossEncoderReranker(
		reranker,
		CrossEncoderOptions{
			ModelName:     cfg.RerankerModel,
			Device:        cfg.RerankerDevice,
			BatchSize:     cfg.RerankerBatchSize,
			MaxCandidates: cfg.RerankerMaxCandidates,
			MaxLength:     cfg.RerankerMaxLength,
			MaxTextChars:  cfg.RerankerMaxTextChars,
		},
		modelapi.DefaultTimeout,
	)
	if err != nil {
		return nil, err
	}
	var warmup func(context.Context) error
	if cfg.PreloadReranker {
		warmup = func(context.Context) error { return crossEncoder.Warmup() }
	}
	return NewOrchestrator(
		map[contracts.RetrievalMode]Retriever{
			contracts.RetrievalBM25:      bm25,
			contracts.RetrievalEmbedding: dense,
			contracts.RetrievalHybrid:    hybrid,
			// The standalone debug CLI stack has no state-store access to
			// resolve a JEV model profile, a lexical corpus or the SAG
			// event-entity index, so both modes stay explicitly unavailable
			// here; the service runtime registers the real implementations.
			contracts.RetrievalSAG2: NewUnavailableRetriever(
				"retrieval sag2 requires the service runtime with a configured JEV decision model",
			),
			contracts.RetrievalSAG: NewUnavailableRetriever(
				"retrieval sag requires the service runtime with the SAG event-entity index",
			),
		},
		map[contracts.RerankMode]Reranker{
			contracts.RerankCrossEncoder: crossEncoder,
		},
		"qdrant-hybrid-rerank-v1",
		nil,
		warmup,
		nil,
	), nil
}

// Repositories re-exports for callers that only need the registries.
var (
	_ Retriever = (*DenseRetriever)(nil)
	_ Retriever = (*Bm25Retriever)(nil)
	_ Retriever = (*HybridRetriever)(nil)
	_ Retriever = (*MockRetriever)(nil)
	_ Retriever = (*UnavailableRetriever)(nil)
	_ Reranker  = (*MockReranker)(nil)
	_ Reranker  = (*CrossEncoderReranker)(nil)
)
