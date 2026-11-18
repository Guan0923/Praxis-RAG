package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/connect"
	"github.com/Guan0923/Praxis-RAG/internal/k8sops"
)

// MonitorActionsRuntime performs the operator actions the Console monitor page
// offers. Each method maps to exactly one button:
//
//	RequeueStuckDocumentJobs -> POST /v1/admin/monitor/actions/requeue-jobs
//	RestartConnector         -> POST /v1/admin/monitor/actions/connectors/{name}/restart
//	WorkerDeployment         -> GET  /v1/admin/monitor/actions/worker-deployment
//	ScaleWorker              -> POST /v1/admin/monitor/actions/worker-deployment/scale
type MonitorActionsRuntime interface {
	// RequeueStuckDocumentJobs requeues due retries and expired leases now.
	RequeueStuckDocumentJobs(ctx context.Context) (int, error)
	// RestartConnector restarts one capture connector's FAILED tasks (all
	// tasks when includeTasks is set), resetting the supervisor budget.
	RestartConnector(ctx context.Context, connector string, includeTasks bool) error
	// ConnectorNames returns the two fixed connector names for validation.
	ConnectorNames() []string
	// WorkerDeployment reads the document worker Deployment replica state.
	WorkerDeployment(ctx context.Context) (*k8sops.Deployment, error)
	// ScaleWorker sets the document worker replica count.
	ScaleWorker(ctx context.Context, replicas int32) error
}

// WithMonitorActions wires the monitor operator-action handlers. Without it
// the endpoints answer 503 (the page hides the buttons), so a deployment that
// does not mount the Kubernetes service account stays read-only.
func WithMonitorActions(runtime MonitorActionsRuntime) Option {
	return func(s *Server) { s.monitorActions = runtime }
}

// monitorActionTimeout bounds every operator action. The requeue and restart
// calls are single round trips; the Kubernetes read/patch is bounded by the
// client too, so a stuck dependency cannot hold the request open.
const monitorActionTimeout = 15 * time.Second

// registerMonitorActionRoutes installs the four operator routes. They share
// the admin boundary with the read-only monitor endpoints (admin session or
// trusted loopback) and are POST except the Deployment read.
func (s *Server) registerMonitorActionRoutes() {
	s.router.add("POST", "/v1/admin/monitor/actions/requeue-jobs", authAdmin, s.handleMonitorRequeueJobs)
	s.router.add("POST", "/v1/admin/monitor/actions/connectors/{connector}/restart", authAdmin, s.handleMonitorRestartConnector)
	s.router.add("GET", "/v1/admin/monitor/actions/worker-deployment", authAdmin, s.handleMonitorWorkerDeployment)
	s.router.add("POST", "/v1/admin/monitor/actions/worker-deployment/scale", authAdmin, s.handleMonitorScaleWorker)
}

// handleMonitorRequeueJobs implements POST /v1/admin/monitor/actions/requeue-jobs:
// the "立即重投" button. It bypasses neither the attempt budget nor the
// terminalization rules; it only skips the quiet window of the periodic sweep.
func (s *Server) handleMonitorRequeueJobs(w http.ResponseWriter, r *http.Request) {
	if s.monitorActions == nil {
		writeDetail(w, http.StatusServiceUnavailable, "监控操作未启用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), monitorActionTimeout)
	defer cancel()
	count, err := s.monitorActions.RequeueStuckDocumentJobs(ctx)
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "任务重投执行失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"requeued": count})
}

// handleMonitorRestartConnector implements POST
// /v1/admin/monitor/actions/connectors/{connector}/restart: the "重启连接器"
// button. includeTasks defaults to true (restart the connector and its tasks)
// because the Console use case is a FAILED task that the supervisor budget
// gave up on; ?includeTasks=false keeps the connector-level restart only.
func (s *Server) handleMonitorRestartConnector(w http.ResponseWriter, r *http.Request) {
	if s.monitorActions == nil {
		writeDetail(w, http.StatusServiceUnavailable, "监控操作未启用")
		return
	}
	connector := pathParam(r, "connector")
	if !s.validMonitorConnector(connector) {
		writeDetail(w, http.StatusBadRequest, "未知连接器")
		return
	}
	includeTasks := true
	if raw := strings.TrimSpace(r.URL.Query().Get("includeTasks")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeDetail(w, http.StatusBadRequest, "includeTasks 必须是 true 或 false")
			return
		}
		includeTasks = parsed
	}
	ctx, cancel := context.WithTimeout(r.Context(), monitorActionTimeout)
	defer cancel()
	if err := s.monitorActions.RestartConnector(ctx, connector, includeTasks); err != nil {
		if errors.Is(err, connect.ErrNotConfigured) {
			writeDetail(w, http.StatusServiceUnavailable, "Kafka Connect 未配置")
			return
		}
		writeDetail(w, http.StatusBadGateway, "连接器重启请求失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"connector": connector, "restarted": true})
}

// validMonitorConnector only accepts the two fixed connector names: the route
// must never forward an arbitrary operator-supplied name to Kafka Connect.
func (s *Server) validMonitorConnector(connector string) bool {
	if s.monitorActions == nil || connector == "" {
		return false
	}
	for _, name := range s.monitorActions.ConnectorNames() {
		if name == connector {
			return true
		}
	}
	return false
}

// handleMonitorWorkerDeployment implements GET
// /v1/admin/monitor/actions/worker-deployment: the worker scale card's read.
func (s *Server) handleMonitorWorkerDeployment(w http.ResponseWriter, r *http.Request) {
	if s.monitorActions == nil {
		writeDetail(w, http.StatusServiceUnavailable, "监控操作未启用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), monitorActionTimeout)
	defer cancel()
	deployment, err := s.monitorActions.WorkerDeployment(ctx)
	if err != nil {
		s.writeMonitorK8sError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"deployment": deployment})
}

// handleMonitorScaleWorker implements POST
// /v1/admin/monitor/actions/worker-deployment/scale: the worker scale card's
// action. The body is a single JSON object {"replicas": N}.
func (s *Server) handleMonitorScaleWorker(w http.ResponseWriter, r *http.Request) {
	if s.monitorActions == nil {
		writeDetail(w, http.StatusServiceUnavailable, "监控操作未启用")
		return
	}
	var body struct {
		Replicas *int32 `json:"replicas"`
	}
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	if body.Replicas == nil || *body.Replicas < 0 || *body.Replicas > 64 {
		writeDetail(w, http.StatusBadRequest, "replicas 必须在 0 到 64 之间")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), monitorActionTimeout)
	defer cancel()
	if err := s.monitorActions.ScaleWorker(ctx, *body.Replicas); err != nil {
		s.writeMonitorK8sError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"replicas": *body.Replicas})
}

// writeMonitorK8sError maps the Kubernetes client's sentinel errors onto the
// HTTP statuses the Console distinguishes (503 not wired, 403 RBAC missing,
// 404 deployment absent); anything else is a bounded 502.
func (s *Server) writeMonitorK8sError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, k8sops.ErrNotInCluster):
		writeDetail(w, http.StatusServiceUnavailable, "Kubernetes 操作未启用（未挂载 ServiceAccount）")
	case errors.Is(err, k8sops.ErrForbidden):
		writeDetail(w, http.StatusForbidden, "Kubernetes RBAC 未授权此操作")
	case errors.Is(err, k8sops.ErrDeploymentNotFound):
		writeDetail(w, http.StatusNotFound, "文档 worker Deployment 不存在")
	default:
		writeDetail(w, http.StatusBadGateway, "Kubernetes API 调用失败")
	}
}

// monitorActionsWired reports whether operator actions are available. Used by
// the overview handler to expose the capability to the Console.
func (s *Server) monitorActionsWired() bool { return s.monitorActions != nil }
