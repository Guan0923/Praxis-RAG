// Package retrieval ports the retrievers, rerankers, deterministic mock
// backend, and orchestrator from rag_mcp. It wires the model HTTP API and the
// Qdrant REST store into the provider-neutral contracts package.
package retrieval

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/modelapi"
	"github.com/Guan0923/xinghe_rag/internal/qdrant"
)

// Retriever produces bounded evidence candidates for one retrieval mode.
type Retriever interface {
	Search(ctx context.Context, query string, limit int, filters map[string]any) ([]contracts.Candidate, error)
}

// Reranker replaces or preserves candidate scores for a second stage.
type Reranker interface {
	Rerank(ctx context.Context, query string, candidates []contracts.Candidate) ([]contracts.Candidate, error)
}

// Embedder is the single-query embedding port used by dense retrieval.
type Embedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float64, error)
}

// ProjectCandidates validates citation fields before returning storage results
// to an Agent. It mirrors retrievers.common.project_candidates.
func ProjectCandidates(points []qdrant.Point) ([]contracts.Candidate, error) {
	candidates := make([]contracts.Candidate, 0, len(points))
	for _, point := range points {
		payload := point.Payload
		if payload == nil {
			payload = map[string]any{}
		}
		metadata := map[string]any{}
		if raw, ok := payload["metadata"]; ok {
			if object, ok := raw.(map[string]any); ok {
				metadata = copyMap(object)
			}
		}
		documentID, ok := payload["document_id"]
		if !ok {
			return nil, invalidPayload("'document_id'")
		}
		chunkID, ok := payload["chunk_id"]
		if !ok {
			return nil, invalidPayload("'chunk_id'")
		}
		source, ok := payload["source"]
		if !ok {
			return nil, invalidPayload("'source'")
		}
		text, ok := payload["text"]
		if !ok {
			return nil, invalidPayload("'text'")
		}
		title := ""
		if value, exists := payload["title"]; exists {
			title = pythonString(value)
		}
		candidates = append(candidates, contracts.Candidate{
			DocumentID: pythonString(documentID),
			ChunkID:    pythonString(chunkID),
			Title:      title,
			Source:     pythonString(source),
			Text:       pythonString(text),
			Score:      point.Score,
			Metadata:   metadata,
		})
	}
	return candidates, nil
}

func invalidPayload(detail string) error {
	return contracts.NewRagOperationError("Qdrant point has an invalid evidence payload: " + detail)
}

func copyMap(values map[string]any) map[string]any {
	copied := make(map[string]any, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

// pythonString mirrors Python's str() for the payload scalars used as
// citations.
func pythonString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case string:
		return typed
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return fmt.Sprint(typed)
	}
}

// isOperationError reports whether err is an already-safe retrieval error that
// Python would re-raise unchanged: RagOperationError and its ModelAPIError
// subclass.
func isOperationError(err error) bool {
	var operationError *contracts.RagOperationError
	if errors.As(err, &operationError) {
		return true
	}
	var modelError *modelapi.ModelAPIError
	return errors.As(err, &modelError)
}

// asMap converts a filter map, tolerating nil.
func asMap(filters map[string]any) map[string]any {
	if filters == nil {
		return map[string]any{}
	}
	return filters
}
