package e2e

// Synchronous-search regression: the orchestrator returns results as
// []map[string]any, and before the runtime-boundary normalization searchrefs
// skipped artifact registration and ResultSnapshots rejected the payload, so
// rag-service synchronous HTTP/MCP search returned snapshot_available=false and
// no artifact references. These tests pin the JSON-shaped contract end to end.

import (
	"context"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/searchrefs"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// syncQueryPoints is a chunk hit without an upload_version, so reference capture
// resolves the source through the legacy per-namespace blob bucket (the seeded
// document row + source blob).
const syncQueryPoints = `[
	{"id":"sync-p1","version":1,"score":0.95,"payload":{
		"record_type":"chunk","document_id":"doc-e03","chunk_id":"chunk-e03",
		"text":"错误代码 E03 表示水路供水不足","title":"e03","source":"e03.md",
		"metadata":{"kind":"test"}}}
]`

// seedLegacyDocument inserts the document row and source blob reference capture
// reads for a versionless (legacy) chunk hit.
func seedLegacyDocument(t *testing.T, h *harness, documentID, name string, content []byte) {
	t.Helper()
	ctx := context.Background()
	if err := h.Stores.Documents.Upsert(ctx, "alpha", state.DocumentRecord{
		Name:       name,
		DocumentID: documentID,
		Collection: "knowledge_test",
	}); err != nil {
		t.Fatalf("Documents.Upsert() error = %v", err)
	}
	if err := h.Stores.Blobs.Put(ctx, "alpha", state.SourceBucket, name, content, &documentID); err != nil {
		t.Fatalf("Blobs.Put() error = %v", err)
	}
}

// artifactReferences normalizes the artifact list shape (searchrefs returns
// []map[string]any; JSON-decoded payloads yield []any).
func artifactReferences(value any) []map[string]any {
	switch typed := value.(type) {
	case []map[string]any:
		return typed
	case []any:
		references := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if reference, ok := item.(map[string]any); ok {
				references = append(references, reference)
			}
		}
		return references
	default:
		return nil
	}
}

func syncSearchRequest() contracts.SearchRequest {
	return contracts.SearchRequest{
		Query:      "E03 供水不足",
		Retrieval:  contracts.RetrievalHybrid,
		Rerank:     contracts.RerankCrossEncoder,
		CandidateK: 10,
		TopK:       5,
	}
}

// TestSyncSearchReturnsArtifactsAndSnapshot is the reported-bug regression:
// Service.Search must return a stored snapshot (snapshot_available=true) and
// artifact references for the returned chunks.
func TestSyncSearchReturnsArtifactsAndSnapshot(t *testing.T) {
	h := newHarness(t, syncQueryPoints)
	ctx := context.Background()
	seedLegacyDocument(t, h, "doc-e03", "e03.md", []byte("E03 水路供水不足的处理步骤"))
	h.Service.SearchEngine().SetReferenceCapture(searchrefs.New(h.Stores, testLogger()))

	payload, err := h.Service.Search(ctx, "alpha", syncSearchRequest())
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if payload["snapshot_available"] != true {
		t.Fatalf("snapshot_available = %v, want true (payload=%v)", payload["snapshot_available"], payload)
	}
	resultID, _ := payload["result_id"].(string)
	if len(resultID) != 36 {
		t.Fatalf("result_id = %v", payload["result_id"])
	}
	if _, present := payload["created_at"]; !present {
		t.Fatalf("snapshot payload missing created_at: %v", payload)
	}
	if _, present := payload["expires_at"]; !present {
		t.Fatalf("snapshot payload missing expires_at: %v", payload)
	}
	results, ok := payload["results"].([]any)
	if !ok || len(results) == 0 {
		t.Fatalf("payload results = %#v, want non-empty []any", payload["results"])
	}
	firstHit, ok := results[0].(map[string]any)
	if !ok || firstHit["document_id"] != "doc-e03" {
		t.Fatalf("first result = %#v", results[0])
	}

	// Schema-2 reference shape: top-level artifact list plus per-hit references.
	references := artifactReferences(payload["artifacts"])
	if len(references) == 0 {
		t.Fatalf("payload artifacts empty, want at least the source reference: %v", payload["artifacts"])
	}
	source := references[0]
	if source["kind"] != "source" || source["name"] != "e03.md" || source["document_id"] != "doc-e03" {
		t.Fatalf("source artifact reference = %v", source)
	}
	if artifactID, _ := source["artifact_id"].(string); artifactID == "" {
		t.Fatalf("artifact reference has no artifact_id: %v", source)
	}
	if resourceURI, _ := source["resource_uri"].(string); resourceURI == "" {
		t.Fatalf("artifact reference has no resource_uri: %v", source)
	}
	hitReferences := artifactReferences(firstHit["artifacts"])
	if len(hitReferences) == 0 {
		t.Fatalf("first hit has no artifacts: %v", firstHit)
	}
	if limits, ok := payload["artifact_limits"].(map[string]any); !ok || limits["max_count"] == nil {
		t.Fatalf("payload artifact_limits = %v", payload["artifact_limits"])
	}

	// The snapshot is readable through the same client read path.
	stored, err := h.Service.GetSearchResult(ctx, "alpha", resultID, 1, 10)
	if err != nil {
		t.Fatalf("GetSearchResult() error = %v", err)
	}
	if stored["result_id"] != resultID || stored["snapshot_available"] != true {
		t.Fatalf("stored snapshot = %v", stored)
	}
	storedResults, ok := stored["results"].([]any)
	if !ok || len(storedResults) == 0 {
		t.Fatalf("stored results = %#v", stored["results"])
	}

	// SearchRaw shares the normalization but never captures or snapshots.
	raw, err := h.Service.SearchRaw(ctx, "alpha", syncSearchRequest())
	if err != nil {
		t.Fatalf("SearchRaw() error = %v", err)
	}
	rawResults, ok := raw["results"].([]any)
	if !ok || len(rawResults) == 0 {
		t.Fatalf("SearchRaw results = %#v, want non-empty []any", raw["results"])
	}
	if _, present := raw["snapshot_available"]; present {
		t.Fatalf("SearchRaw must not snapshot: %v", raw["snapshot_available"])
	}
}

// TestSyncSearchEmptyResultsSnapshot covers the empty-result case: the payload
// must keep a non-nil empty results list and still store/read a snapshot.
func TestSyncSearchEmptyResultsSnapshot(t *testing.T) {
	h := newHarness(t, "") // fake Qdrant answers with no points
	ctx := context.Background()
	h.Service.SearchEngine().SetReferenceCapture(searchrefs.New(h.Stores, testLogger()))

	payload, err := h.Service.Search(ctx, "alpha", syncSearchRequest())
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if payload["snapshot_available"] != true {
		t.Fatalf("snapshot_available = %v, want true", payload["snapshot_available"])
	}
	results, ok := payload["results"].([]any)
	if !ok || results == nil || len(results) != 0 {
		t.Fatalf("empty payload results = %#v, want non-nil []any{}", payload["results"])
	}
	if references := artifactReferences(payload["artifacts"]); len(references) != 0 {
		t.Fatalf("empty search artifacts = %v", payload["artifacts"])
	}
	resultID, _ := payload["result_id"].(string)
	if len(resultID) != 36 {
		t.Fatalf("result_id = %v", payload["result_id"])
	}
	stored, err := h.Service.GetSearchResult(ctx, "alpha", resultID, 1, 10)
	if err != nil {
		t.Fatalf("GetSearchResult() error = %v", err)
	}
	storedResults, ok := stored["results"].([]any)
	if !ok || len(storedResults) != 0 {
		t.Fatalf("stored empty results = %#v", stored["results"])
	}
	if total, _ := stored["total_results"].(float64); total != 0 {
		t.Fatalf("stored total_results = %v", stored["total_results"])
	}
}
