package kbconfig

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/Guan0923/xinghe_rag/internal/modelapi"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// Model-profile field limits, also enforced by the HTTP request validation so a
// request rejected there matches the storage constraints (and the FastAPI
// messages the Console displays).
const (
	// ModelProfileNameMaxLength is the maximum display-name length in runes.
	ModelProfileNameMaxLength = 80
	// ModelProfileModelMaxLength is the maximum provider model-id length in
	// runes.
	ModelProfileModelMaxLength = 512
)

// ModelProfileDraft is the client-supplied draft for one saved model
// configuration. HasAPIKey distinguishes "apikey absent" (keep the stored key
// while base_url is unchanged, DraftConnection semantics) from "apikey present"
// (replace it, even with an empty value).
type ModelProfileDraft struct {
	Kind    string
	Name    string
	BaseURL string
	Model   string
	APIKey  string
	// HasAPIKey reports whether the request carried an apikey field.
	HasAPIKey bool
}

// ModelProfileStored is one persisted model configuration without the
// transport-specific state dependency.
type ModelProfileStored struct {
	ID           string
	Kind         string
	Name         string
	BaseURL      string
	Model        string
	ProtectedKey string
	VectorSize   *int
	CreatedAt    string
	UpdatedAt    string
}

// DraftModelProfile validates one client draft on top of the current connection
// (empty for create) and returns the resolved connection plus the trimmed
// display name.
//
// Rules: kind must be one of Kinds; the name is trimmed to 1..80 runes; the
// model id is trimmed to 1..512 runes; the base URL is normalized by
// DraftConnection (an absent base_url keeps the current value, and an explicit
// apikey replaces the stored one). A configuration without a service URL is
// rejected with MessageModelServiceURLRequired.
func DraftModelProfile(
	current modelapi.ModelConnection,
	kind, name string,
	draft ModelProfileDraft,
) (modelapi.ModelConnection, string, error) {
	if !IsKind(kind) {
		return modelapi.ModelConnection{}, "", NewValidationError(MessageModelKindInvalid)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return modelapi.ModelConnection{}, "", NewValidationError(MessageModelProfileNameRequired)
	}
	if utf8.RuneCountInString(name) > ModelProfileNameMaxLength {
		return modelapi.ModelConnection{}, "", NewValidationError(MessageModelProfileNameTooLong)
	}
	model := strings.TrimSpace(draft.Model)
	if model == "" {
		return modelapi.ModelConnection{}, "", NewValidationError(MessageModelModelRequired)
	}
	if utf8.RuneCountInString(model) > ModelProfileModelMaxLength {
		return modelapi.ModelConnection{}, "", NewValidationError(MessageModelModelTooLong)
	}
	baseURL := current.BaseURL
	if strings.TrimSpace(draft.BaseURL) != "" {
		baseURL = draft.BaseURL
	}
	if strings.TrimSpace(baseURL) == "" {
		return modelapi.ModelConnection{}, "", NewValidationError(MessageModelServiceURLRequired)
	}
	values := map[string]any{"base_url": baseURL}
	if draft.HasAPIKey {
		values["apikey"] = draft.APIKey
	}
	connection, err := DraftConnection(current, values)
	if err != nil {
		return modelapi.ModelConnection{}, "", err
	}
	if strings.TrimSpace(connection.BaseURL) == "" {
		return modelapi.ModelConnection{}, "", NewValidationError(MessageModelServiceURLRequired)
	}
	connection.Model = model
	return connection, name, nil
}

// ProbeModelProfile validates a saved model configuration against its provider
// before it is persisted. An embedding probe records the produced dense
// dimension; a reranker probe only checks accessibility; VLM and JEV endpoints
// have no cheap probe (JEV is stored for Console configuration only), so both
// are stored as entered. A nil prober uses HTTPProber.
func ProbeModelProfile(
	ctx context.Context,
	connection modelapi.ModelConnection,
	kind string,
	prober Prober,
) (*int, error) {
	if prober == nil {
		prober = HTTPProber{}
	}
	switch kind {
	case "embedding":
		vectorSize, err := prober.EmbeddingVectorSize(ctx, connection)
		if err != nil {
			return nil, err
		}
		return &vectorSize, nil
	case "reranker":
		if err := prober.RerankerAccessible(ctx, connection); err != nil {
			return nil, err
		}
		return nil, nil
	case "vlm", "jev":
		return nil, nil
	default:
		return nil, NewValidationError(MessageModelKindInvalid)
	}
}

// ModelProfileConnection decrypts one stored model configuration into a provider
// connection. Embedding profiles carry their recorded vector size; the other
// kinds never do.
func ModelProfileConnection(record ModelProfileStored, encryptionKey string) (modelapi.ModelConnection, error) {
	apiKey, err := state.Unprotect(record.ProtectedKey, encryptionKey)
	if err != nil {
		return modelapi.ModelConnection{}, asModelAPIError(err)
	}
	connection := modelapi.ModelConnection{
		BaseURL: record.BaseURL,
		Model:   record.Model,
		APIKey:  apiKey,
	}
	if record.Kind == "embedding" && record.VectorSize != nil {
		vectorSize := *record.VectorSize
		connection.VectorSize = &vectorSize
	}
	return connection, nil
}

// StoredModelProfileConnection renders the persisted base URL and the protected
// (enc:v1) API key of one model configuration, mirroring StoredConnection.
func StoredModelProfileConnection(connection modelapi.ModelConnection, encryptionKey string) (string, string, error) {
	protected, err := state.Protect(connection.APIKey, encryptionKey)
	if err != nil {
		return "", "", asModelAPIError(err)
	}
	return connection.BaseURL, protected, nil
}

// PublicModelProfile returns the credential-free projection of one saved model
// configuration. The protected key is reduced to api_key_set and never exposed.
func PublicModelProfile(record ModelProfileStored) map[string]any {
	var vectorSize any
	if record.VectorSize != nil {
		vectorSize = *record.VectorSize
	}
	return map[string]any{
		"id":          record.ID,
		"kind":        record.Kind,
		"name":        record.Name,
		"base_url":    record.BaseURL,
		"model":       record.Model,
		"api_key_set": record.ProtectedKey != "",
		"vector_size": vectorSize,
		"created_at":  record.CreatedAt,
		"updated_at":  record.UpdatedAt,
	}
}
