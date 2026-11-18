package modelapi

// Extraction is the offline SAG indexing client: it turns one chunk into one
// event (the chunk's complete semantic unit) plus its entity names, following
// the SAG data model (Zleap-AI/SAG: chunk → one event + multiple indexing
// entities). The wire protocol is an OpenAI-compatible chat completion with a
// JSON response format, so any 302.AI/DeepSeek/Qwen-style endpoint works.
import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// extractionPromptInstruction is the fixed extraction instruction. The event is
// one sentence carrying the chunk's complete meaning; entities are the
// distinct named things (people, orgs, products, codes, places) mentioned in
// it. No relations, no triples, no background prose.
const extractionPromptInstruction = "Read the passage and return strict JSON only: " +
	"{\"event\": string, \"entities\": [string]}. The event is ONE complete sentence that " +
	"preserves the whole meaning of the passage, written in the passage's language. Entities " +
	"are the distinct named objects in the passage (people, organizations, products, codes, " +
	"places, documents), each written exactly as it appears, no duplicates, at most 12. " +
	"No relations, no triples, no commentary. Use no outside knowledge. Output only the JSON."

// ExtractionRequest is one chunk's extraction input.
type ExtractionRequest struct {
	ChunkID string
	Text    string
	Title   string
}

// ExtractionResult is one chunk's parsed extraction.
type ExtractionResult struct {
	ChunkID  string
	Event    string
	Entities []string
}

// ExtractionClient calls one OpenAI-compatible chat endpoint for extraction.
type ExtractionClient struct {
	connection ModelConnection
	client     *ModelHTTPClient
}

// NewExtractionClient builds the extraction client on the shared model HTTP
// transport.
func NewExtractionClient(connection ModelConnection, timeout float64) *ExtractionClient {
	return &ExtractionClient{
		connection: connection,
		client:     NewModelHTTPClient(connection, timeout),
	}
}

// Connection returns the configured connection.
func (c *ExtractionClient) Connection() ModelConnection { return c.connection }

// Extract runs one extraction call for one chunk. The response must be a JSON
// object with an "event" string and an "entities" array of strings; anything
// else is a model error so the caller can treat the chunk as unextracted.
func (c *ExtractionClient) Extract(ctx context.Context, request ExtractionRequest) (ExtractionResult, error) {
	if strings.TrimSpace(request.Text) == "" {
		return ExtractionResult{}, modelError("抽取输入为空")
	}
	passage := request.Text
	if strings.TrimSpace(request.Title) != "" {
		passage = request.Title + "\n" + request.Text
	}
	payload, err := c.client.Request(ctx, "chat/completions", map[string]any{
		"model": c.connection.Model,
		"messages": []map[string]any{
			{"role": "system", "content": extractionPromptInstruction},
			{"role": "user", "content": passage},
		},
		"response_format": map[string]any{"type": "json_object"},
		"temperature":    0.0,
	})
	if err != nil {
		return ExtractionResult{}, err
	}
	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ExtractionResult{}, modelError("抽取模型返回格式无效")
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return ExtractionResult{}, modelError("抽取模型返回格式无效")
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return ExtractionResult{}, modelError("抽取模型返回格式无效")
	}
	content, ok := message["content"].(string)
	if !ok || strings.TrimSpace(content) == "" {
		return ExtractionResult{}, modelError("抽取模型返回格式无效")
	}
	var parsed struct {
		Event    string   `json:"event"`
		Entities []string `json:"entities"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &parsed); err != nil {
		return ExtractionResult{}, modelError("抽取模型返回格式无效")
	}
	event := strings.TrimSpace(parsed.Event)
	if event == "" {
		return ExtractionResult{}, modelError("抽取模型返回格式无效")
	}
	entities := make([]string, 0, len(parsed.Entities))
	seen := make(map[string]struct{}, len(parsed.Entities))
	for _, name := range parsed.Entities {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		entities = append(entities, trimmed)
		if len(entities) >= 12 {
			break
		}
	}
	return ExtractionResult{
		ChunkID:  request.ChunkID,
		Event:    event,
		Entities: entities,
	}, nil
}

// ExtractBatch runs extraction for every request sequentially. Concurrency is
// the caller's concern (the ingest path already bounds parallel model calls
// through its embedder batching pattern).
func (c *ExtractionClient) ExtractBatch(ctx context.Context, requests []ExtractionRequest) ([]ExtractionResult, error) {
	results := make([]ExtractionResult, 0, len(requests))
	for _, request := range requests {
		result, err := c.Extract(ctx, request)
		if err != nil {
			return nil, fmt.Errorf("chunk %s: %w", request.ChunkID, err)
		}
		results = append(results, result)
	}
	return results, nil
}
