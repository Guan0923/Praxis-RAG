// Package runtime: tests of the knowledge-graph mirror helpers.
//
// These tests cover the pure parts of graph_index.go: the settings-to-mirror
// resolution (disabled, missing sidecar, missing model), the workspace
// fingerprint sensitivity and the chunk payload projection. They need no MySQL,
// no Qdrant and no sidecar.

package runtime

import (
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/chunking"
	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
)

// TestResolveGraphMirrorDisabled verifies that a knowledge base without the
// switch resolves to a nil mirror, so every caller can treat it as a no-op.
func TestResolveGraphMirrorDisabled(t *testing.T) {
	service := &Service{config: config.RagConfig{LightRAGServiceURL: "http://127.0.0.1:8093"}}
	for _, settings := range []map[string]any{
		nil,
		{},
		{"graph_enabled": false},
		{"graph_enabled": "true"},
	} {
		mirror, err := service.resolveGraphMirror(settings)
		if err != nil {
			t.Fatalf("resolveGraphMirror(%v) error = %v", settings, err)
		}
		if mirror != nil {
			t.Fatalf("resolveGraphMirror(%v) = %+v, want nil", settings, mirror)
		}
	}
}

// TestResolveGraphMirrorRequiresSidecar verifies that a knowledge base which
// switched the capability on without a configured deployment fails at
// admission with an actionable message instead of silently skipping the graph.
func TestResolveGraphMirrorRequiresSidecar(t *testing.T) {
	service := &Service{config: config.RagConfig{}}
	_, err := service.resolveGraphMirror(map[string]any{"graph_enabled": true})
	if err == nil {
		t.Fatal("resolveGraphMirror() error = nil, want a configuration error")
	}
	if !strings.Contains(err.Error(), "RAG_LIGHTRAG_SERVICE_URL") {
		t.Fatalf("error = %v, want the missing variable name", err)
	}
}

// TestResolveGraphMirrorRequiresModel verifies that a configured deployment
// without a graph model connection still rejects the upload, because the graph
// copy would be empty otherwise.
func TestResolveGraphMirrorRequiresModel(t *testing.T) {
	service := &Service{config: config.RagConfig{LightRAGServiceURL: "http://127.0.0.1:8093"}}
	_, err := service.resolveGraphMirror(map[string]any{
		"graph_enabled":      true,
		"graph":              map[string]any{"base_url": "", "protected_key": ""},
		"graph_model":        "",
		"embedding":          map[string]any{"base_url": "http://embed:8000/v1", "protected_key": ""},
		"embedding_model":    "BAAI/bge-m3",
		"vector_size":        float64(1024),
	})
	if err == nil {
		t.Fatal("resolveGraphMirror() error = nil, want a missing-model error")
	}
	if !strings.Contains(err.Error(), "知识图谱模型") {
		t.Fatalf("error = %v, want the graph model message", err)
	}
}

// TestGraphFingerprintTracksModelChanges verifies that swapping either model or
// the sidecar address changes the fingerprint, which is what makes the sidecar
// rebuild a workspace instead of mixing two entity vocabularies.
func TestGraphFingerprintTracksModelChanges(t *testing.T) {
	embedding := modelapi.ModelConnection{BaseURL: "http://embed:8000/v1", Model: "bge-m3"}
	llm := modelapi.ModelConnection{BaseURL: "http://llm:8000/v1", Model: "flash"}
	base := graphFingerprint("http://127.0.0.1:8093", embedding, llm)
	if base == "" {
		t.Fatal("graphFingerprint() = empty")
	}
	if got := graphFingerprint("http://127.0.0.1:8093/", embedding, llm); got != base {
		t.Fatalf("trailing slash changed the fingerprint: %q != %q", got, base)
	}
	otherLLM := llm
	otherLLM.Model = "pro"
	if graphFingerprint("http://127.0.0.1:8093", embedding, otherLLM) == base {
		t.Fatal("changing the graph model kept the fingerprint")
	}
	otherEmbedding := embedding
	otherEmbedding.Model = "bge-large"
	if graphFingerprint("http://127.0.0.1:8093", otherEmbedding, llm) == base {
		t.Fatal("changing the embedding model kept the fingerprint")
	}
	if graphFingerprint("http://sidecar:8093", embedding, llm) == base {
		t.Fatal("changing the sidecar address kept the fingerprint")
	}
}

// TestGraphChunkPayloadProjectsMetadata verifies the chunk projection: empty
// text is dropped, and page/heading survive in the JSON shapes a parser emits.
func TestGraphChunkPayloadProjectsMetadata(t *testing.T) {
	payload := graphChunkPayload([]chunking.Chunk{
		{ChunkID: "c-1", Text: "第一段", Metadata: map[string]any{"page": float64(3), "heading": "第一章"}},
		{ChunkID: "c-2", Text: "   "},
		{ChunkID: "c-3", Text: "第三段", Metadata: map[string]any{"page": 7, "section": "附录"}},
	})
	if len(payload) != 2 {
		t.Fatalf("payload = %#v, want 2 entries", payload)
	}
	if payload[0].ChunkID != "c-1" || payload[0].Page != 3 || payload[0].Heading != "第一章" {
		t.Fatalf("payload[0] = %#v", payload[0])
	}
	if payload[1].ChunkID != "c-3" || payload[1].Page != 7 || payload[1].Heading != "附录" {
		t.Fatalf("payload[1] = %#v", payload[1])
	}
}
