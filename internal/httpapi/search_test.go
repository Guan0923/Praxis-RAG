package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
)

type fakeServiceRuntime struct {
	searchNamespace string
	searchRequest   contracts.SearchRequest
	searchPayload   map[string]any
	searchErr       error

	artifactOffset  int
	artifactLength  int
	artifactPayload map[string]any
	artifactErr     error

	downloadName string
	downloadMime string
	downloadBody []byte
	downloadErr  error
}

func (f *fakeServiceRuntime) Search(_ context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error) {
	f.searchNamespace = namespace
	f.searchRequest = request
	return f.searchPayload, f.searchErr
}

func (f *fakeServiceRuntime) GetArtifact(_ context.Context, _, _ string, offset, length int) (map[string]any, error) {
	f.artifactOffset = offset
	f.artifactLength = length
	return f.artifactPayload, f.artifactErr
}

func (f *fakeServiceRuntime) DownloadArtifact(_ context.Context, _, _ string) ([]byte, string, string, error) {
	return f.downloadBody, f.downloadMime, f.downloadName, f.downloadErr
}

// GetDocumentStatus satisfies the async document surface for search-only fakes.
func (f *fakeServiceRuntime) GetDocumentStatus(context.Context, string, string) (map[string]any, error) {
	return nil, &apierrors.KnowledgeError{Message: "document job status is not available in this test runtime"}
}

// SubmitDocumentBatch satisfies the batch document surface for search-only fakes.
func (f *fakeServiceRuntime) SubmitDocumentBatch(context.Context, string, []UploadFile, string, string) (map[string]any, error) {
	return nil, &apierrors.KnowledgeError{Message: "batch upload is not available in this test runtime"}
}

// GetDocumentBatch satisfies the batch document surface for search-only fakes.
func (f *fakeServiceRuntime) GetDocumentBatch(context.Context, string, string, string) (map[string]any, error) {
	return nil, &apierrors.KnowledgeError{Message: "document batch status is not available in this test runtime"}
}

// ListDocumentBatches satisfies the batch document surface for search-only fakes.
func (f *fakeServiceRuntime) ListDocumentBatches(context.Context, string, string, int) (map[string]any, error) {
	return nil, &apierrors.KnowledgeError{Message: "document batches are not available in this test runtime"}
}

// WriteDocument satisfies the synchronous document write surface for
// search-only fakes.
func (f *fakeServiceRuntime) WriteDocument(context.Context, string, string, []byte, bool, string) (map[string]any, error) {
	return nil, &apierrors.KnowledgeError{Message: "document writes are not available in this test runtime"}
}

func newRuntimeServer(t *testing.T, runtime Runtime) *Server {
	t.Helper()
	return New(testSettings("test-token"), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), WithRuntime(runtime))
}

func tenantAuth() map[string]string {
	return map[string]string{"Authorization": "Bearer test-token", "X-RAG-Namespace": "alpha"}
}

func TestSearchEndpoint(t *testing.T) {
	runtime := &fakeServiceRuntime{searchPayload: map[string]any{
		"evidence_status": "found",
		"results":         []any{},
		"artifacts":       []any{},
	}}
	handler := newRuntimeServer(t, runtime).Handler()

	response := performRequest(handler, testRequest{
		method:  "POST",
		target:  "/v1/search",
		headers: tenantAuth(),
		body:    `{"query":"E03","retrieval":"bm25","rerank":"none","candidate_k":20,"top_k":5,"filters":{"document_id":"doc-1"},"extra":"ignored"}`,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if runtime.searchNamespace != "alpha" {
		t.Fatalf("namespace = %q", runtime.searchNamespace)
	}
	request := runtime.searchRequest
	if request.Query != "E03" || request.Retrieval != contracts.RetrievalBM25 ||
		request.Rerank != contracts.RerankNone || request.CandidateK != 20 || request.TopK != 5 {
		t.Fatalf("request = %+v", request)
	}
	if request.Filters["document_id"] != "doc-1" {
		t.Fatalf("filters = %v", request.Filters)
	}
	// The synchronous response is the evidence payload itself: no result id, no
	// snapshot availability fields, and the artifact list is returned inline.
	body := decodeBody(t, response)
	for _, field := range []string{"result_id", "created_at", "expires_at", "snapshot_available", "snapshot_error"} {
		if _, present := body[field]; present {
			t.Fatalf("search response still carries removed field %q: %s", field, response.Body.String())
		}
	}
	if body["evidence_status"] != "found" {
		t.Fatalf("search body = %s", response.Body.String())
	}
	if _, ok := body["results"]; !ok {
		t.Fatalf("search response has no results: %s", response.Body.String())
	}
	if _, ok := body["artifacts"]; !ok {
		t.Fatalf("search response has no artifacts: %s", response.Body.String())
	}

	invalidRerank := performRequest(handler, testRequest{
		method: "POST", target: "/v1/search", headers: tenantAuth(),
		body: `{"query":"E03","rerank":"bi_encoder"}`,
	})
	if invalidRerank.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rerank status = %d", invalidRerank.Code)
	}
	details, _ := decodeBody(t, invalidRerank)["detail"].([]any)
	first, _ := details[0].(map[string]any)
	loc, _ := first["loc"].([]any)
	if len(loc) != 2 || loc[1] != "rerank" {
		t.Fatalf("loc = %v", loc)
	}

	invalidRetrieval := performRequest(handler, testRequest{
		method: "POST", target: "/v1/search", headers: tenantAuth(), body: `{"query":"E03","retrieval":"sql"}`,
	})
	if invalidRetrieval.Code != http.StatusBadRequest {
		t.Fatalf("retrieval status = %d body = %s", invalidRetrieval.Code, invalidRetrieval.Body.String())
	}
	if detail := decodeBody(t, invalidRetrieval)["detail"]; detail != "retrieval must be bm25, embedding, hybrid, or sag2" {
		t.Fatalf("detail = %v", detail)
	}

	missingQuery := performRequest(handler, testRequest{
		method: "POST", target: "/v1/search", headers: tenantAuth(), body: `{}`,
	})
	if missingQuery.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing query status = %d", missingQuery.Code)
	}

	unauthenticated := performRequest(handler, testRequest{
		method: "POST", target: "/v1/search", body: `{"query":"x"}`,
	})
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d", unauthenticated.Code)
	}
	if got := unauthenticated.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestSearchEndpointRuntimeErrors(t *testing.T) {
	runtime := &fakeServiceRuntime{
		searchErr: &contracts.RagOperationError{Message: "Qdrant is unavailable"},
	}
	handler := newRuntimeServer(t, runtime).Handler()

	response := performRequest(handler, testRequest{
		method: "POST", target: "/v1/search", headers: tenantAuth(), body: `{"query":"x"}`,
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}

	// The result-snapshot read path was removed: the URL is not routed anymore.
	removed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/search-results/123e4567-e89b-12d3-a456-426614174000", headers: tenantAuth(),
	})
	if removed.Code != http.StatusNotFound {
		t.Fatalf("removed search-results status = %d, want 404", removed.Code)
	}
}

// TestSearchResultEndpointRemoved pins that GET /v1/search-results/{id} no
// longer exists: both a well-formed id and a malformed one are unrouted 404s.
func TestSearchResultEndpointRemoved(t *testing.T) {
	runtime := &fakeServiceRuntime{}
	handler := newRuntimeServer(t, runtime).Handler()

	for _, target := range []string{
		"/v1/search-results/123e4567-e89b-12d3-a456-426614174000",
		"/v1/search-results/123e4567-e89b-12d3-a456-426614174000?page=2&page_size=5",
		"/v1/search-results/not-a-uuid",
		"/v1/search-results/123e4567-e89b-12d3-a456-426614174000?page=0",
	} {
		response := performRequest(handler, testRequest{
			method: "GET", target: target, headers: tenantAuth(),
		})
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", target, response.Code)
		}
	}
}

func TestArtifactEndpoints(t *testing.T) {
	artifactID := "123e4567-e89b-12d3-a456-426614174000"
	runtime := &fakeServiceRuntime{
		artifactPayload: map[string]any{"offset": 0, "length": 4, "data": "aGVsbG8="},
		downloadName:    "报告/../../evil.txt",
		downloadMime:    "text/plain",
		downloadBody:    []byte("hello"),
	}
	handler := newRuntimeServer(t, runtime).Handler()

	content := performRequest(handler, testRequest{
		method: "GET", target: "/v1/artifacts/" + artifactID + "/content?offset=10&length=20", headers: tenantAuth(),
	})
	if content.Code != http.StatusOK {
		t.Fatalf("content status = %d body = %s", content.Code, content.Body.String())
	}
	if runtime.artifactOffset != 10 || runtime.artifactLength != 20 {
		t.Fatalf("offset/length = %d/%d", runtime.artifactOffset, runtime.artifactLength)
	}
	if got := content.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("content Cache-Control = %q", got)
	}

	download := performRequest(handler, testRequest{
		method: "GET", target: "/v1/artifacts/" + artifactID, headers: tenantAuth(),
	})
	if download.Code != http.StatusOK {
		t.Fatalf("download status = %d", download.Code)
	}
	if download.Body.String() != "hello" {
		t.Fatalf("body = %q", download.Body.String())
	}
	disposition := download.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, `filename="evil.txt"`) || !strings.Contains(disposition, "filename*=UTF-8''") {
		t.Fatalf("Content-Disposition = %q", disposition)
	}
	if got := download.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}

	badRange := performRequest(handler, testRequest{
		method: "GET", target: "/v1/artifacts/" + artifactID + "/content?offset=-1", headers: tenantAuth(),
	})
	if badRange.Code != http.StatusBadRequest {
		t.Fatalf("bad range status = %d", badRange.Code)
	}
}

// Document and knowledge-base methods keep fakeServiceRuntime assignable to the
// full httpapi.Runtime interface; the search-focused tests do not exercise them.
func (f *fakeServiceRuntime) ListDocuments(context.Context, string) ([]map[string]any, error) {
	return nil, nil
}

func (f *fakeServiceRuntime) Delete(context.Context, string, string) (map[string]any, error) {
	return map[string]any{}, nil
}

func (f *fakeServiceRuntime) CheckDocuments(context.Context, string, []CheckFile) (map[string]any, error) {
	return map[string]any{}, nil
}

func (f *fakeServiceRuntime) ReadDocument(context.Context, string, string) (string, string, error) {
	return "", "", nil
}

func (f *fakeServiceRuntime) ReadDocumentFile(context.Context, string, string) ([]byte, error) {
	return nil, nil
}

func (f *fakeServiceRuntime) PublicKnowledgeBase(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}

// Upload status read-model methods keep fakeServiceRuntime assignable to the
// full httpapi.Runtime interface.
func (f *fakeServiceRuntime) UploadOperations(context.Context, string) ([]map[string]any, error) {
	return nil, nil
}

func (f *fakeServiceRuntime) UploadOperation(context.Context, string, string) (map[string]any, bool, error) {
	return nil, false, nil
}

func (f *fakeServiceRuntime) PublishedVersions(context.Context, string) (map[string]string, error) {
	return nil, nil
}

func (f *fakeServiceRuntime) UploadLeases(context.Context, string) ([]map[string]any, error) {
	return nil, nil
}
