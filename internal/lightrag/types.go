// Package lightrag is the Go client of the LightRAG sidecar service. The
// sidecar owns the knowledge graph of one workspace per knowledge base and
// answers retrieval requests with evidence: graph entities and relations plus
// the text chunks they came from. It never generates an answer, so the graph
// retrieval mode stays an evidence source for the Agent-facing contract.
package lightrag

// This file defines the sidecar HTTP contract: the workspace configuration and
// its status, the chunk ingest and query bodies, and the health payload. Every
// JSON tag is snake_case because the sidecar is a FastAPI service.

// ModelEndpoint is one OpenAI-compatible endpoint of a workspace
// configuration. The numeric limits are pointers so an endpoint only encodes
// the values its role owns: an embedding endpoint sends dimension, an LLM
// endpoint sends max_output_tokens and timeout_seconds, and every unset field
// is omitted instead of sending a zero the sidecar would apply.
type ModelEndpoint struct {
	// BaseURL is the OpenAI-compatible API root, for example
	// http://127.0.0.1:8000/v1.
	BaseURL string `json:"base_url"`
	// Model is the provider model id the sidecar sends to that endpoint.
	Model string `json:"model"`
	// APIKey is the bearer credential of the endpoint. It is never logged and
	// never quoted in an error message.
	APIKey string `json:"api_key"`
	// Dimension is the dense vector width of an embedding endpoint.
	Dimension *int `json:"dimension,omitempty"`
	// MaxOutputTokens bounds one completion of an LLM endpoint.
	MaxOutputTokens *int `json:"max_output_tokens,omitempty"`
	// TimeoutSeconds bounds one LLM endpoint call.
	TimeoutSeconds *int `json:"timeout_seconds,omitempty"`
}

// LLMEndpoint is the LLM half of a workspace configuration. It deliberately
// excludes the embedding dimension so a graph extraction or keyword prompt can
// never receive a value that belongs to the dense index.
type LLMEndpoint struct {
	// BaseURL is the OpenAI-compatible API root of the LLM endpoint.
	BaseURL string `json:"base_url"`
	// Model is the provider model id used for this role.
	Model string `json:"model"`
	// APIKey is the bearer credential of the endpoint. It is never logged.
	APIKey string `json:"api_key"`
	// MaxOutputTokens bounds one completion; zero omits the field so the
	// sidecar keeps its own default.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// TimeoutSeconds bounds one call; zero omits the field so the sidecar
	// keeps its own default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// WorkspaceConfig is the POST /v1/workspaces body. One workspace holds the
// chunks and the knowledge graph of one knowledge base.
type WorkspaceConfig struct {
	// Namespace is the knowledge-base namespace; the sidecar maps it to an
	// isolated workspace.
	Namespace string `json:"namespace"`
	// ConfigFingerprint is the digest of the model configuration. A changed
	// fingerprint makes the sidecar rebuild the graph instead of reusing it.
	ConfigFingerprint string `json:"config_fingerprint"`
	// Language is the extraction language hint, for example "zh" or "en".
	Language string `json:"language"`
	// Embedding is the dense index endpoint of the workspace.
	Embedding ModelEndpoint `json:"embedding"`
	// ExtractLLM is the endpoint used for entity/relation extraction.
	ExtractLLM LLMEndpoint `json:"extract_llm"`
	// KeywordLLM is the endpoint used to extract query keywords.
	KeywordLLM LLMEndpoint `json:"keyword_llm"`
}

// WorkspaceStatus is the POST /v1/workspaces reply: the resolved workspace
// name and whether this call created or rebuilt it.
type WorkspaceStatus struct {
	// Namespace is the knowledge-base namespace the workspace belongs to.
	Namespace string `json:"namespace"`
	// Workspace is the resolved storage namespace inside the sidecar.
	Workspace string `json:"workspace"`
	// Created reports that this call created the workspace.
	Created bool `json:"created"`
	// Rebuilt reports that this call dropped the stored graph because the
	// configuration fingerprint changed.
	Rebuilt bool `json:"rebuilt"`
}

// Chunk is one retrievable text block sent to the sidecar. The identifiers and
// citation fields mirror the search evidence contract, so a graph context can
// cite the same document, chunk and page the vector index returns.
type Chunk struct {
	// ChunkID is the stable chunk identifier used by the search evidence.
	ChunkID string `json:"chunk_id"`
	// Text is the chunk body the sidecar extracts entities and relations from.
	Text string `json:"text"`
	// Page is the 1-based source page, or zero when the format has no pages.
	Page int `json:"page"`
	// Heading is the nearest source heading, empty when the source has none.
	Heading string `json:"heading"`
	// UploadVersion increments on every re-upload of the same document so the
	// sidecar can replace stale chunks.
	UploadVersion int `json:"upload_version"`
}

// IngestRequest is the POST /v1/workspaces/{namespace}/chunks body.
type IngestRequest struct {
	// DocumentID is the artifact-backed document identifier.
	DocumentID string `json:"document_id"`
	// DocumentName is the display name used in sidecar logs.
	DocumentName string `json:"document_name"`
	// Chunks are the chunks of this document, in reading order.
	Chunks []Chunk `json:"chunks"`
}

// IngestResult is the ingest reply: how many chunks were stored and how much
// graph structure they produced.
type IngestResult struct {
	// DocumentID echoes the ingested document identifier.
	DocumentID string `json:"document_id"`
	// Accepted is the number of stored chunks.
	Accepted int `json:"accepted"`
	// Skipped is the number of chunks dropped as unchanged or duplicated.
	Skipped int `json:"skipped"`
	// Entities is the number of graph entities extracted from the chunks.
	Entities int `json:"entities"`
	// Relations is the number of graph relations extracted from the chunks.
	Relations int `json:"relations"`
}

// QueryRequest is the POST /v1/workspaces/{namespace}/query body. The sidecar
// always returns evidence, never a generated answer.
type QueryRequest struct {
	// Query is the user question or keyword string.
	Query string `json:"query"`
	// Mode is the sidecar retrieval mode name, for example "hybrid" or
	// "local". Empty lets the sidecar apply its default.
	Mode string `json:"mode"`
	// TopK bounds the number of returned contexts.
	TopK int `json:"top_k"`
	// Filters restrict the search to the given documents.
	Filters QueryFilters `json:"filters"`
	// IncludeEntities returns the entity and relation details of each context
	// so the caller can render the graph evidence.
	IncludeEntities bool `json:"include_entities"`
}

// QueryFilters is the optional document restriction of a graph query. An empty
// DocumentIDs slice means unrestricted, the same convention the vector search
// uses for a server-side visibility scope.
type QueryFilters struct {
	// DocumentIDs restricts the result to these document identifiers.
	DocumentIDs []string `json:"document_ids"`
}

// Relation is one knowledge-graph edge attached to a context.
type Relation struct {
	// Source is the source entity name.
	Source string `json:"source"`
	// Target is the target entity name.
	Target string `json:"target"`
	// Description is the natural-language description of the edge.
	Description string `json:"description"`
}

// Context is one retrieved evidence context: a text chunk, its relevance score
// and the graph elements extracted from it.
type Context struct {
	// ChunkID is the cited chunk identifier.
	ChunkID string `json:"chunk_id"`
	// DocumentID is the cited document identifier.
	DocumentID string `json:"document_id"`
	// DocumentName is the display name of the source document. The sidecar
	// records it at ingest time so a graph hit can carry a real citation
	// title instead of a bare identifier.
	DocumentName string `json:"document_name"`
	// Text is the chunk body returned as evidence.
	Text string `json:"text"`
	// Score is the sidecar relevance score in [0, 1].
	Score float64 `json:"score"`
	// Page is the one-based source page the chunk came from, 0 when the
	// source format has no page concept.
	Page int `json:"page"`
	// Heading is the nearest source heading recorded at ingest time.
	Heading string `json:"heading"`
	// Entities are the entity names found in this chunk.
	Entities []string `json:"entities"`
	// Relations are the graph edges found in this chunk.
	Relations []Relation `json:"relations"`
}

// QueryResult is the query reply: the evidence contexts plus the sidecar debug
// counters used by the console to explain a graph result.
type QueryResult struct {
	// Contexts are the retrieved evidence contexts, best first.
	Contexts []Context `json:"contexts"`
	// Debug carries the query mode and per-kind result counts.
	Debug QueryDebug `json:"debug"`
}

// QueryDebug explains how a graph query was answered.
type QueryDebug struct {
	// Mode is the sidecar mode that actually ran.
	Mode string `json:"mode"`
	// Entities is the number of matched graph entities.
	Entities int `json:"entities"`
	// Relations is the number of matched graph relations.
	Relations int `json:"relations"`
	// Chunks is the number of matched text chunks.
	Chunks int `json:"chunks"`
}

// UpstreamVersion pins the LightRAG package the sidecar runs, so an operator
// can tell a stale service from a configuration problem.
type UpstreamVersion struct {
	// Package is the upstream distribution name.
	Package string `json:"package"`
	// Pinned is the version the sidecar requires.
	Pinned string `json:"pinned"`
	// Installed is the version actually importable.
	Installed string `json:"installed"`
}

// HealthStatus is the GET /healthz reply.
type HealthStatus struct {
	// Status is "ok" when the sidecar can serve requests.
	Status string `json:"status"`
	// Service is the fixed service name.
	Service string `json:"service"`
	// Version is the sidecar build version.
	Version string `json:"version"`
	// Upstream is the LightRAG version information.
	Upstream UpstreamVersion `json:"upstream"`
	// Workspaces is the number of open workspaces.
	Workspaces int `json:"workspaces"`
	// ConfigError is a non-empty description when the sidecar configuration is
	// unusable, for example a missing model endpoint.
	ConfigError string `json:"config_error"`
}
