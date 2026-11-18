package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Guan0923/Praxis-RAG/internal/catalog"
)

// TestModernProtocolHeadersAreValidated ports
// test_modern_protocol_headers_are_validated: the SDK enforces SEP-2575 header
// agreement, and the dispatcher must route the request to the stateless
// transport so the rejection is a 400 instead of a session error.
func TestModernProtocolHeadersAreValidated(t *testing.T) {
	_, server := newTestHTTPHandler(t, newFakeRuntime(), Options{})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientCapabilities":{}}}}`
	send := func(overrides map[string]string) (int, string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, server.URL+"/mcp", strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Authorization", "Bearer "+testToken)
		request.Header.Set("X-RAG-Namespace", manualsNS)
		request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		request.Header.Set("Mcp-Method", "tools/list")
		for name, value := range overrides {
			request.Header.Set(name, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer response.Body.Close()
		payload, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(payload)
	}
	if status, payload := send(nil); status != http.StatusOK {
		t.Fatalf("valid modern request status = %d, want 200: %s", status, payload)
	}
	if status, payload := send(map[string]string{"Mcp-Protocol-Version": "2099-01-01"}); status != http.StatusBadRequest {
		t.Fatalf("mismatched protocol version status = %d, want 400: %s", status, payload)
	}
	if status, payload := send(map[string]string{"Mcp-Method": "tools/call"}); status != http.StatusBadRequest {
		t.Fatalf("mismatched Mcp-Method status = %d, want 400: %s", status, payload)
	}
}

func TestModernPromptPagination(t *testing.T) {
	runtime := newFakeRuntime()
	endpoint, server := newTestHTTPHandler(t, runtime, Options{})
	store := endpoint.opts.Catalog.(*catalog.MemoryStore)
	namespaceCatalog, err := catalog.NewNamespaceCatalog(store, manualsNS)
	if err != nil {
		t.Fatalf("namespace catalog: %v", err)
	}
	for index := 0; index < 60; index++ {
		name := fmt.Sprintf("prompt-%02d", index)
		if _, err := namespaceCatalog.PutPrompt(context.Background(), name, catalog.PromptDefinition{
			Messages: []catalog.PromptMessage{{Role: "user", Text: "hello"}},
		}); err != nil {
			t.Fatalf("put prompt %s: %v", name, err)
		}
	}
	session := newModernClient(t, server, manualsNS)
	ctx := context.Background()

	first, err := session.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatalf("list prompts page 1: %v", err)
	}
	if len(first.Prompts) != listPageSize || first.NextCursor != "50" {
		t.Fatalf("page 1 = %d prompts, cursor %q", len(first.Prompts), first.NextCursor)
	}
	second, err := session.ListPrompts(ctx, &sdk.ListPromptsParams{Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("list prompts page 2: %v", err)
	}
	if len(second.Prompts) != 10 || second.NextCursor != "" {
		t.Fatalf("page 2 = %d prompts, cursor %q", len(second.Prompts), second.NextCursor)
	}
	if _, err := session.ListPrompts(ctx, &sdk.ListPromptsParams{Cursor: "abc"}); err == nil {
		t.Fatalf("invalid cursor must fail")
	}
	if _, err := session.ListPrompts(ctx, &sdk.ListPromptsParams{Cursor: "61"}); err == nil {
		t.Fatalf("out-of-range cursor must fail")
	}
}

func TestModernToolValidationErrors(t *testing.T) {
	runtime := newFakeRuntime()
	_, server := newTestHTTPHandler(t, runtime, Options{})
	session := newModernClient(t, server, manualsNS)
	ctx := context.Background()
	cases := []struct {
		name      string
		arguments map[string]any
		want      string
	}{
		{"removed wait argument", map[string]any{"query": "x", "wait_seconds": -1}, "Unknown search arguments: wait_seconds"},
		{"unknown search argument", map[string]any{"query": "x", "namespace": "other"}, "Unknown search arguments: namespace"},
		{"unknown document argument", map[string]any{"name": "a.txt", "content": "x", "path": "C:/x"}, "Unknown document arguments"},
		{"bad artifact uuid", map[string]any{"artifact_id": "nope"}, "artifact_id must be a canonical UUID"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := session.CallTool(ctx, &sdk.CallToolParams{
				Name: "search", Arguments: testCase.arguments,
			})
			if err != nil {
				t.Fatalf("call %s: %v", testCase.name, err)
			}
			if !result.IsError {
				t.Fatalf("%s must be a tool error", testCase.name)
			}
		})
	}

	// Validation runs before retrieval: nothing reached the runtime yet.
	if len(runtime.searchCalls) != 0 {
		t.Fatalf("invalid arguments must not reach the runtime: %#v", runtime.searchCalls)
	}

	// A valid call runs retrieval and returns the evidence payload directly.
	runtime.searchPayload = map[string]any{
		"query": "x", "evidence_status": "found", "results": []any{},
	}
	synced, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "search", Arguments: map[string]any{"query": "x"},
	})
	if err != nil || synced.IsError {
		t.Fatalf("synchronous search failed: %v %#v", err, synced)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(synced.Content[0].(*sdk.TextContent).Text), &payload); err != nil {
		t.Fatalf("decode search payload: %v", err)
	}
	if payload["evidence_status"] != "found" {
		t.Fatalf("search payload = %#v", payload)
	}
	if _, queued := payload["result_id"]; queued {
		t.Fatalf("search must not return a result id: %#v", payload)
	}
	if len(runtime.searchCalls) != 1 {
		t.Fatalf("search calls = %d, want 1", len(runtime.searchCalls))
	}
}

func TestModernToolListChangedNotification(t *testing.T) {
	runtime := newFakeRuntime()
	endpoint := newTestEndpoint(t, runtime, Options{})
	events := make(chan struct{}, 4)
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, &sdk.ClientOptions{
		ToolListChangedHandler: func(context.Context, *sdk.ToolListChangedRequest) {
			select {
			case events <- struct{}{}:
			default:
			}
		},
	})
	server := httptest.NewServer(newTestHandlerFor(t, endpoint))
	t.Cleanup(server.Close)
	transport := &sdk.StreamableClientTransport{
		Endpoint: server.URL + "/mcp",
		HTTPClient: &http.Client{
			Transport: &headerTransport{headers: map[string]string{
				"Authorization":   "Bearer " + testToken,
				"X-RAG-Namespace": manualsNS,
			}},
			Timeout: 15 * time.Second,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	endpoint.ToolsChanged(manualsNS)
	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for notifications/tools/list_changed")
	}
}

// newTestHandlerFor wires an endpoint into the /mcp handler without an extra
// httptest server, for tests that need a live notification stream.
func newTestHandlerFor(t *testing.T, endpoint *Endpoint) http.Handler {
	t.Helper()
	handler, err := NewHTTPHandler(HTTPOptions{
		Endpoint: endpoint,
		NamespaceFromRequest: func(request *http.Request) (string, error) {
			namespace := request.Header.Get("X-RAG-Namespace")
			if namespace == "" {
				return "", ErrUnauthorized
			}
			return namespace, nil
		},
		AllowedHosts:   []string{"127.0.0.1", "127.0.0.1:*", "localhost", "localhost:*"},
		AllowedOrigins: []string{"http://127.0.0.1:*", "http://localhost:*"},
	})
	if err != nil {
		t.Fatalf("NewHTTPHandler: %v", err)
	}
	return handler
}
