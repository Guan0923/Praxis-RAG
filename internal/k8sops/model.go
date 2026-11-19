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
	"errors"
	"time"
)

// ConfigurationHint is the operator-facing explanation served by the Console
// when no cluster is configured. It is kept in one place so the 503 detail and
// the page alert say the same thing.
const ConfigurationHint = "Kubernetes 集群未配置：在集群内运行时请挂载 ServiceAccount 并应用 RBAC 清单；" +
	"在集群外运行时请设置 RAG_K8S_KUBECONFIG 指向 kubeconfig（可用 RAG_K8S_CONTEXT 指定 context），然后重启 rag-service"

// Cluster credential sources.
const (
	SourceServiceAccount = "serviceaccount"
	SourceKubeconfig     = "kubeconfig"
)

// Restart resource identifiers accepted by the restart endpoint. They are the
// Kubernetes resource names (plural, lowercase), so the Console can post the
// row's "resource" field without keeping its own mapping table.
const (
	ResourceDeployments  = "deployments"
	ResourceStatefulSets = "statefulsets"
	ResourceDaemonSets   = "daemonsets"
)

// ErrNotConfigured means no cluster credentials were resolved: the Console
// endpoints must degrade to ConfigurationHint instead of dialing a nonexistent
// API server.
var ErrNotConfigured = errors.New("kubernetes cluster is not configured")

// ErrNotFound means the requested workload does not exist (any more).
var ErrNotFound = errors.New("kubernetes workload not found")

// ErrNotRestartable means the workload's update strategy cannot perform a
// rolling restart (paused Deployment, Recreate strategy, OnDelete strategy or a
// partitioned StatefulSet). The wrapped message is the Chinese reason shown
// inline on the row.
var ErrNotRestartable = errors.New("kubernetes workload does not support rolling restart")

// ErrRestartInProgress means the same workload already has a submitted rolling
// restart that has not reached a terminal state; the Console disables the
// button while that is true.
var ErrRestartInProgress = errors.New("kubernetes workload restart already in progress")

// Filter narrows one snapshot. Namespace is matched exactly (empty means all
// namespaces); Name is a case-insensitive substring match (empty means all).
type Filter struct {
	Namespace string
	Name      string
}

// ClusterInfo is the connected cluster identity rendered in the Kubernetes tab.
// It deliberately omits the API server URL so the Console never echoes an
// internal endpoint.
type ClusterInfo struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// WorkloadView is the payload of GET /v1/admin/k8s/workloads.
type WorkloadView struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Cluster     ClusterInfo `json:"cluster"`
	Namespaces  []string    `json:"namespaces"`
	Workloads   []Workload  `json:"workloads"`
}

// PodView is the payload of GET /v1/admin/k8s/pods.
type PodView struct {
	GeneratedAt time.Time `json:"generated_at"`
	Namespaces  []string  `json:"namespaces"`
	Pods        []Pod     `json:"pods"`
}

// Workload is one row of the workload table. The per-kind count fields are
// always present (zero when the kind does not use them) so the Console can
// render one shared column set without guessing from the kind.
type Workload struct {
	Kind      string `json:"kind"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	// Status is the Chinese, kind-aware summary ("运行中", "滚动更新中",
	// "已完成", "失败", "已暂停", ...).
	Status string `json:"status"`
	// Desired/Ready/Available/Updated mirror the controller replica counters.
	Desired   int32 `json:"desired"`
	Ready     int32 `json:"ready"`
	Available int32 `json:"available"`
	Updated   int32 `json:"updated"`
	// Completions/Succeeded/Failed/Active are the Job and CronJob counters.
	Completions int32 `json:"completions"`
	Succeeded   int32 `json:"succeeded"`
	Failed      int32 `json:"failed"`
	Active      int32 `json:"active"`
	// Schedule and Suspended are CronJob fields.
	Schedule  string `json:"schedule,omitempty"`
	Suspended bool   `json:"suspended,omitempty"`
	// Owner is set for resources owned by another built-in workload
	// (ReplicaSet -> Deployment, Job -> CronJob).
	Owner *OwnerRef `json:"owner,omitempty"`
	// Restartable is false when the update strategy cannot roll, with a
	// Chinese RestartBlockedReason shown inline on the row.
	Scalable             bool   `json:"scalable,omitempty"`
	Restartable          bool   `json:"restartable"`
	RestartBlockedReason string `json:"restart_blocked_reason,omitempty"`
	// Restart is the tracked progress of the last rolling restart of this
	// object (absent before any restart was submitted).
	Restart *RestartState `json:"restart,omitempty"`
	// Pods are the pods owned by this workload, resolved through the
	// Kubernetes owner references (Deployment -> ReplicaSet -> Pod,
	// CronJob -> Job -> Pod).
	Pods []Pod `json:"pods"`
}

// OwnerRef is one owner reference as displayed to the operator.
type OwnerRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Pod is one row of the "all pods" table and one entry of an expanded workload.
type Pod struct {
	Namespace       string    `json:"namespace"`
	Name            string    `json:"name"`
	Node            string    `json:"node"`
	Phase           string    `json:"phase"`
	Reason          string    `json:"reason,omitempty"`
	Message         string    `json:"message,omitempty"`
	ReadyContainers int32     `json:"ready_containers"`
	TotalContainers int32     `json:"total_containers"`
	Restarts        int32     `json:"restarts"`
	Owner           *OwnerRef `json:"owner,omitempty"`
}

// Restart status values tracked by the backend and rendered by the Console.
const (
	RestartSubmitted = "submitted"
	RestartRolling   = "rolling"
	RestartCompleted = "completed"
	RestartFailed    = "failed"
)

// RestartState is the rolling-restart progress of one workload. It is included
// in the workload snapshot on every poll, so the page can turn "已提交" into
// "滚动中"/"已完成" without a second request.
type RestartState struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	// StartedAt is when the pod-template annotation was submitted.
	StartedAt time.Time `json:"started_at"`
	// UpdatedAt is when the last progress evaluation ran.
	UpdatedAt time.Time `json:"updated_at"`
	// Updated/Desired/Ready are the controller counters at that evaluation.
	Updated int32 `json:"updated"`
	Desired int32 `json:"desired"`
	Ready   int32 `json:"ready"`
}
