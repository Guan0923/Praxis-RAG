// Package mcp implements the Praxis-RAG Model Context Protocol server layer on
// top of the official Model Context Protocol Go SDK. It replaces
// rag_mcp/server.py, rag_mcp/document_tools.py, the MCP-facing part of
// rag_mcp/catalog.py and rag_mcp/mcp_http.py.
//
// The package is deliberately a consumer of the runtime: internal/mcp must not
// import internal/runtime, it only depends on the Runtime interface declared
// here and on internal/{contracts,catalog,resources}.
package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Guan0923/xinghe_rag/internal/catalog"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
)

const (
	// MaxFileBytes is the largest client-supplied document accepted over MCP.
	// It mirrors service_knowledge.MAX_FILE_BYTES.
	MaxFileBytes = 10 * 1024 * 1024

	// maxRequestBodyBytes mirrors NamespaceEndpoint's
	// max_request_body_size: JSON may escape one UTF-8 byte as six characters,
	// so six file payloads plus envelope headroom.
	maxRequestBodyBytes = 6*MaxFileBytes + 64*1024

	// listPageSize mirrors the hard-coded page size of mcp_http._paginate.
	listPageSize = 50
)

// ErrUnauthorized asks the HTTP layer to answer 401 with a Bearer challenge.
var ErrUnauthorized = errors.New("MCP 请求必须提供 Authorization: Bearer <API_KEY>")

// ErrForbidden asks the HTTP layer to answer 403.
var ErrForbidden = errors.New("MCP request is not authorized for this namespace")

// ErrInvalidNamespace asks the HTTP layer to answer 400.
var ErrInvalidNamespace = errors.New("Invalid RAG namespace.")

// ErrUploadConflict is returned by Runtime document writes when the operation
// collides with an existing filename, a queued upload or a reused
// idempotency key. internal/mcp maps it to the same safe Arabic messages as
// rag_mcp.mcp_http.mutate_document. Runtime implementations may also return a
// *catalog.Error, *contracts.SearchValidationError or plain error; every error
// is rendered as a tool error (never a protocol error), so runtime adapters
// must keep error messages secret-free.
var ErrUploadConflict = errors.New("upload conflict")

// DocumentRecord is one knowledge-base document: at least {"name": string}
// plus whatever runtime metadata the service keeps (size, modified_at,
// chunking, ...). MCP itself only depends on "name"; document change
// detection compares complete records exactly like
// rag_mcp.mcp_http.documents_changed.
type DocumentRecord map[string]any

// Name returns the document filename, or "" when the record is malformed.
func (r DocumentRecord) Name() string {
	name, _ := r["name"].(string)
	return name
}

// DocumentContent is the stored text and version of one knowledge document.
type DocumentContent struct {
	Text    string
	Version string
}

// Runtime is the consumer-side interface MCP needs from the service runtime.
//
// Every error returned from these methods becomes an MCP tool error whose text
// is the error message, matching rag_mcp.server.ToolRegistry.invoke.
type Runtime interface {
	// ListDocuments lists the documents of one namespace.
	ListDocuments(ctx context.Context, namespace string) ([]DocumentRecord, error)
	// ReadDocument returns the rendered text and current version of a document.
	ReadDocument(ctx context.Context, namespace, name string) (DocumentContent, error)
	// ResolveDocumentIDs maps document names to the document ids stored in
	// retrieval payloads. Names without a document row are skipped. MCP uses it
	// to translate the caller's readable names into the server-side
	// contracts.SearchRequest.DocumentIDs visibility restriction, and to
	// defensively filter stored search results.
	ResolveDocumentIDs(ctx context.Context, namespace string, names []string) ([]string, error)

	// Search runs one retrieval action synchronously (legacy/mock mode).
	Search(ctx context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error)

	// SubmitSearch enqueues one retrieval action and returns the result UUID.
	SubmitSearch(ctx context.Context, namespace string, request contracts.SearchRequest) (string, error)
	// GetSearchResult reads a stored search result page without re-running
	// retrieval. Queued searches report status "queued".
	GetSearchResult(ctx context.Context, namespace, resultID string, page, pageSize int) (map[string]any, error)

	// GetArtifact reads one bounded byte chunk of an expiring artifact.
	GetArtifact(ctx context.Context, namespace, artifactID string, offset, length int) (map[string]any, error)

	// AddDocument durably admits one upload for asynchronous indexing. The
	// returned payload contains at least "artifact_id"; the bytes never cross
	// the broker on the submission path.
	AddDocument(ctx context.Context, namespace, name string, content []byte, idempotencyKey string) (map[string]any, error)
	// GetDocumentStatus reports the indexing status of one artifact.
	GetDocumentStatus(ctx context.Context, namespace, artifactID string) (map[string]any, error)
	// WriteDocument replaces (overwrite) or creates a document synchronously.
	// The payload carries "documents" (the namespace's post-write document
	// list, consumed by change notifications) plus the write status fields.
	WriteDocument(ctx context.Context, namespace, name string, content []byte, overwrite bool, idempotencyKey string) (map[string]any, error)
	// DeleteDocument removes a document synchronously. The payload carries
	// "deleted" (the removed record) and "documents" (the post-delete list).
	DeleteDocument(ctx context.Context, namespace, name string) (map[string]any, error)
}

// Options configures one MCP Endpoint.
type Options struct {
	// Runtime is required.
	Runtime Runtime
	// Catalog is required; it persists per-namespace tool overrides and prompts.
	Catalog catalog.Store
	// QueuedSearch selects the queued search tool contract (wait_seconds +
	// get_search_result polling) instead of the synchronous search tool.
	// The real service uses queued search; mocks and local-only executors use
	// the synchronous form.
	QueuedSearch bool
	// ServerName overrides the MCP implementation name ("Praxis-RAG").
	ServerName string
	// Version overrides the reported MCP implementation version.
	Version string
	// Logger receives server-side diagnostics; nil falls back to slog.Default.
	Logger *slog.Logger
}

type namespaceContextKey struct{}

// WithNamespace returns a context carrying the authenticated tenant namespace.
// The HTTP API layer injects it before dispatching /mcp requests, mirroring the
// httpapi.namespaceContextKey mechanism.
func WithNamespace(ctx context.Context, namespace string) context.Context {
	return context.WithValue(ctx, namespaceContextKey{}, namespace)
}

// NamespaceFromContext returns the authenticated tenant namespace.
func NamespaceFromContext(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(namespaceContextKey{}).(string)
	return value, ok
}
