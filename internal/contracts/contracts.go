// Package contracts defines the provider-neutral RAG action contract shared by
// the HTTP, MCP, and orchestration layers. It is a direct port of
// rag_mcp/contracts.py plus the validation helpers from rag_mcp/server.py.
package contracts

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// RetrievalMode selects the candidate generator.
type RetrievalMode string

const (
	RetrievalBM25      RetrievalMode = "bm25"
	RetrievalEmbedding RetrievalMode = "embedding"
	RetrievalHybrid    RetrievalMode = "hybrid"
	// RetrievalSAG2 is the query-time lexical-association mode (BM25 initial
	// candidates + JEV necessary-evidence decisions + term expansion + DPP).
	// It requires a configured JEV decision model and pairs with rerank=none:
	// the JEV evidence score is the final ordering.
	RetrievalSAG2 RetrievalMode = "sag2"
)

// RerankMode selects the second-stage relevance scorer.
type RerankMode string

const (
	RerankNone         RerankMode = "none"
	RerankCrossEncoder RerankMode = "cross_encoder"
)

// AllowedFilters is the closed set of payload filters exposed to Agents.
var AllowedFilters = map[string]struct{}{
	"document_id": {},
	"source":      {},
	"title":       {},
}

// MaxArtifactLength mirrors server.py constants.
const MaxArtifactLength = 1024 * 1024

// SearchValidationError marks untrusted MCP/HTTP arguments that violate the
// search contract.
type SearchValidationError struct{ Message string }

func (e *SearchValidationError) Error() string { return e.Message }

// ErrorCategory implements the optional interface internal/logging uses to
// classify a failure without importing this package. Untrusted arguments fail
// as validation errors, the same category HTTP/MCP input checks use.
func (e *SearchValidationError) ErrorCategory() string { return "validation_error" }

func validationError(format string, args ...any) *SearchValidationError {
	return &SearchValidationError{Message: fmt.Sprintf(format, args...)}
}

// RagOperationError marks a configured retrieval component that cannot
// complete a request.
type RagOperationError struct{ Message string }

func (e *RagOperationError) Error() string { return e.Message }

// ErrorCategory implements the optional interface internal/logging uses to
// classify a failure without importing this package.
func (e *RagOperationError) ErrorCategory() string { return "rag_operation_error" }

// NewRagOperationError builds a RagOperationError with a fixed message.
func NewRagOperationError(message string) *RagOperationError {
	return &RagOperationError{Message: message}
}

// Candidate is one retrievable evidence chunk before final response projection.
type Candidate struct {
	DocumentID   string
	ChunkID      string
	Title        string
	Source       string
	Text         string
	Score        float64
	Metadata     map[string]any
	MatchedTerms []string
}

// ToResult returns the bounded public representation sent back to the Agent.
// score is rounded to six decimal places like Python's round(score, 6).
func (c Candidate) ToResult(rank int) map[string]any {
	metadata := c.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	return map[string]any{
		"rank":        rank,
		"document_id": c.DocumentID,
		"chunk_id":    c.ChunkID,
		"title":       c.Title,
		"source":      c.Source,
		"text":        c.Text,
		"score":       round6(c.Score),
		"metadata":    metadata,
	}
}

// round6 rounds a float to six decimal places using strconv, which performs
// correctly-rounded decimal conversion (matching Python's round() for the
// values produced by score normalization).
func round6(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return value
	}
	text := strconv.FormatFloat(value, 'f', 6, 64)
	rounded, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return value
	}
	return rounded
}

// SearchRequest is validated input for exactly one retrieval action.
type SearchRequest struct {
	Query      string
	Retrieval  RetrievalMode
	Rerank     RerankMode
	CandidateK int
	TopK       int
	Filters    map[string]any
	// DocumentIDs is a server-side visibility restriction. It is never parsed
	// from HTTP/MCP arguments: the transport sets it from the caller's access
	// scope so a knowledge-base grantee only retrieves documents they may read.
	// A nil slice means unrestricted; a non-nil empty slice means "no document
	// is visible" and therefore returns no evidence.
	DocumentIDs []string
}

// SearchRequestFromArguments validates MCP input without exposing
// storage-specific parameters.
func SearchRequestFromArguments(arguments map[string]any) (SearchRequest, error) {
	allowed := map[string]struct{}{
		"query": {}, "retrieval": {}, "rerank": {}, "candidate_k": {}, "top_k": {}, "filters": {},
	}
	unknown := make([]string, 0)
	for key := range arguments {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return SearchRequest{}, validationError("Unknown search arguments: %s", strings.Join(unknown, ", "))
	}

	rawQuery, exists := arguments["query"]
	query, ok := rawQuery.(string)
	if !exists || !ok || strings.TrimSpace(query) == "" {
		return SearchRequest{}, validationError("query must be a non-empty string")
	}
	query = strings.TrimSpace(query)
	if len([]rune(query)) > 2000 {
		return SearchRequest{}, validationError("query must contain at most 2000 characters")
	}

	retrieval := RetrievalHybrid
	if raw, exists := arguments["retrieval"]; exists {
		value, ok := raw.(string)
		if !ok || (value != string(RetrievalBM25) && value != string(RetrievalEmbedding) &&
			value != string(RetrievalHybrid) && value != string(RetrievalSAG2)) {
			return SearchRequest{}, validationError("retrieval must be bm25, embedding, hybrid, or sag2")
		}
		retrieval = RetrievalMode(value)
	}

	rerank := RerankCrossEncoder
	if raw, exists := arguments["rerank"]; exists {
		value, ok := raw.(string)
		if !ok || (value != string(RerankNone) && value != string(RerankCrossEncoder)) {
			return SearchRequest{}, validationError("rerank must be none or cross_encoder")
		}
		rerank = RerankMode(value)
	}
	// The SAG2 ordering comes from the JEV necessary-evidence decisions; a
	// cross-encoder pass would silently replace it and double the model cost.
	if retrieval == RetrievalSAG2 && rerank != RerankNone {
		return SearchRequest{}, validationError("retrieval sag2 requires rerank none")
	}

	candidateK, err := boundedInteger(arguments["candidate_k"], 10, "candidate_k", 1, 100)
	if err != nil {
		return SearchRequest{}, err
	}
	topK, err := boundedInteger(arguments["top_k"], 5, "top_k", 1, 10)
	if err != nil {
		return SearchRequest{}, err
	}
	if topK > candidateK {
		return SearchRequest{}, validationError("top_k must not be greater than candidate_k")
	}

	filters := map[string]any{}
	if raw, exists := arguments["filters"]; exists {
		parsed, err := parseFilters(raw)
		if err != nil {
			return SearchRequest{}, err
		}
		filters = parsed
	}

	return SearchRequest{
		Query:      query,
		Retrieval:  retrieval,
		Rerank:     rerank,
		CandidateK: candidateK,
		TopK:       topK,
		Filters:    filters,
	}, nil
}

func boundedInteger(value any, fallback int, name string, minimum, maximum int) (int, error) {
	if value == nil {
		return fallback, nil
	}
	parsed, ok := integerValue(value)
	if !ok || parsed < minimum || parsed > maximum {
		return 0, validationError("%s must be an integer between %d and %d", name, minimum, maximum)
	}
	return parsed, nil
}

// integerValue accepts Go integers and integer json.Number tokens while
// rejecting floats and booleans, matching the Python type checks.
func integerValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		text := string(typed)
		if strings.ContainsAny(text, ".eE") {
			return 0, false
		}
		parsed, err := strconv.Atoi(text)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func parseFilters(value any) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, validationError("filters must be an object")
	}
	unknown := make([]string, 0)
	for key := range object {
		if _, allowed := AllowedFilters[key]; !allowed {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, validationError("Unsupported filters: %s", strings.Join(unknown, ", "))
	}
	result := make(map[string]any, len(object))
	for key, item := range object {
		switch item.(type) {
		case string, bool, int, int64, float64, json.Number:
			result[key] = item
		default:
			return nil, validationError("filters.%s must be a string, number, or boolean", key)
		}
	}
	return result, nil
}

// ValidateArtifactRange mirrors server.validate_artifact_range.
func ValidateArtifactRange(offset, length int) error {
	if offset < 0 {
		return validationError("offset must be a non-negative integer")
	}
	if length < 1 || length > MaxArtifactLength {
		return validationError("length must be between 1 and 1048576 bytes")
	}
	return nil
}

// ValidateUUID accepts only canonical lowercase dashed UUID strings.
func ValidateUUID(value any) (string, error) {
	text, ok := value.(string)
	if !ok || len(text) != 36 {
		return "", validationError("Expected a dashed UUID string")
	}
	for index, char := range text {
		switch index {
		case 8, 13, 18, 23:
			if char != '-' {
				return "", validationError("Expected a dashed UUID string")
			}
		default:
			if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
				return "", validationError("Expected a dashed UUID string")
			}
		}
	}
	return text, nil
}

// SearchToolDescription is the stable Agent-facing search description.
const SearchToolDescription = "Search user-uploaded/local knowledge for manuals, internal facts, exact identifiers, and evidence " +
	"not already established in the conversation. Prefer hybrid + cross_encoder initially; use bm25 " +
	"for exact codes/names and embedding for semantic paraphrases. When the knowledge base has a " +
	"configured JEV decision model, retrieval sag2 chains multi-hop lexical evidence with " +
	"necessary-evidence decisions (requires rerank none). Cite returned source and chunk_id. " +
	"If evidence_status is no_evidence, rewrite the query once when useful, otherwise say the knowledge " +
	"base is insufficient. Do not use this tool for greetings or unrelated general tasks."

// ToolAnnotations describes MCP tool safety hints.
type ToolAnnotations struct {
	ReadOnlyHint    bool
	DestructiveHint bool
	IdempotentHint  bool
	OpenWorldHint   bool
}

// SearchToolAnnotations mirrors search_tool_definition annotations.
var SearchToolAnnotations = ToolAnnotations{
	ReadOnlyHint:    true,
	DestructiveHint: false,
	IdempotentHint:  true,
	OpenWorldHint:   false,
}

// SearchToolSchema returns the stable JSON schema exposed to MCP clients.
// Search always executes synchronously: one call runs retrieval and reranking
// and returns the evidence payload directly.
func SearchToolSchema() map[string]any {
	properties := map[string]any{
		"query": map[string]any{"type": "string", "minLength": 1, "maxLength": 2000},
		"retrieval": map[string]any{
			"type": "string", "enum": []string{"bm25", "embedding", "hybrid", "sag2"}, "default": "hybrid",
		},
		"rerank": map[string]any{
			"type": "string", "enum": []string{"none", "cross_encoder"}, "default": "cross_encoder",
		},
		"candidate_k": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 10},
		"top_k":       map[string]any{"type": "integer", "minimum": 1, "maximum": 10, "default": 5},
		"filters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"document_id": map[string]any{"type": []string{"string", "number", "boolean"}},
				"source":      map[string]any{"type": []string{"string", "number", "boolean"}},
				"title":       map[string]any{"type": []string{"string", "number", "boolean"}},
			},
			"additionalProperties": false,
			"default":              map[string]any{},
		},
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             []string{"query"},
		"additionalProperties": false,
	}
}
