package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/qdrant"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// uploadConflictMessage is the fixed Python HTTP detail for UploadConflict.
const uploadConflictMessage = "Upload conflicts with another operation or idempotency key."

// genericKnowledgeBaseMessage is the safe fallback for unexpected state-database
// failures; unexpected internals are never surfaced to callers.
const genericKnowledgeBaseMessage = "知识库操作失败，请稍后重试"

// ListKnowledgeBases returns the knowledge-base list merged with document
// workspaces, exactly like knowledge_bases.public_settings applied by
// state.views.knowledge_bases (configured flags, document counts, no
// credentials).
func (s *Service) ListKnowledgeBases(ctx context.Context) ([]map[string]any, error) {
	items, err := s.stores.Views.KnowledgeBases(ctx)
	if err != nil {
		if state.IsViewBackpressure(err) {
			return nil, err
		}
		return nil, &apierrors.KnowledgeError{Message: "知识库列表读取失败"}
	}
	return items, nil
}

const messageDefaultKBTemplateMissing = "管理员尚未配置默认知识库模板，请联系管理员创建知识库"

// messageModelSelectionRequired is the fixed HTTP 409 detail returned when a
// non-administrator self-create carries no provider selection. Credentials are
// per-account now: the account must add (or pick) its own model configuration
// instead of inheriting the administrator's template.
const messageModelSelectionRequired = "请先在「模型配置」页添加并选择 Embedding 模型后重试"

// CreateKnowledgeBase validates and persists one new knowledge base, probing the
// embedding (and optional reranker) provider for connectivity and vector size.
//
// Python semantics: the namespace pattern is checked first, an existing row or
// pre-existing documents in the namespace are rejected with the same messages,
// and no row is written when a probe fails. The tenant cache is invalidated
// after a successful create. The row keeps a NULL owner (the administrator
// route's legacy shape); CreateKnowledgeBaseForUser records ownership.
func (s *Service) CreateKnowledgeBase(ctx context.Context, input kbconfig.CreateInput) (map[string]any, error) {
	return s.createKnowledgeBase(ctx, input, "", kbconfig.HTTPProber{Timeout: modelapi.DefaultTimeout})
}

// CreateKnowledgeBaseForUser creates one knowledge base owned by a Console
// account.
//
// The tenant self-create route (POST /v1/knowledge-bases) only accepts the
// namespace and chunking for every role. When the request does not carry a
// complete embedding provider draft — always the case for VIPs and normal
// users, and the default for administrators too — the embedding/reranker/VLM
// connections and the embedding vector size are cloned from the administrator
// default template (config.RagConfig.DefaultKBTemplate, RAG_DEFAULT_KB_TEMPLATE).
// An administrator who submits the full provider configuration (the admin route
// shape) keeps those provider settings. The template's stored credentials are
// decrypted with the state encryption key and re-encrypted for the new row. A
// missing template is a *apierrors.KnowledgeConflictError (HTTP 409) and the
// template's own name is never used as the new namespace.
func (s *Service) CreateKnowledgeBaseForUser(
	ctx context.Context,
	ownerUserID string,
	isAdmin bool,
	input kbconfig.CreateInput,
) (map[string]any, error) {
	prober := kbconfig.Prober(kbconfig.HTTPProber{Timeout: modelapi.DefaultTimeout})
	if !providerInputConfigured(input) {
		// Validate the request before touching the template so an invalid
		// namespace is reported as a normal validation failure.
		if err := s.validateKnowledgeBaseCreate(ctx, input.Namespace); err != nil {
			return nil, err
		}
		if !isAdmin {
			// Model configurations are per-account: a normal account must select
			// its own provider instead of inheriting the administrator template.
			return nil, &apierrors.KnowledgeConflictError{Message: messageModelSelectionRequired}
		}
		cloned, templateProber, err := s.cloneDefaultKnowledgeBase(ctx, input)
		if err != nil {
			return nil, err
		}
		input = cloned
		prober = templateProber
	}
	return s.createKnowledgeBase(ctx, input, ownerUserID, prober)
}

// providerInputConfigured reports whether a create input carries a complete
// provider draft (an embedding model plus an embedding base_url). Without both
// the row cannot be created without the default template.
func providerInputConfigured(input kbconfig.CreateInput) bool {
	if strings.TrimSpace(input.EmbeddingModel) == "" {
		return false
	}
	baseURL, _ := input.EmbeddingValues["base_url"].(string)
	return strings.TrimSpace(baseURL) != ""
}

// ResolveDocumentIDs maps document names to their indexed document ids inside
// one knowledge base. Names without a document row are skipped and duplicate
// ids are collapsed, so the MCP visibility filter can feed the result straight
// into contracts.SearchRequest.DocumentIDs.
func (s *Service) ResolveDocumentIDs(ctx context.Context, namespace string, names []string) ([]string, error) {
	ids := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		record, err := s.stores.Documents.Get(ctx, namespace, name)
		if err != nil {
			return nil, &apierrors.KnowledgeError{Message: "文档记录无法读取"}
		}
		if record == nil || record.DocumentID == "" {
			continue
		}
		if _, duplicate := seen[record.DocumentID]; duplicate {
			continue
		}
		seen[record.DocumentID] = struct{}{}
		ids = append(ids, record.DocumentID)
	}
	return ids, nil
}

// createKnowledgeBase is the shared create path (validation, provider probe or
// template replay, owner-aware insert, tenant-cache invalidation).
func (s *Service) createKnowledgeBase(
	ctx context.Context,
	input kbconfig.CreateInput,
	ownerUserID string,
	prober kbconfig.Prober,
) (map[string]any, error) {
	if err := s.validateKnowledgeBaseCreate(ctx, input.Namespace); err != nil {
		return nil, err
	}
	if input.EncryptionKey == "" {
		input.EncryptionKey = s.stores.Settings.EncryptionKey
	}
	settings, err := kbconfig.CreateSettings(ctx, input, prober)
	if err != nil {
		return nil, adminError(err)
	}
	// A personal knowledge base consumes one slot of the owner's current plan.
	// The count and insert share one MySQL transaction; unowned/system template
	// knowledge bases keep the administrative path and are not limited.
	if ownerUserID != "" && s.stores.Membership != nil {
		entitlement, entitlementErr := s.stores.Membership.Effective(ctx, ownerUserID, time.Now())
		if entitlementErr != nil {
			return nil, &apierrors.KnowledgeError{Message: "会员权益读取失败"}
		}
		err = s.stores.KnowledgeBases.CreateWithOwnerLimit(ctx, settings, ownerUserID,
			entitlement.Limits.MaxOwnedKnowledgeBases)
	} else {
		err = s.stores.KnowledgeBases.CreateWithOwner(ctx, settings, ownerUserID)
	}
	if err != nil {
		var conflict *state.StateConflict
		if errors.As(err, &conflict) {
			return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageKnowledgeBaseExists}
		}
		var limit *state.KnowledgeBaseLimitError
		if errors.As(err, &limit) {
			return nil, &apierrors.KnowledgeValidationError{Message: limit.Message}
		}
		return nil, &apierrors.KnowledgeError{Message: "知识库配置无法保存"}
	}
	s.Invalidate(input.Namespace)
	return kbconfig.PublicSettings(settings), nil
}

// validateKnowledgeBaseCreate reproduces the pre-insert checks: namespace
// pattern, no existing configuration row, no documents in the namespace.
func (s *Service) validateKnowledgeBaseCreate(ctx context.Context, namespace string) error {
	if err := kbconfig.ValidateNamespace(namespace); err != nil {
		return adminError(err)
	}
	existing, err := s.stores.KnowledgeBases.Get(ctx, namespace)
	if err != nil {
		return &apierrors.KnowledgeError{Message: "知识库配置无法读取"}
	}
	if existing != nil {
		return &apierrors.KnowledgeValidationError{Message: kbconfig.MessageKnowledgeBaseExists}
	}
	documents, err := s.stores.Documents.List(ctx, namespace)
	if err != nil {
		return &apierrors.KnowledgeError{Message: "文档记录无法读取"}
	}
	if len(documents) > 0 {
		return &apierrors.KnowledgeValidationError{Message: kbconfig.MessageNamespaceHasDocuments}
	}
	return nil
}

// cloneDefaultKnowledgeBase reads the default template row and turns its stored
// provider connections into a kbconfig.CreateInput identical to the request
// except for the cloned provider settings. The returned prober replays the
// template's validated vector size instead of probing the providers again (the
// template was validated by the administrator create).
func (s *Service) cloneDefaultKnowledgeBase(
	ctx context.Context,
	input kbconfig.CreateInput,
) (kbconfig.CreateInput, kbconfig.Prober, error) {
	templateNamespace := strings.TrimSpace(s.config.DefaultKBTemplate)
	if templateNamespace == "" {
		templateNamespace = config.DefaultKBTemplateNamespace
	}
	settings, err := s.stores.KnowledgeBases.Get(ctx, templateNamespace)
	if err != nil {
		return input, nil, &apierrors.KnowledgeError{Message: "知识库配置无法读取"}
	}
	if settings == nil {
		return input, nil, &apierrors.KnowledgeConflictError{Message: messageDefaultKBTemplateMissing}
	}
	encryptionKey := s.stores.Settings.EncryptionKey
	embedding, err := kbconfig.ModelConnectionForKey(settings, "embedding", encryptionKey)
	if err != nil {
		return input, nil, adminError(err)
	}
	reranker, err := kbconfig.ModelConnectionForKey(settings, "reranker", encryptionKey)
	if err != nil {
		return input, nil, adminError(err)
	}
	vlm, err := kbconfig.ModelConnectionForKey(settings, "vlm", encryptionKey)
	if err != nil {
		return input, nil, adminError(err)
	}
	vectorSize := 0
	if embedding.VectorSize != nil {
		vectorSize = *embedding.VectorSize
	}
	if vectorSize <= 0 {
		return input, nil, adminError(fmt.Errorf("default knowledge base template has no vector size"))
	}
	cloned := input
	cloned.EmbeddingModel = embedding.Model
	cloned.EmbeddingValues = connectionValues(embedding)
	cloned.RerankerModel = reranker.Model
	cloned.RerankerValues = connectionValues(reranker)
	cloned.VLMModel = vlm.Model
	cloned.VLMValues = connectionValues(vlm)
	// The billing source is a property of the model connection, not of the
	// requesting account; a template clone must carry it forward.
	if source, ok := settings["embedding_billing_source"].(string); ok {
		cloned.EmbeddingBillingSource = source
	}
	if strings.TrimSpace(cloned.TokenizerModel) == "" {
		if tokenizer, ok := settings["tokenizer"].(string); ok {
			cloned.TokenizerModel = tokenizer
		}
	}
	// The template's credentials are decrypted above and re-encrypted by
	// CreateSettings with the same store key; never forward a caller key.
	cloned.EncryptionKey = ""
	return cloned, templateProber{vectorSize: vectorSize}, nil
}

// connectionValues renders one decrypted connection as the client provider
// draft accepted by kbconfig.DraftConnection ({base_url, apikey}).
func connectionValues(connection modelapi.ModelConnection) map[string]any {
	return map[string]any{"base_url": connection.BaseURL, "apikey": connection.APIKey}
}

// templateProber replays the default template's validated provider state: the
// embedding vector size recorded on the template row, and an already-probed
// reranker. It performs no network I/O, so a template clone does not depend on
// provider availability.
type templateProber struct {
	vectorSize int
}

// EmbeddingVectorSize returns the template's recorded dense dimension.
func (p templateProber) EmbeddingVectorSize(context.Context, modelapi.ModelConnection) (int, error) {
	return p.vectorSize, nil
}

// RerankerAccessible accepts the template's reranker (validated at template
// creation).
func (p templateProber) RerankerAccessible(context.Context, modelapi.ModelConnection) error {
	return nil
}

// UpdateKnowledgeBase validates and publishes one configuration change.
//
// It holds the namespace upload lease (Python's uploads.lock(namespace)), runs
// the kbconfig re-embedding gates, reindexes into a staging collection when
// requested, fences the configuration version, then publishes. The tenant cache
// is invalidated only after the new configuration is committed.
func (s *Service) UpdateKnowledgeBase(
	ctx context.Context,
	namespace string,
	input kbconfig.UpdateInput,
) (map[string]any, error) {
	if input.EncryptionKey == "" {
		input.EncryptionKey = s.stores.Settings.EncryptionKey
	}
	var published map[string]any
	publish := func(token *state.LockToken) error {
		result, err := s.publishKnowledgeBase(ctx, namespace, input, token)
		if err != nil {
			return err
		}
		published = result
		return nil
	}
	var err error
	if s.stores.Uploads != nil {
		// Python holds state.uploads.lock(namespace) for the whole publication.
		err = s.stores.Uploads.WithLock(ctx, namespace, nil, publish)
	} else {
		err = publish(nil)
	}
	if err != nil {
		return nil, updateError(err)
	}
	s.Invalidate(namespace)
	return kbconfig.PublicSettings(published), nil
}

// publishKnowledgeBase is the configuration half of _publish_knowledge_base:
// validate + probe, stage a re-embedded index, fence the version and commit.
// Reindex/publication compensation stays here (not in state) like Python.
func (s *Service) publishKnowledgeBase(
	ctx context.Context,
	namespace string,
	input kbconfig.UpdateInput,
	token *state.LockToken,
) (map[string]any, error) {
	snapshot, err := s.stores.KnowledgeBases.Snapshot(ctx, namespace)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "知识库配置无法读取"}
	}
	if snapshot == nil {
		return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageKnowledgeBaseMissing}
	}
	current := snapshot.Settings
	result, err := kbconfig.UpdateSettings(ctx, current, input, kbconfig.HTTPProber{Timeout: modelapi.DefaultTimeout})
	if err != nil {
		return nil, err
	}
	updated := result.Settings
	staging := result.StagingCollection
	if staging != "" {
		if err := s.reembedIntoStaging(ctx, namespace, current, staging, result, snapshot.Version); err != nil {
			s.discardCollection(staging)
			return nil, err
		}
		updated = kbconfig.ActivateReembed(updated, staging, time.Now())
	}
	// Optimistic publication fence: the configuration version observed above
	// must still be current. The Go repository does not expose Python's atomic
	// publish(expected_version), so this narrows the race window while the
	// namespace upload lease is held.
	latest, err := s.stores.KnowledgeBases.Snapshot(ctx, namespace)
	if err != nil {
		s.discardCollection(staging)
		return nil, &apierrors.KnowledgeError{Message: "知识库配置无法读取"}
	}
	if latest == nil || latest.Version != snapshot.Version {
		s.discardCollection(staging)
		return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessagePublicationRolledBack}
	}
	// The destructive configuration write is fenced by the exclusive lease: a
	// request that lost it (for example after a takeover) must not publish.
	if token != nil {
		if err := s.stores.Uploads.AssertLock(ctx, token); err != nil {
			s.discardCollection(staging)
			return nil, err
		}
	}
	if err := s.stores.KnowledgeBases.Update(ctx, updated); err != nil {
		var lookup *state.LookupError
		if errors.As(err, &lookup) {
			s.discardCollection(staging)
			return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessagePublicationRolledBack}
		}
		// Publication outcome is uncertain: keep the staged index for operators
		// to inspect, exactly like the Python "unconfirmed" contract.
		return nil, &apierrors.KnowledgeError{Message: kbconfig.MessagePublicationUnconfirmed}
	}
	return updated, nil
}

// reembedIntoStaging copies the visible points of the current index into the
// staging collection, replacing the dense vectors with the new embedding model
// (qdrant_store.reembed_into).
func (s *Service) reembedIntoStaging(
	ctx context.Context,
	namespace string,
	current map[string]any,
	staging string,
	result kbconfig.UpdateResult,
	version int,
) error {
	collection, err := settingString(current, "collection")
	if err != nil {
		return &apierrors.KnowledgeError{Message: "知识库配置无效"}
	}
	vectorSize, err := settingInteger(current, "vector_size")
	if err != nil {
		return &apierrors.KnowledgeError{Message: "知识库配置无效"}
	}
	records, err := s.stores.Documents.List(ctx, namespace)
	if err != nil {
		return &apierrors.KnowledgeError{Message: "文档记录无法读取"}
	}
	expectedChunks := make(map[string]int, len(records))
	for _, record := range records {
		expectedChunks[record.DocumentID] = record.ChunkCount
	}
	httpClient := &http.Client{Timeout: qdrant.DefaultTimeout}
	defer httpClient.CloseIdleConnections()
	source, err := qdrant.NewStore(qdrant.StoreConfig{
		Collection: collection,
		URL:        s.config.QdrantURL,
		Path:       s.config.QdrantPath,
		VectorSize: vectorSize,
		Namespace:  namespace,
		ActiveVersions: func() map[string]string {
			versions, versionErr := s.stores.Documents.VisibleVersions(context.Background(), namespace)
			if versionErr != nil {
				return nil
			}
			return versions
		},
		HTTPClient: httpClient,
	})
	if err != nil {
		return adminError(err)
	}
	embedder := modelapi.NewHTTPEmbedder(result.Embedding, modelapi.DefaultTimeout)
	// Re-embedding is an administrator/platform operation. Its quota usage is
	// recorded against the isolated system ledger and never charged to the
	// knowledge-base owner.
	var reembedder qdrant.Embedder = embedder
	if s.stores.Membership != nil && embeddingBillingSource(current) != "customer" {
		reembedder = &meteredReembedder{
			inner:     embedder,
			repo:      s.stores.Membership,
			namespace: namespace,
			model:     result.Embedding.Model,
			version:   version,
			now:       s.clock,
		}
	}
	if err := source.ReembedInto(ctx, staging, reembedder, expectedChunks); err != nil {
		// Model failures are operational (Python ModelAPIError subclasses
		// RagOperationError); Qdrant failures are reported safely as service
		// errors so the HTTP layer renders 503.
		var modelError *modelapi.ModelAPIError
		if errors.As(err, &modelError) {
			return contracts.NewRagOperationError(modelError.Message)
		}
		var operation *contracts.RagOperationError
		if errors.As(err, &operation) {
			return operation
		}
		return &apierrors.KnowledgeError{Message: "文档重新向量化失败，请检查模型服务和 Qdrant 状态"}
	}
	return nil
}

// discardCollection deletes one unpublished staging collection, ignoring every
// failure (Python _discard_unpublished_index logs and continues).
func (s *Service) discardCollection(collection string) {
	if collection == "" {
		return
	}
	httpClient := &http.Client{Timeout: qdrant.DefaultTimeout}
	defer httpClient.CloseIdleConnections()
	client := qdrant.NewClient(s.config.QdrantURL, "", httpClient)
	path := "/collections/" + url.PathEscape(collection)
	var existence struct {
		Exists bool `json:"exists"`
	}
	if err := client.Do(context.Background(), http.MethodGet, path+"/exists", nil, nil, &existence); err != nil {
		return
	}
	if !existence.Exists {
		return
	}
	_ = client.Do(context.Background(), http.MethodDelete, path, nil, nil, nil)
}

// DeleteKnowledgeBase removes one knowledge-base configuration record.
//
// Python's KnowledgeBaseStore.delete only removes the configuration row; the
// namespace's documents and its Qdrant collection are intentionally retained so
// the workspace can still be listed/recovered. The tenant cache is invalidated
// when a row was removed.
func (s *Service) DeleteKnowledgeBase(ctx context.Context, namespace string) (bool, error) {
	deleted, err := s.stores.KnowledgeBases.Delete(ctx, namespace)
	if err != nil {
		return false, &apierrors.KnowledgeError{Message: "知识库配置无法删除"}
	}
	if deleted {
		s.Invalidate(namespace)
	}
	return deleted, nil
}

// KnowledgeBasePurgeResult reports what one knowledge-base purge removed.
//
// The purge is a single batched database transaction: every document is
// soft-deleted and every namespace-scoped row (documents, upload references,
// catalog, leases, permissions, configuration) is removed with a fixed number of
// statements, independent of how many documents or namespaces are involved.
type KnowledgeBasePurgeResult struct {
	Namespace            string
	DocumentsDeleted     int
	FailedDocuments      []map[string]string
	ConfigurationDeleted bool
}

// PurgeKnowledgeBases removes several knowledge bases with one batched purge.
// The results are returned in the requested order.
//
// The batch runs a single transaction regardless of the number of namespaces, so
// a Console batch delete costs one database operation instead of one per
// namespace (and one per document). Namespaces with an active upload lease are
// reported as failed and left untouched. Payload bytes are only dropped from
// content_objects when no reference row points at them anymore, and each purged
// knowledge base has its own Qdrant collection deleted with one request.
func (s *Service) PurgeKnowledgeBases(ctx context.Context, namespaces []string) ([]KnowledgeBasePurgeResult, error) {
	ordered := make([]string, 0, len(namespaces))
	seen := make(map[string]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		if namespace == "" {
			continue
		}
		if _, duplicate := seen[namespace]; duplicate {
			continue
		}
		seen[namespace] = struct{}{}
		ordered = append(ordered, namespace)
	}
	results := make([]KnowledgeBasePurgeResult, len(ordered))
	for index, namespace := range ordered {
		results[index] = KnowledgeBasePurgeResult{Namespace: namespace}
	}
	if len(ordered) == 0 {
		return results, nil
	}
	report, err := s.stores.KnowledgeBases.Purge(ctx, ordered)
	if err != nil {
		var unavailable *state.LockUnavailable
		if errors.As(err, &unavailable) {
			return nil, &apierrors.KnowledgeError{Message: documentLockUnavailableMessage}
		}
		return nil, &apierrors.KnowledgeError{Message: "知识库配置无法删除"}
	}
	for index, namespace := range ordered {
		if report.Busy[namespace] {
			results[index].FailedDocuments = []map[string]string{{
				"name":  "",
				"error": "知识库正在更新，请稍后再删除",
			}}
			continue
		}
		results[index].DocumentsDeleted = report.DocumentsByNamespace[namespace]
		results[index].ConfigurationDeleted = report.Present[namespace]
	}
	s.cleanupPurgedCollections(ctx, report)
	for namespace := range report.Present {
		s.Invalidate(namespace)
	}
	return results, nil
}

// PurgeKnowledgeBase removes one knowledge base through the batched purge path.
func (s *Service) PurgeKnowledgeBase(ctx context.Context, namespace string) (KnowledgeBasePurgeResult, error) {
	results, err := s.PurgeKnowledgeBases(ctx, []string{namespace})
	if err != nil {
		return KnowledgeBasePurgeResult{Namespace: namespace}, err
	}
	if len(results) == 0 {
		return KnowledgeBasePurgeResult{Namespace: namespace}, nil
	}
	return results[0], nil
}

// cleanupPurgedCollections deletes one Qdrant collection per purged knowledge
// base with a single request each. Collections that another knowledge base still
// references are kept. Failures are best-effort: MySQL is the source of truth
// and an orphan collection only costs storage.
func (s *Service) cleanupPurgedCollections(ctx context.Context, report state.KnowledgeBasePurgeReport) {
	if len(report.Collections) == 0 {
		return
	}
	collections := make([]string, 0, len(report.Collections))
	seen := make(map[string]struct{}, len(report.Collections))
	for _, collection := range report.Collections {
		if collection == "" {
			continue
		}
		if _, duplicate := seen[collection]; duplicate {
			continue
		}
		seen[collection] = struct{}{}
		collections = append(collections, collection)
	}
	if len(collections) == 0 {
		return
	}
	inUse, err := s.stores.KnowledgeBases.CollectionsInUse(ctx, collections)
	if err != nil {
		logBackgroundFailure(ctx, "collection usage lookup", err)
		return
	}
	for _, collection := range collections {
		if inUse[collection] {
			continue
		}
		s.deleteQdrantCollection(collection)
	}
}

// deleteQdrantCollection removes one whole collection in one HTTP request.
func (s *Service) deleteQdrantCollection(collection string) {
	if collection == "" {
		return
	}
	httpClient := &http.Client{Timeout: qdrant.DefaultTimeout}
	defer httpClient.CloseIdleConnections()
	client := qdrant.NewClient(s.config.QdrantURL, "", httpClient)
	_ = client.Do(context.Background(), http.MethodDelete, "/collections/"+url.PathEscape(collection), nil, nil, nil)
}

// KnowledgeBaseKey reveals the decrypted provider API key for the local admin
// UI (knowledge_base_key). Errors stay safe: a missing row is a service error
// (Python's KnowledgeValidationError is rendered through the KnowledgeError
// handler on this route) and credential failures are validation errors.
func (s *Service) KnowledgeBaseKey(ctx context.Context, namespace, kind string) (string, error) {
	if !kbconfig.IsKind(kind) {
		return "", fmt.Errorf("unknown model kind %q", kind)
	}
	settings, err := s.stores.KnowledgeBases.Get(ctx, namespace)
	if err != nil {
		return "", &apierrors.KnowledgeError{Message: "知识库配置无法读取"}
	}
	if settings == nil {
		return "", &apierrors.KnowledgeError{Message: kbconfig.MessageKnowledgeBaseMissing}
	}
	connection, err := kbconfig.ModelConnectionForKey(settings, kind, s.stores.Settings.EncryptionKey)
	if err != nil {
		return "", adminError(err)
	}
	return connection.APIKey, nil
}

// KnowledgeBaseModels lists provider models for one model kind using the stored
// connection (when a namespace is supplied) plus the client draft base_url and
// apikey (knowledge_base_models).
func (s *Service) KnowledgeBaseModels(
	ctx context.Context,
	kind string,
	values map[string]any,
	namespace *string,
) ([]string, error) {
	if !kbconfig.IsKind(kind) {
		return nil, fmt.Errorf("unknown model kind %q", kind)
	}
	var current map[string]any
	if namespace != nil && *namespace != "" {
		settings, err := s.stores.KnowledgeBases.Get(ctx, *namespace)
		if err != nil {
			return nil, &apierrors.KnowledgeError{Message: "知识库配置无法读取"}
		}
		if settings == nil {
			return nil, &apierrors.KnowledgeError{Message: kbconfig.MessageKnowledgeBaseMissing}
		}
		current = settings
	}
	models, err := kbconfig.ModelsFor(ctx, kind, current, values, nil)
	if err != nil {
		return nil, adminError(err)
	}
	return models, nil
}

// adminError maps knowledge-base administration failures onto the categories the
// HTTP layer renders like the Python exception handlers:
//
//	kbconfig.ValidationError      -> KnowledgeValidationError (route: 400/409)
//	modelapi.ModelAPIError        -> KnowledgeValidationError (Python global 400)
//	state.UploadConflict/LeaseLost-> UploadConflict (409, fixed detail)
//	contracts/state/apierrors      -> unchanged
//	anything else                 -> KnowledgeError (503, safe message)
func adminError(err error) error {
	if err == nil {
		return nil
	}
	var validation *kbconfig.ValidationError
	if errors.As(err, &validation) {
		return &apierrors.KnowledgeValidationError{Message: validation.Message}
	}
	var modelError *modelapi.ModelAPIError
	if errors.As(err, &modelError) {
		return &apierrors.KnowledgeValidationError{Message: modelError.Message}
	}
	var uploadConflict *state.UploadConflict
	if errors.As(err, &uploadConflict) {
		return &apierrors.UploadConflict{Message: uploadConflictMessage}
	}
	var knowledgeValidation *apierrors.KnowledgeValidationError
	if errors.As(err, &knowledgeValidation) {
		return knowledgeValidation
	}
	var knowledge *apierrors.KnowledgeError
	if errors.As(err, &knowledge) {
		return knowledge
	}
	var upload *apierrors.UploadConflict
	if errors.As(err, &upload) {
		return upload
	}
	var operation *contracts.RagOperationError
	if errors.As(err, &operation) {
		return operation
	}
	return &apierrors.KnowledgeError{Message: genericKnowledgeBaseMessage}
}

// updateError maps update failures like adminError but keeps operational
// retrieval/model failures in the RagOperationError category (Python's update
// route renders both KnowledgeError and RagOperationError as 503).
func updateError(err error) error {
	if err == nil {
		return nil
	}
	var modelError *modelapi.ModelAPIError
	if errors.As(err, &modelError) {
		return contracts.NewRagOperationError(modelError.Message)
	}
	return adminError(err)
}
