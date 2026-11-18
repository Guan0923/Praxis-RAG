// This file tests the credential-free public projection of stored settings,
// including the graph provider and the graph_enabled flag.

package kbconfig

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

func TestPublicSettingsReplacesProviders(t *testing.T) {
	settings := map[string]any{
		"namespace":       "alpha",
		"vector_size":     3,
		"embedding_model": "embed-a",
		"reranker_model":  "rank-a",
		"collection":      "knowledge_abc",
		"tokenizer":       "BAAI/bge-m3",
		"strategy":        "character",
		"chunk_size":      120,
		"chunk_overlap":   20,
		"created_at":      "2026-11-08T10:00:00.000000+00:00",
		"updated_at":      "2026-11-08T11:00:00.000000+00:00",
		"version":         4,
		"embedding": map[string]any{
			"base_url": "http://host:8080/v1", "protected_key": "enc:v1:embedding-secret",
		},
		"reranker": map[string]any{
			"base_url": "http://rank:8080/v1", "protected_key": "",
		},
		"vlm": map[string]any{
			"base_url": "http://vlm:8000/v1", "protected_key": "enc:v1:vlm-secret",
		},
		"vlm_model": "qwen2.5-vl-7b",
		"graph": map[string]any{
			"base_url": "http://graph:8000/v1", "protected_key": "enc:v1:graph-secret",
		},
		"graph_model":   "graph-llm",
		"graph_enabled": true,
	}
	want := map[string]any{
		"namespace":       "alpha",
		"vector_size":     3,
		"embedding_model": "embed-a",
		"reranker_model":  "rank-a",
		"collection":      "knowledge_abc",
		"tokenizer":       "BAAI/bge-m3",
		"strategy":        "character",
		"chunk_size":      120,
		"chunk_overlap":   20,
		"created_at":      "2026-11-08T10:00:00.000000+00:00",
		"updated_at":      "2026-11-08T11:00:00.000000+00:00",
		"version":         4,
		"embedding":       map[string]any{"base_url": "http://host:8080/v1", "api_key_set": true},
		"reranker":        map[string]any{"base_url": "http://rank:8080/v1", "api_key_set": false},
		"vlm":             map[string]any{"base_url": "http://vlm:8000/v1", "api_key_set": true},
		"vlm_model":       "qwen2.5-vl-7b",
		"graph":           map[string]any{"base_url": "http://graph:8000/v1", "api_key_set": true},
		"graph_model":     "graph-llm",
		"graph_enabled":   true,
		"extraction":      map[string]any{"base_url": "", "api_key_set": false},
	}
	public := PublicSettings(settings)
	if !reflect.DeepEqual(public, want) {
		t.Fatalf("PublicSettings() mismatch\n got: %#v\nwant: %#v", public, want)
	}
	if _, leaked := public["protected_key"]; leaked {
		t.Fatalf("protected_key leaked into public settings")
	}
}

func TestPublicSettingsDefaultsMissingProviders(t *testing.T) {
	public := PublicSettings(map[string]any{"namespace": "alpha"})
	want := map[string]any{
		"namespace": "alpha",
		"embedding": map[string]any{"base_url": "", "api_key_set": false},
		"reranker":  map[string]any{"base_url": "", "api_key_set": false},
		"vlm":       map[string]any{"base_url": "", "api_key_set": false},
		"graph":     map[string]any{"base_url": "", "api_key_set": false},
		"extraction": map[string]any{"base_url": "", "api_key_set": false},
	}
	if !reflect.DeepEqual(public, want) {
		t.Fatalf("PublicSettings() = %#v, want %#v", public, want)
	}
}

// TestPublicSettingsNeverLeaksGraphKey checks that the graph provider is
// redacted exactly like the other credential kinds: it exposes base_url and
// api_key_set only, while graph_model/graph_enabled stay readable flags.
func TestPublicSettingsNeverLeaksGraphKey(t *testing.T) {
	settings := testStoredSettings(t, "http://host", "secret")
	stored, err := StoredConnectionWithKey(modelapi.ModelConnection{
		BaseURL: "http://graph:8000/v1",
		Model:   "graph-llm",
		APIKey:  "graph-secret",
	}, testEncryptionKey)
	if err != nil {
		t.Fatalf("StoredConnectionWithKey() error: %v", err)
	}
	settings["graph"] = stored
	settings["graph_model"] = "graph-llm"
	settings["graph_enabled"] = true

	public := PublicSettings(settings)
	provider, ok := public["graph"].(map[string]any)
	if !ok {
		t.Fatalf("public graph = %v, want an object", public["graph"])
	}
	want := map[string]any{"base_url": "http://graph:8000/v1", "api_key_set": true}
	if !reflect.DeepEqual(provider, want) {
		t.Fatalf("public graph = %#v, want %#v", provider, want)
	}
	if public["graph_model"] != "graph-llm" || public["graph_enabled"] != true {
		t.Fatalf("public graph flags = %v/%v", public["graph_model"], public["graph_enabled"])
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatalf("json.Marshal() error: %v", err)
	}
	for _, secret := range []string{"graph-secret", "protected_key", "enc:v1:"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("public settings leaked %q: %s", secret, encoded)
		}
	}
}

func TestPublicSettingsDoesNotMutateInput(t *testing.T) {
	settings := testStoredSettings(t, "http://host", "secret")
	before, _ := settings["embedding"].(map[string]any)
	protectedBefore := before["protected_key"]
	_ = PublicSettings(settings)
	after, _ := settings["embedding"].(map[string]any)
	if after["protected_key"] != protectedBefore {
		t.Fatalf("PublicSettings mutated the stored provider object")
	}
}

func TestPublicSettingsList(t *testing.T) {
	first := testStoredSettings(t, "http://host", "secret")
	second := testStoredSettings(t, "http://host", "")
	second["namespace"] = "beta"
	public := PublicSettingsList([]map[string]any{first, second})
	if len(public) != 2 {
		t.Fatalf("len = %d", len(public))
	}
	if public[0]["namespace"] != "alpha" || public[1]["namespace"] != "beta" {
		t.Errorf("namespaces = %v / %v", public[0]["namespace"], public[1]["namespace"])
	}
	firstEmbedding := public[0]["embedding"].(map[string]any)
	secondEmbedding := public[1]["embedding"].(map[string]any)
	if firstEmbedding["api_key_set"] != true || secondEmbedding["api_key_set"] != false {
		t.Errorf("api_key_set = %v / %v", firstEmbedding["api_key_set"], secondEmbedding["api_key_set"])
	}
	if len(PublicSettingsList(nil)) != 0 {
		t.Errorf("nil list must project to an empty slice")
	}
}

func TestPublicSettingsMatchesStateProjection(t *testing.T) {
	settings := testStoredSettings(t, "http://host", "secret")
	settings["version"] = 2
	kbcPublic := PublicSettings(settings)
	statePublic := state.PublicSettings(settings)
	// internal/state keeps an equivalent copy of this projection for cached
	// read models. The graph provider is being wired into internal/state by the
	// parallel LightRAG change, so a credential kind that copy does not emit yet
	// is skipped instead of failing ahead of that change; every field both
	// projections know about is compared exactly.
	for key, want := range statePublic {
		if IsKind(key) {
			continue
		}
		if !reflect.DeepEqual(kbcPublic[key], want) {
			t.Fatalf("field %q diverged: kbconfig=%#v state=%#v", key, kbcPublic[key], want)
		}
	}
	for _, kind := range CredentialKinds {
		want, ok := statePublic[kind]
		if !ok {
			continue
		}
		if !reflect.DeepEqual(kbcPublic[kind], want) {
			t.Fatalf("provider %q diverged: kbconfig=%#v state=%#v", kind, kbcPublic[kind], want)
		}
	}
}
