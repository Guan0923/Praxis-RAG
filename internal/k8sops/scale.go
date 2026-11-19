package k8sops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

var ErrScaleConflict = errors.New("deployment changed before scaling")

// Only named Praxis workers in the operator-configured namespace are targets.
// The configured document-worker name also supports renamed installations.
func (c *Cluster) managedWorker(namespace, name string) bool {
	if namespace == "" || namespace != c.settings.DeploymentNamespace || name == "" {
		return false
	}
	if name == c.settings.DocumentWorkerDeployment {
		return true
	}
	switch name {
	case "rag-cache-worker", "rag-sag-worker", "rag-delete-worker", "rag-reembed-worker", "rag-dead-letter-document", "rag-dead-letter-cache":
		return true
	}
	return false
}

// The tests in the JSON patch run atomically in the apiserver. A stale dialog
// cannot resize a replacement Deployment or overwrite a concurrent resize.
func (c *Cluster) ScaleWorker(ctx context.Context, namespace, name, uid string, current, target int32) error {
	if !c.Configured() || !c.managedWorker(namespace, name) {
		return ErrForbidden
	}
	if uid == "" || current < 0 || target < 0 || target > 64 {
		return fmt.Errorf("invalid scale request")
	}
	type operation struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value any    `json:"value"`
	}
	patch, err := json.Marshal([]operation{{"test", "/metadata/uid", uid}, {"test", "/metadata/labels/app.kubernetes.io~1part-of", "praxis-rag"}, {"test", "/spec/replicas", current}, {"replace", "/spec/replicas", target}})
	if err != nil {
		return err
	}
	_, status, err := c.do(ctx, http.MethodPatch, c.objectPath(specDeployments, namespace, name), patch, "application/json-patch+json")
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusConflict, http.StatusUnprocessableEntity:
		return ErrScaleConflict
	}
	return fmt.Errorf("Kubernetes scale HTTP %d", status)
}
