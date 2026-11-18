package httpapi

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// fakeDocRuntime extends fakeServiceRuntime with document and knowledge-base
// methods so the same server can serve the full HTTP surface in tests.
type fakeDocRuntime struct {
	*fakeServiceRuntime

	documents     []map[string]any
	listErr       error
	deletePayload map[string]any
	deleteErr     error
	checkPayload  map[string]any
	checkFiles    []CheckFile
	readText      string
	readVersion   string
	readErr       error
	readFileBytes []byte
	readFileErr   error
	publicKB      map[string]any
	publicKBErr   error
	statusByID    map[string]map[string]any
	statusErr     error

	batchFiles   []UploadFile
	batchKey     string
	batchOwner   string
	batchPayload map[string]any
	batchErr     error
	batchByID    map[string]map[string]any
	batchViewer  string
	batchLimit   int
	batches      []map[string]any

	writeName      string
	writeContent   []byte
	writeOverwrite bool
	writeKey       string
	writePayload   map[string]any
	writeErr       error
}

func newFakeDocRuntime() *fakeDocRuntime {
	return &fakeDocRuntime{
		fakeServiceRuntime: &fakeServiceRuntime{},
		deletePayload:      map[string]any{"deleted": map[string]any{}, "documents": []any{}},
		checkPayload:       map[string]any{"duplicates": []any{}},
		readText:           "hello",
		publicKB:           map[string]any{"namespace": "alpha", "collection": "knowledge_x"},
		statusByID:         map[string]map[string]any{},
	}
}

func (f *fakeDocRuntime) ListDocuments(context.Context, string) ([]map[string]any, error) {
	return f.documents, f.listErr
}

func (f *fakeDocRuntime) GetDocumentStatus(_ context.Context, _ string, artifactID string) (map[string]any, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	status, ok := f.statusByID[artifactID]
	if !ok {
		return map[string]any{
			"artifact_id": artifactID, "status": "queued", "stage": "queued", "error": nil,
		}, nil
	}
	return status, nil
}

func (f *fakeDocRuntime) Delete(context.Context, string, string) (map[string]any, error) {
	return f.deletePayload, f.deleteErr
}

// WriteDocument records the synchronous replace request for assertions.
func (f *fakeDocRuntime) WriteDocument(_ context.Context, _, name string, content []byte, overwrite bool, idempotencyKey string) (map[string]any, error) {
	f.writeName = name
	f.writeContent = append([]byte(nil), content...)
	f.writeOverwrite = overwrite
	f.writeKey = idempotencyKey
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	if f.writePayload != nil {
		return f.writePayload, nil
	}
	return map[string]any{
		"status":    "updated",
		"document":  map[string]any{"name": name},
		"documents": []any{map[string]any{"name": name}},
	}, nil
}

// SubmitDocumentBatch records the batch request for assertions.
func (f *fakeDocRuntime) SubmitDocumentBatch(_ context.Context, _ string, files []UploadFile, idempotencyKey, owner string) (map[string]any, error) {
	f.batchFiles = files
	f.batchKey = idempotencyKey
	f.batchOwner = owner
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	if f.batchPayload != nil {
		return f.batchPayload, nil
	}
	return map[string]any{"batch_id": "batch-1", "status": "processing", "total": len(files)}, nil
}

// GetDocumentBatch returns the canned batch record, or nil for an unknown id.
func (f *fakeDocRuntime) GetDocumentBatch(_ context.Context, _ string, batchID, viewer string) (map[string]any, error) {
	f.batchViewer = viewer
	payload, ok := f.batchByID[batchID]
	if !ok {
		return nil, nil
	}
	return payload, nil
}

// ListDocumentBatches returns the canned active batches.
func (f *fakeDocRuntime) ListDocumentBatches(_ context.Context, _ string, viewer string, limit int) (map[string]any, error) {
	f.batchViewer = viewer
	f.batchLimit = limit
	return map[string]any{"batches": f.batches}, nil
}

func (f *fakeDocRuntime) CheckDocuments(_ context.Context, _ string, files []CheckFile) (map[string]any, error) {
	f.checkFiles = files
	return f.checkPayload, nil
}

func (f *fakeDocRuntime) ReadDocument(context.Context, string, string) (string, string, error) {
	return f.readText, f.readVersion, f.readErr
}

func (f *fakeDocRuntime) ReadDocumentFile(context.Context, string, string) ([]byte, error) {
	return f.readFileBytes, f.readFileErr
}

func (f *fakeDocRuntime) PublicKnowledgeBase(context.Context, string) (map[string]any, error) {
	return f.publicKB, f.publicKBErr
}

func multipartBody(t *testing.T, fields map[string]string, filename, content string) (string, string) {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	part, err := writer.CreateFormFile("files", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatalf("write content: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buffer.String(), writer.FormDataContentType()
}

func TestListDocumentsReturnsServiceUnavailableOnCacheRetry(t *testing.T) {
	runtime := newFakeDocRuntime()
	runtime.listErr = &state.CacheRetryable{Message: "State cache backfill wait timed out; retry later"}
	handler := newRuntimeServer(t, runtime).Handler()

	response := performRequest(handler, testRequest{method: "GET", target: "/v1/documents", headers: tenantAuth()})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("list status = %d body = %s", response.Code, response.Body.String())
	}
	if detail := decodeBody(t, response)["detail"]; detail != "服务繁忙，请稍后重试" {
		t.Fatalf("list detail = %v", detail)
	}
}

func TestDocumentsEndpoints(t *testing.T) {
	runtime := newFakeDocRuntime()
	runtime.documents = []map[string]any{{"name": "a.md", "size": 5}}
	runtime.readFileBytes = []byte("%PDF-1.4")
	handler := newRuntimeServer(t, runtime).Handler()

	listed := performRequest(handler, testRequest{method: "GET", target: "/v1/documents", headers: tenantAuth()})
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", listed.Code, listed.Body.String())
	}
	body := decodeBody(t, listed)
	if docs, ok := body["documents"].([]any); !ok || len(docs) != 1 {
		t.Fatalf("documents = %v", body["documents"])
	}
	if extensions, ok := body["supported_extensions"].([]any); !ok || len(extensions) != 7 {
		t.Fatalf("extensions = %v", body["supported_extensions"])
	}
	if body["max_files_per_batch"] != float64(10) || body["max_file_bytes"] != float64(10485760) {
		t.Fatalf("limits = %v / %v", body["max_files_per_batch"], body["max_file_bytes"])
	}

	content, contentType := multipartBody(t, nil, "notes.md", "hello")
	uploaded := performRequest(handler, testRequest{
		method: "POST", target: "/v1/document-batches", headers: map[string]string{
			"Authorization": "Bearer test-token", "X-RAG-Namespace": "alpha",
			"Content-Type": contentType, "Idempotency-Key": "retry-1",
		},
		body: content,
	})
	if uploaded.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d body = %s", uploaded.Code, uploaded.Body.String())
	}
	if len(runtime.batchFiles) != 1 || runtime.batchFiles[0].Name != "notes.md" ||
		string(runtime.batchFiles[0].Content) != "hello" {
		t.Fatalf("uploaded files = %+v", runtime.batchFiles)
	}
	if runtime.batchKey != "retry-1" {
		t.Fatalf("idempotency key = %q", runtime.batchKey)
	}

	extraFieldBody, extraContentType := multipartBody(t, map[string]string{"chunking": "token"}, "notes.md", "hello")
	rejected := performRequest(handler, testRequest{
		method: "POST", target: "/v1/document-batches", headers: map[string]string{
			"Authorization": "Bearer test-token", "X-RAG-Namespace": "alpha", "Content-Type": extraContentType,
		},
		body: extraFieldBody,
	})
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("extra field status = %d", rejected.Code)
	}
	if detail := decodeBody(t, rejected)["detail"]; detail != "上传仅接受文件，切分和向量参数由知识库配置决定" {
		t.Fatalf("detail = %v", detail)
	}

	text := performRequest(handler, testRequest{
		method: "GET", target: "/v1/documents/a.md/content", headers: tenantAuth(),
	})
	if text.Code != http.StatusOK || text.Body.String() != "hello" {
		t.Fatalf("content = %d %q", text.Code, text.Body.String())
	}
	if got := text.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("Content-Type = %q", got)
	}

	pdf := performRequest(handler, testRequest{
		method: "GET", target: "/v1/documents/a.pdf/content", headers: tenantAuth(),
	})
	if pdf.Code != http.StatusOK || pdf.Body.String() != "%PDF-1.4" {
		t.Fatalf("pdf content = %d %q", pdf.Code, pdf.Body.String())
	}
	if got := pdf.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("pdf Content-Type = %q", got)
	}

	deleted := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/documents/a.md", headers: tenantAuth(),
	})
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status = %d", deleted.Code)
	}

	sha := strings.Repeat("a", 64)
	checked := performRequest(handler, testRequest{
		method: "POST", target: "/v1/documents/check", headers: tenantAuth(),
		body: `{"files":[{"name":"a.md","sha256":"` + sha + `"}]}`,
	})
	if checked.Code != http.StatusOK {
		t.Fatalf("check status = %d body = %s", checked.Code, checked.Body.String())
	}
	if len(runtime.checkFiles) != 1 || runtime.checkFiles[0].Name != "a.md" {
		t.Fatalf("check files = %+v", runtime.checkFiles)
	}

	invalidCheck := performRequest(handler, testRequest{
		method: "POST", target: "/v1/documents/check", headers: tenantAuth(),
		body: `{"files":[{"name":"a.md","sha256":"xyz"}]}`,
	})
	if invalidCheck.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid check status = %d", invalidCheck.Code)
	}

	unsupported := performRequest(handler, testRequest{
		method: "POST", target: "/v1/documents/check", headers: tenantAuth(),
		body: `{"files":[{"name":"bad.exe","sha256":"` + sha + `"}]}`,
	})
	if unsupported.Code != http.StatusBadRequest {
		t.Fatalf("unsupported check status = %d body = %s", unsupported.Code, unsupported.Body.String())
	}
	wantMessage := "暂不支持该文件类型：bad.exe；请选择 .doc、.docx、.htm、.html、.md、.pdf、.txt。"
	if body := decodeBody(t, unsupported); body["detail"] != wantMessage {
		t.Fatalf("unsupported detail = %v, want %q", body["detail"], wantMessage)
	}
	if len(runtime.checkFiles) != 1 {
		t.Fatalf("handler forwarded an unsupported name to the runtime: %+v", runtime.checkFiles)
	}
}

func TestKnowledgeBaseEndpoint(t *testing.T) {
	runtime := newFakeDocRuntime()
	handler := newRuntimeServer(t, runtime).Handler()

	response := performRequest(handler, testRequest{method: "GET", target: "/v1/knowledge-base", headers: tenantAuth()})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	body := decodeBody(t, response)
	if body["namespace"] != "alpha" || body["collection"] != "knowledge_x" {
		t.Fatalf("body = %v", body)
	}
	strategies, ok := body["strategies"].([]any)
	if !ok || len(strategies) != 5 {
		t.Fatalf("strategies = %v", body["strategies"])
	}

	runtime.publicKBErr = missingKnowledgeBase()
	missing := performRequest(handler, testRequest{method: "GET", target: "/v1/knowledge-base", headers: tenantAuth()})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d", missing.Code)
	}
}

func missingKnowledgeBase() error {
	return &apierrors.KnowledgeValidationError{Message: "该知识库没有配置记录，请先创建知识库"}
}

func TestMCPMountAuthentication(t *testing.T) {
	server := newTestServer(t, testSettings("test-token"), nil)
	server.mcpHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		namespace, _ := NamespaceFromContext(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"namespace": namespace})
	})
	handler := server.Handler()

	missingAuth := performRequest(handler, testRequest{method: "GET", target: "/mcp"})
	if missingAuth.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d", missingAuth.Code)
	}
	if got := missingAuth.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
	if detail := decodeBody(t, missingAuth)["detail"]; detail != "MCP 请求必须提供 Authorization: Bearer <API_KEY>" {
		t.Fatalf("detail = %v", detail)
	}

	wrongToken := performRequest(handler, testRequest{
		method: "GET", target: "/mcp", headers: map[string]string{"Authorization": "Bearer wrong", "X-RAG-Namespace": "alpha"},
	})
	if wrongToken.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", wrongToken.Code)
	}
	if got := wrongToken.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("unexpected WWW-Authenticate = %q", got)
	}

	missingNamespace := performRequest(handler, testRequest{
		method: "GET", target: "/mcp", headers: map[string]string{"Authorization": "Bearer test-token"},
	})
	if missingNamespace.Code != http.StatusBadRequest {
		t.Fatalf("missing namespace status = %d", missingNamespace.Code)
	}

	ok := performRequest(handler, testRequest{
		method: "POST", target: "/mcp",
		headers: map[string]string{"Authorization": "Bearer test-token", "X-RAG-Namespace": "alpha"},
	})
	if ok.Code != http.StatusOK {
		t.Fatalf("ok status = %d body = %s", ok.Code, ok.Body.String())
	}
	if namespace := decodeBody(t, ok)["namespace"]; namespace != "alpha" {
		t.Fatalf("namespace = %v", namespace)
	}
	if got := ok.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}

	unmounted := newTestServer(t, testSettings("test-token"), nil)
	response := performRequest(unmounted.Handler(), testRequest{
		method: "POST", target: "/mcp",
		headers: map[string]string{"Authorization": "Bearer test-token", "X-RAG-Namespace": "alpha"},
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unmounted status = %d", response.Code)
	}
}
