package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
)

func newPurgeKBHandler(t *testing.T, runtime *fakeKBAdminRuntime) http.Handler {
	t.Helper()
	if runtime.fakeDocRuntime == nil {
		runtime.fakeDocRuntime = newFakeDocRuntime()
	}
	return newKBAdminServer(t, runtime)
}

func TestPurgeKnowledgeBaseDeletesAndReturnsNoContent(t *testing.T) {
	runtime := &fakeKBAdminRuntime{
		purgeResults: map[string]KnowledgeBasePurgeResult{
			"alpha": {DocumentsDeleted: 3, ConfigurationDeleted: true},
		},
	}
	handler := newPurgeKBHandler(t, runtime)
	response := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/admin/knowledge-bases/alpha", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("purge status = %d body = %s", response.Code, response.Body.String())
	}
	if response.Body.Len() != 0 {
		t.Fatalf("204 response body = %q", response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	if len(runtime.purgeCalls) != 1 || runtime.purgeCalls[0] != "alpha" {
		t.Fatalf("purge calls = %v", runtime.purgeCalls)
	}
}

func TestPurgeKnowledgeBaseMissingNamespaceReturnsNotFound(t *testing.T) {
	runtime := &fakeKBAdminRuntime{}
	handler := newPurgeKBHandler(t, runtime)
	response := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/admin/knowledge-bases/gone", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if detail := decodeBody(t, response)["detail"]; detail != messageKnowledgeBasePurgeMissing {
		t.Fatalf("detail = %v", detail)
	}
}

func TestPurgeKnowledgeBasePartialFailureKeepsConfiguration(t *testing.T) {
	runtime := &fakeKBAdminRuntime{
		purgeResults: map[string]KnowledgeBasePurgeResult{
			"alpha": {
				DocumentsDeleted:     1,
				ConfigurationDeleted: false,
				FailedDocuments: []map[string]string{
					{"name": "locked.md", "error": "文档正在更新，请稍后再删除"},
				},
			},
		},
	}
	handler := newPurgeKBHandler(t, runtime)
	response := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/admin/knowledge-bases/alpha", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	body := decodeBody(t, response)
	if body["detail"] != messageKnowledgeBasePurgePartial {
		t.Fatalf("detail = %v", body["detail"])
	}
	failed, _ := body["failed_documents"].([]any)
	if len(failed) != 1 {
		t.Fatalf("failed_documents = %v", body["failed_documents"])
	}
	item, _ := failed[0].(map[string]any)
	if item["name"] != "locked.md" || item["error"] != "文档正在更新，请稍后再删除" {
		t.Fatalf("failed document = %v", item)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
}

func TestPurgeKnowledgeBaseRuntimeErrorMapsToServiceUnavailable(t *testing.T) {
	runtime := &fakeKBAdminRuntime{
		purgeBatchErr: &apierrors.KnowledgeError{Message: "知识库文档无法读取"},
	}
	handler := newPurgeKBHandler(t, runtime)
	response := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/admin/knowledge-bases/alpha", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if detail := decodeBody(t, response)["detail"]; detail != "知识库文档无法读取" {
		t.Fatalf("detail = %v", detail)
	}
}

func TestBatchDeleteKnowledgeBasesSeparatesDeletedMissingAndFailed(t *testing.T) {
	runtime := &fakeKBAdminRuntime{
		purgeResults: map[string]KnowledgeBasePurgeResult{
			"alpha": {DocumentsDeleted: 2, ConfigurationDeleted: true},
			"beta": {
				DocumentsDeleted: 1,
				FailedDocuments: []map[string]string{
					{"name": "locked.md", "error": "文档正在更新，请稍后再删除"},
				},
			},
		},
		purgeErrors: map[string]error{
			"gamma": &apierrors.KnowledgeError{Message: "知识库文档无法读取"},
		},
	}
	handler := newPurgeKBHandler(t, runtime)
	response := performRequest(handler, testRequest{
		method:  "POST",
		target:  "/v1/admin/knowledge-bases/batch-delete",
		remote:  "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"namespaces":["alpha","gone","beta","gamma","alpha"]}`,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	body := decodeBody(t, response)
	deleted, _ := body["deleted"].([]any)
	if len(deleted) != 1 || deleted[0] != "alpha" {
		t.Fatalf("deleted = %v", body["deleted"])
	}
	missing, _ := body["missing"].([]any)
	if len(missing) != 1 || missing[0] != "gone" {
		t.Fatalf("missing = %v", body["missing"])
	}
	failed, _ := body["failed"].([]any)
	if len(failed) != 2 {
		t.Fatalf("failed = %v", body["failed"])
	}
	beta, _ := failed[0].(map[string]any)
	if beta["namespace"] != "beta" || beta["error"] != messageKnowledgeBasePurgePartial {
		t.Fatalf("beta failure = %v", beta)
	}
	if documents, _ := beta["failed_documents"].([]any); len(documents) != 1 {
		t.Fatalf("beta failed_documents = %v", beta["failed_documents"])
	}
	gamma, _ := failed[1].(map[string]any)
	if gamma["namespace"] != "gamma" || gamma["error"] != messageKnowledgeBasePurgePartial {
		t.Fatalf("gamma failure = %v", gamma)
	}
	gammaDocuments, _ := gamma["failed_documents"].([]any)
	if len(gammaDocuments) != 1 {
		t.Fatalf("gamma failed_documents = %v", gamma["failed_documents"])
	}
	gammaItem, _ := gammaDocuments[0].(map[string]any)
	if gammaItem["error"] != "知识库文档无法读取" {
		t.Fatalf("gamma failed document = %v", gammaItem)
	}
	if body["documents_deleted"] != float64(2) {
		t.Fatalf("documents_deleted = %v", body["documents_deleted"])
	}
	if body["configurations_deleted"] != float64(1) {
		t.Fatalf("configurations_deleted = %v", body["configurations_deleted"])
	}
	want := []string{"alpha", "gone", "beta", "gamma"}
	if len(runtime.purgeCalls) != len(want) {
		t.Fatalf("purge calls = %v", runtime.purgeCalls)
	}
	for index, namespace := range want {
		if runtime.purgeCalls[index] != namespace {
			t.Fatalf("purge calls = %v, want %v", runtime.purgeCalls, want)
		}
	}
}

func TestBatchDeleteKnowledgeBasesValidation(t *testing.T) {
	handler := newPurgeKBHandler(t, &fakeKBAdminRuntime{})
	valid := make([]string, 101)
	for index := range valid {
		valid[index] = "ns"
	}
	tooMany, err := json.Marshal(map[string]any{"namespaces": valid})
	if err != nil {
		t.Fatalf("marshal namespaces: %v", err)
	}
	cases := []struct {
		name   string
		body   string
		status int
	}{
		{name: "missing field", body: `{}`, status: http.StatusUnprocessableEntity},
		{name: "empty list", body: `{"namespaces":[]}`, status: http.StatusUnprocessableEntity},
		{name: "invalid name", body: `{"namespaces":["ok","bad name"]}`, status: http.StatusUnprocessableEntity},
		{name: "extra field", body: `{"namespaces":["ok"],"extra":1}`, status: http.StatusUnprocessableEntity},
		{name: "too many", body: string(tooMany), status: http.StatusUnprocessableEntity},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := performRequest(handler, testRequest{
				method:  "POST",
				target:  "/v1/admin/knowledge-bases/batch-delete",
				remote:  "127.0.0.1:5000",
				headers: map[string]string{"Content-Type": "application/json"},
				body:    testCase.body,
			})
			if response.Code != testCase.status {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
			details, _ := decodeBody(t, response)["detail"].([]any)
			if len(details) == 0 {
				t.Fatalf("missing validation details: %s", response.Body.String())
			}
			if testCase.name == "invalid name" {
				item, _ := details[0].(map[string]any)
				if item["type"] != "string_pattern_mismatch" {
					t.Fatalf("detail = %v", item)
				}
				location, _ := item["loc"].([]any)
				if len(location) != 3 || location[2] != float64(1) {
					t.Fatalf("loc = %v", location)
				}
			}
			if testCase.name == "too many" {
				item, _ := details[0].(map[string]any)
				if item["type"] != "too_long" {
					t.Fatalf("detail = %v", item)
				}
			}
		})
	}
}

func TestBatchDeleteKnowledgeBasesRequiresAdminForNonLoopback(t *testing.T) {
	handler := newPurgeKBHandler(t, &fakeKBAdminRuntime{})
	response := performRequest(handler, testRequest{
		method:  "POST",
		target:  "/v1/admin/knowledge-bases/batch-delete",
		remote:  "10.0.0.9:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"namespaces":["alpha"]}`,
	})
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}
