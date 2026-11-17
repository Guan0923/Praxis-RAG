package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/auth"
	"github.com/Guan0923/xinghe_rag/internal/k8sops"
)

// fakeKubernetesRuntime records the calls the transport layer makes and returns
// whatever the test configured, so no test ever dials a real cluster.
type fakeKubernetesRuntime struct {
	configured bool
	info       k8sops.ClusterInfo

	workloads    *k8sops.WorkloadView
	workloadsErr error
	pods         *k8sops.PodView
	podsErr      error
	restart      *k8sops.RestartState
	restartErr   error

	workloadFilters []k8sops.Filter
	podFilters      []k8sops.Filter
	restartCalls    []fakeKubernetesRestartCall
}

type fakeKubernetesRestartCall struct {
	resource  string
	namespace string
	name      string
}

func (f *fakeKubernetesRuntime) Configured() bool { return f.configured }

func (f *fakeKubernetesRuntime) ClusterInfo() k8sops.ClusterInfo { return f.info }

func (f *fakeKubernetesRuntime) Workloads(_ context.Context, filter k8sops.Filter) (*k8sops.WorkloadView, error) {
	f.workloadFilters = append(f.workloadFilters, filter)
	if f.workloadsErr != nil {
		return nil, f.workloadsErr
	}
	return f.workloads, nil
}

func (f *fakeKubernetesRuntime) Pods(_ context.Context, filter k8sops.Filter) (*k8sops.PodView, error) {
	f.podFilters = append(f.podFilters, filter)
	if f.podsErr != nil {
		return nil, f.podsErr
	}
	return f.pods, nil
}

func (f *fakeKubernetesRuntime) Restart(_ context.Context, resource, namespace, name string) (*k8sops.RestartState, error) {
	f.restartCalls = append(f.restartCalls, fakeKubernetesRestartCall{resource: resource, namespace: namespace, name: name})
	if f.restartErr != nil {
		return nil, f.restartErr
	}
	return f.restart, nil
}

var _ KubernetesRuntime = (*fakeKubernetesRuntime)(nil)

const (
	kubernetesAdminToken = "admin-token"
	kubernetesUserToken  = "user-token"
)

// newKubernetesTestServer builds an auth-enabled server with the fake runtime
// wired through the public option, exactly like cmd/rag-service does.
func newKubernetesTestServer(t *testing.T, runtime KubernetesRuntime) *Server {
	t.Helper()
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		kubernetesAdminToken: adminSession(kubernetesAdminToken),
		kubernetesUserToken:  userSession(kubernetesUserToken),
	}}
	settings := testSettings("")
	settings.AuthEnabled = true
	settings.RegistrationEnabled = true
	settings.SessionTTL = time.Hour
	return New(settings, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithUserAuth(users),
		WithKubernetesRuntime(runtime),
	)
}

func kubernetesBearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func kubernetesRestartTarget(resource, namespace, name string) string {
	return "/v1/admin/k8s/workloads/" + resource + "/" + namespace + "/" + name + "/restart"
}

// TestKubernetesRoutesRequireSession locks the key security property: the
// Kubernetes endpoints use authAdminSession and therefore never accept the
// trusted loopback break-glass that authAdmin keeps for host automation. A
// session-less request from 127.0.0.1 must be 401 on reads and on the write.
func TestKubernetesRoutesRequireSession(t *testing.T) {
	runtime := &fakeKubernetesRuntime{configured: true}
	handler := newKubernetesTestServer(t, runtime).Handler()
	requests := []testRequest{
		{method: "GET", target: "/v1/admin/k8s/workloads", remote: "127.0.0.1:5000"},
		{method: "GET", target: "/v1/admin/k8s/pods", remote: "127.0.0.1:5000"},
		{method: "POST", target: kubernetesRestartTarget("deployments", "alpha", "web"), remote: "127.0.0.1:5000"},
	}
	for _, request := range requests {
		response := performRequest(handler, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d body = %s", request.method, request.target,
				response.Code, response.Body.String())
		}
		if cache := response.Header().Get("Cache-Control"); cache != "no-store" {
			t.Fatalf("%s %s Cache-Control = %q", request.method, request.target, cache)
		}
	}
	if len(runtime.workloadFilters) != 0 || len(runtime.podFilters) != 0 || len(runtime.restartCalls) != 0 {
		t.Fatalf("session-less requests reached the runtime: %+v %+v %+v",
			runtime.workloadFilters, runtime.podFilters, runtime.restartCalls)
	}
}

func TestKubernetesRoutesRejectNonAdmin(t *testing.T) {
	runtime := &fakeKubernetesRuntime{configured: true}
	handler := newKubernetesTestServer(t, runtime).Handler()
	requests := []testRequest{
		{method: "GET", target: "/v1/admin/k8s/workloads", remote: "10.0.0.9:5000",
			headers: kubernetesBearer(kubernetesUserToken)},
		{method: "GET", target: "/v1/admin/k8s/pods", remote: "10.0.0.9:5000",
			headers: kubernetesBearer(kubernetesUserToken)},
		{method: "POST", target: kubernetesRestartTarget("deployments", "alpha", "web"), remote: "10.0.0.9:5000",
			headers: kubernetesBearer(kubernetesUserToken)},
	}
	for _, request := range requests {
		response := performRequest(handler, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s %s status = %d body = %s", request.method, request.target,
				response.Code, response.Body.String())
		}
		if detail := decodeBody(t, response)["detail"]; detail != "需要管理员权限" {
			t.Fatalf("%s %s detail = %v", request.method, request.target, detail)
		}
	}
	if len(runtime.workloadFilters) != 0 || len(runtime.podFilters) != 0 || len(runtime.restartCalls) != 0 {
		t.Fatalf("non-admin requests reached the runtime")
	}
}

func TestKubernetesAdminReadsWorkloadsAndPods(t *testing.T) {
	generated := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	runtime := &fakeKubernetesRuntime{
		configured: true,
		info:       k8sops.ClusterInfo{Name: "test-cluster", Source: k8sops.SourceKubeconfig},
		workloads: &k8sops.WorkloadView{
			GeneratedAt: generated,
			Cluster:     k8sops.ClusterInfo{Name: "test-cluster", Source: k8sops.SourceKubeconfig},
			Namespaces:  []string{"alpha"},
			Workloads: []k8sops.Workload{{
				Kind: "Deployment", Resource: k8sops.ResourceDeployments,
				Namespace: "alpha", Name: "web", Status: "运行中", Restartable: true,
			}},
		},
		pods: &k8sops.PodView{
			GeneratedAt: generated,
			Namespaces:  []string{"beta"},
			Pods: []k8sops.Pod{{
				Namespace: "beta", Name: "db-0", Node: "node-1", Phase: "Running",
				ReadyContainers: 1, TotalContainers: 1,
			}},
		},
	}
	handler := newKubernetesTestServer(t, runtime).Handler()

	workloads := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/k8s/workloads?namespace=alpha&name=web",
		remote: "10.0.0.9:5000", headers: kubernetesBearer(kubernetesAdminToken),
	})
	if workloads.Code != http.StatusOK {
		t.Fatalf("workloads status = %d body = %s", workloads.Code, workloads.Body.String())
	}
	if cache := workloads.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("workloads Cache-Control = %q", cache)
	}
	if len(runtime.workloadFilters) != 1 ||
		runtime.workloadFilters[0] != (k8sops.Filter{Namespace: "alpha", Name: "web"}) {
		t.Fatalf("workload filters = %+v", runtime.workloadFilters)
	}
	workloadBody := decodeBody(t, workloads)
	if workloadBody["cluster"].(map[string]any)["name"] != "test-cluster" {
		t.Fatalf("workload cluster = %v", workloadBody["cluster"])
	}
	rows, _ := workloadBody["workloads"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["name"] != "web" {
		t.Fatalf("workloads = %v", workloadBody["workloads"])
	}

	pods := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/k8s/pods?namespace=beta&name=db",
		remote: "10.0.0.9:5000", headers: kubernetesBearer(kubernetesAdminToken),
	})
	if pods.Code != http.StatusOK {
		t.Fatalf("pods status = %d body = %s", pods.Code, pods.Body.String())
	}
	if len(runtime.podFilters) != 1 ||
		runtime.podFilters[0] != (k8sops.Filter{Namespace: "beta", Name: "db"}) {
		t.Fatalf("pod filters = %+v", runtime.podFilters)
	}
	podBody := decodeBody(t, pods)
	podRows, _ := podBody["pods"].([]any)
	if len(podRows) != 1 || podRows[0].(map[string]any)["name"] != "db-0" {
		t.Fatalf("pods = %v", podBody["pods"])
	}

	// Whitespace-only filters normalize to "all namespaces / all names".
	all := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/k8s/workloads?namespace=%20%20&name=%20",
		remote: "10.0.0.9:5000", headers: kubernetesBearer(kubernetesAdminToken),
	})
	if all.Code != http.StatusOK {
		t.Fatalf("all status = %d body = %s", all.Code, all.Body.String())
	}
	if last := runtime.workloadFilters[len(runtime.workloadFilters)-1]; last != (k8sops.Filter{}) {
		t.Fatalf("empty filter = %+v", last)
	}
}

func TestKubernetesAdminRestartForwardsTarget(t *testing.T) {
	started := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, resource := range []string{
		k8sops.ResourceDeployments, k8sops.ResourceStatefulSets, k8sops.ResourceDaemonSets,
	} {
		t.Run(resource, func(t *testing.T) {
			runtime := &fakeKubernetesRuntime{
				configured: true,
				restart: &k8sops.RestartState{
					Status: k8sops.RestartSubmitted, Message: "已提交滚动重启", StartedAt: started,
				},
			}
			handler := newKubernetesTestServer(t, runtime).Handler()
			response := performRequest(handler, testRequest{
				method: "POST", target: kubernetesRestartTarget(resource, "alpha", "web"),
				remote: "10.0.0.9:5000", headers: kubernetesBearer(kubernetesAdminToken),
			})
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
			if cache := response.Header().Get("Cache-Control"); cache != "no-store" {
				t.Fatalf("Cache-Control = %q", cache)
			}
			if len(runtime.restartCalls) != 1 {
				t.Fatalf("restart calls = %+v", runtime.restartCalls)
			}
			call := runtime.restartCalls[0]
			if call.resource != resource || call.namespace != "alpha" || call.name != "web" {
				t.Fatalf("restart call = %+v", call)
			}
			body := decodeBody(t, response)
			restart, ok := body["restart"].(map[string]any)
			if !ok || restart["status"] != k8sops.RestartSubmitted || restart["message"] != "已提交滚动重启" {
				t.Fatalf("restart payload = %v", body["restart"])
			}
			if _, ok := restart["started_at"]; !ok {
				t.Fatalf("restart payload misses started_at: %v", restart)
			}
		})
	}
}

func TestKubernetesRestartCookieSessionRequiresCSRF(t *testing.T) {
	runtime := &fakeKubernetesRuntime{
		configured: true,
		restart:    &k8sops.RestartState{Status: k8sops.RestartSubmitted, Message: "已提交滚动重启"},
	}
	handler := newKubernetesTestServer(t, runtime).Handler()
	target := kubernetesRestartTarget("deployments", "alpha", "web")

	withoutCSRF := performRequest(handler, testRequest{
		method: "POST", target: target, remote: "10.0.0.9:5000",
		headers: cookieFor(kubernetesAdminToken),
	})
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d body = %s", withoutCSRF.Code, withoutCSRF.Body.String())
	}
	if len(runtime.restartCalls) != 0 {
		t.Fatalf("CSRF-less restart reached the runtime")
	}

	withCSRF := performRequest(handler, testRequest{
		method: "POST", target: target, remote: "10.0.0.9:5000",
		headers: mergeHeaders(cookieFor(kubernetesAdminToken), map[string]string{
			csrfHeaderName: "csrf-" + kubernetesAdminToken,
		}),
	})
	if withCSRF.Code != http.StatusOK {
		t.Fatalf("valid CSRF status = %d body = %s", withCSRF.Code, withCSRF.Body.String())
	}
	if len(runtime.restartCalls) != 1 {
		t.Fatalf("restart calls = %+v", runtime.restartCalls)
	}
}

func TestKubernetesRuntimeAbsentDegrades(t *testing.T) {
	handler := newKubernetesTestServer(t, nil).Handler()
	for _, request := range []testRequest{
		{method: "GET", target: "/v1/admin/k8s/workloads", headers: kubernetesBearer(kubernetesAdminToken)},
		{method: "GET", target: "/v1/admin/k8s/pods", headers: kubernetesBearer(kubernetesAdminToken)},
		{method: "POST", target: kubernetesRestartTarget("deployments", "alpha", "web"),
			headers: kubernetesBearer(kubernetesAdminToken)},
	} {
		response := performRequest(handler, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s status = %d body = %s", request.method, request.target,
				response.Code, response.Body.String())
		}
		detail := fmt.Sprint(decodeBody(t, response)["detail"])
		if !strings.Contains(detail, "Kubernetes 监控未启用") || !strings.Contains(detail, "RAG_K8S_KUBECONFIG") {
			t.Fatalf("%s %s detail = %v", request.method, request.target, detail)
		}
	}

	// A wired runtime without resolved credentials is a configuration problem,
	// not a wiring one: it answers with the operator hint.
	unconfigured := &fakeKubernetesRuntime{configured: false}
	unconfiguredHandler := newKubernetesTestServer(t, unconfigured).Handler()
	response := performRequest(unconfiguredHandler, testRequest{
		method: "GET", target: "/v1/admin/k8s/workloads", headers: kubernetesBearer(kubernetesAdminToken),
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status = %d", response.Code)
	}
	if detail := decodeBody(t, response)["detail"]; !strings.Contains(fmt.Sprint(detail), "RAG_K8S_KUBECONFIG") {
		t.Fatalf("unconfigured detail = %v", detail)
	}
}

func TestKubernetesNotConfiguredErrorMapsToHint(t *testing.T) {
	runtime := &fakeKubernetesRuntime{
		configured:   true,
		workloadsErr: k8sops.ErrNotConfigured,
		podsErr:      k8sops.ErrNotConfigured,
		restartErr:   k8sops.ErrNotConfigured,
	}
	handler := newKubernetesTestServer(t, runtime).Handler()
	requests := []testRequest{
		{method: "GET", target: "/v1/admin/k8s/workloads", headers: kubernetesBearer(kubernetesAdminToken)},
		{method: "GET", target: "/v1/admin/k8s/pods", headers: kubernetesBearer(kubernetesAdminToken)},
		{method: "POST", target: kubernetesRestartTarget("deployments", "alpha", "web"),
			headers: kubernetesBearer(kubernetesAdminToken)},
	}
	for _, request := range requests {
		response := performRequest(handler, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s status = %d body = %s", request.method, request.target,
				response.Code, response.Body.String())
		}
		if detail := decodeBody(t, response)["detail"]; !strings.Contains(fmt.Sprint(detail), "RAG_K8S_KUBECONFIG") {
			t.Fatalf("%s %s detail = %v", request.method, request.target, detail)
		}
	}
}

func TestKubernetesRestartErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		detail string
	}{
		{
			name:   "not found",
			err:    fmt.Errorf("%w: alpha/web", k8sops.ErrNotFound),
			status: http.StatusNotFound,
			detail: "工作负载不存在或已被删除",
		},
		{
			name:   "not restartable",
			err:    fmt.Errorf("%w: Deployment 已暂停", k8sops.ErrNotRestartable),
			status: http.StatusConflict,
			detail: "kubernetes workload does not support rolling restart: Deployment 已暂停",
		},
		{
			name:   "restart in progress",
			err:    fmt.Errorf("%w: 滚动重启尚未完成", k8sops.ErrRestartInProgress),
			status: http.StatusConflict,
			detail: "kubernetes workload restart already in progress: 滚动重启尚未完成",
		},
		{
			name:   "forbidden",
			err:    k8sops.ErrForbidden,
			status: http.StatusBadGateway,
			detail: "Kubernetes 拒绝操作，请检查 RBAC 清单",
		},
		{
			name:   "other",
			err:    errors.New("apiserver timeout"),
			status: http.StatusBadGateway,
			detail: "重启请求失败：apiserver timeout",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runtime := &fakeKubernetesRuntime{configured: true, restartErr: testCase.err}
			handler := newKubernetesTestServer(t, runtime).Handler()
			response := performRequest(handler, testRequest{
				method: "POST", target: kubernetesRestartTarget("deployments", "alpha", "web"),
				remote: "10.0.0.9:5000", headers: kubernetesBearer(kubernetesAdminToken),
			})
			if response.Code != testCase.status {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
			if detail := decodeBody(t, response)["detail"]; detail != testCase.detail {
				t.Fatalf("detail = %v, want %q", detail, testCase.detail)
			}
		})
	}
}

func TestKubernetesReadErrorMapping(t *testing.T) {
	runtime := &fakeKubernetesRuntime{
		configured:   true,
		workloadsErr: errors.New("apiserver timeout"),
		podsErr:      k8sops.ErrForbidden,
	}
	handler := newKubernetesTestServer(t, runtime).Handler()
	for _, target := range []string{"/v1/admin/k8s/workloads", "/v1/admin/k8s/pods"} {
		response := performRequest(handler, testRequest{
			method: "GET", target: target, remote: "10.0.0.9:5000",
			headers: kubernetesBearer(kubernetesAdminToken),
		})
		if response.Code != http.StatusBadGateway {
			t.Fatalf("%s status = %d body = %s", target, response.Code, response.Body.String())
		}
		if detail := decodeBody(t, response)["detail"]; detail != "Kubernetes 数据读取失败" {
			t.Fatalf("%s detail = %v", target, detail)
		}
	}
}

func TestKubernetesRestartValidation(t *testing.T) {
	runtime := &fakeKubernetesRuntime{
		configured: true,
		restart:    &k8sops.RestartState{Status: k8sops.RestartSubmitted},
	}
	handler := newKubernetesTestServer(t, runtime).Handler()
	cases := []struct {
		name   string
		target string
		detail string
	}{
		{"unsupported resource", kubernetesRestartTarget("pods", "alpha", "web"), "不支持的工作负载类型"},
		{"invalid namespace", kubernetesRestartTarget("deployments", "bad%20namespace", "web"), "Invalid RAG namespace."},
		{"name too long", kubernetesRestartTarget("deployments", "alpha", strings.Repeat("a", 254)), "工作负载名称不合法"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := performRequest(handler, testRequest{
				method: "POST", target: testCase.target, remote: "10.0.0.9:5000",
				headers: kubernetesBearer(kubernetesAdminToken),
			})
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
			if detail := decodeBody(t, response)["detail"]; detail != testCase.detail {
				t.Fatalf("detail = %v, want %q", detail, testCase.detail)
			}
		})
	}
	if len(runtime.restartCalls) != 0 {
		t.Fatalf("invalid restarts reached the runtime: %+v", runtime.restartCalls)
	}
}

func TestKubernetesReadFilterValidation(t *testing.T) {
	runtime := &fakeKubernetesRuntime{configured: true}
	handler := newKubernetesTestServer(t, runtime).Handler()
	cases := []struct {
		name   string
		target string
	}{
		{"invalid namespace", "/v1/admin/k8s/workloads?namespace=bad%20namespace"},
		{"name filter too long", "/v1/admin/k8s/pods?name=" + strings.Repeat("a", 129)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := performRequest(handler, testRequest{
				method: "GET", target: testCase.target, remote: "10.0.0.9:5000",
				headers: kubernetesBearer(kubernetesAdminToken),
			})
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
		})
	}
	if len(runtime.workloadFilters) != 0 || len(runtime.podFilters) != 0 {
		t.Fatalf("invalid filters reached the runtime")
	}
}
