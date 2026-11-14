package runtime

import (
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/qdrant"
)

func TestSag2CorpusRevisionChangesWithEveryMutation(t *testing.T) {
	base := map[string]string{"doc-a": "v1", "doc-b": "v2"}
	revision := sag2CorpusRevision(base)
	if revision == "" {
		t.Fatal("revision is empty")
	}
	if second := sag2CorpusRevision(map[string]string{"doc-a": "v1", "doc-b": "v2"}); second != revision {
		t.Fatalf("equal maps produced %q and %q", revision, second)
	}
	if changed := sag2CorpusRevision(map[string]string{"doc-a": "v1"}); changed == revision {
		t.Fatal("removing a document kept the revision")
	}
	if changed := sag2CorpusRevision(map[string]string{"doc-a": "v1", "doc-b": "v3"}); changed == revision {
		t.Fatal("republishing a document kept the revision")
	}
	if changed := sag2CorpusRevision(map[string]string{"doc-a": "", "doc-b": "v2"}); changed == revision {
		t.Fatal("a soft-delete tombstone kept the revision")
	}
}

func TestSag2DocumentProjection(t *testing.T) {
	point := qdrant.Point{Payload: map[string]any{
		"record_type": "chunk",
		"chunk_id":    "doc-1:0",
		"document_id": "doc-1",
		"text":        "E03 表示传感器超时。",
		"title":        "维护手册",
		"source":       "manual.md",
		"metadata":    map[string]any{"pages": []any{float64(3)}},
	}}
	document, ok := sag2Document(point)
	if !ok {
		t.Fatal("chunk point was rejected")
	}
	if document.ChunkID != "doc-1:0" || document.DocumentID != "doc-1" ||
		document.Title != "维护手册" || document.Source != "manual.md" {
		t.Fatalf("document = %+v", document)
	}
	if document.Text != "E03 表示传感器超时。" {
		t.Fatalf("text = %q", document.Text)
	}

	// Assets and blank-text points never enter the lexical corpus.
	asset := qdrant.Point{Payload: map[string]any{"record_type": "asset", "asset_id": "a1"}}
	if _, ok := sag2Document(asset); ok {
		t.Fatal("asset point was projected as a document")
	}
	blank := qdrant.Point{Payload: map[string]any{"record_type": "chunk", "chunk_id": "c1", "text": "   "}}
	if _, ok := sag2Document(blank); ok {
		t.Fatal("blank-text chunk was projected as a document")
	}
}
