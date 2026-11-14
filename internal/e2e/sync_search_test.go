package e2e

// Synchronous-search contract: one Service.Search call performs retrieval and
// reranking and returns the evidence directly. The response has no result_id,
// no snapshot/task metadata and no polling path; artifacts are registered on
// the same call and remain readable by id through GetArtifact. SearchRaw is the
// internal/worker-facing entry point and never captures artifact references.

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

// retiredSearchPayloadFields are the queued-search/snapshot keys the synchronous
// contract must never emit.
var retiredSearchPayloadFields = []string{
	"result_id",
	"created_at",
	"expires_at",
	"snapshot_available",
	"snapshot_error",
	"wait_seconds",
}

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

// requireNoSnapshotFields asserts the synchronous payload does not expose any
// queued-search/snapshot key.
func requireNoSnapshotFields(t *testing.T, payload map[string]any) {
	t.Helper()
	for _, field := range retiredSearchPayloadFields {
		if value, present := payload[field]; present {
			t.Fatalf("synchronous search payload must not expose %q: %v", field, value)
		}
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

// TestSyncSearchReturnsArtifacts pins the synchronous contract end to end:
// Service.Search returns results plus projected artifact references in one
// call, exposes no snapshot/result_id fields, and the referenced artifact is
// independently readable by id.
func TestSyncSearchReturnsArtifacts(t *testing.T) {
	h := newHarness(t, syncQueryPoints)
	ctx := context.Background()
	seedLegacyDocument(t, h, "doc-e03", "e03.md", []byte("E03 水路供水不足的处理步骤"))
	h.Service.SearchEngine().SetReferenceCapture(searchrefs.New(h.Stores, testLogger()))

	payload, err := h.Service.Search(ctx, "alpha", syncSearchRequest())
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	requireNoSnapshotFields(t, payload)
	results, ok := payload["results"].([]any)
	if !ok || len(results) == 0 {
		t.Fatalf("payload results = %#v, want non-empty []any", payload["results"])
	}
	firstHit, ok := results[0].(map[string]any)
	if !ok || firstHit["document_id"] != "doc-e03" {
		t.Fatalf("first result = %#v", results[0])
	}

	// Top-level artifact list plus per-hit references; the artifact is the only
	// durable reference surface for a synchronous response.
	references := artifactReferences(payload["artifacts"])
	if len(references) == 0 {
		t.Fatalf("payload artifacts empty, want at least the source reference: %v", payload["artifacts"])
	}
	source := references[0]
	if source["kind"] != "source" || source["name"] != "e03.md" || source["document_id"] != "doc-e03" {
		t.Fatalf("source artifact reference = %v", source)
	}
	artifactID, _ := source["artifact_id"].(string)
	if artifactID == "" {
		t.Fatalf("artifact reference has no artifact_id: %v", source)
	}
	if resourceURI, _ := source["resource_uri"].(string); resourceURI == "" {
		t.Fatalf("artifact reference has no resource_uri: %v", source)
	}
	if hitReferences := artifactReferences(firstHit["artifacts"]); len(hitReferences) == 0 {
		t.Fatalf("first hit has no artifacts: %v", firstHit)
	}
	if limits, ok := payload["artifact_limits"].(map[string]any); !ok || limits["max_count"] == nil {
		t.Fatalf("payload artifact_limits = %v", payload["artifact_limits"])
	}

	// The artifact referenced by the response is readable by id afterwards.
	chunk, err := h.Service.GetArtifact(ctx, "alpha", artifactID, 0, 64)
	if err != nil {
		t.Fatalf("GetArtifact(%s) error = %v", artifactID, err)
	}
	if chunk["artifact_id"] != artifactID || chunk["document_id"] != "doc-e03" {
		t.Fatalf("artifact chunk = %v", chunk)
	}

	// SearchRaw shares the synchronous shape but never captures references.
	raw, err := h.Service.SearchRaw(ctx, "alpha", syncSearchRequest())
	if err != nil {
		t.Fatalf("SearchRaw() error = %v", err)
	}
	requireNoSnapshotFields(t, raw)
	rawResults, ok := raw["results"].([]any)
	if !ok || len(rawResults) == 0 {
		t.Fatalf("SearchRaw results = %#v, want non-empty []any", raw["results"])
	}
	if references := artifactReferences(raw["artifacts"]); len(references) != 0 {
		t.Fatalf("SearchRaw must not capture artifacts: %v", raw["artifacts"])
	}
}

// TestSyncSearchHonorsDocumentIDs covers the restricted-scope path: the
// transport sets DocumentIDs and the synchronous engine still executes retrieval
// while returning only visible documents (an empty non-nil list means nothing is
// visible).
func TestSyncSearchHonorsDocumentIDs(t *testing.T) {
	h := newHarness(t, syncQueryPoints)
	ctx := context.Background()
	seedLegacyDocument(t, h, "doc-e03", "e03.md", []byte("E03 水路供水不足的处理步骤"))
	h.Service.SearchEngine().SetReferenceCapture(searchrefs.New(h.Stores, testLogger()))

	visible := syncSearchRequest()
	visible.DocumentIDs = []string{"doc-e03"}
	payload, err := h.Service.Search(ctx, "alpha", visible)
	if err != nil {
		t.Fatalf("Search(visible) error = %v", err)
	}
	if results, ok := payload["results"].([]any); !ok || len(results) == 0 {
		t.Fatalf("restricted-visible results = %#v, want the allowed document", payload["results"])
	}

	hidden := syncSearchRequest()
	hidden.DocumentIDs = []string{"doc-other"}
	payload, err = h.Service.Search(ctx, "alpha", hidden)
	if err != nil {
		t.Fatalf("Search(hidden) error = %v", err)
	}
	requireNoSnapshotFields(t, payload)
	if results, ok := payload["results"].([]any); !ok || len(results) != 0 {
		t.Fatalf("restricted-hidden results = %#v, want none", payload["results"])
	}
	if references := artifactReferences(payload["artifacts"]); len(references) != 0 {
		t.Fatalf("restricted-hidden artifacts = %v, want none", payload["artifacts"])
	}
	if payload["evidence_status"] != "no_evidence" {
		t.Fatalf("restricted-hidden evidence_status = %v, want no_evidence", payload["evidence_status"])
	}
}

// TestSyncSearchEmptyResults covers the empty-result case: the payload keeps a
// non-nil empty results list, no artifact references and no snapshot fields.
func TestSyncSearchEmptyResults(t *testing.T) {
	h := newHarness(t, "") // fake Qdrant answers with no points
	ctx := context.Background()
	h.Service.SearchEngine().SetReferenceCapture(searchrefs.New(h.Stores, testLogger()))

	payload, err := h.Service.Search(ctx, "alpha", syncSearchRequest())
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	requireNoSnapshotFields(t, payload)
	results, ok := payload["results"].([]any)
	if !ok || results == nil || len(results) != 0 {
		t.Fatalf("empty payload results = %#v, want non-nil []any{}", payload["results"])
	}
	if references := artifactReferences(payload["artifacts"]); len(references) != 0 {
		t.Fatalf("empty search artifacts = %v", payload["artifacts"])
	}
	if evidence, _ := payload["evidence_status"].(string); evidence == "" {
		t.Fatalf("empty search payload missing evidence_status: %v", payload)
	}
}
