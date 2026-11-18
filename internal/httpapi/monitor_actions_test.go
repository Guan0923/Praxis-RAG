package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/connect"
	"github.com/Guan0923/Praxis-RAG/internal/k8sops"
)

// fakeMonitorActions records the transport contract and returns scripted
// results, mirroring fakeMonitorRuntime for the read-only endpoints.
type fakeMonitorActions struct {
	requeueVal   int
	requeueErr   error
	requeueCalls int

	restarted  []string
	includeAll []bool
	restartErr error

	deployment    *k8sops.Deployment
	deploymentErr error

	scaledTo  []int32
	scaleErr  error
	scaleCall int
}

func (f *fakeMonitorActions) RequeueStuckDocumentJobs(context.Context) (int, error) {
	f.requeueCalls++
	return f.requeueVal, f.requeueErr
}

func (f *fakeMonitorActions) RestartConnector(_ context.Context, connector string, includeTasks bool) error {
	f.restarted = append(f.restarted, connector)
	f.includeAll = append(f.includeAll, includeTasks)
	return f.restartErr
}

func (f *fakeMonitorActions) ConnectorNames() []string {
	return []string{"praxis-document-cdc", "praxis-cache-cdc"}
}

func (f *fakeMonitorActions) WorkerDeployment(context.Context) (*k8sops.Deployment, error) {
	return f.deployment, f.deploymentErr
}

func (f *fakeMonitorActions) ScaleWorker(_ context.Context, replicas int32) error {
	f.scaleCall++
	f.scaledTo = append(f.scaledTo, replicas)
	return f.scaleErr
}

func newActionsServer(t *testing.T, actions MonitorActionsRuntime) http.Handler {
	t.Helper()
	options := make([]Option, 0, 1)
	if actions != nil {
		options = append(options, WithMonitorActions(actions))
	}
	return New(testSettings(""), nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), options...).Handler()
}

// TestMonitorRequeueJobsAction asserts the requeue transport contract: loopback
// admin access, no-store caching, the runtime count echoed as JSON.
func TestMonitorRequeueJobsAction(t *testing.T) {
	actions := &fakeMonitorActions{requeueVal: 2}
	handler := newActionsServer(t, actions)

	response := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/monitor/actions/requeue-jobs", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if body := decodeBody(t, response); body["requeued"] != float64(2) {
		t.Fatalf("body = %v", body)
	}
	if actions.requeueCalls != 1 {
		t.Fatalf("calls = %d", actions.requeueCalls)
	}
}

// TestMonitorActionsRequireAdmin keeps the admin boundary: a non-loopback
// anonymous request must be rejected before the runtime is called.
func TestMonitorActionsRequireAdmin(t *testing.T) {
	actions := &fakeMonitorActions{requeueVal: 1}
	handler := newActionsServer(t, actions)

	for _, target := range []string{
		"/v1/admin/monitor/actions/requeue-jobs",
		"/v1/admin/monitor/actions/connectors/praxis-document-cdc/restart",
		"/v1/admin/monitor/actions/worker-deployment",
		"/v1/admin/monitor/actions/worker-deployment/scale",
	} {
		method := "POST"
		if target == "/v1/admin/monitor/actions/worker-deployment" {
			method = "GET"
		}
		response := performRequest(handler, testRequest{
			method: method, target: target, remote: "10.9.9.9:5000",
		})
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d body = %s", target, response.Code, response.Body.String())
		}
	}
	if actions.requeueCalls != 0 || actions.scaleCall != 0 || len(actions.restarted) != 0 {
		t.Fatalf("non-admin reached the runtime: %+v", actions)
	}
}

// TestMonitorRestartConnectorAction asserts the restart transport contract:
// the fixed connector names only, includeTasks default true, and a scripted
// Connect-not-configured error mapping to 503.
func TestMonitorRestartConnectorAction(t *testing.T) {
	actions := &fakeMonitorActions{}
	handler := newActionsServer(t, actions)

	ok := performRequest(handler, testRequest{
		method: "POST",
		target: "/v1/admin/monitor/actions/connectors/praxis-cache-cdc/restart",
		remote: "127.0.0.1:5000",
	})
	if ok.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", ok.Code, ok.Body.String())
	}
	if len(actions.restarted) != 1 || actions.restarted[0] != "praxis-cache-cdc" || !actions.includeAll[0] {
		t.Fatalf("restarted = %v includeAll = %v", actions.restarted, actions.includeAll)
	}

	// An unknown connector name is rejected before the runtime is called.
	unknown := performRequest(handler, testRequest{
		method: "POST",
		target: "/v1/admin/monitor/actions/connectors/other-connector/restart",
		remote: "127.0.0.1:5000",
	})
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown status = %d body = %s", unknown.Code, unknown.Body.String())
	}

	// includeTasks=false is honored.
	_ = performRequest(handler, testRequest{
		method: "POST",
		target: "/v1/admin/monitor/actions/connectors/praxis-document-cdc/restart?includeTasks=false",
		remote: "127.0.0.1:5000",
	})
	if actions.includeAll[1] != false {
		t.Fatalf("includeAll = %v", actions.includeAll)
	}

	// A scripted not-configured error becomes 503 with the Chinese reason.
	notConfigured := &fakeMonitorActions{restartErr: connect.ErrNotConfigured}
	handler2 := newActionsServer(t, notConfigured)
	failure := performRequest(handler2, testRequest{
		method: "POST",
		target: "/v1/admin/monitor/actions/connectors/praxis-document-cdc/restart",
		remote: "127.0.0.1:5000",
	})
	if failure.Code != http.StatusServiceUnavailable {
		t.Fatalf("not configured status = %d body = %s", failure.Code, failure.Body.String())
	}
	if detail := decodeBody(t, failure)["detail"]; detail != "Kafka Connect 未配置" {
		t.Fatalf("detail = %v", detail)
	}
}

// TestMonitorWorkerDeploymentAction asserts the Deployment read: the payload
// shape and the Kubernetes sentinel error mappings.
func TestMonitorWorkerDeploymentAction(t *testing.T) {
	actions := &fakeMonitorActions{deployment: &k8sops.Deployment{
		Name: "rag-document-worker", Namespace: "praxis-rag",
		DesiredReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2, ObservedGeneration: 3,
	}}
	handler := newActionsServer(t, actions)

	response := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/monitor/actions/worker-deployment", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	deployment, _ := decodeBody(t, response)["deployment"].(map[string]any)
	if deployment["name"] != "rag-document-worker" || deployment["desired_replicas"] != float64(2) ||
		deployment["ready_replicas"] != float64(2) {
		t.Fatalf("deployment = %v", deployment)
	}

	for _, item := range []struct {
		name     string
		err      error
		wantCode int
	}{
		{"not in cluster", k8sops.ErrNotInCluster, http.StatusServiceUnavailable},
		{"rbac forbidden", k8sops.ErrForbidden, http.StatusForbidden},
		{"deployment missing", k8sops.ErrDeploymentNotFound, http.StatusNotFound},
	} {
		scripted := &fakeMonitorActions{deploymentErr: item.err}
		body := performRequest(newActionsServer(t, scripted), testRequest{
			method: "GET", target: "/v1/admin/monitor/actions/worker-deployment", remote: "127.0.0.1:5000",
		})
		if body.Code != item.wantCode {
			t.Fatalf("%s status = %d body = %s", item.name, body.Code, body.Body.String())
		}
	}
}

// TestMonitorScaleWorkerAction asserts the scale transport contract: the
// replica bound check, the JSON body requirement, and the echoed result.
func TestMonitorScaleWorkerAction(t *testing.T) {
	actions := &fakeMonitorActions{}
	handler := newActionsServer(t, actions)

	scaled := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/monitor/actions/worker-deployment/scale",
		remote: "127.0.0.1:5000", body: `{"replicas":3}`,
	})
	if scaled.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", scaled.Code, scaled.Body.String())
	}
	if body := decodeBody(t, scaled); body["replicas"] != float64(3) {
		t.Fatalf("body = %v", body)
	}
	if len(actions.scaledTo) != 1 || actions.scaledTo[0] != 3 {
		t.Fatalf("scaledTo = %v", actions.scaledTo)
	}

	for _, item := range []struct {
		name string
		body string
		want int
	}{
		{"missing body", "", http.StatusUnprocessableEntity},
		{"non-integer", `{"replicas":1.5}`, http.StatusUnprocessableEntity},
		{"negative", `{"replicas":-1}`, http.StatusBadRequest},
		{"too large", `{"replicas":65}`, http.StatusBadRequest},
	} {
		response := performRequest(handler, testRequest{
			method: "POST", target: "/v1/admin/monitor/actions/worker-deployment/scale",
			remote: "127.0.0.1:5000", body: item.body,
		})
		if response.Code != item.want {
			t.Fatalf("%s status = %d body = %s", item.name, response.Code, response.Body.String())
		}
	}
	if len(actions.scaledTo) != 1 {
		t.Fatalf("invalid bodies reached the runtime: %v", actions.scaledTo)
	}
}

// TestMonitorActionsUnavailable asserts the 503 degradation before the
// runtime is wired (no service account mounted, no action support).
func TestMonitorActionsUnavailable(t *testing.T) {
	handler := newActionsServer(t, nil)
	for _, target := range []string{
		"/v1/admin/monitor/actions/requeue-jobs",
		"/v1/admin/monitor/actions/connectors/praxis-document-cdc/restart",
		"/v1/admin/monitor/actions/worker-deployment/scale",
	} {
		response := performRequest(handler, testRequest{
			method: "POST", target: target, remote: "127.0.0.1:5000", body: `{}`,
		})
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d", target, response.Code)
		}
	}
	read := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/monitor/actions/worker-deployment", remote: "127.0.0.1:5000",
	})
	if read.Code != http.StatusServiceUnavailable {
		t.Fatalf("worker-deployment status = %d", read.Code)
	}
}

// TestMonitorActionsRuntimeErrorMapping asserts a scripted runtime failure
// (database down) becomes a bounded 500, not a panic.
func TestMonitorActionsRuntimeErrorMapping(t *testing.T) {
	actions := &fakeMonitorActions{requeueErr: errors.New("database down")}
	handler := newActionsServer(t, actions)
	response := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/monitor/actions/requeue-jobs", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if detail := decodeBody(t, response)["detail"]; detail != "任务重投执行失败" {
		t.Fatalf("detail = %v", detail)
	}
}
