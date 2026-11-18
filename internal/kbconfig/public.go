// This file defines the credential-free public projection of stored
// knowledge-base settings.

package kbconfig

// PublicSettings returns the credential-free projection of stored settings
// (rag_mcp.knowledge_bases.public_settings): the protected provider objects are
// replaced with {base_url, api_key_set} per kind while every other field
// (namespace, collection, vector_size, model names such as vlm_model and
// graph_model, the graph_enabled flag, tokenizer, strategy, chunk fields,
// timestamps, version) is preserved exactly.
//
// The "graph" provider is projected like the other CredentialKinds: only
// base_url/api_key_set survive, so a graph API key is never exposed. A legacy
// row without graph_enabled stays without it here; readers use GraphEnabled for
// the false default and the stored settings shape is never rewritten.
//
// internal/state.CachedReadViews keeps an equivalent copy for cached read
// models; the two projections must stay in sync.
func PublicSettings(settings map[string]any) map[string]any {
	public := make(map[string]any, len(settings))
	for key, value := range settings {
		if IsKind(key) {
			continue
		}
		public[key] = value
	}
	for _, kind := range CredentialKinds {
		provider := map[string]any{}
		if raw, ok := settings[kind].(map[string]any); ok {
			provider = raw
		}
		baseURL := any("")
		if raw, ok := provider["base_url"]; ok {
			baseURL = raw
		}
		public[kind] = map[string]any{
			"base_url":    baseURL,
			"api_key_set": pythonTruthy(provider["protected_key"]),
		}
	}
	return public
}

// PublicSettingsList applies PublicSettings to every knowledge-base record, as
// the admin list endpoint and /v1/knowledge-base expose them.
func PublicSettingsList(settings []map[string]any) []map[string]any {
	public := make([]map[string]any, 0, len(settings))
	for _, item := range settings {
		public = append(public, PublicSettings(item))
	}
	return public
}

// pythonTruthy mirrors Python bool() for the scalar values stored in settings.
func pythonTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	default:
		return true
	}
}
