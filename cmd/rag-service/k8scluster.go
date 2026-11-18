package main

import (
	"context"
	"log/slog"

	"github.com/Guan0923/Praxis-RAG/internal/k8sops"
)

// kubernetesMonitorBuild resolves the Console Kubernetes monitor backend: the
// shared list/watch cache over workloads and pods plus the rolling-restart
// client. It binds the two credential sources k8sops understands:
//
//   - the mounted ServiceAccount projection when the service runs in a pod;
//   - an explicit kubeconfig (RAG_K8S_KUBECONFIG, optional RAG_K8S_CONTEXT)
//     when it runs outside a cluster.
//
// No credentials at all is not an error: it returns nil so Compose / host
// deployments keep working and the Console Kubernetes tab answers 503 with the
// configuration hint. An explicitly configured source that cannot be resolved
// (unreadable/unsupported kubeconfig, missing context, ...) is a startup error:
// the operator asked for cluster control but the credentials are broken.
func kubernetesMonitorBuild(logger *slog.Logger) (*k8sops.Cluster, error) {
	cluster, err := k8sops.NewClusterFromEnv(logger)
	if err != nil {
		return nil, err
	}
	if cluster == nil {
		logger.Info("Kubernetes 监控未启用：未挂载 ServiceAccount 且未配置 RAG_K8S_KUBECONFIG，Console 的 Kubernetes 标签将显示为未启用")
		return nil, nil
	}
	// Start launches the background LIST+WATCH loops; Close (deferred by the
	// caller) cancels them when the process exits.
	cluster.Start(context.Background())
	return cluster, nil
}
