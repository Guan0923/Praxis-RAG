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
		{map[string]any{"query": "x", "retrieval": "sql"}, "retrieval"},
		{map[string]any{"query": "x", "rerank": "bi_encoder"}, "rerank must be none or cross_encoder"},
		{map[string]any{"query": "x", "filters": map[string]any{"raw_qdrant": "unsafe"}}, "Unsupported filters"},
		{map[string]any{"query": "x", "unknown": true}, "Unknown search arguments"},
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

func TestQueuedSearchRequestValidation(t *testing.T) {
	request, err := QueuedSearchRequestFromArguments(map[string]any{"query": "manual"})
	if err != nil {
		t.Fatalf("QueuedSearchRequestFromArguments() error = %v", err)
	}
	if request.WaitSeconds != 10 || request.Search.Query != "manual" {
		t.Fatalf("unexpected queued request: %+v", request)
	}
	for _, wait := range []any{0, 0.03, 30, json.Number("5")} {
		if _, err := QueuedSearchRequestFromArguments(map[string]any{"query": "manual", "wait_seconds": wait}); err != nil {
			t.Fatalf("wait_seconds=%v rejected: %v", wait, err)
		}
	}
	for _, wait := range []any{-1, 31, "5", true} {
		if _, err := QueuedSearchRequestFromArguments(map[string]any{"query": "manual", "wait_seconds": wait}); err == nil {
			t.Fatalf("wait_seconds=%v accepted", wait)
		}
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

func TestValidateSearchResultArguments(t *testing.T) {
	resultID := "123e4567-e89b-12d3-a456-426614174000"
	request, err := ValidateSearchResultArguments(map[string]any{"result_id": resultID})
	if err != nil {
		t.Fatalf("ValidateSearchResultArguments() error = %v", err)
	}
	if request.Page != 1 || request.PageSize != 10 {
		t.Fatalf("unexpected request: %+v", request)
	}
	for _, arguments := range []map[string]any{
		{"result_id": resultID, "page": 0},
		{"result_id": resultID, "page_size": 101},
		{"result_id": resultID, "unexpected": 1},
	} {
		if _, err := ValidateSearchResultArguments(arguments); err == nil {
			t.Fatalf("accepted invalid arguments: %v", arguments)
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
	schema := SearchToolSchema(false)
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
	if _, exists := properties["wait_seconds"]; exists {
		t.Fatalf("non-queued schema exposed wait_seconds")
	}
	queued := SearchToolSchema(true)["properties"].(map[string]any)
	if _, exists := queued["wait_seconds"]; !exists {
		t.Fatalf("queued schema is missing wait_seconds")
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
