package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/auth"
	"github.com/Guan0923/xinghe_rag/internal/logging"
	"github.com/Guan0923/xinghe_rag/internal/monitor"
)

// fakeMonitorRuntime records the transport contract and returns fixed payloads.
type fakeMonitorRuntime struct {
	payload     map[string]any
	calls       int
	deadline    time.Duration
	logs        *monitor.LogsView
	logsErr     error
	logQuery    monitor.LogQuery
	logCalls    int
	logDeadline time.Duration
}

func (f *fakeMonitorRuntime) MonitorOverview(ctx context.Context) map[string]any {
	f.calls++
	if deadline, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(deadline)
	}
	return f.payload
}

func (f *fakeMonitorRuntime) MonitorLogs(ctx context.Context, query monitor.LogQuery) (*monitor.LogsView, error) {
	f.logCalls++
	f.logQuery = query
	if deadline, ok := ctx.Deadline(); ok {
		f.logDeadline = time.Until(deadline)
	}
	return f.logs, f.logsErr
}

func newMonitorServer(t *testing.T, runtime MonitorRuntime) http.Handler {
	t.Helper()
	options := make([]Option, 0, 1)
	if runtime != nil {
		options = append(options, WithMonitorRuntime(runtime))
	}
	server := New(testSettings(""), nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), options...)
	return server.Handler()
}

// TestMonitorOverviewReturnsSnapshot asserts the admin snapshot response: 200,
// no-store caching, the runtime payload and the 5s runtime deadline.
func TestMonitorOverviewReturnsSnapshot(t *testing.T) {
	runtime := &fakeMonitorRuntime{payload: map[string]any{
		"generated_at": "2026-11-11T00:00:00Z",
		"ok":           true,
		"service":      map[string]any{"name": "praxis-rag", "version": "0.4.0"},
		"dependencies": []any{},
		"queues":       map[string]any{},
		"library":      map[string]any{},
		"activity":     map[string]any{},
		"errors":       []any{},
	}}
	handler := newMonitorServer(t, runtime)

	response := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/monitor/overview", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	body := decodeBody(t, response)
	if body["ok"] != true || body["generated_at"] != "2026-11-11T00:00:00Z" {
		t.Fatalf("body = %v", body)
	}
	service, _ := body["service"].(map[string]any)
	if service["name"] != "praxis-rag" || service["version"] != "0.4.0" {
		t.Fatalf("service = %v", service)
	}
	if _, present := body["dependencies"]; !present {
		t.Fatalf("dependencies missing from %v", body)
	}
	if runtime.calls != 1 {
		t.Fatalf("runtime calls = %d", runtime.calls)
	}
	if runtime.deadline <= 0 || runtime.deadline > 5*time.Second {
		t.Fatalf("runtime deadline = %v, want a 5s bound", runtime.deadline)
	}
}

// TestMonitorOverviewUnavailable asserts the endpoint reports 503 before the
// monitoring runtime is wired (e.g. unit tests or a trimmed-down binary).
func TestMonitorOverviewUnavailable(t *testing.T) {
	handler := newMonitorServer(t, nil)

	response := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/monitor/overview", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	body := decodeBody(t, response)
	if body["detail"] != "监控服务未启用" {
		t.Fatalf("detail = %v", body["detail"])
	}
}

// TestMonitorOverviewRequiresAdmin asserts the route keeps the admin boundary:
// a non-loopback request without an admin session is rejected before the
// runtime is called.
func TestMonitorOverviewRequiresAdmin(t *testing.T) {
	runtime := &fakeMonitorRuntime{payload: map[string]any{"ok": true}}
	handler := newMonitorServer(t, runtime)

	response := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/monitor/overview", remote: "10.1.2.3:5000",
	})
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if runtime.calls != 0 {
		t.Fatalf("runtime calls = %d, want 0", runtime.calls)
	}
}

// TestMonitorOverviewRequiresAdminSession asserts the monitoring snapshot is
// admin-only for logged-in accounts: a normal Console user is rejected with 403
// even from loopback, and an administrator session is accepted.
func TestMonitorOverviewRequiresAdminSession(t *testing.T) {
	settings := testSettings("")
	settings.AuthEnabled = true
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		"admin-token": adminSession("admin-token"),
		"user-token":  userSession("user-token"),
	}}
	runtime := &fakeMonitorRuntime{payload: map[string]any{"ok": true}}
	handler := New(settings, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMonitorRuntime(runtime), WithUserAuth(users)).Handler()

	forbidden := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/monitor/overview", remote: "127.0.0.1:5000",
		headers: cookieFor("user-token"),
	})
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("user status = %d body = %s", forbidden.Code, forbidden.Body.String())
	}
	if detail := decodeBody(t, forbidden)["detail"]; detail != "需要管理员权限" {
		t.Fatalf("user detail = %v", detail)
	}
	if runtime.calls != 0 {
		t.Fatalf("non-admin reached the runtime: calls = %d", runtime.calls)
	}

	admin := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/monitor/overview", remote: "127.0.0.1:5000",
		headers: cookieFor("admin-token"),
	})
	if admin.Code != http.StatusOK {
		t.Fatalf("admin status = %d body = %s", admin.Code, admin.Body.String())
	}
}

// TestMonitorLogsReturnsPage asserts the log transport contract: admin route,
// no-store caching, parsed query parameters and the runtime 5s deadline.
func TestMonitorLogsReturnsPage(t *testing.T) {
	runtime := &fakeMonitorRuntime{logs: &monitor.LogsView{
		GeneratedAt: "2026-11-14T06:00:00Z",
		Source:      "file",
		Available:   true,
		Entries: []logging.Entry{{
			Seq: 7, Time: "2026-11-14T05:59:59Z", Level: "ERROR",
			Component: "mysql", ComponentLabel: "MySQL",
			Message: "mysql down", Attrs: map[string]string{"request_id": "abc"},
			Source: "internal/state/store.go:431",
		}},
		Total:      1,
		Components: []logging.ComponentCount{{ID: "mysql", Label: "MySQL", Count: 1}},
		File:       logging.FileInfo{Configured: true, Path: "C:/logs/praxis.jsonl", SizeBytes: 42},
		Notes:      []string{},
	}}
	handler := newMonitorServer(t, runtime)

	response := performRequest(handler, testRequest{
		method: "GET",
		target: "/v1/admin/monitor/logs?source=file&component=mysql&level=warn&query=down&limit=20&offset=5",
		remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	body := decodeBody(t, response)
	if body["source"] != "file" || body["available"] != true || body["total"] != 1.0 {
		t.Fatalf("body = %v", body)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %v", body["entries"])
	}
	entry, _ := entries[0].(map[string]any)
	if entry["component"] != "mysql" || entry["component_label"] != "MySQL" ||
		entry["level"] != "ERROR" || entry["source"] != "internal/state/store.go:431" {
		t.Fatalf("entry = %v", entry)
	}
	if runtime.logCalls != 1 {
		t.Fatalf("runtime log calls = %d", runtime.logCalls)
	}
	want := monitor.LogQuery{Source: "file", Component: "mysql", Level: "warn", Query: "down", Limit: 20, Offset: 5}
	if runtime.logQuery != want {
		t.Fatalf("query = %+v, want %+v", runtime.logQuery, want)
	}
	if runtime.logDeadline <= 0 || runtime.logDeadline > 5*time.Second {
		t.Fatalf("runtime deadline = %v, want a 5s bound", runtime.logDeadline)
	}
}

// TestMonitorLogsDefaultsOmittedParameters asserts the HTTP layer leaves the
// defaults to the monitor service (source memory, limit/offset unset).
func TestMonitorLogsDefaultsOmittedParameters(t *testing.T) {
	runtime := &fakeMonitorRuntime{logs: &monitor.LogsView{
		GeneratedAt: "2026-11-14T06:00:00Z",
		Source:      "memory",
		Entries:     []logging.Entry{},
		Components:  []logging.ComponentCount{},
		Notes:       []string{},
	}}
	handler := newMonitorServer(t, runtime)
	response := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/monitor/logs", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if runtime.logQuery.Source != "" || runtime.logQuery.Limit != 0 || runtime.logQuery.Offset != 0 {
		t.Fatalf("query = %+v, want the zero-value defaults", runtime.logQuery)
	}
}

// TestMonitorLogsInvalidQueryMaps400 exercises the real monitor validation
// through the transport: the sentinel becomes a 400 with the Chinese reason.
func TestMonitorLogsInvalidQueryMaps400(t *testing.T) {
	service := monitor.New(monitor.Options{})
	handler := newMonitorServer(t, service)

	for target, wantDetail := range map[string]string{
		"/v1/admin/monitor/logs?component=nope": "未知日志组件: nope",
		"/v1/admin/monitor/logs?source=socket":  "未知日志来源: socket",
		"/v1/admin/monitor/logs?level=verbose":  "未知日志级别: verbose",
		"/v1/admin/monitor/logs?offset=100001":  "偏移量超出范围（最大 100000）",
	} {
		response := performRequest(handler, testRequest{
			method: "GET", target: target, remote: "127.0.0.1:5000",
		})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d body = %s", target, response.Code, response.Body.String())
		}
		if detail := decodeBody(t, response)["detail"]; detail != wantDetail {
			t.Fatalf("%s detail = %v, want %q", target, detail, wantDetail)
		}
	}
}

// TestMonitorLogsNonIntegerPagination asserts non-numeric pagination values are
// rejected before the runtime is called.
func TestMonitorLogsNonIntegerPagination(t *testing.T) {
	runtime := &fakeMonitorRuntime{}
	handler := newMonitorServer(t, runtime)

	for target, wantDetail := range map[string]string{
		"/v1/admin/monitor/logs?limit=abc":  "limit 必须是整数",
		"/v1/admin/monitor/logs?offset=1.5": "offset 必须是整数",
	} {
		response := performRequest(handler, testRequest{
			method: "GET", target: target, remote: "127.0.0.1:5000",
		})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d body = %s", target, response.Code, response.Body.String())
		}
		if detail := decodeBody(t, response)["detail"]; detail != wantDetail {
			t.Fatalf("%s detail = %v, want %q", target, detail, wantDetail)
		}
	}
	if runtime.logCalls != 0 {
		t.Fatalf("runtime log calls = %d, want 0", runtime.logCalls)
	}
}

// TestMonitorLogsUnavailable asserts the endpoint reports 503 before the
// monitoring runtime is wired.
func TestMonitorLogsUnavailable(t *testing.T) {
	handler := newMonitorServer(t, nil)
	response := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/monitor/logs", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if detail := decodeBody(t, response)["detail"]; detail != "监控服务未启用" {
		t.Fatalf("detail = %v", detail)
	}
}

// TestMonitorLogsRequiresAdmin asserts the log endpoint keeps the admin
// boundary: a non-admin session and a non-loopback request are rejected before
// the runtime is called.
func TestMonitorLogsRequiresAdmin(t *testing.T) {
	settings := testSettings("")
	settings.AuthEnabled = true
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		"admin-token": adminSession("admin-token"),
		"user-token":  userSession("user-token"),
	}}
	runtime := &fakeMonitorRuntime{logs: &monitor.LogsView{
		GeneratedAt: "2026-11-14T06:00:00Z",
		Source:      "memory",
		Entries:     []logging.Entry{},
		Components:  []logging.ComponentCount{},
		Notes:       []string{},
	}}
	handler := New(settings, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMonitorRuntime(runtime), WithUserAuth(users)).Handler()

	anonymous := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/monitor/logs", remote: "10.1.2.3:5000",
	})
	if anonymous.Code != http.StatusForbidden {
		t.Fatalf("anonymous status = %d body = %s", anonymous.Code, anonymous.Body.String())
	}
	normalUser := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/monitor/logs", remote: "127.0.0.1:5000",
		headers: cookieFor("user-token"),
	})
	if normalUser.Code != http.StatusForbidden {
		t.Fatalf("user status = %d body = %s", normalUser.Code, normalUser.Body.String())
	}
	if runtime.logCalls != 0 {
		t.Fatalf("non-admin reached the runtime: calls = %d", runtime.logCalls)
	}
	admin := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/monitor/logs", remote: "127.0.0.1:5000",
		headers: cookieFor("admin-token"),
	})
	if admin.Code != http.StatusOK {
		t.Fatalf("admin status = %d body = %s", admin.Code, admin.Body.String())
	}
}
