package kbconfig

import (
	"reflect"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/state"
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
	}
	if !reflect.DeepEqual(public, want) {
		t.Fatalf("PublicSettings() = %#v, want %#v", public, want)
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
	if !reflect.DeepEqual(PublicSettings(settings), state.PublicSettings(settings)) {
		t.Fatalf("kbconfig and state public projections diverged")
	}
}
