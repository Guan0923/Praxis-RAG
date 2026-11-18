package retrieval

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/sparse"
)

// KnowledgeChunk is one entry of the deterministic Phase-1 knowledge fixture.
type KnowledgeChunk struct {
	DocumentID string
	ChunkID    string
	Title      string
	Source     string
	Text       string
	Metadata   map[string]any
}

// DefaultKnowledge mirrors rag_mcp.knowledge.DEFAULT_KNOWLEDGE exactly.
var DefaultKnowledge = []KnowledgeChunk{
	{
		DocumentID: "doc-rrf",
		ChunkID:    "doc-rrf:1",
		Title:      "RRF 排名融合",
		Source:     "knowledge/rrf.md",
		Text: "RRF（Reciprocal Rank Fusion，倒数排名融合）用于合并多组检索排名。" +
			"它主要依赖文档在每组结果中的名次，而不是要求不同检索器的原始分数处于同一尺度。" +
			"Hybrid RAG 可以使用 RRF 融合 BM25 稀疏检索和 Dense 向量检索。",
		Metadata: map[string]any{"topic": "retrieval", "phase": "mock"},
	},
	{
		DocumentID: "doc-agentic-rag",
		ChunkID:    "doc-agentic-rag:1",
		Title:      "Agentic RAG 职责边界",
		Source:     "knowledge/agentic-rag.md",
		Text: "Agentic RAG 中，Agent 决定是否检索、如何改写查询以及是否再次检索。" +
			"RAG 服务只负责执行一次检索动作，包括召回候选、重排和返回证据。" +
			"检索次数属于 Agent Runtime 的循环，不应该成为 search_times 参数。",
		Metadata: map[string]any{"topic": "agent", "phase": "mock"},
	},
	{
		DocumentID: "doc-qdrant",
		ChunkID:    "doc-qdrant:1",
		Title:      "Qdrant Hybrid Collection",
		Source:     "knowledge/qdrant.md",
		Text: "Qdrant collection 可以为同一个 Point 保存命名 Dense Vector 和 Sparse Vector。" +
			"BGE-M3 Dense Vector 的维度是 1024；BM25 可以表示为 Sparse Vector。" +
			"在线查询时可以分别召回 dense 与 sparse 候选，再使用 RRF 进行融合。",
		Metadata: map[string]any{"topic": "storage", "phase": "mock"},
	},
}

var (
	mockLatinTerm = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_.-]*`)
	mockCJKRuns   = regexp.MustCompile(`[\x{3400}-\x{9fff}]+`)
)

// MockRetriever searches a fixed knowledge fixture while preserving the final
// Retriever API. It mirrors rag_mcp.mock.MockRetriever.
type MockRetriever struct {
	chunks []KnowledgeChunk
}

// NewMockRetriever builds the deterministic Phase-1 retriever.
func NewMockRetriever() *MockRetriever {
	return &MockRetriever{chunks: DefaultKnowledge}
}

// NewMockRetrieverWithChunks builds a mock retriever over a custom fixture.
func NewMockRetrieverWithChunks(chunks []KnowledgeChunk) *MockRetriever {
	return &MockRetriever{chunks: chunks}
}

// Search scores the fixture chunks by shared terms and returns the top limit.
func (r *MockRetriever) Search(
	_ context.Context,
	query string,
	limit int,
	filters map[string]any,
) ([]contracts.Candidate, error) {
	queryTerms := mockTerms(query)
	candidates := make([]contracts.Candidate, 0, len(r.chunks))
	for _, chunk := range r.chunks {
		if !mockMatchesFilters(chunk, filters) {
			continue
		}
		titleMatches := intersectTerms(queryTerms, mockTerms(chunk.Title))
		bodyMatches := intersectTerms(queryTerms, mockTerms(chunk.Text))
		matched := unionTerms(titleMatches, bodyMatches)
		if len(matched) == 0 {
			continue
		}
		denominator := len(queryTerms)
		if denominator < 1 {
			denominator = 1
		}
		score := (float64(len(bodyMatches)) + 1.5*float64(len(titleMatches))) / float64(denominator)
		candidates = append(candidates, contracts.Candidate{
			DocumentID:   chunk.DocumentID,
			ChunkID:      chunk.ChunkID,
			Title:        chunk.Title,
			Source:       chunk.Source,
			Text:         chunk.Text,
			Score:        score,
			Metadata:     copyMap(chunk.Metadata),
			MatchedTerms: sortedTerms(matched),
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if candidates[i].DocumentID != candidates[j].DocumentID {
			return candidates[i].DocumentID < candidates[j].DocumentID
		}
		return candidates[i].ChunkID < candidates[j].ChunkID
	})
	if limit >= 0 && limit < len(candidates) {
		return candidates[:limit], nil
	}
	return candidates, nil
}

// MockReranker keeps deterministic ordering while exercising the reranker seam.
type MockReranker struct {
	Name string
}

// NewMockReranker builds a named deterministic reranker.
func NewMockReranker(name string) *MockReranker { return &MockReranker{Name: name} }

// Rerank sorts candidates by descending score, then document and chunk id.
func (r *MockReranker) Rerank(
	_ context.Context,
	_ string,
	candidates []contracts.Candidate,
) ([]contracts.Candidate, error) {
	ordered := make([]contracts.Candidate, len(candidates))
	copy(ordered, candidates)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Score != ordered[j].Score {
			return ordered[i].Score > ordered[j].Score
		}
		if ordered[i].DocumentID != ordered[j].DocumentID {
			return ordered[i].DocumentID < ordered[j].DocumentID
		}
		return ordered[i].ChunkID < ordered[j].ChunkID
	})
	return ordered, nil
}

// mockTerms extracts folded Latin terms and CJK unigrams/bigrams.
func mockTerms(value string) map[string]struct{} {
	normalized := sparse.CaseFold(value)
	terms := map[string]struct{}{}
	for _, match := range mockLatinTerm.FindAllString(normalized, -1) {
		terms[match] = struct{}{}
	}
	for _, run := range mockCJKRuns.FindAllString(normalized, -1) {
		characters := []rune(run)
		if len(characters) == 1 {
			terms[string(characters[0])] = struct{}{}
			continue
		}
		for index := 0; index+1 < len(characters); index++ {
			terms[string(characters[index:index+2])] = struct{}{}
		}
	}
	return terms
}

func intersectTerms(left, right map[string]struct{}) map[string]struct{} {
	shared := map[string]struct{}{}
	for term := range left {
		if _, ok := right[term]; ok {
			shared[term] = struct{}{}
		}
	}
	return shared
}

func unionTerms(left, right map[string]struct{}) map[string]struct{} {
	union := make(map[string]struct{}, len(left)+len(right))
	for term := range left {
		union[term] = struct{}{}
	}
	for term := range right {
		union[term] = struct{}{}
	}
	return union
}

func sortedTerms(terms map[string]struct{}) []string {
	sorted := make([]string, 0, len(terms))
	for term := range terms {
		sorted = append(sorted, term)
	}
	sort.Strings(sorted)
	return sorted
}

// mockMatchesFilters compares filter values with Python equality semantics.
func mockMatchesFilters(chunk KnowledgeChunk, filters map[string]any) bool {
	values := map[string]any{
		"document_id": chunk.DocumentID,
		"source":      chunk.Source,
		"title":       chunk.Title,
	}
	for key, want := range filters {
		if !pythonEqual(values[key], want) {
			return false
		}
	}
	return true
}

func pythonEqual(left, right any) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	switch typed := left.(type) {
	case string:
		other, ok := right.(string)
		return ok && typed == other
	case bool:
		other, ok := right.(bool)
		return ok && typed == other
	case float64:
		return numericEqual(typed, right)
	case json.Number:
		parsed, err := typed.Float64()
		return err == nil && numericEqual(parsed, right)
	}
	return false
}

func numericEqual(left float64, right any) bool {
	switch typed := right.(type) {
	case float64:
		return left == typed
	case json.Number:
		parsed, err := typed.Float64()
		return err == nil && left == parsed
	case int:
		return left == float64(typed)
	case int64:
		return left == float64(typed)
	}
	return false
}
