package contracts

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSearchRequestDefaults(t *testing.T) {
	request, err := SearchRequestFromArguments(map[string]any{"query": "RRF 怎么融合 BM25 和 Dense？"})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error = %v", err)
	}
	if request.Retrieval != RetrievalHybrid || request.Rerank != RerankCrossEncoder {
		t.Fatalf("unexpected modes: %+v", request)
	}
	if request.CandidateK != 10 || request.TopK != 5 {
		t.Fatalf("unexpected limits: %+v", request)
	}
	if len(request.Filters) != 0 {
		t.Fatalf("filters = %v", request.Filters)
	}
	if request.Query != "RRF 怎么融合 BM25 和 Dense？" {
		t.Fatalf("query = %q", request.Query)
	}
}

func TestInvalidSearchArgumentsFailAtTheBoundary(t *testing.T) {
	cases := []struct {
		arguments map[string]any
		contains  string
	}{
		{map[string]any{}, "query"},
		{map[string]any{"query": "x", "top_k": 6, "candidate_k": 5}, "top_k"},
		{map[string]any{"query": "x", "retrieval": "sql"}, "retrieval must be bm25, embedding, hybrid, sag2, or graph"},
		{map[string]any{"query": "x", "rerank": "bi_encoder"}, "rerank must be none or cross_encoder"},
		{map[string]any{"query": "x", "retrieval": "sag2"}, "retrieval sag2 requires rerank none"},
		{map[string]any{"query": "x", "retrieval": "sag2", "rerank": "cross_encoder"}, "retrieval sag2 requires rerank none"},
		{map[string]any{"query": "x", "filters": map[string]any{"raw_qdrant": "unsafe"}}, "Unsupported filters"},
		{map[string]any{"query": "x", "unknown": true}, "Unknown search arguments"},
		{map[string]any{"query": "x", "wait_seconds": 3}, "Unknown search arguments"},
		{map[string]any{"query": "x", "result_id": "123e4567-e89b-12d3-a456-426614174000"}, "Unknown search arguments"},
		{map[string]any{"query": "   "}, "query must be a non-empty string"},
		{map[string]any{"query": strings.Repeat("a", 2001)}, "at most 2000 characters"},
		{map[string]any{"query": "x", "candidate_k": true}, "candidate_k must be an integer"},
		{map[string]any{"query": "x", "candidate_k": json.Number("1.5")}, "candidate_k must be an integer"},
		{map[string]any{"query": "x", "top_k": json.Number("5.0")}, "top_k must be an integer"},
	}
	for _, testCase := range cases {
		_, err := SearchRequestFromArguments(testCase.arguments)
		if err == nil {
			t.Fatalf("SearchRequestFromArguments(%v) accepted invalid input", testCase.arguments)
		}
		if _, ok := err.(*SearchValidationError); !ok {
			t.Fatalf("error type = %T, want *SearchValidationError", err)
		}
		if !strings.Contains(err.Error(), testCase.contains) {
			t.Fatalf("error = %q, want it to contain %q", err.Error(), testCase.contains)
		}
	}
}

func TestFilterValuesMustBeScalar(t *testing.T) {
	_, err := SearchRequestFromArguments(map[string]any{
		"query":   "x",
		"filters": map[string]any{"document_id": nil},
	})
	if err == nil || !strings.Contains(err.Error(), "filters.document_id must be a string, number, or boolean") {
		t.Fatalf("unexpected error: %v", err)
	}

	request, err := SearchRequestFromArguments(map[string]any{
		"query":   "x",
		"filters": map[string]any{"document_id": "doc-qdrant", "source": json.Number("3"), "title": true},
	})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error = %v", err)
	}
	if request.Filters["document_id"] != "doc-qdrant" {
		t.Fatalf("filters = %v", request.Filters)
	}
}

func TestSAG2SearchRequestRequiresRerankNone(t *testing.T) {
	request, err := SearchRequestFromArguments(map[string]any{
		"query": "多跳问题", "retrieval": "sag2", "rerank": "none", "candidate_k": 100,
	})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error = %v", err)
	}
	if request.Retrieval != RetrievalSAG2 || request.Rerank != RerankNone {
		t.Fatalf("request = %+v", request)
	}
}

func TestGraphSearchRequestKeepsTheDefaultRerank(t *testing.T) {
	// graph has no mutual-exclusion rule with rerank: the graph score is the
	// relevance signal and a cross-encoder pass stays legal.
	request, err := SearchRequestFromArguments(map[string]any{
		"query": "谁负责 RRF 融合？", "retrieval": "graph",
	})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error = %v", err)
	}
	if request.Retrieval != RetrievalGraph || request.Rerank != RerankCrossEncoder {
		t.Fatalf("request = %+v", request)
	}
	if _, err := SearchRequestFromArguments(map[string]any{
		"query": "谁负责 RRF 融合？", "retrieval": "graph", "rerank": "none",
	}); err != nil {
		t.Fatalf("graph with rerank none error = %v", err)
	}
}

func TestExistingRetrievalModesStillValidate(t *testing.T) {
	cases := map[string]RetrievalMode{
		"bm25":      RetrievalBM25,
		"embedding": RetrievalEmbedding,
		"hybrid":    RetrievalHybrid,
		"sag2":      RetrievalSAG2,
		"graph":     RetrievalGraph,
	}
	for value, want := range cases {
		request, err := SearchRequestFromArguments(map[string]any{
			"query": "x", "retrieval": value, "rerank": "none",
		})
		if err != nil {
			t.Fatalf("retrieval %q error = %v", value, err)
		}
		if request.Retrieval != want {
			t.Fatalf("retrieval %q = %q, want %q", value, request.Retrieval, want)
		}
	}
	// The documented default does not move when a mode is added.
	defaults, err := SearchRequestFromArguments(map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error = %v", err)
	}
	if defaults.Retrieval != RetrievalHybrid || defaults.Rerank != RerankCrossEncoder {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}
}

func TestSearchToolSchemaExposesSAG2AndGraph(t *testing.T) {
	schema := SearchToolSchema()
	retrieval, ok := schema["properties"].(map[string]any)["retrieval"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", schema["properties"])
	}
	values, ok := retrieval["enum"].([]string)
	if !ok || len(values) != 5 || values[3] != "sag2" || values[4] != "graph" {
		t.Fatalf("retrieval enum = %v", retrieval["enum"])
	}
	// The graph mode is described in the schema so an Agent knows it returns
	// graph evidence instead of an answer.
	description, ok := retrieval["description"].(string)
	if !ok || !strings.Contains(description, "graph") {
		t.Fatalf("retrieval description = %v", retrieval["description"])
	}
}
func TestValidateUUID(t *testing.T) {
	valid := "123e4567-e89b-12d3-a456-426614174000"
	if got, err := ValidateUUID(valid); err != nil || got != valid {
		t.Fatalf("ValidateUUID(%q) = %q, %v", valid, got, err)
	}
	for _, value := range []any{
		"123E4567-E89B-12D3-A456-426614174000",
		"123e4567e89b12d3a456426614174000",
		"123e4567-e89b-12d3-a456-42661417400",
		"not-a-uuid",
		42,
	} {
		if _, err := ValidateUUID(value); err == nil || err.Error() != "Expected a dashed UUID string" {
			t.Fatalf("ValidateUUID(%v) error = %v", value, err)
		}
	}
}

func TestValidateArtifactRange(t *testing.T) {
	if err := ValidateArtifactRange(0, 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateArtifactRange(-1, 1); err == nil || err.Error() != "offset must be a non-negative integer" {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateArtifactRange(0, 0); err == nil || err.Error() != "length must be between 1 and 1048576 bytes" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSearchToolSchemaKeepsThePublicContract(t *testing.T) {
	schema := SearchToolSchema()
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "query" {
		t.Fatalf("required = %v", schema["required"])
	}
	properties := schema["properties"].(map[string]any)
	candidate := properties["candidate_k"].(map[string]any)
	if candidate["default"] != 10 {
		t.Fatalf("candidate_k default = %v", candidate["default"])
	}
	rerank := properties["rerank"].(map[string]any)
	allowed := rerank["enum"].([]string)
	if len(allowed) != 2 || allowed[0] != "none" || allowed[1] != "cross_encoder" {
		t.Fatalf("rerank enum = %v", allowed)
	}
	if len(properties) != 6 {
		t.Fatalf("properties = %v, want exactly query/retrieval/rerank/candidate_k/top_k/filters", properties)
	}
	for _, gone := range []string{"wait_seconds", "result_id"} {
		if _, exists := properties[gone]; exists {
			t.Fatalf("synchronous search schema exposed %q", gone)
		}
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %v, want false", schema["additionalProperties"])
	}
	if !SearchToolAnnotations.ReadOnlyHint || SearchToolAnnotations.DestructiveHint {
		t.Fatalf("unexpected annotations: %+v", SearchToolAnnotations)
	}
	if !strings.Contains(SearchToolDescription, "exact codes/names") {
		t.Fatalf("search description changed")
	}
}

func TestCandidateToResultRoundsScore(t *testing.T) {
	candidate := Candidate{
		DocumentID: "doc-rrf",
		ChunkID:    "doc-rrf:1",
		Title:      "RRF",
		Source:     "knowledge/rrf.md",
		Text:       "evidence",
		Score:      0.123456789,
	}
	result := candidate.ToResult(1)
	if result["rank"] != 1 || result["document_id"] != "doc-rrf" {
		t.Fatalf("unexpected result: %v", result)
	}
	if result["score"] != 0.123457 {
		t.Fatalf("score = %v, want 0.123457", result["score"])
	}
	if result["metadata"] == nil {
		t.Fatalf("metadata must never be nil")
	}
}
