// Package k8sops implements the Kubernetes side channel of the Console system
// monitor page: one shared list/watch cache over the built-in workload and pod
// resources, read-only snapshots for the browser, rolling restarts of
// Deployment / StatefulSet / DaemonSet, and the legacy document-worker scale
// operation.
//
// The package talks to the Kubernetes API over HTTPS. Credentials come either
// from the mounted service-account projection (in-cluster) or from an
// explicitly configured kubeconfig (out-of-cluster); with neither, every
// cluster endpoint degrades to a configuration hint instead of failing
// startup.
package k8sops

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ErrNotInCluster means no Kubernetes service account or kubeconfig is
// configured: the Console endpoints must degrade instead of dialing a
// nonexistent API server.
var ErrNotInCluster = errors.New("kubernetes service account is not mounted")

// ErrForbidden means the API server answered 403: the RBAC role is missing or
// does not cover this operation. The Console surfaces it so the operator knows
// to apply the RBAC manifest instead of guessing.
var ErrForbidden = errors.New("kubernetes api rejected the operation (check RBAC)")

// ErrDeploymentNotFound means the API server answered 404 for the Deployment.
var ErrDeploymentNotFound = errors.New("kubernetes deployment not found")

// Paths of the projected service-account volume in a standard pod.
const (
	DefaultTokenPath  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	DefaultRootCAPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	DefaultNamespace  = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// Settings carries the resolved cluster access. An empty APIEndpoint means
// "no cluster is configured"; Enabled then reports false and the Console
// endpoints degrade to ConfigurationHint instead of failing startup.
//
// The legacy in-cluster fields (TokenPath, RootCAPath, Namespace) and the
// kubeconfig-derived fields (CAData, Token, TokenFile, ...) are unified here so
// the same REST client can talk to either credential source.
type Settings struct {
	// APIEndpoint is the kube-apiserver URL (https://kubernetes.default.svc:443).
	APIEndpoint string
	// Namespace is the pod namespace (in-cluster) or the kubeconfig context
	// namespace.
	Namespace string
	// TokenPath is the projected service-account token file (in-cluster). It
	// is re-read on every call because kubelet rotates it.
	TokenPath string
	// RootCAPath is the cluster CA bundle used to verify the API server
	// (in-cluster).
	RootCAPath string
	// DeploymentNamespace overrides the namespace of the managed Deployment
	// (defaults to the pod / context namespace).
	DeploymentNamespace string
	// DocumentWorkerDeployment is the Deployment the scale endpoint manages.
	DocumentWorkerDeployment string

	// Source identifies the credential source (SourceServiceAccount or
	// SourceKubeconfig). It is surfaced by ClusterInfo.
	Source string
	// KubeconfigPath is the explicit kubeconfig the credentials came from.
	KubeconfigPath string
	// ContextName is the kubeconfig context used for the cluster identity.
	ContextName string
	// CAData is the PEM-encoded cluster CA bundle from the kubeconfig.
	CAData []byte
	// InsecureSkipTLSVerify disables API-server certificate verification
	// (kubeconfig insecure-skip-tls-verify).
	InsecureSkipTLSVerify bool
	// Token is a static kubeconfig bearer token.
	Token string
	// TokenFile is a kubeconfig token file; it is re-read on every call so
	// external rotation keeps working.
	TokenFile string
	// ClientCertData / ClientKeyData are the PEM client certificate pair from
	// the kubeconfig (inline data or loaded from the referenced files).
	ClientCertData []byte
	ClientKeyData  []byte
	// Username / Password are kubeconfig basic-auth credentials.
	Username string
	Password string
}

// SettingsFromEnv resolves the cluster access, in priority order:
//
//  1. RAG_K8S_KUBECONFIG (optional RAG_K8S_CONTEXT): an explicit out-of-cluster
//     kubeconfig. Unreadable files, malformed YAML and unsupported exec /
//     auth-provider plugins fail startup with a Chinese configuration error.
//  2. The mounted service-account projection (standard in-cluster access).
//  3. Neither: the zero Settings and a nil error, so the Console degrades.
//
// In-cluster env overrides: RAG_K8S_API_ENDPOINT, RAG_K8S_WORKER_DEPLOYMENT,
// RAG_K8S_DEPLOYMENT_NAMESPACE. An endpoint set without the mounted token is a
// configuration error: the operator asked for Kubernetes control without
// providing credentials.
func SettingsFromEnv() (Settings, error) {
	if path := strings.TrimSpace(os.Getenv("RAG_K8S_KUBECONFIG")); path != "" {
		return settingsFromKubeconfig(path, strings.TrimSpace(os.Getenv("RAG_K8S_CONTEXT")))
	}
	return settingsFromInCluster()
}

// settingsFromInCluster keeps the original service-account behavior.
func settingsFromInCluster() (Settings, error) {
	settings := Settings{
		APIEndpoint:              strings.TrimSpace(os.Getenv("RAG_K8S_API_ENDPOINT")),
		DocumentWorkerDeployment: strings.TrimSpace(os.Getenv("RAG_K8S_WORKER_DEPLOYMENT")),
		DeploymentNamespace:      strings.TrimSpace(os.Getenv("RAG_K8S_DEPLOYMENT_NAMESPACE")),
		TokenPath:                DefaultTokenPath,
		RootCAPath:               DefaultRootCAPath,
	}
	token, tokenErr := os.ReadFile(DefaultTokenPath)
	if tokenErr != nil {
		if settings.APIEndpoint != "" {
			return Settings{}, fmt.Errorf("RAG_K8S_API_ENDPOINT is set but %s is unreadable: %w", DefaultTokenPath, tokenErr)
		}
		// Not in a Kubernetes pod (or the projection is disabled): the
		// operations stay disabled rather than failing startup.
		return Settings{}, nil
	}
	if len(bytes.TrimSpace(token)) == 0 {
		return Settings{}, nil
	}
	if settings.APIEndpoint == "" {
		settings.APIEndpoint = "https://kubernetes.default.svc:443"
	}
	if settings.DocumentWorkerDeployment == "" {
		settings.DocumentWorkerDeployment = "rag-document-worker"
	}
	if settings.DeploymentNamespace == "" {
		namespace, err := os.ReadFile(DefaultNamespace)
		if err != nil || len(bytes.TrimSpace(namespace)) == 0 {
			return Settings{}, fmt.Errorf("cannot resolve the pod namespace from %s", DefaultNamespace)
		}
		settings.Namespace = strings.TrimSpace(string(namespace))
		settings.DeploymentNamespace = settings.Namespace
	} else {
		settings.Namespace = settings.DeploymentNamespace
	}
	settings.Source = SourceServiceAccount
	return settings, nil
}

// Enabled reports whether Kubernetes operations are wired.
func (s Settings) Enabled() bool { return s.APIEndpoint != "" }

// authorize attaches the resolved credentials to one request. Token files are
// re-read every call: kubelet rotates the projected service-account token and
// external operators rotate kubeconfig token files, so a long-lived service
// process must not pin the first value forever.
func (s Settings) authorize(request *http.Request) error {
	if s.TokenFile != "" {
		token, err := os.ReadFile(s.TokenFile)
		if err != nil {
			return fmt.Errorf("无法读取 kubeconfig tokenFile 文件 %s: %w", s.TokenFile, err)
		}
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		return nil
	}
	if s.Token != "" {
		request.Header.Set("Authorization", "Bearer "+s.Token)
		return nil
	}
	if s.TokenPath != "" {
		token, err := os.ReadFile(s.TokenPath)
		if err != nil {
			return fmt.Errorf("无法读取 Kubernetes token 文件 %s: %w", s.TokenPath, err)
		}
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		return nil
	}
	if s.Username != "" || s.Password != "" {
		request.SetBasicAuth(s.Username, s.Password)
	}
	return nil
}

// tlsConfig builds the API-server transport configuration. An explicit CA
// bundle wins; otherwise the in-cluster service-account CA is loaded when
// present. A missing CA file is tolerated for backward compatibility with the
// legacy client (the mounted bundle may be absent during local runs), but any
// other read error is fatal.
func (s Settings) tlsConfig() (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.InsecureSkipTLSVerify {
		config.InsecureSkipVerify = true
	}
	caData := s.CAData
	if len(caData) == 0 && s.RootCAPath != "" {
		certificate, err := os.ReadFile(s.RootCAPath)
		switch {
		case err == nil:
			caData = certificate
		case !os.IsNotExist(err):
			return nil, fmt.Errorf("cannot read the cluster CA bundle: %w", err)
		}
	}
	if len(caData) > 0 {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(caData) {
			config.RootCAs = pool
		}
	}
	if len(s.ClientCertData) > 0 || len(s.ClientKeyData) > 0 {
		certificate, err := tls.X509KeyPair(s.ClientCertData, s.ClientKeyData)
		if err != nil {
			return nil, fmt.Errorf("无法加载 kubeconfig 客户端证书: %w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return config, nil
}

// Deployment is the read model the Console needs: no status details beyond
// replica counts, so the page cannot leak unrelated cluster information.
type Deployment struct {
	Name               string `json:"name"`
	Namespace          string `json:"namespace"`
	DesiredReplicas    int32  `json:"desired_replicas"`
	ReadyReplicas      int32  `json:"ready_replicas"`
	AvailableReplicas  int32  `json:"available_replicas"`
	ObservedGeneration int64  `json:"observed_generation"`
}

// Client performs the two bounded REST calls. It is safe for concurrent use.
type Client struct {
	settings Settings
	http     *http.Client
}

// timeout bounds every API call so a stuck apiserver cannot hold the HTTP
// request open (the transport handler applies its own shorter bound too).
const timeout = 10 * time.Second

// NewClient builds the REST client from resolved settings. It works with both
// credential sources: the mounted service account and an explicit kubeconfig.
func NewClient(settings Settings) (*Client, error) {
	if !settings.Enabled() {
		return nil, ErrNotInCluster
	}
	tlsSettings, err := settings.tlsConfig()
	if err != nil {
		return nil, err
	}
	return &Client{
		settings: settings,
		http: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: tlsSettings},
		},
	}, nil
}

// deploymentPath builds the apps/v1 Deployment URL.
func (s Settings) deploymentPath(namespace, name string) string {
	return strings.TrimSuffix(s.APIEndpoint, "/") +
		"/apis/apps/v1/namespaces/" + strings.Trim(namespace, "/") + "/deployments/" + strings.Trim(name, "/")
}

// request issues one authenticated API call.
func (c *Client) request(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, path, reader)
	if err != nil {
		return nil, 0, err
	}
	if err := c.settings.authorize(request); err != nil {
		return nil, 0, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/merge-patch+json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, response.StatusCode, err
	}
	return payload, response.StatusCode, nil
}

// apiError converts an unexpected status code to a bounded error string. The
// Kubernetes error body is not propagated verbatim: it can embed object
// details; a short code-based message keeps the Console log clean.
func apiError(status int, name string) error {
	switch status {
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrDeploymentNotFound, name)
	default:
		return fmt.Errorf("kubernetes api returned HTTP %d for %s", status, name)
	}
}

// DocumentWorker reads the document worker Deployment replica state.
func (c *Client) DocumentWorker(ctx context.Context) (*Deployment, error) {
	return c.Deployment(ctx, c.settings.DeploymentNamespace, c.settings.DocumentWorkerDeployment)
}

// Deployment reads one Deployment's replica state.
func (c *Client) Deployment(ctx context.Context, namespace, name string) (*Deployment, error) {
	payload, status, err := c.request(ctx, http.MethodGet, c.settings.deploymentPath(namespace, name), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, namespace+"/"+name)
	}
	var document struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int32 `json:"replicas"`
		} `json:"spec"`
		Status struct {
			Replicas           int32 `json:"replicas"`
			ReadyReplicas      int32 `json:"readyReplicas"`
			AvailableReplicas  int32 `json:"availableReplicas"`
			ObservedGeneration int64 `json:"observedGeneration"`
		} `json:"status"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, fmt.Errorf("kubernetes api response is not a deployment: %w", err)
	}
	view := &Deployment{
		Name:               document.Metadata.Name,
		Namespace:          document.Metadata.Namespace,
		ReadyReplicas:      document.Status.ReadyReplicas,
		AvailableReplicas:  document.Status.AvailableReplicas,
		ObservedGeneration: document.Status.ObservedGeneration,
	}
	if document.Spec.Replicas != nil {
		view.DesiredReplicas = *document.Spec.Replicas
	}
	return view, nil
}

// ScaleDocumentWorker sets the document worker Deployment's replica count.
// The merge patch is exactly what `kubectl scale` issues, so admission
// controllers and quota apply identically.
func (c *Client) ScaleDocumentWorker(ctx context.Context, replicas int32) error {
	return c.Scale(ctx, c.settings.DeploymentNamespace, c.settings.DocumentWorkerDeployment, replicas)
}

// Scale sets one Deployment's replica count.
func (c *Client) Scale(ctx context.Context, namespace, name string, replicas int32) error {
	if replicas < 0 || replicas > 64 {
		return errors.New("replicas must be between 0 and 64")
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"replicas": replicas}})
	if err != nil {
		return err
	}
	_, status, err := c.request(ctx, http.MethodPatch,
		c.settings.deploymentPath(namespace, name), patch)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return apiError(status, namespace+"/"+name)
	}
	return nil
}
