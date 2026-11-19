// Package retrieval: tests of the LightRAG knowledge-graph retriever adapter.
//
// A fake sidecar (httptest) stands in for python-workers/lightrag-service, so
// these tests pin the Go-side contract without requiring LightRAG or a model
// endpoint. They cover candidate projection, document restriction forwarding,
// score filtering and safe failure mapping.

package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/lightrag"
)

// graphTestServer starts a fake sidecar that answers one query with contexts
// and records the decoded request body for assertions.
func graphTestServer(
	t *testing.T,
	contexts []map[string]any,
	record *map[string]any,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/workspaces/legal/query" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if record != nil {
			body := map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode query body: %v", err)
			}
			*record = body
		}
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]any{
			"contexts": contexts,
			"debug":    map[string]any{"mode": "mix", "entities": 2, "relations": 1, "chunks": len(contexts)},
		}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Errorf("encode query reply: %v", err)
		}
	}))
}

// newGraphRetrieverForTest wires a retriever against the fake sidecar URL.
func newGraphRetrieverForTest(t *testing.T, baseURL string, options GraphOptions) *GraphRetriever {
	t.Helper()
	client, err := lightrag.NewClient(lightrag.Options{BaseURL: baseURL})
	if err != nil {
		t.Fatalf("lightrag.NewClient() error = %v", err)
	}
	return NewGraphRetriever(client, "legal", options)
}

// TestGraphRetrieverProjectsContexts verifies that sidecar contexts become
// evidence candidates carrying the citation fields the contract requires.
func TestGraphRetrieverProjectsContexts(t *testing.T) {
	contexts := []map[string]any{{
		"chunk_id":      "c-2",
		"document_id":   "doc-1",
		"document_name": "民法典.pdf",
		"text":          "处理个人信息应当取得个人同意。",
		"score":         0.87,
		"page":          12,
		"heading":       "第一编 总则",
		"entities":      []string{"个人信息", "同意"},
		"relations": []map[string]any{
			{"source": "个人信息", "target": "同意", "description": "取得同意是处理前提"},
		},
	}}
	server := graphTestServer(t, contexts, nil)
	defer server.Close()

	retriever := newGraphRetrieverForTest(t, server.URL, GraphOptions{IncludeEntities: true})
	candidates, err := retriever.Search(context.Background(), "用户画像条款", 5, nil)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("Search() returned %d candidates, want 1", len(candidates))
	}
	candidate := candidates[0]
	if candidate.DocumentID != "doc-1" || candidate.ChunkID != "c-2" {
		t.Fatalf("candidate identity = %q/%q", candidate.DocumentID, candidate.ChunkID)
	}
	if candidate.Title != "民法典.pdf" || candidate.Source != "民法典.pdf" {
		t.Fatalf("candidate citation = %q/%q", candidate.Title, candidate.Source)
	}
	if candidate.Score != 0.87 {
		t.Fatalf("candidate score = %v, want 0.87", candidate.Score)
	}
	if candidate.Metadata["graph_mode"] != GraphModeMix {
		t.Fatalf("graph_mode = %v, want %q", candidate.Metadata["graph_mode"], GraphModeMix)
	}
	if candidate.Metadata["page"] != 12 {
		t.Fatalf("page = %v, want 12", candidate.Metadata["page"])
	}
	relations, ok := candidate.Metadata["relations"].([]map[string]any)
	if !ok || len(relations) != 1 || relations[0]["source"] != "个人信息" {
		t.Fatalf("relations = %#v", candidate.Metadata["relations"])
	}
}

// TestGraphRetrieverUsesDocumentNameFallback verifies that a missing document
// name still yields a stable citation instead of an empty title.
func TestGraphRetrieverUsesDocumentNameFallback(t *testing.T) {
	contexts := []map[string]any{{
		"chunk_id":    "c-1",
		"document_id": "doc-9",
		"text":        "证据正文",
		"score":       0.5,
	}}
	server := graphTestServer(t, contexts, nil)
	defer server.Close()

	retriever := newGraphRetrieverForTest(t, server.URL, GraphOptions{})
	candidates, err := retriever.Search(context.Background(), "问题", 3, nil)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].Title != "doc-9" {
		t.Fatalf("candidates = %#v", candidates)
	}
}

// TestGraphRetrieverForwardsDocumentFilter verifies that a contract
// document_id filter reaches the sidecar as a restriction list, in both the
// scalar and the slice form.
func TestGraphRetrieverForwardsDocumentFilter(t *testing.T) {
	cases := []struct {
		name    string
		filters map[string]any
		want    []any
	}{
		{"scalar", map[string]any{"document_id": "doc-1"}, []any{"doc-1"}},
		{"slice", map[string]any{"document_id": []any{"doc-1", "doc-2"}}, []any{"doc-1", "doc-2"}},
		{"none", map[string]any{"title": "民法典"}, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var record map[string]any
			server := graphTestServer(t, nil, &record)
			defer server.Close()

			retriever := newGraphRetrieverForTest(t, server.URL, GraphOptions{Mode: GraphModeGlobal})
			if _, err := retriever.Search(context.Background(), "问题", 4, testCase.filters); err != nil {
				t.Fatalf("Search() error = %v", err)
			}
			if record["mode"] != GraphModeGlobal {
				t.Fatalf("mode = %v, want %q", record["mode"], GraphModeGlobal)
			}
			if record["top_k"] != float64(4) {
				t.Fatalf("top_k = %v, want 4", record["top_k"])
			}
			filters, _ := record["filters"].(map[string]any)
			got, _ := filters["document_ids"].([]any)
			if len(got) != len(testCase.want) {
				t.Fatalf("document_ids = %#v, want %#v", got, testCase.want)
			}
			for index := range testCase.want {
				if got[index] != testCase.want[index] {
					t.Fatalf("document_ids = %#v, want %#v", got, testCase.want)
				}
			}
		})
	}
}

// TestGraphRetrieverDropsLowScoresAndEmptyText verifies the client-side
// filtering applied before candidates reach the orchestrator.
func TestGraphRetrieverDropsLowScoresAndEmptyText(t *testing.T) {
	contexts := []map[string]any{
		{"chunk_id": "c-1", "document_id": "doc-1", "document_name": "a.md", "text": "保留", "score": 0.9},
		{"chunk_id": "c-2", "document_id": "doc-1", "document_name": "a.md", "text": "低分", "score": 0.1},
		{"chunk_id": "c-3", "document_id": "doc-1", "document_name": "a.md", "text": "   ", "score": 0.9},
	}
	server := graphTestServer(t, contexts, nil)
	defer server.Close()

	retriever := newGraphRetrieverForTest(t, server.URL, GraphOptions{MinScore: 0.5})
	candidates, err := retriever.Search(context.Background(), "问题", 10, nil)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].ChunkID != "c-1" {
		t.Fatalf("candidates = %#v", candidates)
	}
}

// TestGraphRetrieverReportsSidecarFailure verifies that a sidecar outage is
// mapped to the operational error category rather than leaking a raw HTTP
// error to the transport layers.
func TestGraphRetrieverReportsSidecarFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"detail":"lightrag workspace is not initialized"}`))
	}))
	defer server.Close()

	retriever := newGraphRetrieverForTest(t, server.URL, GraphOptions{})
	_, err := retriever.Search(context.Background(), "问题", 5, nil)
	if err == nil {
		t.Fatal("Search() error = nil, want an operational error")
	}
	var operation *contracts.RagOperationError
	if !strings.Contains(err.Error(), "Graph retrieval failed") {
		t.Fatalf("error = %v, want a graph retrieval message", err)
	}
	if !errors.As(err, &operation) {
		t.Fatalf("error = %v, want *contracts.RagOperationError", err)
	}
}

// TestGraphRetrieverRequiresClient verifies the misconfiguration guard.
func TestGraphRetrieverRequiresClient(t *testing.T) {
	retriever := NewGraphRetriever(nil, "legal", GraphOptions{})
	_, err := retriever.Search(context.Background(), "问题", 5, nil)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("error = %v, want a not-configured message", err)
	}
}
