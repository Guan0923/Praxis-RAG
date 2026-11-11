package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/auth"
)

// fakeMonitorRuntime records the transport contract and returns a fixed payload.
type fakeMonitorRuntime struct {
	payload  map[string]any
	calls    int
	deadline time.Duration
}

func (f *fakeMonitorRuntime) MonitorOverview(ctx context.Context) map[string]any {
	f.calls++
	if deadline, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(deadline)
	}
	return f.payload
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
