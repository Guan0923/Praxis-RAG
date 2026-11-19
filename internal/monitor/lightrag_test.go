package monitor

// This file tests the optional LightRAG knowledge-graph dependency probe: an
// unconfigured sidecar must not degrade the snapshot, a healthy sidecar must
// surface its version and upstream pin, and an unreachable or misconfigured
// sidecar must report a safe error without panicking or echoing the configured
// address, its credentials or the shared token.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// lightragHealthyServer serves a healthy sidecar /healthz payload whose
// installed upstream version matches the pinned one.
func lightragHealthyServer(t *testing.T) *httptest.Server {
	t.Helper()
	return jsonServer(t, "/healthz", http.StatusOK,
		`{"status":"ok","service":"praxis-lightrag-service","version":"0.1.0",`+
			`"upstream":{"package":"lightrag-hku","pinned":"1.5.7","installed":"1.5.7"},`+
			`"workspaces":2,"config_error":null}`)
}

// lightragProbeOptions builds a healthy snapshot whose only configured sidecar
// is the LightRAG service at url, including the shared token the client would
// send to the authenticated endpoints.
func lightragProbeOptions(t *testing.T, url string) Options {
	t.Helper()
	options := healthyDependencyOptions(t)
	options.LightRAGServiceURL = url
	options.LightRAGServiceToken = "monitor-graph-secret-token"
	options.LightRAGServiceTimeout = 5 * time.Second
	return options
}

// decodeDependencyRow renders one dependency row back into its wire document so
// the assertions cover the JSON contract, not only the Go struct.
func decodeDependencyRow(t *testing.T, item dependency) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("json.Marshal(dependency) error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("json.Unmarshal(dependency) error = %v", err)
	}
	return document
}

// TestMonitorLightRAGUnconfigured pins the optional default: no sidecar URL
// reports disabled, keeps the row shape of the other dependencies, omits the
// additive upstream block and leaves the overall verdict untouched.
func TestMonitorLightRAGUnconfigured(t *testing.T) {
	overview := New(healthyDependencyOptions(t)).MonitorOverview(context.Background())
	item := dependencyByName(t, overview, "lightrag")
	if item.Status != statusDisabled || !strings.Contains(item.Detail, "未配置 LightRAG") {
		t.Fatalf("lightrag = %+v", item)
	}
	if item.Upstream != nil {
		t.Fatalf("lightrag.upstream = %+v, want nil when unconfigured", item.Upstream)
	}
	if overview["status"] != statusOK || overview["ok"] != true {
		t.Fatalf("status/ok = %v/%v, want ok/true", overview["status"], overview["ok"])
	}
	row := decodeDependencyRow(t, item)
	for _, key := range []string{"name", "label", "status", "latency_ms", "detail"} {
		if _, present := row[key]; !present {
			t.Fatalf("lightrag row misses key %q: %v", key, row)
		}
	}
	if _, present := row["upstream"]; present {
		t.Fatalf("lightrag row should omit upstream when unconfigured: %v", row)
	}
}

// TestMonitorLightRAGHealthy asserts a reachable sidecar reports ok with its
// build version, workspace count and matching upstream pin, and that the shared
// token never reaches the row.
func TestMonitorLightRAGHealthy(t *testing.T) {
	server := lightragHealthyServer(t)
	overview := New(lightragProbeOptions(t, server.URL)).MonitorOverview(context.Background())
	item := dependencyByName(t, overview, "lightrag")
	if item.Status != statusOK {
		t.Fatalf("lightrag = %+v", item)
	}
	for _, want := range []string{"工作区 2 个", "版本 0.1.0", "lightrag-hku 1.5.7（与 pin 一致）"} {
		if !strings.Contains(item.Detail, want) {
			t.Fatalf("lightrag detail = %q, want %q", item.Detail, want)
		}
	}
	if strings.Contains(item.Detail, "monitor-graph-secret-token") {
		t.Fatalf("lightrag detail leaks the shared token: %q", item.Detail)
	}
	if item.Upstream == nil || item.Upstream.Package != "lightrag-hku" ||
		item.Upstream.Expected != "1.5.7" || item.Upstream.Installed != "1.5.7" ||
		item.Upstream.Drift {
		t.Fatalf("lightrag.upstream = %+v", item.Upstream)
	}
	row := decodeDependencyRow(t, item)
	upstream, ok := row["upstream"].(map[string]any)
	if !ok {
		t.Fatalf("lightrag.upstream has type %T", row["upstream"])
	}
	requireKeys(t, "lightrag.upstream", upstream, "package", "expected", "installed", "drift")
	if upstream["drift"] != false {
		t.Fatalf("lightrag.upstream.drift = %v", upstream["drift"])
	}
	if overview["status"] != statusOK || overview["ok"] != true {
		t.Fatalf("status/ok = %v/%v, want ok/true", overview["status"], overview["ok"])
	}
}

// TestMonitorLightRAGUpstreamDrift asserts a stale image degrades the row and
// exposes both versions, so an operator sees the drift instead of a silent ok.
func TestMonitorLightRAGUpstreamDrift(t *testing.T) {
	server := jsonServer(t, "/healthz", http.StatusOK,
		`{"status":"ok","service":"praxis-lightrag-service","version":"0.1.0",`+
			`"upstream":{"package":"lightrag-hku","pinned":"1.5.7","installed":"1.5.6"},`+
			`"workspaces":1,"config_error":null}`)
	overview := New(lightragProbeOptions(t, server.URL)).MonitorOverview(context.Background())
	item := dependencyByName(t, overview, "lightrag")
	if item.Status != statusDegraded ||
		!strings.Contains(item.Detail, "上游版本漂移") ||
		!strings.Contains(item.Detail, "期望 1.5.7") ||
		!strings.Contains(item.Detail, "实际 1.5.6") {
		t.Fatalf("lightrag = %+v", item)
	}
	if item.Upstream == nil || !item.Upstream.Drift {
		t.Fatalf("lightrag.upstream = %+v, want drift", item.Upstream)
	}
	if overview["status"] != statusDegraded || overview["ok"] != false {
		t.Fatalf("status/ok = %v/%v, want degraded/false", overview["status"], overview["ok"])
	}
}

// TestMonitorLightRAGMissingUpstream treats an uninstalled upstream package as
// unknown drift rather than a version mismatch.
func TestMonitorLightRAGMissingUpstream(t *testing.T) {
	server := jsonServer(t, "/healthz", http.StatusOK,
		`{"status":"ok","service":"praxis-lightrag-service","version":"0.1.0",`+
			`"upstream":{"package":"lightrag-hku","pinned":"1.5.7","installed":""},`+
			`"workspaces":0,"config_error":"LightRAG 未安装"}`)
	item := dependencyByName(t, New(lightragProbeOptions(t, server.URL)).
		MonitorOverview(context.Background()), "lightrag")
	if item.Status != statusDegraded || !strings.Contains(item.Detail, "LightRAG 未安装") {
		t.Fatalf("lightrag = %+v", item)
	}
	if item.Upstream == nil || item.Upstream.Drift || item.Upstream.Installed != "" {
		t.Fatalf("lightrag.upstream = %+v", item.Upstream)
	}
}

// TestMonitorLightRAGConfigError asserts a reachable sidecar with an unusable
// configuration degrades instead of reporting ok, like the pdf-service probe.
func TestMonitorLightRAGConfigError(t *testing.T) {
	server := jsonServer(t, "/healthz", http.StatusOK,
		`{"status":"ok","service":"praxis-lightrag-service","version":"0.1.0",`+
			`"upstream":{"package":"lightrag-hku","pinned":"1.5.7","installed":"1.5.7"},`+
			`"workspaces":0,"config_error":"视觉模型端点未配置"}`)
	overview := New(lightragProbeOptions(t, server.URL)).MonitorOverview(context.Background())
	item := dependencyByName(t, overview, "lightrag")
	if item.Status != statusDegraded ||
		!strings.Contains(item.Detail, "配置不可用") ||
		!strings.Contains(item.Detail, "视觉模型端点未配置") {
		t.Fatalf("lightrag = %+v", item)
	}
	if overview["status"] != statusDegraded {
		t.Fatalf("status = %v, want degraded", overview["status"])
	}
}

// TestMonitorLightRAGUnreachable asserts a configured but unreachable sidecar
// reports an error without panicking, and that neither the configured address
// nor the shared token is echoed in the row or the errors array.
func TestMonitorLightRAGUnreachable(t *testing.T) {
	address := unreachableAddress(t)
	overview := New(lightragProbeOptions(t, "http://"+address)).MonitorOverview(context.Background())
	item := dependencyByName(t, overview, "lightrag")
	if item.Status != statusError || !strings.Contains(item.Detail, "LightRAG 服务不可用") {
		t.Fatalf("lightrag = %+v", item)
	}
	if strings.Contains(item.Detail, address) {
		t.Fatalf("lightrag detail leaks the configured address: %q", item.Detail)
	}
	if strings.Contains(item.Detail, "monitor-graph-secret-token") {
		t.Fatalf("lightrag detail leaks the shared token: %q", item.Detail)
	}
	if overview["status"] != statusError {
		t.Fatalf("status = %v, want error", overview["status"])
	}
	if errorsList, _ := overview["errors"].([]string); len(errorsList) != 0 {
		t.Fatalf("errors = %v, want the dependency row to carry the failure", errorsList)
	}
}

// TestMonitorLightRAGHTTPFailure asserts a non-200 health reply is reported as
// unavailable with the status code, not as a healthy dependency.
func TestMonitorLightRAGHTTPFailure(t *testing.T) {
	server := jsonServer(t, "/healthz", http.StatusServiceUnavailable,
		`{"detail":"sidecar starting"}`)
	item := dependencyByName(t, New(lightragProbeOptions(t, server.URL)).
		MonitorOverview(context.Background()), "lightrag")
	if item.Status != statusError || !strings.Contains(item.Detail, "HTTP 503") {
		t.Fatalf("lightrag = %+v", item)
	}
}

// TestMonitorLightRAGInvalidAddress asserts a malformed or credential-bearing
// URL is rejected with a safe message that never echoes the value itself.
func TestMonitorLightRAGInvalidAddress(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{name: "内嵌凭据", url: "http://user:password@127.0.0.1:8093"},
		{name: "协议非法", url: "ftp://127.0.0.1:8093"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			options := healthyDependencyOptions(t)
			options.LightRAGServiceURL = testCase.url
			overview := New(options).MonitorOverview(context.Background())
			item := dependencyByName(t, overview, "lightrag")
			if item.Status != statusError || !strings.Contains(item.Detail, "地址无效") {
				t.Fatalf("lightrag = %+v", item)
			}
			if strings.Contains(item.Detail, testCase.url) ||
				strings.Contains(item.Detail, "password") {
				t.Fatalf("lightrag detail leaks the configured value: %q", item.Detail)
			}
			if overview["status"] != statusError {
				t.Fatalf("status = %v, want error", overview["status"])
			}
		})
	}
}

// TestMonitorLightRAGStatusVerdicts pins the health-status mapping branches:
// a "degraded" sidecar downgrades the row while still rendering the workspace
// detail, an empty status is reported as its own degraded verdict, and an
// unknown status escalates the row to error.
func TestMonitorLightRAGStatusVerdicts(t *testing.T) {
	cases := []struct {
		name       string
		payload    string
		wantStatus string
		wantDetail string
	}{
		{
			name: "degraded",
			payload: `{"status":"degraded","service":"praxis-lightrag-service","version":"0.1.0",` +
				`"upstream":null,"workspaces":2,"config_error":null}`,
			wantStatus: statusDegraded,
			wantDetail: "工作区 2 个",
		},
		{
			name: "empty status",
			payload: `{"status":"","service":"praxis-lightrag-service","version":"0.1.0",` +
				`"upstream":null,"workspaces":0,"config_error":null}`,
			wantStatus: statusDegraded,
			wantDetail: "健康检查未返回状态",
		},
		{
			name: "unknown status",
			payload: `{"status":"melting","service":"praxis-lightrag-service","version":"0.1.0",` +
				`"upstream":null,"workspaces":0,"config_error":null}`,
			wantStatus: statusError,
			wantDetail: "LightRAG 服务状态为 melting",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := jsonServer(t, "/healthz", http.StatusOK, testCase.payload)
			overview := New(lightragProbeOptions(t, server.URL)).MonitorOverview(context.Background())
			item := dependencyByName(t, overview, "lightrag")
			if item.Status != testCase.wantStatus {
				t.Fatalf("lightrag status = %q, want %q (%+v)", item.Status, testCase.wantStatus, item)
			}
			if !strings.Contains(item.Detail, testCase.wantDetail) {
				t.Fatalf("lightrag detail = %q, want %q", item.Detail, testCase.wantDetail)
			}
		})
	}
}
