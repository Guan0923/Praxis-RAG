package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
)

// TestReplaceDocumentKeepsOneNameAndSearchesNewText covers the Console replace
// flow at the runtime boundary: the document keeps its original name and one
// record, reading returns the replacement bytes, and retrieval is served from
// the newly published index instead of the old content.
func TestReplaceDocumentKeepsOneNameAndSearchesNewText(t *testing.T) {
	service, stores, _, _ := newUploadService(t)
	ctx := context.Background()
	const name = "handbook.md"
	oldText := repeatedDocumentText(3) + "\n\nOLD-REVISION-MARKER.\n"
	newText := "NEW-REVISION-MARKER with the revised procedure.\n\n" + repeatedDocumentText(3)

	if _, err := service.writeDocumentContent(ctx, "alpha", name, []byte(oldText), false, ""); err != nil {
		t.Fatalf("initial write error = %v", err)
	}
	updated, err := service.writeDocumentContent(ctx, "alpha", name, []byte(newText), true, "")
	if err != nil {
		t.Fatalf("replace write error = %v", err)
	}
	if updated["status"] != "updated" {
		t.Fatalf("replace status = %v", updated["status"])
	}

	documents, err := service.ListDocuments(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListDocuments() error = %v", err)
	}
	if len(documents) != 1 || documents[0]["name"] != name {
		t.Fatalf("documents after replace = %v", documents)
	}
	if size, _ := documents[0]["size"].(float64); int(size) != len(newText) {
		t.Fatalf("document size = %v, want %d", documents[0]["size"], len(newText))
	}

	stored, _, err := service.ReadDocument(ctx, "alpha", name)
	if err != nil {
		t.Fatalf("ReadDocument() error = %v", err)
	}
	if !strings.Contains(stored, "NEW-REVISION-MARKER") || strings.Contains(stored, "OLD-REVISION-MARKER") {
		t.Fatalf("stored content is not the replacement: %q", stored)
	}

	request, err := contracts.SearchRequestFromArguments(map[string]any{
		"query": "NEW-REVISION-MARKER", "retrieval": "embedding", "rerank": "none",
		"candidate_k": 5, "top_k": 5,
	})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error = %v", err)
	}
	payload, err := service.Search(ctx, "alpha", request)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	results, _ := payload["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("search returned no evidence: %v", payload)
	}
	foundNew := false
	for _, raw := range results {
		result, _ := raw.(map[string]any)
		text, _ := result["text"].(string)
		if strings.Contains(text, "NEW-REVISION-MARKER") {
			foundNew = true
		}
		if strings.Contains(text, "OLD-REVISION-MARKER") {
			t.Fatalf("search returned stale content after replace: %q", text)
		}
	}
	if !foundNew {
		t.Fatalf("search did not return the replacement text: %v", results)
	}

	records, err := stores.Documents.List(ctx, "alpha")
	if err != nil || len(records) != 1 || records[0].Name != name {
		t.Fatalf("record rows after replace = %+v, %v", records, err)
	}
}

// TestReplaceDocumentFailureKeepsOriginalUsable proves a failed replacement
// never publishes a partial index: the old record, bytes and searchable chunks
// stay available.
func TestReplaceDocumentFailureKeepsOriginalUsable(t *testing.T) {
	service, _, qdrantFake, _ := newUploadService(t)
	ctx := context.Background()
	const name = "manual.md"
	oldText := "OLD-KEPT-MARKER with the working procedure.\n\n" + repeatedDocumentText(3)
	newText := "NEW-LOST-MARKER that must not be published.\n\n" + repeatedDocumentText(3)

	if _, err := service.writeDocumentContent(ctx, "alpha", name, []byte(oldText), false, ""); err != nil {
		t.Fatalf("initial write error = %v", err)
	}
	qdrantFake.failUpsert = true
	if _, err := service.writeDocumentContent(ctx, "alpha", name, []byte(newText), true, ""); err == nil {
		t.Fatalf("replacement succeeded despite a Qdrant failure")
	}

	documents, err := service.ListDocuments(ctx, "alpha")
	if err != nil || len(documents) != 1 || documents[0]["name"] != name {
		t.Fatalf("documents after failed replace = %v, %v", documents, err)
	}
	if size, _ := documents[0]["size"].(float64); int(size) != len(oldText) {
		t.Fatalf("document size after failed replace = %v, want %d", documents[0]["size"], len(oldText))
	}
	stored, _, err := service.ReadDocument(ctx, "alpha", name)
	if err != nil || !strings.Contains(stored, "OLD-KEPT-MARKER") {
		t.Fatalf("original content is not readable after failed replace: %q, %v", stored, err)
	}
	for _, text := range searchTexts(t, service, "OLD-KEPT-MARKER") {
		if strings.Contains(text, "NEW-LOST-MARKER") {
			t.Fatalf("failed replace published new content: %q", text)
		}
	}
	if !containsText(searchTexts(t, service, "OLD-KEPT-MARKER"), "OLD-KEPT-MARKER") {
		t.Fatalf("original content is not searchable after failed replace")
	}
}

// searchTexts runs one dense search and returns the returned result texts.
func searchTexts(t *testing.T, service *Service, query string) []string {
	t.Helper()
	request, err := contracts.SearchRequestFromArguments(map[string]any{
		"query": query, "retrieval": "embedding", "rerank": "none",
		"candidate_k": 5, "top_k": 5,
	})
	if err != nil {
		t.Fatalf("SearchRequestFromArguments() error = %v", err)
	}
	payload, err := service.Search(context.Background(), "alpha", request)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	results, _ := payload["results"].([]any)
	texts := make([]string, 0, len(results))
	for _, raw := range results {
		result, _ := raw.(map[string]any)
		text, _ := result["text"].(string)
		texts = append(texts, text)
	}
	return texts
}

func containsText(texts []string, marker string) bool {
	for _, text := range texts {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
