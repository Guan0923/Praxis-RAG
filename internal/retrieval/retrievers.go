package retrieval

import (
	"context"
	"fmt"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/qdrant"
	"github.com/Guan0923/Praxis-RAG/internal/sparse"
)

// DenseRetriever converts a query to a vector and projects scored Qdrant
// points. It mirrors retrievers.dense.DenseRetriever.
type DenseRetriever struct {
	embedder Embedder
	store    *qdrant.Store
}

// NewDenseRetriever wires one embedder to the vector store.
func NewDenseRetriever(embedder Embedder, store *qdrant.Store) *DenseRetriever {
	return &DenseRetriever{embedder: embedder, store: store}
}

// Search returns dense candidates while preserving citation payload fields.
func (r *DenseRetriever) Search(
	ctx context.Context,
	query string,
	limit int,
	filters map[string]any,
) ([]contracts.Candidate, error) {
	vector, err := r.embedder.EmbedQuery(ctx, query)
	if err != nil {
		return nil, contracts.NewRagOperationError(fmt.Sprintf("Dense retrieval failed: %v", err))
	}
	points, err := r.store.DenseSearch(ctx, vector, limit, asMap(filters))
	if err != nil {
		return nil, contracts.NewRagOperationError(fmt.Sprintf("Dense retrieval failed: %v", err))
	}
	return ProjectCandidates(points)
}

// Bm25Retriever encodes a lexical query and searches Qdrant's sparse vector.
type Bm25Retriever struct {
	encoder *sparse.Encoder
	store   *qdrant.Store
}

// NewBm25Retriever wires the sparse encoder to the vector store.
func NewBm25Retriever(encoder *sparse.Encoder, store *qdrant.Store) *Bm25Retriever {
	return &Bm25Retriever{encoder: encoder, store: store}
}

// Search returns BM25 candidates for the query.
func (r *Bm25Retriever) Search(
	ctx context.Context,
	query string,
	limit int,
	filters map[string]any,
) ([]contracts.Candidate, error) {
	points, err := r.searchPoints(ctx, query, limit, asMap(filters))
	if err != nil {
		return nil, err
	}
	if points == nil {
		return []contracts.Candidate{}, nil
	}
	candidates, err := ProjectCandidates(points)
	if err != nil {
		return nil, err
	}
	return candidates, nil
}

func (r *Bm25Retriever) searchPoints(
	ctx context.Context,
	query string,
	limit int,
	filters map[string]any,
) ([]qdrant.Point, error) {
	vector, err := r.encoder.EncodeQuery(query)
	if err != nil {
		if isOperationError(err) {
			return nil, err
		}
		return nil, contracts.NewRagOperationError(fmt.Sprintf("BM25 retrieval failed: %v", err))
	}
	if vector.Empty() {
		return nil, nil
	}
	points, err := r.store.SparseSearch(ctx, vector, limit, filters)
	if err != nil {
		if isOperationError(err) {
			return nil, err
		}
		return nil, contracts.NewRagOperationError(fmt.Sprintf("BM25 retrieval failed: %v", err))
	}
	return points, nil
}

// HybridRetriever fuses dense and BM25 candidate ranks inside Qdrant with RRF.
type HybridRetriever struct {
	embedder Embedder
	encoder  *sparse.Encoder
	store    *qdrant.Store
}

// NewHybridRetriever wires the embedder, sparse encoder, and store.
func NewHybridRetriever(embedder Embedder, encoder *sparse.Encoder, store *qdrant.Store) *HybridRetriever {
	return &HybridRetriever{embedder: embedder, encoder: encoder, store: store}
}

// Search returns RRF-fused candidates for the query.
func (r *HybridRetriever) Search(
	ctx context.Context,
	query string,
	limit int,
	filters map[string]any,
) ([]contracts.Candidate, error) {
	dense, err := r.embedder.EmbedQuery(ctx, query)
	if err != nil {
		if isOperationError(err) {
			return nil, err
		}
		return nil, contracts.NewRagOperationError(fmt.Sprintf("Hybrid retrieval failed: %v", err))
	}
	sparseVector, err := r.encoder.EncodeQuery(query)
	if err != nil {
		if isOperationError(err) {
			return nil, err
		}
		return nil, contracts.NewRagOperationError(fmt.Sprintf("Hybrid retrieval failed: %v", err))
	}
	if sparseVector.Empty() {
		points, err := r.store.DenseSearch(ctx, dense, limit, asMap(filters))
		if err != nil {
			if isOperationError(err) {
				return nil, err
			}
			return nil, contracts.NewRagOperationError(fmt.Sprintf("Hybrid retrieval failed: %v", err))
		}
		return ProjectCandidates(points)
	}
	points, err := r.store.HybridSearch(ctx, dense, sparseVector, limit, asMap(filters))
	if err != nil {
		if isOperationError(err) {
			return nil, err
		}
		return nil, contracts.NewRagOperationError(fmt.Sprintf("Hybrid retrieval failed: %v", err))
	}
	return ProjectCandidates(points)
}

// UnavailableRetriever explains a retrieval mode that belongs to a later
// implementation phase.
type UnavailableRetriever struct {
	Message string
}

// NewUnavailableRetriever builds a retriever that always fails safely.
func NewUnavailableRetriever(message string) *UnavailableRetriever {
	return &UnavailableRetriever{Message: message}
}

// Search always returns the configured limitation as a RagOperationError.
func (r *UnavailableRetriever) Search(
	context.Context,
	string,
	int,
	map[string]any,
) ([]contracts.Candidate, error) {
	return nil, contracts.NewRagOperationError(r.Message)
}
