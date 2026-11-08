package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/kbconfig"
	"github.com/Guan0923/xinghe_rag/internal/modelapi"
	"github.com/Guan0923/xinghe_rag/internal/qdrant"
	"github.com/Guan0923/xinghe_rag/internal/state"
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
		return nil, &apierrors.KnowledgeError{Message: "知识库列表读取失败"}
	}
	return items, nil
}

// CreateKnowledgeBase validates and persists one new knowledge base, probing the
// embedding (and optional reranker) provider for connectivity and vector size.
//
// Python semantics: the namespace pattern is checked first, an existing row or
// pre-existing documents in the namespace are rejected with the same messages,
// and no row is written when a probe fails. The tenant cache is invalidated
// after a successful create.
func (s *Service) CreateKnowledgeBase(ctx context.Context, input kbconfig.CreateInput) (map[string]any, error) {
	if err := kbconfig.ValidateNamespace(input.Namespace); err != nil {
		return nil, adminError(err)
	}
	existing, err := s.stores.KnowledgeBases.Get(ctx, input.Namespace)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "知识库配置无法读取"}
	}
	if existing != nil {
		return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageKnowledgeBaseExists}
	}
	documents, err := s.stores.Documents.List(ctx, input.Namespace)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "文档记录无法读取"}
	}
	if len(documents) > 0 {
		return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageNamespaceHasDocuments}
	}
	if input.EncryptionKey == "" {
		input.EncryptionKey = s.stores.Settings.EncryptionKey
	}
	settings, err := kbconfig.CreateSettings(ctx, input, kbconfig.HTTPProber{Timeout: modelapi.DefaultTimeout})
	if err != nil {
		return nil, adminError(err)
	}
	if err := s.stores.KnowledgeBases.Create(ctx, settings); err != nil {
		var conflict *state.StateConflict
		if errors.As(err, &conflict) {
			return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageKnowledgeBaseExists}
		}
		return nil, &apierrors.KnowledgeError{Message: "知识库配置无法保存"}
	}
	s.Invalidate(input.Namespace)
	return kbconfig.PublicSettings(settings), nil
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
	publish := func(*state.LockToken) error {
		result, err := s.publishKnowledgeBase(ctx, namespace, input)
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
		if err := s.reembedIntoStaging(ctx, namespace, current, staging, result); err != nil {
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
	if err := source.ReembedInto(ctx, staging, embedder, expectedChunks); err != nil {
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
