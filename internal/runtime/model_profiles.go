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

// ModelProfileViewer is the Console account performing a model-configuration
// operation. Profiles are per-account: everyone manages their own list, and an
// administrator may additionally READ every account's list for support without
// being able to use, edit, delete or reveal another account's profile.
type ModelProfileViewer struct {
	UserID  string
	IsAdmin bool
}

// owns reports whether the viewer is the account that owns the row.
func (v ModelProfileViewer) owns(record state.ModelProfile) bool {
	return v.UserID != "" && record.OwnerUserID == v.UserID
}

// mayRead reports whether the viewer may see the row: its owner always, an
// administrator for the read-only全量视图. Rows without an owner (a legacy row
// that predates ownership, before the migration assigned it) stay
// administrator-only.
func (v ModelProfileViewer) mayRead(record state.ModelProfile) bool {
	return v.owns(record) || v.IsAdmin
}

// modelProfileProjection renders one row for the Console: the credential-free
// projection plus the ownership facts the frontend needs to keep selection
// isolated (mine) and to label the administrator's read-only view (owner).
func modelProfileProjection(record state.ModelProfile, viewer ModelProfileViewer) map[string]any {
	item := kbconfig.PublicModelProfile(modelProfileStored(record))
	item["owner_user_id"] = record.OwnerUserID
	item["owner_username"] = record.OwnerUsername
	item["mine"] = viewer.owns(record)
	return item
}

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

// ListModelProfiles returns the saved model configurations the viewer may see,
// as the credential-free Console projection ordered by kind then display name.
// A normal account only ever receives its own rows; an administrator receives
// every account's rows for the read-only overview, each carrying owner_user_id,
// owner_username and mine so the Console can keep selection isolated.
func (s *Service) ListModelProfiles(ctx context.Context, viewerUserID string, isAdmin bool) ([]map[string]any, error) {
	viewer := ModelProfileViewer{UserID: viewerUserID, IsAdmin: isAdmin}
	var (
		records []state.ModelProfile
		err     error
	)
	if viewer.IsAdmin {
		records, err = s.stores.ModelProfiles.List(ctx)
	} else {
		records, err = s.stores.ModelProfiles.ListByOwner(ctx, viewer.UserID)
	}
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: modelProfileReadError}
	}
	items := make([]map[string]any, 0, len(records))
	for _, record := range records {
		items = append(items, modelProfileProjection(record, viewer))
	}
	return items, nil
}

// ModelProfile returns one saved model configuration as the credential-free
// Console projection. Another account's row (and a missing one) both answer with
// the same 404-shaped validation error so the id space never leaks existence.
func (s *Service) ModelProfile(ctx context.Context, viewerUserID string, isAdmin bool, id string) (map[string]any, error) {
	viewer := ModelProfileViewer{UserID: viewerUserID, IsAdmin: isAdmin}
	record, err := s.stores.ModelProfiles.Get(ctx, id)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: modelProfileReadError}
	}
	if record == nil || !viewer.mayRead(*record) {
		return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageModelProfileMissing}
	}
	return modelProfileProjection(*record, viewer), nil
}

// CreateModelProfile validates one saved model configuration, probes the
// provider (embedding records the dense dimension, reranker validates access,
// VLM and JEV are not probed), then persists it under the viewer's account.
// Duplicate (owner, kind, name) triples are a conflict; the key is stored
// protected with the shared state encryption key.
func (s *Service) CreateModelProfile(ctx context.Context, viewerUserID string, isAdmin bool, draft kbconfig.ModelProfileDraft) (map[string]any, error) {
	viewer := ModelProfileViewer{UserID: viewerUserID, IsAdmin: isAdmin}
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
		OwnerUserID:  viewer.UserID,
		ProtectedKey: protected,
		VectorSize:   vectorSize,
	})
	if err != nil {
		return nil, modelProfileWriteError(err)
	}
	return modelProfileProjection(record, viewer), nil
}

// UpdateModelProfile rewrites one saved model configuration. The kind is
// immutable; absent fields keep their stored values. The provider is probed
// only when the provider-facing connection (base_url, model or apikey) actually
// changed, so a rename or an unchanged save never calls the provider and keeps
// the recorded embedding dimension.
func (s *Service) UpdateModelProfile(ctx context.Context, viewerUserID string, isAdmin bool, id string, draft kbconfig.ModelProfileDraft) (map[string]any, error) {
	viewer := ModelProfileViewer{UserID: viewerUserID, IsAdmin: isAdmin}
	current, err := s.stores.ModelProfiles.Get(ctx, id)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: modelProfileReadError}
	}
	// Editing someone else's profile is indistinguishable from a missing one:
	// an administrator may read every list but only ever writes its own rows.
	if current == nil || !viewer.owns(*current) {
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
	return modelProfileProjection(record, viewer), nil
}

// DeleteModelProfile removes one saved model configuration the viewer owns.
// Later profile edits or deletions never touch knowledge bases that copied the
// configuration.
func (s *Service) DeleteModelProfile(ctx context.Context, viewerUserID string, isAdmin bool, id string) (bool, error) {
	viewer := ModelProfileViewer{UserID: viewerUserID, IsAdmin: isAdmin}
	current, err := s.stores.ModelProfiles.Get(ctx, id)
	if err != nil {
		return false, &apierrors.KnowledgeError{Message: modelProfileReadError}
	}
	// A missing row stays an idempotent "nothing deleted"; another account's row
	// is reported like a missing one so the id space never leaks existence.
	if current == nil {
		return false, nil
	}
	if !viewer.owns(*current) {
		return false, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageModelProfileMissing}
	}
	deleted, err := s.stores.ModelProfiles.Delete(ctx, id)
	if err != nil {
		return false, &apierrors.KnowledgeError{Message: "模型配置无法删除"}
	}
	return deleted, nil
}

// ModelProfileKey reveals the decrypted API key of one saved model configuration
// for its owner (the Console loads it when a model is selected). An
// administrator's read-only view never reveals another account's key, and the
// key is never part of any list/details response.
func (s *Service) ModelProfileKey(ctx context.Context, viewerUserID string, isAdmin bool, id string) (string, error) {
	viewer := ModelProfileViewer{UserID: viewerUserID, IsAdmin: isAdmin}
	record, err := s.stores.ModelProfiles.Get(ctx, id)
	if err != nil {
		return "", &apierrors.KnowledgeError{Message: modelProfileReadError}
	}
	if record == nil || !viewer.owns(*record) {
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
// configurations never call the provider because they have no such route. An
// administrator may list another account's models (the probe only returns model
// ids), but never its key.
func (s *Service) ModelProfileModels(ctx context.Context, viewerUserID string, isAdmin bool, id string) ([]string, error) {
	viewer := ModelProfileViewer{UserID: viewerUserID, IsAdmin: isAdmin}
	record, err := s.stores.ModelProfiles.Get(ctx, id)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: modelProfileReadError}
	}
	if record == nil || !viewer.mayRead(*record) {
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
