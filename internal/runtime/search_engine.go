package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/kbconfig"
	"github.com/Guan0923/xinghe_rag/internal/modelapi"
	"github.com/Guan0923/xinghe_rag/internal/qdrant"
	"github.com/Guan0923/xinghe_rag/internal/searchrefs"
	"github.com/Guan0923/xinghe_rag/internal/sparse"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// SearchEngine owns the retrieval half of the service runtime: the per-namespace
// tenant cache (model connections + Qdrant store + orchestrator), synchronous
// search, public knowledge-base settings, and stored search-result reads. It is
// a port of the corresponding RagServiceRuntime methods in
// rag_mcp/service_knowledge.py.
//
// The engine is safe for concurrent use. Tenant construction and invalidation
// are serialized by mu; retrieval itself runs outside the lock and relies on the
// stateless HTTP/Qdrant clients.
type SearchEngine struct {
	config     config.RagConfig
	stores     *state.Stores
	encoder    *sparse.Encoder
	httpClient *http.Client

	mu         sync.Mutex
	tenants    map[string]*tenantResources
	closed     bool
	references *searchrefs.Capture
}

// NewSearchEngine builds the search engine. It validates the local-file
// Qdrant mode eagerly (the Go backend only supports QDRANT_URL) but connects
// lazily on the first search.
func NewSearchEngine(cfg config.RagConfig, stores *state.Stores) (*SearchEngine, error) {
	if cfg.QdrantPath != "" {
		return nil, qdrant.ErrLocalPathUnsupported
	}
	if stores == nil {
		return nil, fmt.Errorf("search engine requires the shared state store")
	}
	return &SearchEngine{
		config:     cfg,
		stores:     stores,
		encoder:    sparse.NewDefaultEncoder(),
		httpClient: &http.Client{Timeout: qdrant.DefaultTimeout},
		tenants:    make(map[string]*tenantResources),
	}, nil
}

// Search runs one validated retrieval action for a namespace and returns the
// orchestrator payload (evidence_status/answer_guidance/results/debug).
//
// Errors keep the Python categories: missing knowledge-base configuration is a
// *apierrors.KnowledgeValidationError, model-service failures are folded into
// *contracts.RagOperationError (Python's ModelAPIError subclasses
// RagOperationError), and other failures are returned unchanged.
func (e *SearchEngine) Search(
	ctx context.Context,
	namespace string,
	request contracts.SearchRequest,
) (map[string]any, error) {
	tenant, err := e.tenant(ctx, namespace)
	if err != nil {
		return nil, searchError(err)
	}
	payload, err := tenant.orchestrator.Search(ctx, request)
	if err != nil {
		return nil, searchError(err)
	}
	if e.references != nil {
		payload, _ = e.references.Capture(ctx, namespace, payload, true)
	}
	return payload, nil
}

// PublicKnowledgeBase returns the credential-free settings projection of one
// knowledge base (knowledge_bases.public_settings). A missing row is reported
// like public_knowledge_base.
func (e *SearchEngine) PublicKnowledgeBase(ctx context.Context, namespace string) (map[string]any, error) {
	settings, err := e.stores.KnowledgeBases.Get(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if settings == nil {
		return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageKnowledgeBaseMissing}
	}
	return kbconfig.PublicSettings(settings), nil
}

// GetSearchResult reads one bounded page of a stored search result without
// re-running retrieval (state.ResultSnapshots.Get). Missing, expired, or
// unavailable snapshots become *apierrors.SnapshotUnavailable (HTTP 410); an
// out-of-range page becomes *contracts.SearchValidationError (Python raises
// ValueError, which the HTTP/MCP boundaries surface as a 400/tool error).
func (e *SearchEngine) GetSearchResult(
	ctx context.Context,
	namespace, resultID string,
	page, pageSize int,
) (map[string]any, error) {
	_ = ctx
	if e.stores.Results == nil {
		return nil, apierrors.SnapshotUnavailableError("Search result expired or unavailable")
	}
	payload, err := e.stores.Results.Get(namespace, resultID, page, pageSize)
	if err != nil {
		var unavailable *state.SnapshotUnavailable
		if errors.As(err, &unavailable) {
			return nil, apierrors.SnapshotUnavailableError(unavailable.Message)
		}
		var valueError *state.ValueError
		if errors.As(err, &valueError) {
			return nil, &contracts.SearchValidationError{Message: valueError.Message}
		}
		return nil, err
	}
	return payload, nil
}

// Invalidate drops one cached tenant so the next search rebuilds it from the
// current knowledge-base row. The runtime calls it after configuration changes.
func (e *SearchEngine) Invalidate(namespace string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.tenants, namespace)
}

// Close releases the engine's pooled Qdrant/model connections. It is
// idempotent and leaves the shared state store untouched (the Service owns it).
func (e *SearchEngine) Close(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	e.tenants = make(map[string]*tenantResources)
	if e.httpClient != nil {
		e.httpClient.CloseIdleConnections()
	}
	return nil
}

// searchError maps runtime/retrieval failures onto the error categories the
// HTTP and MCP layers understand.
func searchError(err error) error {
	if err == nil {
		return nil
	}
	var validation *contracts.SearchValidationError
	if errors.As(err, &validation) {
		return validation
	}
	var knowledgeValidation *apierrors.KnowledgeValidationError
	if errors.As(err, &knowledgeValidation) {
		return knowledgeValidation
	}
	var operation *contracts.RagOperationError
	if errors.As(err, &operation) {
		return operation
	}
	// Python's ModelAPIError subclasses RagOperationError; the Go type is
	// separate, so fold it into the operational category (HTTP 503).
	var modelError *modelapi.ModelAPIError
	if errors.As(err, &modelError) {
		return contracts.NewRagOperationError(modelError.Message)
	}
	return err
}

// SetReferenceCapture attaches the search-reference/result-snapshot capture
// stage (internal/searchrefs). Capture absorbs its own failures, so retrieval
// results are never lost.
func (e *SearchEngine) SetReferenceCapture(capture *searchrefs.Capture) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.references = capture
}
