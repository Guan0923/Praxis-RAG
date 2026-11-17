// Package k8sops implements the minimal Kubernetes operations the Console
// monitor page offers: reading one Deployment's replica state and scaling the
// document worker. It speaks the Kubernetes API over HTTPS with the pod's
// service-account credentials, and fails closed when the service account is
// not mounted (RBAC not applied) so the endpoints degrade to 503 instead of
// pretending to work.
//
// The package is deliberately tiny: no informers, no watch, no typed clientset
// dependency. Two REST calls cover the Console needs:
//
//   - GET  /apis/apps/v1/namespaces/{ns}/deployments/{name}
//     (read spec.replicas, status.replicas, observedGeneration),
//   - PATCH (application/merge-patch+json) on the same path with
//     {"spec":{"replicas":N}} — the server-side equivalent of kubectl scale.
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

// ErrNotInCluster means no Kubernetes service account is mounted: the Console
// endpoints must degrade instead of dialing a nonexistent API server.
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

// Settings carries the service-account in-cluster resolution. An empty
// APIEndpoint means "not running inside a Kubernetes pod with a mounted
// service account"; Enabled then reports false and the operations degrade.
type Settings struct {
	// APIEndpoint is the kube-apiserver URL (https://kubernetes.default.svc:443).
	APIEndpoint string
	// Namespace is the pod namespace (used when DeploymentNamespace is empty).
	Namespace string
	// TokenPath is the projected service-account token file.
	TokenPath string
	// RootCAPath is the cluster CA bundle used to verify the API server.
	RootCAPath string
	// DeploymentNamespace overrides the namespace of the managed Deployment
	// (defaults to the pod namespace).
	DeploymentNamespace string
	// DocumentWorkerDeployment is the Deployment the scale endpoint manages.
	DocumentWorkerDeployment string
}

// SettingsFromEnv resolves the in-cluster access from the standard mounted
// files plus optional env overrides:
//
//	RAG_K8S_API_ENDPOINT         (default https://kubernetes.default.svc:443)
//	RAG_K8S_WORKER_DEPLOYMENT    (default rag-document-worker)
//	RAG_K8S_DEPLOYMENT_NAMESPACE (default: the pod namespace file)
//
// An endpoint set without the mounted token is a configuration error: the
// operator asked for Kubernetes control without providing credentials.
func SettingsFromEnv() (Settings, error) {
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
	return settings, nil
}

// Enabled reports whether Kubernetes operations are wired.
func (s Settings) Enabled() bool { return s.APIEndpoint != "" }

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

// NewClient builds the REST client from resolved settings.
func NewClient(settings Settings) (*Client, error) {
	if !settings.Enabled() {
		return nil, ErrNotInCluster
	}
	pool := x509.NewCertPool()
	ca, err := os.ReadFile(settings.RootCAPath)
	if err == nil && len(ca) > 0 {
		pool.AppendCertsFromPEM(ca)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot read the cluster CA bundle: %w", err)
	}
	return &Client{
		settings: settings,
		http: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		},
	}, nil
}

// deploymentPath builds the apps/v1 Deployment URL.
func (s Settings) deploymentPath(namespace, name string) string {
	return strings.TrimSuffix(s.APIEndpoint, "/") +
		"/apis/apps/v1/namespaces/" + strings.Trim(namespace, "/") + "/deployments/" + strings.Trim(name, "/")
}

// request issues one authenticated API call. The token is re-read from the
// projected volume on every call: kubelet rotates it, and a long-lived
// service process must not pin the first token forever.
func (c *Client) request(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	token, err := os.ReadFile(c.settings.TokenPath)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot read the service-account token: %w", err)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, path, reader)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
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
