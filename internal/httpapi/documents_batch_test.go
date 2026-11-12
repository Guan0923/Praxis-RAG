package httpapi

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"sort"
	"testing"
)

// batchMultipartBody builds a multipart body with one part per file.
func batchMultipartBody(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write([]byte(files[name])); err != nil {
			t.Fatalf("write content: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buffer.String(), writer.FormDataContentType()
}

func TestDocumentBatchEndpoints(t *testing.T) {
	runtime := newFakeDocRuntime()
	handler := newRuntimeServer(t, runtime).Handler()

	content, contentType := batchMultipartBody(t, map[string]string{"a.pdf": "one", "b.md": "two"})
	created := performRequest(handler, testRequest{
		method: "POST", target: "/v1/document-batches",
		headers: map[string]string{
			"Authorization": "Bearer test-token", "X-RAG-Namespace": "alpha",
			"Content-Type": contentType, "Idempotency-Key": "batch-1",
		},
		body: content,
	})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status = %d body = %s", created.Code, created.Body.String())
	}
	names := make([]string, 0, len(runtime.batchFiles))
	for _, file := range runtime.batchFiles {
		names = append(names, file.Name)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "a.pdf" || names[1] != "b.md" {
		t.Fatalf("batch files = %v", names)
	}
	if runtime.batchKey != "batch-1" {
		t.Fatalf("idempotency key = %q", runtime.batchKey)
	}
	if body := decodeBody(t, created); body["batch_id"] != "batch-1" {
		t.Fatalf("create body = %v", body)
	}

	runtime.batchByID = map[string]map[string]any{
		"batch-1": {
			"batch_id": "batch-1", "status": "completed", "total": float64(2),
			"accepted": float64(1), "skipped": float64(1), "failed": float64(0),
			"items": []any{},
		},
	}
	detail := performRequest(handler, testRequest{
		method: "GET", target: "/v1/document-batches/batch-1", headers: tenantAuth(),
	})
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status = %d body = %s", detail.Code, detail.Body.String())
	}
	if body := decodeBody(t, detail); body["status"] != "completed" {
		t.Fatalf("detail body = %v", body)
	}

	missing := performRequest(handler, testRequest{
		method: "GET", target: "/v1/document-batches/unknown", headers: tenantAuth(),
	})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing batch status = %d", missing.Code)
	}

	runtime.batches = []map[string]any{{"batch_id": "batch-2", "status": "processing"}}
	listed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/document-batches?limit=5", headers: tenantAuth(),
	})
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", listed.Code, listed.Body.String())
	}
	if runtime.batchLimit != 5 {
		t.Fatalf("list limit = %d", runtime.batchLimit)
	}
	body := decodeBody(t, listed)
	if batches, ok := body["batches"].([]any); !ok || len(batches) != 1 {
		t.Fatalf("list body = %v", body)
	}

	invalidLimit := performRequest(handler, testRequest{
		method: "GET", target: "/v1/document-batches?limit=0", headers: tenantAuth(),
	})
	if invalidLimit.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid limit status = %d", invalidLimit.Code)
	}
}

func TestDocumentBatchRejectsOversizedBatch(t *testing.T) {
	runtime := newFakeDocRuntime()
	handler := newRuntimeServer(t, runtime).Handler()
	files := map[string]string{}
	for index := 0; index < 11; index++ {
		files[string(rune('a'+index))+".md"] = "content"
	}
	content, contentType := batchMultipartBody(t, files)
	rejected := performRequest(handler, testRequest{
		method: "POST", target: "/v1/document-batches",
		headers: map[string]string{
			"Authorization": "Bearer test-token", "X-RAG-Namespace": "alpha",
			"Content-Type": contentType,
		},
		body: content,
	})
	if rejected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized batch status = %d body = %s", rejected.Code, rejected.Body.String())
	}
	if len(runtime.batchFiles) != 0 {
		t.Fatalf("oversized batch reached the runtime: %v", runtime.batchFiles)
	}
}
