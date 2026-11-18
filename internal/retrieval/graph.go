// Package retrieval: the LightRAG knowledge-graph retriever.
//
// GraphRetriever adapts the python-workers/lightrag-service sidecar to the
// Retriever interface. The sidecar owns graph construction and hybrid graph
// retrieval; this adapter only translates its evidence contexts into the
// Praxis candidate contract so the orchestrator, reranker, visibility filter
// and artifact projection keep working unchanged.
//
// It never asks the sidecar for a generated answer: the search contract of
// this service is evidence only.
package retrieval

import (
	"context"
	"fmt"
	"strings"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/lightrag"
)

// Graph query modes understood by the LightRAG sidecar. "mix" merges local,
// global and chunk retrieval and is the sidecar default when Mode is empty.
const (
	GraphModeLocal  = "local"
	GraphModeGlobal = "global"
	GraphModeHybrid = "hybrid"
	GraphModeNaive  = "naive"
	GraphModeMix    = "mix"
)

// DefaultGraphTopK bounds one graph query when the caller asks for no limit.
const DefaultGraphTopK = 10

// GraphOptions configures one namespace-bound graph retriever.
type GraphOptions struct {
	// Mode is the sidecar query mode: local, global, hybrid, naive or mix.
	// Empty selects GraphModeMix.
	Mode string
	// MinScore drops contexts whose sidecar score is below this value.
	MinScore float64
	// IncludeEntities asks the sidecar to return entity and relation names,
	// which land in Candidate.Metadata for console rendering.
	IncludeEntities bool
}

// GraphRetriever queries the LightRAG sidecar for one namespace. The
// namespace is bound at construction time because the runtime builds one
// orchestrator per tenant.
type GraphRetriever struct {
	client    *lightrag.Client
	namespace string
	options   GraphOptions
}

// NewGraphRetriever binds one sidecar client to one namespace.
//
// Parameters:
//   - client: the sidecar HTTP client; nil is rejected by Search so a
//     misconfigured runtime reports an operational error instead of panicking.
//   - namespace: the Praxis knowledge base the orchestrator belongs to.
//   - options: query mode and score filtering.
func NewGraphRetriever(client *lightrag.Client, namespace string, options GraphOptions) *GraphRetriever {
	return &GraphRetriever{client: client, namespace: namespace, options: options}
}

// Search runs one graph query and returns evidence candidates.
//
// Parameters:
//   - ctx: request context; cancellation propagates to the sidecar call.
//   - query: the user question.
//   - limit: maximum contexts requested; non-positive falls back to
//     DefaultGraphTopK.
//   - filters: the closed contract filter set; a document_id entry (string or
//     string slice) is forwarded to the sidecar as a document restriction.
//
// It returns the graph contexts projected onto contracts.Candidate, best
// first. A sidecar failure becomes a *contracts.RagOperationError so the
// transport layers map it to a service-unavailable response.
func (r *GraphRetriever) Search(
	ctx context.Context,
	query string,
	limit int,
	filters map[string]any,
) ([]contracts.Candidate, error) {
	if r.client == nil {
		return nil, contracts.NewRagOperationError("Graph retrieval is not configured")
	}
	topK := limit
	if topK <= 0 {
		topK = DefaultGraphTopK
	}
	mode := strings.TrimSpace(r.options.Mode)
	if mode == "" {
		mode = GraphModeMix
	}
	result, err := r.client.Query(ctx, r.namespace, lightrag.QueryRequest{
		Query:           query,
		Mode:            mode,
		TopK:            topK,
		Filters:         lightrag.QueryFilters{DocumentIDs: graphDocumentIDs(filters)},
		IncludeEntities: r.options.IncludeEntities,
	})
	if err != nil {
		return nil, contracts.NewRagOperationError(fmt.Sprintf("Graph retrieval failed: %v", err))
	}
	if result == nil || len(result.Contexts) == 0 {
		return []contracts.Candidate{}, nil
	}
	candidates := make([]contracts.Candidate, 0, len(result.Contexts))
	for _, item := range result.Contexts {
		if strings.TrimSpace(item.Text) == "" {
			continue
		}
		if item.Score < r.options.MinScore {
			continue
		}
		candidates = append(candidates, graphCandidate(item, mode))
	}
	return candidates, nil
}

// graphCandidate converts one sidecar evidence context into the shared
// candidate shape. Empty document names fall back to the document id so a
// citation always renders something stable.
func graphCandidate(item lightrag.Context, mode string) contracts.Candidate {
	name := strings.TrimSpace(item.DocumentName)
	if name == "" {
		name = item.DocumentID
	}
	metadata := map[string]any{
		"graph_mode": mode,
		"page":       item.Page,
		"heading":    item.Heading,
	}
	if len(item.Entities) > 0 {
		metadata["entities"] = item.Entities
	}
	if len(item.Relations) > 0 {
		relations := make([]map[string]any, 0, len(item.Relations))
		for _, relation := range item.Relations {
			relations = append(relations, map[string]any{
				"source":      relation.Source,
				"target":      relation.Target,
				"description": relation.Description,
			})
		}
		metadata["relations"] = relations
	}
	return contracts.Candidate{
		DocumentID: item.DocumentID,
		ChunkID:    item.ChunkID,
		Title:      name,
		Source:     name,
		Text:       item.Text,
		Score:      item.Score,
		Metadata:   metadata,
	}
}

// graphDocumentIDs extracts the contract's document_id filter in either the
// scalar or the slice form and drops blanks, so the sidecar receives a clean
// restriction list.
func graphDocumentIDs(filters map[string]any) []string {
	if len(filters) == 0 {
		return nil
	}
	switch value := filters["document_id"].(type) {
	case string:
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return []string{trimmed}
		}
	case []string:
		return nonBlankStrings(value)
	case []any:
		values := make([]string, 0, len(value))
		for _, raw := range value {
			if text, ok := raw.(string); ok {
				values = append(values, text)
			}
		}
		return nonBlankStrings(values)
	}
	return nil
}

// nonBlankStrings trims and drops empty entries while preserving order.
func nonBlankStrings(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

// compile-time assertion that the retriever satisfies the registry contract.
var _ Retriever = (*GraphRetriever)(nil)
