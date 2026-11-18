package httpapi

import (
	"net/http"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
)

func TestDocumentJobStatus(t *testing.T) {
	runtime := newFakeDocRuntime()
	artifactID := "9b2f9e9e-0000-4000-8000-000000000001"
	runtime.statusByID[artifactID] = map[string]any{
		"artifact_id": artifactID, "status": "processing", "stage": "embedding", "error": nil,
	}
	handler := newRuntimeServer(t, runtime).Handler()
	status := performRequest(handler, testRequest{
		method: "GET", target: "/v1/document-jobs/" + artifactID, headers: tenantAuth(),
	})
	if status.Code != http.StatusOK || status.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("job status = %d headers = %v body = %s", status.Code, status.Header(), status.Body.String())
	}
	payload := decodeBody(t, status)
	if payload["artifact_id"] != artifactID || payload["status"] != "processing" || payload["stage"] != "embedding" {
		t.Fatalf("job status payload = %v", payload)
	}
	reason := "Document processing failed: attempt 3/3: parse timeout"
	runtime.statusByID[artifactID] = map[string]any{
		"artifact_id": artifactID, "status": "failed", "stage": "failed", "error": reason,
	}
	failed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/document-jobs/" + artifactID, headers: tenantAuth(),
	})
	if failed.Code != http.StatusOK {
		t.Fatalf("failed job status = %d body = %s", failed.Code, failed.Body.String())
	}
	failedPayload := decodeBody(t, failed)
	if failedPayload["status"] != "failed" || failedPayload["error"] != reason {
		t.Fatalf("failed job payload = %v, want the last failure reason", failedPayload)
	}
	runtime.statusErr = &apierrors.KnowledgeValidationError{Message: "Document upload not found"}
	missing := performRequest(handler, testRequest{
		method: "GET", target: "/v1/document-jobs/" + artifactID, headers: tenantAuth(),
	})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing job status = %d body = %s", missing.Code, missing.Body.String())
	}
}

func TestDocumentUploadRouteRemoved(t *testing.T) {
	handler := newRuntimeServer(t, newFakeDocRuntime()).Handler()
	response := performRequest(handler, testRequest{
		method: "POST", target: "/v1/documents", headers: tenantAuth(),
	})
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET" {
		t.Fatalf("removed upload route = %d Allow=%q", response.Code, response.Header().Get("Allow"))
	}
}
