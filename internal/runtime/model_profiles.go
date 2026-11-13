package runtime

import (
	"context"
	"errors"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/kbconfig"
	"github.com/Guan0923/xinghe_rag/internal/modelapi"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// modelProfileReadError is the safe fallback for state-database failures while
// reading the saved model configuration list.
const modelProfileReadError = "模型配置读取失败"

// modelProfileStored converts one state row into the transport-neutral
// projection shared with the validation layer.
func modelProfileStored(record state.ModelProfile) kbconfig.ModelProfileStored {
	return kbconfig.ModelProfileStored{
		ID:           record.ID,
		Kind:         record.Kind,
		Name:         record.Name,
		BaseURL:      record.BaseURL,
		Model:        record.Model,
		ProtectedKey: record.ProtectedKey,
		VectorSize:   record.VectorSize,
		CreatedAt:    record.CreatedAt,
		UpdatedAt:    record.UpdatedAt,
	}
}

// modelProfileProber is the production provider probe used before persisting a
// model configuration.
func modelProfileProber() kbconfig.Prober {
	return kbconfig.HTTPProber{Timeout: modelapi.DefaultTimeout}
}

// ListModelProfiles returns every saved model configuration as the
// credential-free Console projection, ordered by kind then display name.
func (s *Service) ListModelProfiles(ctx context.Context) ([]map[string]any, error) {
	records, err := s.stores.ModelProfiles.List(ctx)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: modelProfileReadError}
	}
	items := make([]map[string]any, 0, len(records))
	for _, record := range records {
		items = append(items, kbconfig.PublicModelProfile(modelProfileStored(record)))
	}
	return items, nil
}

// ModelProfile returns one saved model configuration as the credential-free
// Console projection. A missing record keeps the 404-shaped validation category
// the HTTP layer renders for the details/key/models routes.
func (s *Service) ModelProfile(ctx context.Context, id string) (map[string]any, error) {
	record, err := s.stores.ModelProfiles.Get(ctx, id)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: modelProfileReadError}
	}
	if record == nil {
		return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageModelProfileMissing}
	}
	return kbconfig.PublicModelProfile(modelProfileStored(*record)), nil
}

// CreateModelProfile validates one saved model configuration, probes the
// provider (embedding records the dense dimension, reranker validates access,
// VLM and JEV are not probed), then persists it. Duplicate (kind, name) pairs
// are a conflict; the key is stored protected with the shared state encryption
// key.
func (s *Service) CreateModelProfile(ctx context.Context, draft kbconfig.ModelProfileDraft) (map[string]any, error) {
	connection, name, err := kbconfig.DraftModelProfile(modelapi.ModelConnection{}, draft.Kind, draft.Name, draft)
	if err != nil {
		return nil, adminError(err)
	}
	vectorSize, err := kbconfig.ProbeModelProfile(ctx, connection, draft.Kind, modelProfileProber())
	if err != nil {
		return nil, adminError(err)
	}
	baseURL, protected, err := kbconfig.StoredModelProfileConnection(connection, s.stores.Settings.EncryptionKey)
	if err != nil {
		return nil, adminError(err)
	}
	record, err := s.stores.ModelProfiles.Create(ctx, state.ModelProfile{
		Kind:         draft.Kind,
		Name:         name,
		BaseURL:      baseURL,
		Model:        connection.Model,
		ProtectedKey: protected,
		VectorSize:   vectorSize,
	})
	if err != nil {
		return nil, modelProfileWriteError(err)
	}
	return kbconfig.PublicModelProfile(modelProfileStored(record)), nil
}

// UpdateModelProfile rewrites one saved model configuration. The kind is
// immutable; absent fields keep their stored values. The provider is probed
// only when the provider-facing connection (base_url, model or apikey) actually
// changed, so a rename or an unchanged save never calls the provider and keeps
// the recorded embedding dimension.
func (s *Service) UpdateModelProfile(ctx context.Context, id string, draft kbconfig.ModelProfileDraft) (map[string]any, error) {
	current, err := s.stores.ModelProfiles.Get(ctx, id)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: modelProfileReadError}
	}
	if current == nil {
		return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageModelProfileMissing}
	}
	encryptionKey := s.stores.Settings.EncryptionKey
	currentStored := modelProfileStored(*current)
	currentConnection, err := kbconfig.ModelProfileConnection(currentStored, encryptionKey)
	if err != nil {
		return nil, adminError(err)
	}
	// The transport patch carries only the fields the Console submitted; empty
	// values keep the stored ones (kind is never editable).
	merged := draft
	if strings.TrimSpace(merged.Name) == "" {
		merged.Name = currentStored.Name
	}
	if strings.TrimSpace(merged.BaseURL) == "" {
		merged.BaseURL = currentConnection.BaseURL
	}
	if strings.TrimSpace(merged.Model) == "" {
		merged.Model = currentConnection.Model
	}
	connection, name, err := kbconfig.DraftModelProfile(currentConnection, currentStored.Kind, merged.Name, merged)
	if err != nil {
		return nil, adminError(err)
	}
	vectorSize := currentStored.VectorSize
	if connection.BaseURL != currentConnection.BaseURL ||
		connection.Model != currentConnection.Model ||
		connection.APIKey != currentConnection.APIKey {
		vectorSize, err = kbconfig.ProbeModelProfile(ctx, connection, currentStored.Kind, modelProfileProber())
		if err != nil {
			return nil, adminError(err)
		}
	}
	baseURL, protected, err := kbconfig.StoredModelProfileConnection(connection, encryptionKey)
	if err != nil {
		return nil, adminError(err)
	}
	record, err := s.stores.ModelProfiles.Update(ctx, state.ModelProfile{
		ID:           id,
		Name:         name,
		BaseURL:      baseURL,
		Model:        connection.Model,
		ProtectedKey: protected,
		VectorSize:   vectorSize,
	})
	if err != nil {
		return nil, modelProfileWriteError(err)
	}
	return kbconfig.PublicModelProfile(modelProfileStored(record)), nil
}

// DeleteModelProfile removes one saved model configuration. Later profile edits
// or deletions never touch knowledge bases that copied the configuration.
func (s *Service) DeleteModelProfile(ctx context.Context, id string) (bool, error) {
	deleted, err := s.stores.ModelProfiles.Delete(ctx, id)
	if err != nil {
		return false, &apierrors.KnowledgeError{Message: "模型配置无法删除"}
	}
	return deleted, nil
}

// ModelProfileKey reveals the decrypted API key of one saved model
// configuration for the local admin UI (the Console loads it when a model is
// selected). The key is never part of any list/details response.
func (s *Service) ModelProfileKey(ctx context.Context, id string) (string, error) {
	record, err := s.stores.ModelProfiles.Get(ctx, id)
	if err != nil {
		return "", &apierrors.KnowledgeError{Message: modelProfileReadError}
	}
	if record == nil {
		return "", &apierrors.KnowledgeValidationError{Message: kbconfig.MessageModelProfileMissing}
	}
	connection, err := kbconfig.ModelProfileConnection(modelProfileStored(*record), s.stores.Settings.EncryptionKey)
	if err != nil {
		return "", adminError(err)
	}
	return connection.APIKey, nil
}

// ModelProfileModels lists the provider models of one saved model configuration
// through its stored connection, with no client overrides. A provider without a
// /models route yields an empty list, exactly like the knowledge-base form; JEV
// configurations never call the provider because they have no such route.
func (s *Service) ModelProfileModels(ctx context.Context, id string) ([]string, error) {
	record, err := s.stores.ModelProfiles.Get(ctx, id)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: modelProfileReadError}
	}
	if record == nil {
		return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageModelProfileMissing}
	}
	if record.Kind == "jev" {
		// JEV has no OpenAI-compatible /models route; the model id is entered
		// by hand in the Console.
		return []string{}, nil
	}
	connection, err := kbconfig.ModelProfileConnection(modelProfileStored(*record), s.stores.Settings.EncryptionKey)
	if err != nil {
		return nil, adminError(err)
	}
	models, err := kbconfig.ModelsForConnection(ctx, connection, nil)
	if err != nil {
		return nil, adminError(err)
	}
	if models == nil {
		models = []string{}
	}
	return models, nil
}

// modelProfileWriteError maps state-store failures of a model-configuration
// write: a duplicate (kind, name) pair is an HTTP 409 conflict, a missing row an
// HTTP 404-shaped validation failure, and any other store failure a 503.
func modelProfileWriteError(err error) error {
	var conflict *state.StateConflict
	if errors.As(err, &conflict) {
		return &apierrors.KnowledgeConflictError{Message: kbconfig.MessageModelProfileExists}
	}
	var lookup *state.LookupError
	if errors.As(err, &lookup) {
		return &apierrors.KnowledgeValidationError{Message: kbconfig.MessageModelProfileMissing}
	}
	var stateError *state.StateError
	if errors.As(err, &stateError) {
		return &apierrors.KnowledgeError{Message: stateError.Message}
	}
	return adminError(err)
}
