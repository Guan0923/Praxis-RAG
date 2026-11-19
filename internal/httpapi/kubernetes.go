package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/k8sops"
)

// KubernetesRuntime is the Console Kubernetes monitor backend: read-only
// workload/pod snapshots served to the administrator page plus the rolling
// restart operator action. internal/k8sops.Cluster implements it; tests use a
// fake so the transport package never dials a real API server.
type KubernetesRuntime interface {
	// Configured reports whether cluster credentials were resolved.
	Configured() bool
	Memory(context.Context) (*k8sops.MemoryView, error)
	// ClusterInfo identifies the connected cluster (no API server URL).
	ClusterInfo() k8sops.ClusterInfo
	// Workloads returns the filtered workload snapshot.
	Workloads(ctx context.Context, filter k8sops.Filter) (*k8sops.WorkloadView, error)
	// Pods returns the filtered pod snapshot.
	Pods(ctx context.Context, filter k8sops.Filter) (*k8sops.PodView, error)
	// Restart submits a rolling restart of one workload and returns its
	// initial progress state.
	Restart(ctx context.Context, resource, namespace, name string) (*k8sops.RestartState, error)
}

// WithKubernetesRuntime wires the Console Kubernetes monitor endpoints. Without
// it both read routes answer 503 (Kubernetes 监控未启用) and no cluster client
// is ever constructed.
func WithKubernetesRuntime(runtime KubernetesRuntime) Option {
	return func(s *Server) { s.kubernetes = runtime }
}

// kubernetesRequestTimeout bounds every cluster call so a stuck API server
// cannot hold the HTTP request open (the client applies its own bound too).
const kubernetesRequestTimeout = 10 * time.Second

// Query/path parameter bounds. The name filter is a substring matched by the
// backend; the workload name is one Kubernetes object name (253 chars maximum).
const (
	kubernetesNameFilterLimit   = 128
	kubernetesWorkloadNameLimit = 253
)

// registerKubernetesRoutes installs the administrator-only Kubernetes routes.
// They use authAdminSession, which requires an explicit administrator Console
// session and never accepts the local loopback break-glass.
func (s *Server) registerKubernetesRoutes() {
	s.router.add("GET", "/v1/admin/k8s/workloads", authAdminSession, s.handleKubernetesWorkloads)
	s.router.add("GET", "/v1/admin/k8s/memory", authAdminSession, s.handleKubernetesMemory)
	s.router.add("GET", "/v1/admin/k8s/pods", authAdminSession, s.handleKubernetesPods)
	s.router.add("POST", "/v1/admin/k8s/workloads/{resource}/{namespace}/{name}/restart",
		authAdminSession, s.handleKubernetesRestart)
}

// kubernetesReadRuntime returns the wired runtime for a read route and writes
// the 503 body when the backend is absent or has no cluster credentials. A
// missing backend is a wiring decision (503 "未启用"); missing credentials are
// a deployment concern explained by k8sops.ConfigurationHint.
func (s *Server) kubernetesReadRuntime(w http.ResponseWriter) (KubernetesRuntime, bool) {
	w.Header().Set("Cache-Control", "no-store")
	if s.kubernetes == nil {
		// No runtime was wired at all (the usual Compose / host deployment).
		// Combine "not enabled" with the operator hint: the Console alert must
		// tell the administrator how to configure the cluster, not just that
		// the feature is absent.
		writeDetail(w, http.StatusServiceUnavailable, "Kubernetes 监控未启用："+k8sops.ConfigurationHint)
		return nil, false
	}
	if !s.kubernetes.Configured() {
		writeDetail(w, http.StatusServiceUnavailable, k8sops.ConfigurationHint)
		return nil, false
	}
	return s.kubernetes, true
}

// kubernetesFilterFromQuery validates the shared ?namespace=&name= filter. The
// namespace must match the tenant identifier format (empty means all
// namespaces); the name substring is trimmed and bounded.
func kubernetesFilterFromQuery(w http.ResponseWriter, r *http.Request) (k8sops.Filter, bool) {
	values := r.URL.Query()
	namespace := strings.TrimSpace(values.Get("namespace"))
	if namespace != "" && !namespacePattern.MatchString(namespace) {
		writeDetail(w, http.StatusBadRequest, "Invalid RAG namespace.")
		return k8sops.Filter{}, false
	}
	name := strings.TrimSpace(values.Get("name"))
	if len(name) > kubernetesNameFilterLimit {
		writeDetail(w, http.StatusBadRequest, "名称过滤条件不能超过 128 个字符")
		return k8sops.Filter{}, false
	}
	return k8sops.Filter{Namespace: namespace, Name: name}, true
}

// handleKubernetesWorkloads implements GET /v1/admin/k8s/workloads: the
// workload table snapshot (workloads with their owner-resolved pods).
func (s *Server) handleKubernetesWorkloads(w http.ResponseWriter, r *http.Request) {
	runtime, ok := s.kubernetesReadRuntime(w)
	if !ok {
		return
	}
	filter, ok := kubernetesFilterFromQuery(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), kubernetesRequestTimeout)
	defer cancel()
	view, err := runtime.Workloads(ctx, filter)
	if err != nil {
		s.writeKubernetesReadError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleKubernetesMemory(w http.ResponseWriter, r *http.Request) {
	runtime, ok := s.kubernetesReadRuntime(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), kubernetesRequestTimeout)
	defer cancel()
	view, err := runtime.Memory(ctx)
	if err != nil {
		s.writeKubernetesReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleKubernetesPods implements GET /v1/admin/k8s/pods: the all-pods snapshot
// filtered by namespace/name.
func (s *Server) handleKubernetesPods(w http.ResponseWriter, r *http.Request) {
	runtime, ok := s.kubernetesReadRuntime(w)
	if !ok {
		return
	}
	filter, ok := kubernetesFilterFromQuery(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), kubernetesRequestTimeout)
	defer cancel()
	view, err := runtime.Pods(ctx, filter)
	if err != nil {
		s.writeKubernetesReadError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, view)
}

// handleKubernetesRestart implements POST
// /v1/admin/k8s/workloads/{resource}/{namespace}/{name}/restart: a rolling
// restart of one Deployment / StatefulSet / DaemonSet. The resource segment is
// an allow-list so an arbitrary resource can never reach the cluster client.
func (s *Server) handleKubernetesRestart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	resource := pathParam(r, "resource")
	namespace := pathParam(r, "namespace")
	name := pathParam(r, "name")
	if !kubernetesRestartResource(resource) {
		writeDetail(w, http.StatusBadRequest, "不支持的工作负载类型")
		return
	}
	if !namespacePattern.MatchString(namespace) {
		writeDetail(w, http.StatusBadRequest, "Invalid RAG namespace.")
		return
	}
	if name == "" || len(name) > kubernetesWorkloadNameLimit {
		writeDetail(w, http.StatusBadRequest, "工作负载名称不合法")
		return
	}
	if s.kubernetes == nil {
		writeDetail(w, http.StatusServiceUnavailable, "Kubernetes 监控未启用："+k8sops.ConfigurationHint)
		return
	}
	if !s.kubernetes.Configured() {
		writeDetail(w, http.StatusServiceUnavailable, k8sops.ConfigurationHint)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), kubernetesRequestTimeout)
	defer cancel()
	state, err := s.kubernetes.Restart(ctx, resource, namespace, name)
	if err != nil {
		s.writeKubernetesRestartError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"restart": state})
}

// kubernetesRestartResource accepts only the three workload kinds whose rolling
// restart the backend implements.
func kubernetesRestartResource(resource string) bool {
	switch resource {
	case k8sops.ResourceDeployments, k8sops.ResourceStatefulSets, k8sops.ResourceDaemonSets:
		return true
	default:
		return false
	}
}

// writeKubernetesReadError maps a snapshot read failure: an unconfigured
// cluster keeps the configuration hint, everything else is a bounded 502
// because the snapshot cannot be served without the cluster.
func (s *Server) writeKubernetesReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, k8sops.ErrNotConfigured) {
		writeDetail(w, http.StatusServiceUnavailable, k8sops.ConfigurationHint)
		return
	}
	writeDetail(w, http.StatusBadGateway, "Kubernetes 数据读取失败")
}

// writeKubernetesRestartError maps the cluster client's sentinel errors onto the
// statuses the Console distinguishes: unconfigured 503, absent workload 404,
// strategy/in-flight conflicts 409 with the backend's Chinese reason, RBAC
// rejection 502 with the manifest hint, anything else a bounded 502.
func (s *Server) writeKubernetesRestartError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, k8sops.ErrNotConfigured):
		writeDetail(w, http.StatusServiceUnavailable, k8sops.ConfigurationHint)
	case errors.Is(err, k8sops.ErrNotFound):
		writeDetail(w, http.StatusNotFound, "工作负载不存在或已被删除")
	case errors.Is(err, k8sops.ErrNotRestartable):
		writeDetail(w, http.StatusConflict, err.Error())
	case errors.Is(err, k8sops.ErrRestartInProgress):
		writeDetail(w, http.StatusConflict, err.Error())
	case errors.Is(err, k8sops.ErrForbidden):
		writeDetail(w, http.StatusBadGateway, "Kubernetes 拒绝操作，请检查 RBAC 清单")
	default:
		writeDetail(w, http.StatusBadGateway, fmt.Sprintf("重启请求失败：%v", err))
	}
}
