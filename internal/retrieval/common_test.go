package retrieval

import (
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/qdrant"
)

func TestProjectCandidatesBuildsCitableEvidence(t *testing.T) {
	points := []qdrant.Point{
		{
			ID:    qdrant.PointID("p1"),
			Score: 0.75,
			Payload: map[string]any{
				"document_id": "doc-a",
				"chunk_id":    "chunk-a",
				"title":       "标题",
				"source":      "knowledge/a.md",
				"text":        "证据文本",
				"metadata":    map[string]any{"kind": "test", "asset_id": "asset-1"},
			},
		},
	}
	candidates, err := ProjectCandidates(points)
	if err != nil {
		t.Fatalf("ProjectCandidates() error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %v", candidates)
	}
	candidate := candidates[0]
	if candidate.DocumentID != "doc-a" || candidate.ChunkID != "chunk-a" || candidate.Title != "标题" ||
		candidate.Source != "knowledge/a.md" || candidate.Text != "证据文本" || candidate.Score != 0.75 {
		t.Errorf("candidate = %+v", candidate)
	}
	if candidate.Metadata["kind"] != "test" {
		t.Errorf("metadata = %v", candidate.Metadata)
	}
}

func TestProjectCandidatesAppliesPythonDefaults(t *testing.T) {
	points := []qdrant.Point{{
		Score: 0.1,
		Payload: map[string]any{
			"document_id": 7,
			"chunk_id":    true,
			"source":      nil,
			"text":        "body",
			"metadata":    "not-a-dict",
		},
	}}
	candidates, err := ProjectCandidates(points)
	if err != nil {
		t.Fatalf("ProjectCandidates() error: %v", err)
	}
	candidate := candidates[0]
	if candidate.DocumentID != "7" || candidate.ChunkID != "True" || candidate.Source != "None" {
		t.Errorf("python str conversion mismatch: %+v", candidate)
	}
	if candidate.Title != "" {
		t.Errorf("missing title must default to empty string, got %q", candidate.Title)
	}
	if candidate.Metadata == nil || len(candidate.Metadata) != 0 {
		t.Errorf("non-map metadata must be replaced with {}, got %v", candidate.Metadata)
	}
}

func TestProjectCandidatesRejectsInvalidPayloads(t *testing.T) {
	base := map[string]any{
		"document_id": "doc", "chunk_id": "chunk", "source": "s", "text": "t",
	}
	for _, missing := range []string{"document_id", "chunk_id", "source", "text"} {
		payload := map[string]any{}
		for key, value := range base {
			if key == missing {
				continue
			}
			payload[key] = value
		}
		_, err := ProjectCandidates([]qdrant.Point{{Payload: payload}})
		if err == nil || !strings.Contains(err.Error(), "Qdrant point has an invalid evidence payload") {
			t.Errorf("missing %s error = %v", missing, err)
		}
		var operationError *contracts.RagOperationError
		if ok := asRagOperationError(err, &operationError); !ok {
			t.Errorf("missing %s error is not a RagOperationError: %v", missing, err)
		}
	}
}

func TestPythonString(t *testing.T) {
	cases := map[any]string{
		nil:        "None",
		"value":    "value",
		true:       "True",
		false:      "False",
		float64(2): "2",
		2:          "2",
	}
	for value, want := range cases {
		if got := pythonString(value); got != want {
			t.Errorf("pythonString(%v) = %q, want %q", value, got, want)
		}
	}
}
