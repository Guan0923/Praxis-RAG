package httpapi

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/access"
	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/auth"
)

// replacementMultipart builds one multipart body whose single binary field is
// the replace endpoint's "file" part.
func replacementMultipart(t *testing.T, filename, content string) (string, string) {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)
	part, err := writer.CreateFormFile("file", filename)
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

func replaceRequest(t *testing.T, name, filename, content string, headers map[string]string) testRequest {
	t.Helper()
	body, contentType := replacementMultipart(t, filename, content)
	merged := map[string]string{"Content-Type": contentType}
	for key, value := range headers {
		merged[key] = value
	}
	return testRequest{
		method:  http.MethodPost,
		target:  "/v1/documents/" + name + "/replace",
		headers: merged,
		body:    body,
	}
}

func TestReplaceDocumentKeepsTargetName(t *testing.T) {
	runtime := newFakeDocRuntime()
	handler := newRuntimeServer(t, runtime).Handler()

	response := performRequest(handler, replaceRequest(t,
		"%E4%BD%BF%E7%94%A8%E6%89%8B%E5%86%8C.pdf", "使用手册-修订版.pdf", "%PDF-1.4 replacement", tenantAuth()))
	if response.Code != http.StatusOK {
		t.Fatalf("replace status = %d body = %s", response.Code, response.Body.String())
	}
	if runtime.writeName != "使用手册.pdf" {
		t.Fatalf("replaced name = %q", runtime.writeName)
	}
	if string(runtime.writeContent) != "%PDF-1.4 replacement" {
		t.Fatalf("replaced content = %q", runtime.writeContent)
	}
	if !runtime.writeOverwrite || runtime.writeKey != "" {
		t.Fatalf("overwrite/key = %v/%q", runtime.writeOverwrite, runtime.writeKey)
	}
	body := decodeBody(t, response)
	if body["status"] != "updated" {
		t.Fatalf("status = %v", body["status"])
	}
	documents, ok := body["documents"].([]any)
	if !ok || len(documents) != 1 {
		t.Fatalf("documents = %v", body["documents"])
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestReplaceDocumentRejectsMismatchedType(t *testing.T) {
	runtime := newFakeDocRuntime()
	handler := newRuntimeServer(t, runtime).Handler()

	response := performRequest(handler, replaceRequest(t, "manual.pdf", "manual.txt", "plain text", tenantAuth()))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("mismatched type status = %d body = %s", response.Code, response.Body.String())
	}
	detail, _ := decodeBody(t, response)["detail"].(string)
	if !strings.Contains(detail, "类型必须与原文档一致") {
		t.Fatalf("detail = %q", detail)
	}
	if runtime.writeName != "" {
		t.Fatalf("runtime was called for a mismatched type: %q", runtime.writeName)
	}
}

func TestReplaceDocumentRejectsUnsupportedAndMissing(t *testing.T) {
	runtime := newFakeDocRuntime()
	handler := newRuntimeServer(t, runtime).Handler()

	unsupported := performRequest(handler, replaceRequest(t, "manual.md", "manual.exe", "MZ", tenantAuth()))
	if unsupported.Code != http.StatusBadRequest {
		t.Fatalf("unsupported status = %d body = %s", unsupported.Code, unsupported.Body.String())
	}
	if detail, _ := decodeBody(t, unsupported)["detail"].(string); !strings.Contains(detail, "暂不支持该文件类型") {
		t.Fatalf("unsupported detail = %q", detail)
	}

	runtime.writeErr = &apierrors.KnowledgeValidationError{Message: "Document does not exist: gone.md"}
	missing := performRequest(handler, replaceRequest(t, "gone.md", "replacement.md", "new", tenantAuth()))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d body = %s", missing.Code, missing.Body.String())
	}
	if detail := decodeBody(t, missing)["detail"]; detail != "知识文件不存在：gone.md" {
		t.Fatalf("missing detail = %v", detail)
	}
}

func TestReplaceDocumentRequiresTargetWriteGrant(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		"user-token": userSession("user-token"),
	}}
	control := &fakeAccessControl{scopes: map[string]access.ScopeSet{
		// A single-file grant must be enough for the matching document and must
		// not extend to any other name.
		"u-user|alpha": writeFilesScope("manual.pdf"),
	}}
	runtime := newFakeDocRuntime()
	handler := newAccessTestServer(t, users, control, runtime).Handler()

	allowed := performRequest(handler, replaceRequest(t,
		"manual.pdf", "manual-revised.pdf", "%PDF-1.4 new", sessionHeaders("user-token", "alpha")))
	if allowed.Code != http.StatusOK {
		t.Fatalf("single-file writer status = %d body = %s", allowed.Code, allowed.Body.String())
	}
	if runtime.writeName != "manual.pdf" {
		t.Fatalf("allowed replaced name = %q", runtime.writeName)
	}

	runtime.writeName = ""
	denied := performRequest(handler, replaceRequest(t,
		"other.pdf", "other-revised.pdf", "%PDF-1.4 new", sessionHeaders("user-token", "alpha")))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("ungranted writer status = %d body = %s", denied.Code, denied.Body.String())
	}
	if runtime.writeName != "" {
		t.Fatalf("runtime was called without a write grant: %q", runtime.writeName)
	}
}

func TestReplaceDocumentRequiresOneFile(t *testing.T) {
	runtime := newFakeDocRuntime()
	handler := newRuntimeServer(t, runtime).Handler()

	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)
	if err := writer.WriteField("name", "manual.pdf"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	response := performRequest(handler, testRequest{
		method: http.MethodPost, target: "/v1/documents/manual.pdf/replace",
		headers: map[string]string{
			"Authorization": "Bearer test-token", "X-RAG-Namespace": "alpha",
			"Content-Type": writer.FormDataContentType(),
		},
		body: buffer.String(),
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("no-file status = %d body = %s", response.Code, response.Body.String())
	}
	if runtime.writeName != "" {
		t.Fatalf("runtime was called without a file")
	}
}
