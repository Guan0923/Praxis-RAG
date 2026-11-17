package k8sops

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useKubeconfig points the environment at one kubeconfig file and clears the
// other connection overrides so the test is deterministic.
func useKubeconfig(t *testing.T, path, contextName string) {
	t.Helper()
	t.Setenv("RAG_K8S_KUBECONFIG", path)
	t.Setenv("RAG_K8S_CONTEXT", contextName)
	t.Setenv("RAG_K8S_API_ENDPOINT", "")
	t.Setenv("RAG_K8S_WORKER_DEPLOYMENT", "")
	t.Setenv("RAG_K8S_DEPLOYMENT_NAMESPACE", "")
}

func writeKubeconfig(t *testing.T, directory, content string) string {
	t.Helper()
	path := filepath.Join(directory, "kubeconfig.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func mustBase64(value []byte) string { return base64.StdEncoding.EncodeToString(value) }

// TestSettingsFromKubeconfigStaticCredentials asserts the parser fills the
// unified Settings from the selected context, including base64 CA data and the
// worker Deployment defaults.
func TestSettingsFromKubeconfigStaticCredentials(t *testing.T) {
	directory := t.TempDir()
	caPEM := []byte("-----BEGIN CERTIFICATE-----\nCA-PEM\n-----END CERTIFICATE-----\n")
	path := writeKubeconfig(t, directory, `
apiVersion: v1
kind: Config
current-context: praxis
clusters:
  - name: dev
    cluster:
      server: https://kube.example:6443
      certificate-authority-data: `+mustBase64(caPEM)+`
users:
  - name: ops
    user:
      token: static-token
contexts:
  - name: praxis
    context:
      cluster: dev
      user: ops
      namespace: praxis-rag
`)
	// An explicit API endpoint must lose to the explicit kubeconfig.
	t.Setenv("RAG_K8S_API_ENDPOINT", "https://ignored.example:6443")
	useKubeconfig(t, path, "")

	settings, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("SettingsFromEnv: %v", err)
	}
	if !settings.Enabled() {
		t.Fatalf("settings disabled: %+v", settings)
	}
	if settings.APIEndpoint != "https://kube.example:6443" {
		t.Fatalf("APIEndpoint = %q", settings.APIEndpoint)
	}
	if settings.Source != SourceKubeconfig || settings.ContextName != "praxis" ||
		settings.KubeconfigPath != path {
		t.Fatalf("identity fields = %+v", settings)
	}
	if settings.Namespace != "praxis-rag" || settings.DeploymentNamespace != "praxis-rag" ||
		settings.DocumentWorkerDeployment != "rag-document-worker" {
		t.Fatalf("namespace/deployment fields = %+v", settings)
	}
	if settings.Token != "static-token" || settings.TokenFile != "" {
		t.Fatalf("token fields = %+v", settings)
	}
	if string(settings.CAData) != string(caPEM) {
		t.Fatalf("CAData = %q", settings.CAData)
	}

	cluster, err := NewCluster(settings, discardLogger())
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	defer cluster.Close()
	if info := cluster.ClusterInfo(); info.Name != "praxis" || info.Source != SourceKubeconfig {
		t.Fatalf("ClusterInfo = %+v", info)
	}
}

// TestSettingsFromKubeconfigContextOverride selects another context through
// RAG_K8S_CONTEXT and reports an actionable error for unknown contexts.
func TestSettingsFromKubeconfigContextOverride(t *testing.T) {
	directory := t.TempDir()
	path := writeKubeconfig(t, directory, `
apiVersion: v1
kind: Config
current-context: praxis
clusters:
  - name: dev
    cluster:
      server: https://dev.example:6443
  - name: prod
    cluster:
      server: https://prod.example:6443
      insecure-skip-tls-verify: true
users:
  - name: ops
    user:
      username: admin
      password: secret
contexts:
  - name: praxis
    context:
      cluster: dev
      user: ops
      namespace: praxis-rag
  - name: production
    context:
      cluster: prod
      user: ops
      namespace: production
`)
	useKubeconfig(t, path, "production")
	settings, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("SettingsFromEnv: %v", err)
	}
	if settings.APIEndpoint != "https://prod.example:6443" || settings.ContextName != "production" {
		t.Fatalf("settings = %+v", settings)
	}
	if !settings.InsecureSkipTLSVerify || settings.Namespace != "production" {
		t.Fatalf("settings = %+v", settings)
	}
	if settings.Username != "admin" || settings.Password != "secret" {
		t.Fatalf("basic auth = %+v", settings)
	}

	useKubeconfig(t, path, "missing")
	_, err = SettingsFromEnv()
	if err == nil || !strings.Contains(err.Error(), "不存在 context") {
		t.Fatalf("missing context error = %v", err)
	}
}

// TestSettingsFromKubeconfigRejectsExecPlugins keeps the startup failure
// explicit: helper plugins cannot be executed in this deployment.
func TestSettingsFromKubeconfigRejectsExecPlugins(t *testing.T) {
	for name, userBlock := range map[string]string{
		"exec":          "      exec:\n        command: aws\n        args: [eks, get-token]\n",
		"auth-provider": "      auth-provider:\n        name: gcp\n",
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			path := writeKubeconfig(t, directory, `
apiVersion: v1
kind: Config
current-context: praxis
clusters:
  - name: dev
    cluster:
      server: https://kube.example:6443
users:
  - name: ops
    user:
`+userBlock+`contexts:
  - name: praxis
    context:
      cluster: dev
      user: ops
`)
			useKubeconfig(t, path, "")
			_, err := SettingsFromEnv()
			if err == nil {
				t.Fatal("SettingsFromEnv should reject credential plugins")
			}
			if !strings.Contains(err.Error(), "不支持") || !strings.Contains(err.Error(), name) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

// TestSettingsFromKubeconfigBrokenFiles covers the other startup-failure
// paths: unreadable file, malformed YAML, missing current-context.
func TestSettingsFromKubeconfigBrokenFiles(t *testing.T) {
	t.Run("unreadable", func(t *testing.T) {
		useKubeconfig(t, filepath.Join(t.TempDir(), "absent.yaml"), "")
		if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "无法读取 kubeconfig") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		path := writeKubeconfig(t, t.TempDir(), "clusters: [not-a-list")
		useKubeconfig(t, path, "")
		if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "格式错误") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("no-current-context", func(t *testing.T) {
		path := writeKubeconfig(t, t.TempDir(), `
apiVersion: v1
kind: Config
clusters:
  - name: dev
    cluster:
      server: https://kube.example:6443
`)
		useKubeconfig(t, path, "")
		if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "current-context") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing-cluster", func(t *testing.T) {
		path := writeKubeconfig(t, t.TempDir(), `
apiVersion: v1
kind: Config
current-context: praxis
contexts:
  - name: praxis
    context:
      cluster: gone
      namespace: praxis-rag
`)
		useKubeconfig(t, path, "")
		if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "cluster") {
			t.Fatalf("error = %v", err)
		}
	})
}

// TestKubeconfigTokenFileRotation proves the kubeconfig tokenFile is re-read on
// every call (external rotation) and that the CA bundle verifies the TLS
// server, i.e. the credentials really work end to end.
func TestKubeconfigTokenFileRotation(t *testing.T) {
	var authorizations []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":{"name":"rag-document-worker","namespace":"praxis-rag"},"spec":{"replicas":1},"status":{"replicas":1,"readyReplicas":1,"availableReplicas":1,"observedGeneration":1}}`))
	}))
	defer server.Close()

	directory := t.TempDir()
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	tokenPath := filepath.Join(directory, "token.txt")
	if err := os.WriteFile(tokenPath, []byte("first-token"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	path := writeKubeconfig(t, directory, `
apiVersion: v1
kind: Config
current-context: praxis
clusters:
  - name: dev
    cluster:
      server: `+server.URL+`
      certificate-authority-data: `+mustBase64(certificatePEM)+`
users:
  - name: ops
    user:
      tokenFile: token.txt
contexts:
  - name: praxis
    context:
      cluster: dev
      user: ops
      namespace: praxis-rag
`)
	useKubeconfig(t, path, "")
	settings, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("SettingsFromEnv: %v", err)
	}
	if settings.TokenFile != tokenPath {
		t.Fatalf("TokenFile = %q, want %q", settings.TokenFile, tokenPath)
	}
	if string(settings.CAData) != string(certificatePEM) {
		t.Fatalf("CAData mismatch: %q", settings.CAData)
	}
	client, err := NewClient(settings)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.DocumentWorker(t.Context()); err != nil {
		t.Fatalf("DocumentWorker: %v", err)
	}
	if err := os.WriteFile(tokenPath, []byte("second-token"), 0o600); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if _, err := client.DocumentWorker(t.Context()); err != nil {
		t.Fatalf("DocumentWorker after rotation: %v", err)
	}
	if len(authorizations) != 2 || authorizations[0] != "Bearer first-token" || authorizations[1] != "Bearer second-token" {
		t.Fatalf("authorizations = %v", authorizations)
	}
}

// TestKubeconfigInsecureAndClientCertificate covers the remaining TLS modes:
// insecure-skip-tls-verify works without a CA bundle, and inline client
// certificate data is parsed into the transport.
func TestKubeconfigInsecureAndClientCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":{"name":"rag-document-worker","namespace":"praxis-rag"},"spec":{"replicas":1},"status":{}}`))
	}))
	defer server.Close()

	// Insecure mode: the httptest certificate is not in any system pool.
	insecurePath := writeKubeconfig(t, t.TempDir(), `
apiVersion: v1
kind: Config
current-context: praxis
clusters:
  - name: dev
    cluster:
      server: `+server.URL+`
      insecure-skip-tls-verify: true
users:
  - name: ops
    user:
      token: insecure-token
contexts:
  - name: praxis
    context:
      cluster: dev
      user: ops
      namespace: praxis-rag
`)
	useKubeconfig(t, insecurePath, "")
	settings, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("SettingsFromEnv: %v", err)
	}
	if !settings.InsecureSkipTLSVerify {
		t.Fatalf("InsecureSkipTLSVerify = false")
	}
	client, err := NewClient(settings)
	if err != nil {
		t.Fatalf("NewClient(insecure): %v", err)
	}
	if _, err := client.DocumentWorker(t.Context()); err != nil {
		t.Fatalf("DocumentWorker(insecure): %v", err)
	}

	// Client certificate mode: reuse the httptest server's own key pair as the
	// kubeconfig client certificate; the TLS handshake exercises it.
	certificate := server.TLS.Certificates[0]
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	certPath := writeKubeconfig(t, t.TempDir(), `
apiVersion: v1
kind: Config
current-context: praxis
clusters:
  - name: dev
    cluster:
      server: `+server.URL+`
      certificate-authority-data: `+mustBase64(caPEM)+`
users:
  - name: ops
    user:
      client-certificate-data: `+mustBase64(certificatePEM)+`
      client-key-data: `+mustBase64(keyPEM)+`
contexts:
  - name: praxis
    context:
      cluster: dev
      user: ops
      namespace: praxis-rag
`)
	useKubeconfig(t, certPath, "")
	settings, err = SettingsFromEnv()
	if err != nil {
		t.Fatalf("SettingsFromEnv(client cert): %v", err)
	}
	if len(settings.ClientCertData) == 0 || len(settings.ClientKeyData) == 0 {
		t.Fatalf("client certificate data not parsed: %+v", settings)
	}
	client, err = NewClient(settings)
	if err != nil {
		t.Fatalf("NewClient(client cert): %v", err)
	}
	if _, err := client.DocumentWorker(t.Context()); err != nil {
		t.Fatalf("DocumentWorker(client cert): %v", err)
	}
}

// TestSettingsFromKubeconfigContextNamespaceDefault keeps the Kubernetes
// default namespace when the context omits it.
func TestSettingsFromKubeconfigContextNamespaceDefault(t *testing.T) {
	path := writeKubeconfig(t, t.TempDir(), `
apiVersion: v1
kind: Config
current-context: praxis
clusters:
  - name: dev
    cluster:
      server: https://kube.example:6443
contexts:
  - name: praxis
    context:
      cluster: dev
`)
	useKubeconfig(t, path, "")
	settings, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("SettingsFromEnv: %v", err)
	}
	if settings.Namespace != "default" || settings.DeploymentNamespace != "default" {
		t.Fatalf("namespaces = %q/%q", settings.Namespace, settings.DeploymentNamespace)
	}
}

// TestNewClusterRequiresConfiguration keeps the sentinel contract used by the
// transport layer.
func TestNewClusterRequiresConfiguration(t *testing.T) {
	if _, err := NewCluster(Settings{}, discardLogger()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("NewCluster(empty) error = %v, want ErrNotConfigured", err)
	}
	t.Setenv("RAG_K8S_KUBECONFIG", "")
	t.Setenv("RAG_K8S_API_ENDPOINT", "")
	cluster, err := NewClusterFromEnv(discardLogger())
	if err != nil || cluster != nil {
		t.Fatalf("NewClusterFromEnv = %v, %v; want nil, nil", cluster, err)
	}
}
