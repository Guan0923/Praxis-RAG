package k8sops

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeTempFile creates one credential file for the test settings.
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// testSettings builds a Settings that points at a local httptest server with
// temporary credential files (token / CA placeholder / namespace).
func testSettings(t *testing.T, endpoint string) Settings {
	t.Helper()
	return Settings{
		APIEndpoint:              endpoint,
		Namespace:                "praxis-rag",
		DeploymentNamespace:      "praxis-rag",
		DocumentWorkerDeployment: "rag-document-worker",
		TokenPath:                writeTempFile(t, "token", "test-token"),
		RootCAPath:               writeTempFile(t, "ca.crt", "not-a-real-ca"),
	}
}

// TestSettingsNotInCluster keeps the disabled path: no mounted token and no
// explicit endpoint means the operations degrade, not fail startup.
func TestSettingsNotInCluster(t *testing.T) {
	if err := os.Unsetenv("RAG_K8S_API_ENDPOINT"); err != nil {
		t.Fatalf("unset: %v", err)
	}
	settings, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("SettingsFromEnv outside a pod: %v", err)
	}
	if settings.Enabled() {
		t.Fatalf("settings should be disabled, got %+v", settings)
	}
}

// TestSettingsEndpointWithoutTokenIsAnError: asking for Kubernetes control
// without credentials must fail loudly at startup, not silently degrade.
func TestSettingsEndpointWithoutTokenIsAnError(t *testing.T) {
	t.Setenv("RAG_K8S_API_ENDPOINT", "https://kubernetes.default.svc")
	if _, err := SettingsFromEnv(); err == nil {
		t.Fatal("SettingsFromEnv should fail with an explicit error message")
	}
}

// TestDeploymentRead asserts the GET contract: bearer auth from the token
// file, path composition, and the replica view fields.
func TestDeploymentRead(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"metadata": {"name": "rag-document-worker", "namespace": "praxis-rag"},
			"spec": {"replicas": 2},
			"status": {"replicas": 2, "readyReplicas": 1, "availableReplicas": 1, "observedGeneration": 5}
		}`))
	}))
	defer server.Close()
	client, err := NewClient(testSettings(t, server.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	view, err := client.DocumentWorker(context.Background())
	if err != nil {
		t.Fatalf("DocumentWorker: %v", err)
	}
	if view.Name != "rag-document-worker" || view.Namespace != "praxis-rag" ||
		view.DesiredReplicas != 2 || view.ReadyReplicas != 1 ||
		view.AvailableReplicas != 1 || view.ObservedGeneration != 5 {
		t.Fatalf("view = %+v", view)
	}
	if gotPath != "/apis/apps/v1/namespaces/praxis-rag/deployments/rag-document-worker" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("authorization = %q", gotAuth)
	}
}

// TestScaleIssuesMergePatch asserts the PATCH contract: method, merge-patch
// content type, and the exact patch body kubectl scale would send.
func TestScaleIssuesMergePatch(t *testing.T) {
	var gotMethod, gotType string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata": {"name": "rag-document-worker"}, "spec": {"replicas": 3}}`))
	}))
	defer server.Close()
	client, err := NewClient(testSettings(t, server.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.ScaleDocumentWorker(context.Background(), 3); err != nil {
		t.Fatalf("ScaleDocumentWorker: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Fatalf("method = %q", gotMethod)
	}
	if gotType != "application/merge-patch+json" {
		t.Fatalf("content type = %q", gotType)
	}
	if string(gotBody) != `{"spec":{"replicas":3}}` {
		t.Fatalf("body = %q", gotBody)
	}
}

// TestScaleRejectsOutOfRange keeps the Console from asking for absurd or
// negative replica counts before the API server sees them.
func TestScaleRejectsOutOfRange(t *testing.T) {
	client, err := NewClient(testSettings(t, "http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	for _, replicas := range []int32{-1, 65} {
		if err := client.ScaleDocumentWorker(context.Background(), replicas); err == nil {
			t.Fatalf("replicas %d should be rejected", replicas)
		}
	}
}

// TestAPIErrorMapping asserts the 403/404 to sentinel errors the transport
// layer uses to build actionable Console messages.
func TestAPIErrorMapping(t *testing.T) {
	for _, item := range []struct {
		status int
		want   error
	}{
		{http.StatusForbidden, ErrForbidden},
		{http.StatusNotFound, ErrDeploymentNotFound},
	} {
		if err := apiError(item.status, "ns/name"); !errors.Is(err, item.want) {
			t.Fatalf("apiError(%d) = %v, want %v", item.status, err, item.want)
		}
	}
}
