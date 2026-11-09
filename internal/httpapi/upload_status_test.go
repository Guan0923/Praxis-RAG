package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
)

type fakeUploadRuntime struct {
	*fakeDocRuntime
	operations []map[string]any
	operation  map[string]any
	found      bool
	versions   map[string]string
	leases     []map[string]any
}

func (f *fakeUploadRuntime) UploadOperations(context.Context, string) ([]map[string]any, error) {
	return f.operations, nil
}

func (f *fakeUploadRuntime) UploadOperation(context.Context, string, string) (map[string]any, bool, error) {
	return f.operation, f.found, nil
}

func (f *fakeUploadRuntime) PublishedVersions(context.Context, string) (map[string]string, error) {
	return f.versions, nil
}

func (f *fakeUploadRuntime) UploadLeases(context.Context, string) ([]map[string]any, error) {
	return f.leases, nil
}

func TestUploadStatusEndpoints(t *testing.T) {
	runtime := &fakeUploadRuntime{
		fakeDocRuntime: newFakeDocRuntime(),
		operations:     []map[string]any{{"id": "op-1", "status": "COMMITTED"}},
		operation:      map[string]any{"id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "status": "RUNNING"},
		found:          true,
	}
	server := New(testSettings("test-token"), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), WithRuntime(runtime))
	handler := server.Handler()

	listed := performRequest(handler, testRequest{method: "GET", target: "/v1/uploads", headers: tenantAuth()})
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", listed.Code, listed.Body.String())
	}
	body := decodeBody(t, listed)
	operations, ok := body["operations"].([]any)
	if !ok || len(operations) != 1 {
		t.Fatalf("operations = %v", body["operations"])
	}
	if body["limit"] != float64(100) || body["display_only"] != true {
		t.Fatalf("body = %v", body)
	}
	if got := listed.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}

	detail := performRequest(handler, testRequest{
		method: "GET", target: "/v1/uploads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", headers: tenantAuth(),
	})
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status = %d body = %s", detail.Code, detail.Body.String())
	}
	if body := decodeBody(t, detail); body["display_only"] != true {
		t.Fatalf("detail body = %v", body)
	}

	invalid := performRequest(handler, testRequest{
		method: "GET", target: "/v1/uploads/not-an-operation", headers: tenantAuth(),
	})
	if invalid.Code != http.StatusNotFound {
		t.Fatalf("invalid status = %d", invalid.Code)
	}
	if detail := decodeBody(t, invalid)["detail"]; detail != "Upload operation does not exist" {
		t.Fatalf("detail = %v", detail)
	}

	runtime.found = false
	missing := performRequest(handler, testRequest{
		method: "GET", target: "/v1/uploads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", headers: tenantAuth(),
	})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d", missing.Code)
	}
}

func TestUploadAdminReadModels(t *testing.T) {
	runtime := &fakeUploadRuntime{
		fakeDocRuntime: newFakeDocRuntime(),
		versions:       map[string]string{"a.md": "v1"},
		leases:         []map[string]any{{"resource": "doc:a.md"}},
	}
	server := New(testSettings("test-token"), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), WithRuntime(runtime))
	handler := server.Handler()

	// Both routes require the local-management boundary AND tenant credentials.
	for _, target := range []string{"/v1/admin/published-versions", "/v1/admin/upload-leases"} {
		ok := performRequest(handler, testRequest{
			method: "GET", target: target, remote: "127.0.0.1:5000", headers: tenantAuth(),
		})
		if ok.Code != http.StatusOK {
			t.Fatalf("%s status = %d body = %s", target, ok.Code, ok.Body.String())
		}
		if got := ok.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s Cache-Control = %q", target, got)
		}
		noCredential := performRequest(handler, testRequest{
			method: "GET", target: target, remote: "127.0.0.1:5000",
			headers: map[string]string{"X-RAG-Namespace": "alpha"},
		})
		if noCredential.Code != http.StatusUnauthorized {
			t.Fatalf("%s without credentials status = %d", target, noCredential.Code)
		}
		remote := performRequest(handler, testRequest{
			method: "GET", target: target, remote: "10.0.0.5:5000", headers: tenantAuth(),
		})
		if remote.Code != http.StatusForbidden {
			t.Fatalf("%s remote status = %d", target, remote.Code)
		}
	}

	versions := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/published-versions", remote: "127.0.0.1:5000", headers: tenantAuth(),
	})
	if versionMap, ok := decodeBody(t, versions)["versions"].(map[string]any); !ok || versionMap["a.md"] != "v1" {
		t.Fatalf("versions = %v", decodeBody(t, versions)["versions"])
	}
	leases := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/upload-leases", remote: "127.0.0.1:5000", headers: tenantAuth(),
	})
	if leaseList, ok := decodeBody(t, leases)["leases"].([]any); !ok || len(leaseList) != 1 {
		t.Fatalf("leases = %v", decodeBody(t, leases)["leases"])
	}
}
