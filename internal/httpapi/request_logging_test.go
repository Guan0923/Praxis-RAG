package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func requestLogTestServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	var buffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	status := fakeStatus{payload: map[string]any{"status": "ok"}}
	return New(testSettings("test-token"), status, nil, logger), &buffer
}

func TestAccessLogReusesValidRequestID(t *testing.T) {
	server, logs := requestLogTestServer(t)
	response := performRequest(server.Handler(), testRequest{
		method:  "GET",
		target:  "/health",
		headers: map[string]string{requestIDHeader: "proxy-abc.123"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get(requestIDHeader); got != "proxy-abc.123" {
		t.Fatalf("response %s = %q, want proxy-abc.123", requestIDHeader, got)
	}
	if !strings.Contains(logs.String(), "request_id=proxy-abc.123") {
		t.Fatalf("access log does not carry the inbound request id: %q", logs.String())
	}
}

func TestAccessLogGeneratesRequestID(t *testing.T) {
	generatedPattern := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for name, inbound := range map[string]string{
		"missing": "",
		"invalid": "bad id!",
	} {
		t.Run(name, func(t *testing.T) {
			server, logs := requestLogTestServer(t)
			headers := map[string]string{}
			if inbound != "" {
				headers[requestIDHeader] = inbound
			}
			response := performRequest(server.Handler(), testRequest{
				method: "GET", target: "/health", headers: headers,
			})
			requestID := response.Header().Get(requestIDHeader)
			if !generatedPattern.MatchString(requestID) {
				t.Fatalf("generated %s = %q, want 32 lowercase hex chars", requestIDHeader, requestID)
			}
			if !strings.Contains(logs.String(), "request_id="+requestID) {
				t.Fatalf("access log %q does not carry generated id %q", logs.String(), requestID)
			}
		})
	}
}

func TestAccessLogRecordsNamespaceForTenantRoutes(t *testing.T) {
	server, logs := requestLogTestServer(t)
	response := performRequest(server.Handler(), testRequest{
		method:  "GET",
		target:  "/v1/chunking/strategies",
		headers: tenantAuth(),
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(logs.String(), "namespace=alpha") {
		t.Fatalf("access log does not carry the tenant namespace: %q", logs.String())
	}
}
