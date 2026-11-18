package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/qdrant"
	"github.com/Guan0923/Praxis-RAG/internal/searchrefs"
	"github.com/Guan0923/Praxis-RAG/internal/sparse"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// SearchEngine owns the retrieval half of the service runtime: the per-namespace
// tenant cache (model connections + Qdrant store + orchestrator), synchronous
// search and public knowledge-base settings. It is a port of the corresponding
// RagServiceRuntime methods in rag_mcp/service_knowledge.py.
//
// Search always runs synchronously and returns the evidence payload directly;
// there is no queued mode, stored result snapshot or result-id read path.
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
	// Python snapshots the tenant configuration *before* retrieval and merges it
	// into the payload, so callers record which collection/config version
	// produced the evidence.
	snapshot := snapshotContext(tenant)
	payload, err := searchTenant(ctx, tenant, request)
	if err != nil {
		return nil, searchError(err)
	}
	payload["snapshot_context"] = snapshot
	payload = normalizeSearchPayload(payload)
	if e.references != nil {
		payload, _ = e.references.Capture(ctx, namespace, payload)
	}
	return payload, nil
}

// restrictedCandidateK is the internal candidate/selection bound used when a
// search is restricted to a document set. A wider pool lets the reranker surface
// visible documents before the post-retrieval visibility filter runs (DESIGN §7).
const restrictedCandidateK = 100

// Evidence guidance strings are the retrieval contract's fixed wording; the
// runtime recomputes evidence_status/answer_guidance after visibility filtering.
const (
	evidenceFoundGuidance   = "Answer from the returned evidence and cite source plus chunk_id."
	evidenceMissingGuidance = "No supporting local evidence was found; do not guess or present unsupported knowledge claims."
)

// searchTenant runs one retrieval request for a tenant, applying the server-side
// document visibility restriction when request.DocumentIDs is non-nil (an empty
// non-nil slice means "nothing is visible" and yields no evidence).
func searchTenant(
	ctx context.Context,
	tenant *tenantResources,
	request contracts.SearchRequest,
) (map[string]any, error) {
	restricted := request.DocumentIDs != nil
	internal := request
	if restricted {
		internal = restrictedSearchRequest(request)
	}
	payload, err := tenant.orchestrator.Search(ctx, internal)
	if err != nil {
		return nil, err
	}
	if restricted {
		payload = filterSearchPayload(payload, request)
	}
	return payload, nil
}

// restrictedSearchRequest returns the internal request used for a document-id
// restricted search: CandidateK and the post-rerank selection are widened to
// restrictedCandidateK so the visibility filter can still fill the caller's
// top_k. The caller-visible candidate_k/top_k are restored on the payload by
// filterSearchPayload.
func restrictedSearchRequest(request contracts.SearchRequest) contracts.SearchRequest {
	restricted := request
	if restricted.CandidateK < restrictedCandidateK {
		restricted.CandidateK = restrictedCandidateK
	}
	if restricted.TopK < restrictedCandidateK {
		restricted.TopK = restrictedCandidateK
	}
	return restricted
}

// filterSearchPayload applies a DocumentIDs restriction to an orchestrator
// payload: results whose document_id is not allowed are dropped, ranks are
// renumbered from 1 and the list is bounded by the caller's top_k. The
// caller-visible candidate_k/top_k are restored and evidence_status,
// answer_guidance and debug.returned_count are recomputed so an empty filtered
// result keeps the existing no-evidence semantics. It must run before
// searchrefs.Capture so snapshots contain only visible results.
func filterSearchPayload(payload map[string]any, request contracts.SearchRequest) map[string]any {
	if payload == nil {
		return nil
	}
	allowed := make(map[string]struct{}, len(request.DocumentIDs))
	for _, documentID := range request.DocumentIDs {
		allowed[documentID] = struct{}{}
	}
	filtered := make([]any, 0, len(request.DocumentIDs))
	for _, item := range payloadResultItems(payload["results"]) {
		if len(filtered) >= request.TopK {
			break
		}
		documentID, _ := item["document_id"].(string)
		if _, visible := allowed[documentID]; !visible {
			continue
		}
		visible := make(map[string]any, len(item))
		for key, value := range item {
			visible[key] = value
		}
		visible["rank"] = len(filtered) + 1
		filtered = append(filtered, visible)
	}
	updated := make(map[string]any, len(payload))
	for key, value := range payload {
		updated[key] = value
	}
	updated["results"] = filtered
	// The restriction is server-side: the payload keeps echoing the caller's
	// requested bounds, not the widened internal ones.
	updated["candidate_k"] = request.CandidateK
	updated["top_k"] = request.TopK
	if len(filtered) == 0 {
		updated["evidence_status"] = "no_evidence"
		updated["answer_guidance"] = evidenceMissingGuidance
	} else {
		updated["evidence_status"] = "found"
		updated["answer_guidance"] = evidenceFoundGuidance
	}
	if debug, ok := updated["debug"].(map[string]any); ok {
		debug["returned_count"] = len(filtered)
	}
	return updated
}

// payloadResultItems accepts the orchestrator's typed result list and the
// JSON-shaped ([]any) form produced by the runtime boundary.
func payloadResultItems(raw any) []map[string]any {
	switch results := raw.(type) {
	case []map[string]any:
		return results
	case []any:
		items := make([]map[string]any, 0, len(results))
		for _, rawItem := range results {
			if item, ok := rawItem.(map[string]any); ok {
				items = append(items, item)
			}
		}
		return items
	default:
		return nil
	}
}

// snapshotContext mirrors the Python ingester-config context:
// collection/embedding_model/reranker_model/vector_size/chunk_tokenizer_model
// plus the knowledge-base configuration version at search time. Missing values
// stay nil rather than guessing, because table rows created before a field
// existed have no value to report.
func snapshotContext(tenant *tenantResources) map[string]any {
	settings := tenant.settings
	return map[string]any{
		"collection":            tenant.collection,
		"embedding_model":       settings["embedding_model"],
		"reranker_model":        settings["reranker_model"],
		"vector_size":           snapshotInteger(settings["vector_size"]),
		"chunk_tokenizer_model": settings["tokenizer"],
		"configuration_version": snapshotInteger(settings["version"]),
	}
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

// SetReferenceCapture attaches the artifact-reference capture stage
// (internal/searchrefs). Capture absorbs its own failures, so retrieval results
// are never lost.
func (e *SearchEngine) SetReferenceCapture(capture *searchrefs.Capture) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.references = capture
}

// SearchRaw runs one retrieval action without projecting artifact references.
// It shares the same validation, visibility filtering and payload normalization
// as Search; it exists for internal callers that need the bare evidence payload.
func (e *SearchEngine) SearchRaw(ctx context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error) {
	tenant, err := e.tenant(ctx, namespace)
	if err != nil {
		return nil, searchError(err)
	}
	snapshot := snapshotContext(tenant)
	payload, err := searchTenant(ctx, tenant, request)
	if err != nil {
		return nil, searchError(err)
	}
	payload["snapshot_context"] = snapshot
	return normalizeSearchPayload(payload), nil
}

// normalizeSearchPayload renders the orchestrator payload as the JSON-shaped
// contract consumed by reference capture and the HTTP/MCP response encoders.
//
// Python's retrieval returns a list of dicts; the Go projector returns
// []map[string]any. internal/searchrefs.project iterates only []any, so typed
// results would silently skip artifact registration. Normalizing once at the
// runtime boundary keeps synchronous HTTP and MCP search on the same shape.
func normalizeSearchPayload(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	results, ok := payload["results"].([]map[string]any)
	if !ok {
		return payload
	}
	normalized := make([]any, len(results))
	for index, item := range results {
		normalized[index] = map[string]any(item)
	}
	updated := make(map[string]any, len(payload))
	for key, value := range payload {
		updated[key] = value
	}
	updated["results"] = normalized
	return updated
}

// snapshotInteger normalizes JSON-decoded numbers to integers (Python's ingester
// config exposes vector_size/configuration_version as ints; encoding/json decodes
// stored settings as float64/json.Number).
func snapshotInteger(value any) any {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		if typed == float64(int64(typed)) {
			return int64(typed)
		}
		return typed
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed
		}
		return typed
	default:
		return value
	}
}
