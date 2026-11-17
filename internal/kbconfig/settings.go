package kbconfig

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/modelapi"
)

// DefaultTokenizerModel is the tokenizer recorded for new knowledge bases when
// the runtime configuration does not override RAG_CHUNK_TOKENIZER_MODEL.
const DefaultTokenizerModel = "BAAI/bge-m3"

// collectionPrefix matches the Python f"knowledge_{uuid4().hex}" naming.
const collectionPrefix = "knowledge_"

// TimestampLayout is Python datetime.isoformat() for UTC timestamps with
// microsecond precision (the format stored in created_at/updated_at).
const TimestampLayout = "2006-01-02T15:04:05.000000+00:00"

var namespacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ValidateNamespace enforces the required knowledge-base identifier pattern
// (create_knowledge_base): one leading alphanumeric, then up to 63 of
// [A-Za-z0-9_.-].
func ValidateNamespace(namespace string) error {
	if !namespacePattern.MatchString(namespace) {
		return NewValidationError(MessageNamespaceInvalid)
	}
	return nil
}

// NewCollectionName returns a fresh Qdrant collection name using the same
// shape as Python: "knowledge_" + 32 lowercase hex characters.
func NewCollectionName() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return collectionPrefix + hex.EncodeToString(buffer), nil
}

// ISOTimestamp formats a timestamp like datetime.now(UTC).isoformat().
func ISOTimestamp(moment time.Time) string {
	return moment.UTC().Format(TimestampLayout)
}

// Prober validates model provider connections during create/update. The runtime
// injects HTTPProber in production; tests inject deterministic fakes.
type Prober interface {
	// EmbeddingVectorSize embeds the connection test text and returns the
	// produced dense dimension (Python: len(HTTPEmbedder(...).embed_query(...))).
	EmbeddingVectorSize(ctx context.Context, connection modelapi.ModelConnection) (int, error)
	// RerankerAccessible performs one bounded rerank call
	// (Python: HTTPReranker(...).scores("connection test", ["connection test"])).
	RerankerAccessible(ctx context.Context, connection modelapi.ModelConnection) error
}

// connectionProbeText is the fixed text used for provider connectivity checks.
const connectionProbeText = "connection test"

// HTTPProber is the production Prober backed by the HTTP model API.
type HTTPProber struct {
	// Timeout in seconds; non-positive uses modelapi.DefaultTimeout (60).
	Timeout float64
}

func (p HTTPProber) timeoutSeconds() float64 {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return modelapi.DefaultTimeout
}

// EmbeddingVectorSize embeds the connection probe text and returns the vector
// length.
func (p HTTPProber) EmbeddingVectorSize(ctx context.Context, connection modelapi.ModelConnection) (int, error) {
	embedder := modelapi.NewHTTPEmbedder(connection, p.timeoutSeconds())
	vector, err := embedder.EmbedQuery(ctx, connectionProbeText)
	if err != nil {
		return 0, err
	}
	return len(vector), nil
}

// RerankerAccessible performs one bounded rerank call against the provider.
func (p HTTPProber) RerankerAccessible(ctx context.Context, connection modelapi.ModelConnection) error {
	reranker := modelapi.NewHTTPReranker(connection, 30, 4000, p.timeoutSeconds())
	_, err := reranker.Scores(ctx, connectionProbeText, []string{connectionProbeText})
	return err
}

// CreateInput is the validated, client-supplied input for a new knowledge base.
// The runtime resolves client JSON into these fields before calling
// CreateSettings.
type CreateInput struct {
	// Namespace is the knowledge-base identifier (validated by CreateSettings).
	Namespace string
	// Chunking is the immutable chunking configuration chosen at creation.
	Chunking chunking.Config
	// EmbeddingModel and RerankerModel are the provider model ids; the reranker
	// may be blank.
	EmbeddingModel string
	RerankerModel  string
	// EmbeddingValues/RerankerValues are the client provider drafts
	// ({base_url, apikey}) validated by DraftConnection.
	EmbeddingValues map[string]any
	RerankerValues  map[string]any
	// VLMModel/VLMValues are the optional remote vision-language endpoint that
	// powers PDF layout, OCR, table and formula recognition and picture
	// descriptions. Both empty disables it and lets the service environment
	// (RAG_VLM_*) apply.
	VLMModel  string
	VLMValues map[string]any
	// EmbeddingBillingSource marks who pays the embedding provider for
	// document ingestion: "platform" (default) or "customer". Only an
	// administrator may set "customer"; plain request fields never bypass
	// metering by themselves.
	EmbeddingBillingSource string
	// TokenizerModel defaults to DefaultTokenizerModel when blank.
	TokenizerModel string
	// Collection overrides the generated collection name (tests/imports only).
	Collection string
	// CreatedAt defaults to time.Now() when zero.
	CreatedAt time.Time
	// EncryptionKey optionally overrides RAG_STATE_ENCRYPTION_KEY for the
	// protected_key values written into settings.
	EncryptionKey string
}

// CreateSettings validates a new knowledge base, probes both providers, and
// returns the stored settings map (create_knowledge_base minus the state-store
// existence checks, which remain in the runtime).
func CreateSettings(ctx context.Context, input CreateInput, prober Prober) (map[string]any, error) {
	if err := ValidateNamespace(input.Namespace); err != nil {
		return nil, err
	}
	if _, err := chunking.NewConfig(
		input.Chunking.Strategy, input.Chunking.ChunkSize, input.Chunking.ChunkOverlap,
	); err != nil {
		return nil, err
	}
	embeddingModel := strings.TrimSpace(input.EmbeddingModel)
	if embeddingModel == "" {
		return nil, NewValidationError(MessageEmbeddingModelRequired)
	}
	rerankerModel := strings.TrimSpace(input.RerankerModel)
	if prober == nil {
		prober = HTTPProber{}
	}

	embeddingValues := input.EmbeddingValues
	if embeddingValues == nil {
		embeddingValues = map[string]any{}
	}
	embeddingConnection, err := DraftConnection(modelapi.ModelConnection{}, embeddingValues)
	if err != nil {
		return nil, err
	}
	embeddingConnection.Model = embeddingModel
	vectorSize, err := prober.EmbeddingVectorSize(ctx, embeddingConnection)
	if err != nil {
		return nil, err
	}
	embeddingConnection.VectorSize = &vectorSize

	rerankerConnection := modelapi.ModelConnection{}
	if rerankerModel != "" {
		rerankerValues := input.RerankerValues
		if rerankerValues == nil {
			rerankerValues = map[string]any{}
		}
		rerankerConnection, err = DraftConnection(modelapi.ModelConnection{}, rerankerValues)
		if err != nil {
			return nil, err
		}
		rerankerConnection.Model = rerankerModel
		if err := prober.RerankerAccessible(ctx, rerankerConnection); err != nil {
			return nil, err
		}
	}

	storedEmbedding, err := StoredConnectionWithKey(embeddingConnection, input.EncryptionKey)
	if err != nil {
		return nil, err
	}
	storedReranker, err := StoredConnectionWithKey(rerankerConnection, input.EncryptionKey)
	if err != nil {
		return nil, err
	}
	vlmConnection, err := vlmConnectionFor(modelapi.ModelConnection{}, input.VLMValues, input.VLMModel)
	if err != nil {
		return nil, err
	}
	storedVLM, err := StoredConnectionWithKey(vlmConnection, input.EncryptionKey)
	if err != nil {
		return nil, err
	}
	collection := input.Collection
	if collection == "" {
		collection, err = NewCollectionName()
		if err != nil {
			return nil, err
		}
	}
	tokenizer := strings.TrimSpace(input.TokenizerModel)
	if tokenizer == "" {
		tokenizer = DefaultTokenizerModel
	}
	createdAt := input.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	billingSource, err := normalizeBillingSource(input.EmbeddingBillingSource)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"namespace":                input.Namespace,
		"embedding_billing_source": billingSource,
		"vector_size":              vectorSize,
		"embedding_model":          embeddingModel,
		"reranker_model":           rerankerModel,
		"embedding":                storedEmbedding,
		"reranker":                 storedReranker,
		"vlm":                      storedVLM,
		"vlm_model":                vlmConnection.Model,
		"collection":               collection,
		"tokenizer":                tokenizer,
		"strategy":                 input.Chunking.Strategy,
		"chunk_size":               input.Chunking.ChunkSize,
		"chunk_overlap":            input.Chunking.ChunkOverlap,
		"created_at":               ISOTimestamp(createdAt),
	}, nil
}

// UpdateInput is the validated, client-supplied draft for one update.
type UpdateInput struct {
	// Values may contain embedding_model, reranker_model, vlm_model, embedding,
	// reranker and vlm provider drafts. Absent keys keep the current values.
	Values map[string]any
	// Reembed confirms that a changed embedding URL/model/dimension may proceed
	// by reindexing into a staging collection.
	Reembed bool
	// EmbeddingBillingSource changes the platform/customer marker. Nil keeps
	// the stored value; "platform" is the safe default for legacy rows.
	EmbeddingBillingSource *string
	// UpdatedAt defaults to time.Now() when zero.
	UpdatedAt time.Time
	// EncryptionKey optionally overrides RAG_STATE_ENCRYPTION_KEY.
	EncryptionKey string
}

// UpdateResult is the validated outcome of UpdateSettings. The runtime reindexes
// into StagingCollection (when non-empty) and then activates it on the returned
// Settings.
type UpdateResult struct {
	// Settings is the updated stored-settings map (collection unchanged until
	// ActivateReembed runs).
	Settings map[string]any
	// StagingCollection is the unpublished collection to fill when Reembed was
	// requested; empty otherwise.
	StagingCollection string
	// Embedding is the resolved embedding connection (with the probed vector
	// size) for the reindex writer.
	Embedding modelapi.ModelConnection
	// Reranker is the resolved reranker connection; empty when disabled.
	Reranker modelapi.ModelConnection
}

// UpdateSettings validates one configuration update against the current stored
// settings and returns the updated map plus the staging collection decision
// (_publish_knowledge_base up to publication; reindex/publication stays in the
// runtime).
func UpdateSettings(
	ctx context.Context,
	current map[string]any,
	input UpdateInput,
	prober Prober,
) (UpdateResult, error) {
	if current == nil {
		return UpdateResult{}, NewValidationError(MessageKnowledgeBaseMissing)
	}
	if prober == nil {
		prober = HTTPProber{}
	}
	oldEmbedding, err := ModelConnectionForKey(current, "embedding", input.EncryptionKey)
	if err != nil {
		return UpdateResult{}, err
	}
	oldReranker, err := ModelConnectionForKey(current, "reranker", input.EncryptionKey)
	if err != nil {
		return UpdateResult{}, err
	}

	embeddingValues, err := providerValues(input.Values, "embedding")
	if err != nil {
		return UpdateResult{}, err
	}
	embedding, err := DraftConnection(oldEmbedding, embeddingValues)
	if err != nil {
		return UpdateResult{}, err
	}
	embeddingModel, err := optionalString(input.Values, "embedding_model", oldEmbedding.Model)
	if err != nil {
		return UpdateResult{}, err
	}
	embeddingModel = strings.TrimSpace(embeddingModel)
	if embeddingModel == "" {
		return UpdateResult{}, NewValidationError(MessageEmbeddingModelRequired)
	}
	embedding.Model = embeddingModel
	changed := embedding.Model != oldEmbedding.Model || embedding.BaseURL != oldEmbedding.BaseURL
	if changed && !input.Reembed {
		return UpdateResult{}, NewValidationError(MessageEmbeddingChangeRequiresReembed)
	}

	rerankerModel, err := optionalString(input.Values, "reranker_model", oldReranker.Model)
	if err != nil {
		return UpdateResult{}, err
	}
	rerankerModel = strings.TrimSpace(rerankerModel)
	reranker := modelapi.ModelConnection{}
	if rerankerModel != "" {
		rerankerValues, err := providerValues(input.Values, "reranker")
		if err != nil {
			return UpdateResult{}, err
		}
		reranker, err = DraftConnection(oldReranker, rerankerValues)
		if err != nil {
			return UpdateResult{}, err
		}
		reranker.Model = rerankerModel
		if err := prober.RerankerAccessible(ctx, reranker); err != nil {
			return UpdateResult{}, err
		}
	}

	vectorSize, err := prober.EmbeddingVectorSize(ctx, embedding)
	if err != nil {
		return UpdateResult{}, err
	}
	currentVectorSize, err := requiredInteger(current, "vector_size")
	if err != nil {
		return UpdateResult{}, err
	}
	if vectorSize != currentVectorSize && !input.Reembed {
		return UpdateResult{}, NewValidationError(MessageVectorSizeRequiresReembed)
	}
	embedding.VectorSize = &vectorSize

	storedEmbedding, err := StoredConnectionWithKey(embedding, input.EncryptionKey)
	if err != nil {
		return UpdateResult{}, err
	}
	storedReranker, err := StoredConnectionWithKey(reranker, input.EncryptionKey)
	if err != nil {
		return UpdateResult{}, err
	}
	oldVLM, err := ModelConnectionForKey(current, "vlm", input.EncryptionKey)
	if err != nil {
		return UpdateResult{}, err
	}
	vlmValues, err := providerValues(input.Values, "vlm")
	if err != nil {
		return UpdateResult{}, err
	}
	vlmModel, err := optionalString(input.Values, "vlm_model", oldVLM.Model)
	if err != nil {
		return UpdateResult{}, err
	}
	vlm, err := vlmConnectionFor(oldVLM, vlmValues, vlmModel)
	if err != nil {
		return UpdateResult{}, err
	}
	storedVLM, err := StoredConnectionWithKey(vlm, input.EncryptionKey)
	if err != nil {
		return UpdateResult{}, err
	}
	updatedAt := input.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	updated := copySettings(current)
	// Legacy OCR settings are removed on write: OCR is part of the remote
	// vision model now and the VLM settings above are the only PDF vision
	// selection. The startup migration also strips them from stored rows.
	delete(updated, "ocr")
	delete(updated, "ocr_model")
	updated["embedding"] = storedEmbedding
	updated["reranker"] = storedReranker
	updated["vlm"] = storedVLM
	updated["vlm_model"] = vlm.Model
	updated["embedding_model"] = embedding.Model
	updated["reranker_model"] = reranker.Model
	updated["vector_size"] = vectorSize
	updated["updated_at"] = ISOTimestamp(updatedAt)
	if input.EmbeddingBillingSource != nil {
		billingSource, sourceErr := normalizeBillingSource(*input.EmbeddingBillingSource)
		if sourceErr != nil {
			return UpdateResult{}, sourceErr
		}
		updated["embedding_billing_source"] = billingSource
	} else if _, exists := updated["embedding_billing_source"]; !exists {
		updated["embedding_billing_source"] = "platform"
	}

	staging := ""
	if input.Reembed {
		staging, err = NewCollectionName()
		if err != nil {
			return UpdateResult{}, err
		}
	}
	return UpdateResult{
		Settings:          updated,
		StagingCollection: staging,
		Embedding:         embedding,
		Reranker:          reranker,
	}, nil
}

// ActivateReembed applies a successfully published staging collection to the
// settings map (_publish_knowledge_base's updated.update(collection=...,
// reembedded_at=...)).
func ActivateReembed(settings map[string]any, collection string, reembeddedAt time.Time) map[string]any {
	activated := copySettings(settings)
	activated["collection"] = collection
	activated["reembedded_at"] = ISOTimestamp(reembeddedAt)
	return activated
}

// normalizeBillingSource validates the model-connection billing owner marker.
// Empty means the platform default; only the two explicit values are accepted
// so a typo cannot silently bypass metering.
func normalizeBillingSource(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "platform":
		return "platform", nil
	case "customer":
		return "customer", nil
	default:
		return "", NewValidationError("Embedding 计费归属只能是 platform 或 customer")
	}
}

// providerValues returns one nested provider draft object; absent or null is
// treated as {} like Python's values.get(kind, {}).
func providerValues(values map[string]any, kind string) (map[string]any, error) {
	if values == nil {
		return map[string]any{}, nil
	}
	raw, ok := values[kind]
	if !ok || raw == nil {
		return map[string]any{}, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, NewValidationError("模型服务配置格式无效")
	}
	return object, nil
}

// optionalString reads one optional string value with a Python-style fallback.
func optionalString(values map[string]any, key, fallback string) (string, error) {
	raw, ok := values[key]
	if !ok {
		return fallback, nil
	}
	text, ok := raw.(string)
	if !ok {
		return "", NewValidationError("模型名称必须是字符串")
	}
	return text, nil
}

func copySettings(settings map[string]any) map[string]any {
	copied := make(map[string]any, len(settings)+3)
	for key, value := range settings {
		copied[key] = value
	}
	return copied
}
