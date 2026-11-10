package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/config"
)

// asyncSettings enables the asynchronous admission mode on a test server.
func asyncSettings(token string) config.ServiceSettings {
	settings := testSettings(token)
	settings.UploadMode = config.UploadModeAsync
	return settings
}

func newAsyncRuntimeServer(t *testing.T, runtime Runtime) *Server {
	t.Helper()
	return New(asyncSettings("test-token"), nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithRuntime(runtime))
}

// multipartFilesBody renders one multipart body with several "files" parts.
func multipartFilesBody(t *testing.T, files map[string]string, fields map[string]string) (string, string) {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	for filename, content := range files {
		part, err := writer.CreateFormFile("files", filename)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatalf("write content: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buffer.String(), writer.FormDataContentType()
}

// asyncUpload posts one multipart batch to the async server and decodes the
// response body.
func asyncUpload(t *testing.T, handler http.Handler, body, contentType, idempotencyKey string) (int, map[string]any) {
	t.Helper()
	headers := map[string]string{
		"Authorization": "Bearer test-token", "X-RAG-Namespace": "alpha",
		"Content-Type": contentType,
	}
	if idempotencyKey != "" {
		headers["Idempotency-Key"] = idempotencyKey
	}
	response := performRequest(handler, testRequest{
		method: "POST", target: "/v1/documents", headers: headers, body: body,
	})
	return response.Code, decodeBody(t, response)
}

func TestAsyncUploadAdmitsJobsAndServesStatus(t *testing.T) {
	runtime := newFakeDocRuntime()
	handler := newAsyncRuntimeServer(t, runtime).Handler()

	body, contentType := multipartFilesBody(t, map[string]string{
		"manual.md": "# Manual\n\nWater supply procedure.",
		"guide.txt": "Guide text",
	}, nil)
	code, payload := asyncUpload(t, handler, body, contentType, "batch-7")
	if code != http.StatusOK {
		t.Fatalf("status = %d payload = %v", code, payload)
	}
	if payload["mode"] != config.UploadModeAsync {
		t.Fatalf("mode = %v", payload["mode"])
	}
	// The synchronous runtime must never be touched in async mode.
	if len(runtime.upoadFiles) != 0 {
		t.Fatalf("sync Upload called in async mode: %+v", runtime.upoadFiles)
	}
	jobs, ok := payload["jobs"].([]any)
	if !ok || len(jobs) != 2 {
		t.Fatalf("jobs = %v", payload["jobs"])
	}
	if failed, ok := payload["failed"].([]any); !ok || len(failed) != 0 {
		t.Fatalf("failed = %v", payload["failed"])
	}
	if len(runtime.admitted) != 2 {
		t.Fatalf("admitted = %v", runtime.admitted)
	}
	// Per-file idempotency keys follow sha256("<header>:<position>").
	for position, want := range []string{
		hashKey("batch-7:0"), hashKey("batch-7:1"),
	} {
		if runtime.admittedKeys[position] != want {
			t.Errorf("admitted key %d = %q, want %q", position, runtime.admittedKeys[position], want)
		}
	}
	firstJob, _ := jobs[0].(map[string]any)
	artifactID, _ := firstJob["artifact_id"].(string)
	if artifactID == "" || firstJob["status"] != "queued" {
		t.Fatalf("first job = %v", firstJob)
	}

	runtime.statusByID[artifactID] = map[string]any{
		"artifact_id": artifactID, "status": "processing", "stage": "embedding", "error": nil,
	}
	status := performRequest(handler, testRequest{
		method: "GET", target: "/v1/document-jobs/" + artifactID, headers: tenantAuth(),
	})
	if status.Code != http.StatusOK {
		t.Fatalf("status endpoint = %d body = %s", status.Code, status.Body.String())
	}
	if got := status.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	statusBody := decodeBody(t, status)
	if statusBody["status"] != "processing" || statusBody["stage"] != "embedding" {
		t.Fatalf("status body = %v", statusBody)
	}

	// Unknown and invalid artifact ids are a 404, never a cross-tenant probe.
	runtime.statusErr = &apierrors.KnowledgeValidationError{Message: "Document upload not found"}
	missing := performRequest(handler, testRequest{
		method: "GET", target: "/v1/document-jobs/00000000-0000-4000-8000-000000000000",
		headers: tenantAuth(),
	})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d body = %s", missing.Code, missing.Body.String())
	}
	if detail := decodeBody(t, missing)["detail"]; detail != "Document upload not found" {
		t.Fatalf("missing detail = %v", detail)
	}
}

func TestAsyncUploadIsolatesAdmissionFailures(t *testing.T) {
	runtime := newFakeDocRuntime()
	runtime.admitErrFor["conflict.md"] = &apierrors.KnowledgeValidationError{
		Message: "Document conflicts with an existing filename, queued upload or idempotency key.",
	}
	handler := newAsyncRuntimeServer(t, runtime).Handler()

	body, contentType := multipartFilesBody(t, map[string]string{
		"good.md":     "# Good",
		"conflict.md": "# Conflict",
	}, nil)
	code, payload := asyncUpload(t, handler, body, contentType, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d payload = %v", code, payload)
	}
	jobs, _ := payload["jobs"].([]any)
	failed, _ := payload["failed"].([]any)
	if len(jobs) != 1 || len(failed) != 1 {
		t.Fatalf("jobs/failed = %v / %v", jobs, failed)
	}
	failure, _ := failed[0].(map[string]any)
	if failure["name"] != "conflict.md" ||
		failure["error"] != "Document conflicts with an existing filename, queued upload or idempotency key." {
		t.Fatalf("failed entry = %v", failure)
	}
	if len(runtime.admittedKeys) != 2 || runtime.admittedKeys[0] != "" || runtime.admittedKeys[1] != "" {
		t.Fatalf("idempotency keys without header = %v", runtime.admittedKeys)
	}
}

func TestAsyncUploadValidatesBatchBeforeAdmission(t *testing.T) {
	runtime := newFakeDocRuntime()
	handler := newAsyncRuntimeServer(t, runtime).Handler()

	tooMany := map[string]string{}
	for index := 0; index < MaxFilesPerBatch+1; index++ {
		tooMany[fmt.Sprintf("file-%d.md", index)] = "content"
	}
	body, contentType := multipartFilesBody(t, tooMany, nil)
	code, payload := asyncUpload(t, handler, body, contentType, "")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("batch limit status = %d payload = %v", code, payload)
	}
	detail, _ := payload["detail"].([]any)
	if len(detail) != 1 {
		t.Fatalf("validation detail = %v", payload["detail"])
	}
	entry, _ := detail[0].(map[string]any)
	if entry["type"] != "too_long" {
		t.Fatalf("validation entry = %v", entry)
	}

	unsupportedBody, unsupportedType := multipartFilesBody(t, map[string]string{"notes.exe": "x"}, nil)
	code, payload = asyncUpload(t, handler, unsupportedBody, unsupportedType, "")
	if code != http.StatusBadRequest {
		t.Fatalf("unsupported status = %d payload = %v", code, payload)
	}
	want := "暂不支持该文件类型：notes.exe；请选择 .doc、.docx、.md、.pdf、.txt。"
	if payload["detail"] != want {
		t.Fatalf("unsupported detail = %v, want %q", payload["detail"], want)
	}
	if len(runtime.admitted) != 0 {
		t.Fatalf("invalid batch reached admission: %v", runtime.admitted)
	}
}

func TestDefaultUploadModeIsAsync(t *testing.T) {
	runtime := newFakeDocRuntime()
	// An empty setting follows the asynchronous default: admission only.
	settings := testSettings("test-token")
	settings.UploadMode = ""
	handler := New(settings, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithRuntime(runtime)).Handler()

	body, contentType := multipartBody(t, nil, "notes.md", "hello")
	code, payload := asyncUpload(t, handler, body, contentType, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d payload = %v", code, payload)
	}
	if payload["mode"] != config.UploadModeAsync {
		t.Fatalf("mode = %v", payload["mode"])
	}
	if len(runtime.admitted) != 1 || runtime.admitted[0] != "notes.md" {
		t.Fatalf("async admission did not run: %v", runtime.admitted)
	}
	if len(runtime.upoadFiles) != 0 {
		t.Fatalf("sync Upload called in the default async mode: %v", runtime.upoadFiles)
	}
}

func TestExplicitSyncUploadModeRemainsAvailable(t *testing.T) {
	runtime := newFakeDocRuntime()
	// Only an explicit "sync" setting keeps the legacy in-request parse.
	settings := testSettings("test-token")
	settings.UploadMode = config.UploadModeSync
	handler := New(settings, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithRuntime(runtime)).Handler()

	body, contentType := multipartBody(t, nil, "notes.md", "hello")
	code, payload := asyncUpload(t, handler, body, contentType, "retry-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d payload = %v", code, payload)
	}
	if payload["mode"] != config.UploadModeSync {
		t.Fatalf("mode = %v", payload["mode"])
	}
	if len(runtime.upoadFiles) != 1 || runtime.uploadKey != "retry-1" {
		t.Fatalf("sync upload did not run: files=%v key=%q", runtime.upoadFiles, runtime.uploadKey)
	}
	if len(runtime.admitted) != 0 {
		t.Fatalf("async admission used in sync mode: %v", runtime.admitted)
	}
}

// hashKey mirrors uploadFileIdempotencyKey in the test.
func hashKey(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
