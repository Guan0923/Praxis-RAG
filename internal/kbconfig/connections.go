package kbconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// Kinds lists the supported saved-model kinds (rag_mcp.model_settings.KINDS).
//
// "vlm" is the remote vision-language endpoint that powers PDF layout, OCR,
// table and formula recognition and picture descriptions; it is optional per
// knowledge base and falls back to the service environment (RAG_VLM_*) when
// unset. "jev" is the TypeSafe System One decision-model configuration kept for
// later use by the Console; it is not a knowledge-base model.
var Kinds = []string{"embedding", "reranker", "vlm", "jev"}

// CredentialKinds lists the kinds whose settings are stored as
// {base_url, protected_key} pairs and replaced by the credential-free public
// view.
var CredentialKinds = []string{"embedding", "reranker", "vlm"}

// IsKind reports whether kind is one of Kinds.
func IsKind(kind string) bool {
	for _, candidate := range Kinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// ModelConnectionFor reads one provider connection from stored settings,
// decrypting the protected key with the shared state encryption key from the
// environment (rag_mcp.knowledge_bases.model_connection).
//
// The embedding connection carries the configured vector_size; the reranker
// connection never does.
func ModelConnectionFor(settings map[string]any, kind string) (modelapi.ModelConnection, error) {
	return ModelConnectionForKey(settings, kind, "")
}

// ModelConnectionForKey is ModelConnectionFor with an explicit encryption key.
// An empty key falls back to RAG_STATE_ENCRYPTION_KEY, matching Python.
func ModelConnectionForKey(settings map[string]any, kind, encryptionKey string) (modelapi.ModelConnection, error) {
	if !IsKind(kind) {
		return modelapi.ModelConnection{}, fmt.Errorf("unknown model kind %q", kind)
	}
	provider, err := providerSettings(settings, kind)
	if err != nil {
		return modelapi.ModelConnection{}, err
	}
	baseURL := ""
	if raw, ok := provider["base_url"]; ok && raw != nil {
		text, ok := raw.(string)
		if !ok {
			return modelapi.ModelConnection{}, fmt.Errorf("knowledge-base settings %q base_url must be a string", kind)
		}
		baseURL = text
	}
	protected := ""
	if raw, ok := provider["protected_key"]; ok && raw != nil {
		text, ok := raw.(string)
		if !ok {
			return modelapi.ModelConnection{}, fmt.Errorf("knowledge-base settings %q protected_key must be a string", kind)
		}
		protected = text
	}
	apiKey, err := state.Unprotect(protected, encryptionKey)
	if err != nil {
		return modelapi.ModelConnection{}, asModelAPIError(err)
	}
	model, err := requiredString(settings, kind+"_model")
	if err != nil {
		// Knowledge bases created before the remote VLM settings existed have
		// no vlm_model; treat that as "not configured" instead of corrupt
		// settings.
		if kind != "vlm" {
			return modelapi.ModelConnection{}, err
		}
		model = ""
	}
	connection := modelapi.ModelConnection{BaseURL: baseURL, Model: model, APIKey: apiKey}
	if kind == "embedding" {
		vectorSize, err := requiredInteger(settings, "vector_size")
		if err != nil {
			return modelapi.ModelConnection{}, err
		}
		connection.VectorSize = &vectorSize
	}
	return connection, nil
}

// DraftConnection validates the client-supplied base_url/apikey drafts on top
// of the current connection (rag_mcp.model_settings.draft_connection).
//
// Rules reproduced from Python:
//   - the base URL is normalized and must be HTTP(S) without credentials,
//     query parameters or fragments;
//   - an explicit apikey must be a string without CR/LF, otherwise
//     "apikey 格式无效";
//   - when base_url is absent or unchanged the current key is preserved;
//     changing the base URL clears it unless a key is supplied explicitly.
func DraftConnection(current modelapi.ModelConnection, values map[string]any) (modelapi.ModelConnection, error) {
	rawBaseURL := any(current.BaseURL)
	if values != nil {
		if value, ok := values["base_url"]; ok {
			rawBaseURL = value
		}
	}
	baseURL, err := modelapi.NormalizeBaseURL(pythonString(rawBaseURL))
	if err != nil {
		return modelapi.ModelConnection{}, err
	}
	key := ""
	explicitKey, hasKey := any(nil), false
	if values != nil {
		explicitKey, hasKey = values["apikey"]
	}
	if hasKey {
		text, ok := explicitKey.(string)
		if !ok || strings.ContainsAny(text, "\r\n") {
			return modelapi.ModelConnection{}, &modelapi.ModelAPIError{Message: "apikey 格式无效"}
		}
		key = text
	} else if baseURL == current.BaseURL {
		key = current.APIKey
	}
	return modelapi.ModelConnection{BaseURL: baseURL, APIKey: key}, nil
}

// StoredConnection renders the persisted provider object with the API key
// protected as an enc:v1 value (rag_mcp.knowledge_bases.stored_connection).
func StoredConnection(connection modelapi.ModelConnection) (map[string]any, error) {
	return StoredConnectionWithKey(connection, "")
}

// StoredConnectionWithKey is StoredConnection with an explicit encryption key.
// An empty key falls back to RAG_STATE_ENCRYPTION_KEY, matching Python.
func StoredConnectionWithKey(connection modelapi.ModelConnection, encryptionKey string) (map[string]any, error) {
	protected, err := state.Protect(connection.APIKey, encryptionKey)
	if err != nil {
		return nil, asModelAPIError(err)
	}
	return map[string]any{"base_url": connection.BaseURL, "protected_key": protected}, nil
}

// APIKeyFor returns the decrypted provider API key for one kind. It backs the
// local admin key-reveal endpoint.
func APIKeyFor(settings map[string]any, kind string) (string, error) {
	connection, err := ModelConnectionFor(settings, kind)
	if err != nil {
		return "", err
	}
	return connection.APIKey, nil
}

// providerSettings returns the nested provider object for one kind; a missing
// or null provider is treated as {} like settings.get(kind, {}).
func providerSettings(settings map[string]any, kind string) (map[string]any, error) {
	raw, ok := settings[kind]
	if !ok || raw == nil {
		return map[string]any{}, nil
	}
	provider, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("knowledge-base settings %q must be an object", kind)
	}
	return provider, nil
}

// requiredString reads one required string setting. Python raises KeyError when
// it is missing; corrupt configuration is reported instead of guessed.
func requiredString(settings map[string]any, key string) (string, error) {
	raw, ok := settings[key]
	if !ok {
		return "", fmt.Errorf("knowledge-base settings are missing %q", key)
	}
	text, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("knowledge-base settings %q must be a string", key)
	}
	return text, nil
}

// requiredInteger reads one required integral setting stored by JSON decoding
// (int from Go maps, float64 from the state store, json.Number from APIs).
func requiredInteger(settings map[string]any, key string) (int, error) {
	raw, ok := settings[key]
	if !ok {
		return 0, fmt.Errorf("knowledge-base settings are missing %q", key)
	}
	value, ok := integerValue(raw)
	if !ok {
		return 0, fmt.Errorf("knowledge-base settings %q must be an integer", key)
	}
	return value, nil
}

// integerValue accepts only integral numeric representations.
func integerValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case float32:
		converted := float64(typed)
		if converted != math.Trunc(converted) {
			return 0, false
		}
		return int(converted), true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		text := string(typed)
		if strings.ContainsAny(text, ".eE") {
			return 0, false
		}
		parsed, err := strconv.Atoi(text)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// asModelAPIError converts the shared-state credential error into the
// ModelAPIError type Python raises from crypto.protect/unprotect.
func asModelAPIError(err error) error {
	var cryptoError *state.CryptoError
	if errors.As(err, &cryptoError) {
		return &modelapi.ModelAPIError{Message: cryptoError.Message}
	}
	return err
}

// pythonString mirrors Python's str() for the scalar values accepted from
// client JSON (base URLs), including None -> "None".
func pythonString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case string:
		return typed
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case json.Number:
		return string(typed)
	default:
		return fmt.Sprint(typed)
	}
}

// vlmConnectionFor resolves the optional remote VLM provider. The URL and the
// model name must be configured together; clearing both disables the provider
// and lets the service environment (RAG_VLM_*) apply again.
func vlmConnectionFor(current modelapi.ModelConnection, values map[string]any, model string) (modelapi.ModelConnection, error) {
	model = strings.TrimSpace(model)
	rawURL := current.BaseURL
	if values != nil {
		if raw, ok := values["base_url"]; ok {
			text, ok := raw.(string)
			if !ok {
				return modelapi.ModelConnection{}, NewValidationError("VLM 服务配置格式无效")
			}
			rawURL = text
		}
	}
	baseURL := strings.TrimSpace(rawURL)
	if baseURL == "" && model == "" {
		return modelapi.ModelConnection{}, nil
	}
	if baseURL == "" {
		return modelapi.ModelConnection{}, NewValidationError(MessageVLMURLRequired)
	}
	if model == "" {
		return modelapi.ModelConnection{}, NewValidationError(MessageVLMModelRequired)
	}
	connection, err := DraftConnection(current, values)
	if err != nil {
		return modelapi.ModelConnection{}, err
	}
	connection.Model = model
	return connection, nil
}
