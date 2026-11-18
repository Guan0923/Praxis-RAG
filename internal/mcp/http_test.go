package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Guan0923/Praxis-RAG/internal/catalog"
)

const (
	testToken = "test-token"
	manualsNS = "manuals"
	otherNS   = "other"
)

// newTestHTTPHandler builds an Endpoint plus the dual-era /mcp handler with the
// local allowlists the Python protocol tests use.
func newTestHTTPHandler(t *testing.T, runtime Runtime, options Options) (*Endpoint, *httptest.Server) {
	t.Helper()
	endpoint := newTestEndpoint(t, runtime, options)
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
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return endpoint, server
}

// rawRPC is a minimal legacy-protocol Streamable HTTP JSON-RPC client. The SDK
// client always negotiates the modern protocol, so the legacy era is exercised
// over raw HTTP like the Python parity tests do.
type rawRPC struct {
	t               *testing.T
	baseURL         string
	namespace       string
	sessionID       string
	protocolVersion string
	nextID          int
	client          *http.Client
}

func newRawRPC(t *testing.T, baseURL, namespace string) *rawRPC {
	return &rawRPC{
		t:         t,
		baseURL:   baseURL,
		namespace: namespace,
		client:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (r *rawRPC) do(payload map[string]any, headers map[string]string) (*http.Response, map[string]any) {
	r.t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		r.t.Fatalf("marshal request: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, r.baseURL+"/mcp", bytes.NewReader(encoded))
	if err != nil {
		r.t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("X-RAG-Namespace", r.namespace)
	if r.sessionID != "" {
		request.Header.Set("Mcp-Session-Id", r.sessionID)
	}
	if r.protocolVersion != "" {
		request.Header.Set("Mcp-Protocol-Version", r.protocolVersion)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := r.client.Do(request)
	if err != nil {
		r.t.Fatalf("do request: %v", err)
	}
	return response, decodeJSONRPCResponse(r.t, response)
}

func (r *rawRPC) call(method string, params map[string]any) (map[string]any, *http.Response) {
	r.t.Helper()
	r.nextID++
	body := map[string]any{"jsonrpc": "2.0", "id": r.nextID, "method": method}
	if params != nil {
		body["params"] = params
	}
	response, payload := r.do(body, nil)
	return payload, response
}

func (r *rawRPC) notify(method string) *http.Response {
	r.t.Helper()
	response, _ := r.do(map[string]any{"jsonrpc": "2.0", "method": method}, nil)
	return response
}

func (r *rawRPC) initialize() map[string]any {
	r.t.Helper()
	payload, response := r.call("initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	})
	r.sessionID = response.Header.Get("Mcp-Session-Id")
	r.protocolVersion = "2025-11-25"
	if r.sessionID == "" {
		r.t.Fatalf("initialize did not set Mcp-Session-Id")
	}
	if response.StatusCode != http.StatusOK {
		r.t.Fatalf("initialize status = %d", response.StatusCode)
	}
	initialized := r.notify("notifications/initialized")
	if initialized.StatusCode != http.StatusAccepted {
		r.t.Fatalf("initialized status = %d, want 202", initialized.StatusCode)
	}
	return payload
}

// decodeJSONRPCResponse decodes either a JSON poll response or the single SSE
// event that carries the JSON-RPC response.
func decodeJSONRPCResponse(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	// Transport-level rejections (401/403/404/413/...) are plain text.
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		return nil
	}
	if strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode json response %q: %v", body, err)
		}
		return payload
	}
	// text/event-stream: responses are one JSON object per data line.
	var last map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &payload); err == nil {
			last = payload
		}
	}
	if last == nil {
		t.Fatalf("no JSON-RPC payload in SSE response %q", body)
	}
	return last
}

// toolPayload decodes the first text content block of a tools/call result.
func toolPayload(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	result, ok := payload["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result in %#v", payload)
	}
	if isError, _ := result["isError"].(bool); isError {
		t.Fatalf("tool call unexpectedly failed: %#v", result)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("missing content in %#v", result)
	}
	text, ok := content[0].(map[string]any)["text"].(string)
	if !ok {
		t.Fatalf("missing text content in %#v", content[0])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("tool payload is not JSON: %v (%q)", err, text)
	}
	return decoded
}

// toolErrorText returns the error text of a tools/call result, failing when the
// result was not an error.
func toolErrorText(t *testing.T, payload map[string]any) string {
	t.Helper()
	result, ok := payload["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result in %#v", payload)
	}
	isError, _ := result["isError"].(bool)
	if !isError {
		t.Fatalf("tool call did not fail: %#v", result)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("error result has no content")
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text
}

func TestLegacyProtocolLifecycle(t *testing.T) {
	runtime := newFakeRuntime()
	runtime.searchPayload = map[string]any{
		"query":           "first",
		"namespace":       manualsNS,
		"evidence_status": "found",
		"results":         []any{},
	}
	_, server := newTestHTTPHandler(t, runtime, Options{})
	client := newRawRPC(t, server.URL, manualsNS)

	initialized := client.initialize()
	result := initialized["result"].(map[string]any)
	if protocol := result["protocolVersion"]; protocol != "2025-11-25" {
		t.Fatalf("protocolVersion = %v", protocol)
	}
	serverInfo := result["serverInfo"].(map[string]any)
	if serverInfo["name"] != "Praxis-RAG" || serverInfo["version"] != "0.4.0" {
		t.Fatalf("serverInfo = %#v", serverInfo)
	}
	capabilities := result["capabilities"].(map[string]any)
	toolsCapability := capabilities["tools"].(map[string]any)
	if toolsCapability["listChanged"] != true {
		t.Fatalf("tools capability = %#v", toolsCapability)
	}

	listed, _ := client.call("tools/list", nil)
	tools := listed["result"].(map[string]any)["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	want := []string{"search", "add_document", "update_document", "delete_document", "get_document_status"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	searchTool := tools[0].(map[string]any)
	searchProperties := searchTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := searchProperties["wait_seconds"]; ok {
		t.Fatalf("synchronous search must not expose wait_seconds")
	}
	searchAnnotations := searchTool["annotations"].(map[string]any)
	if searchAnnotations["readOnlyHint"] != true || searchAnnotations["idempotentHint"] != true ||
		searchAnnotations["destructiveHint"] != false || searchAnnotations["openWorldHint"] != false {
		t.Fatalf("search annotations = %#v", searchAnnotations)
	}
	addAnnotations := tools[1].(map[string]any)["annotations"].(map[string]any)
	if addAnnotations["readOnlyHint"] != false || addAnnotations["destructiveHint"] != false ||
		addAnnotations["idempotentHint"] != false || addAnnotations["openWorldHint"] != false {
		t.Fatalf("add_document annotations = %#v", addAnnotations)
	}
	updateAnnotations := tools[2].(map[string]any)["annotations"].(map[string]any)
	if updateAnnotations["destructiveHint"] != true || updateAnnotations["readOnlyHint"] != false {
		t.Fatalf("update_document annotations = %#v", updateAnnotations)
	}

	searched, _ := client.call("tools/call", map[string]any{
		"name": "search", "arguments": map[string]any{"query": "first"},
	})
	searchPayload := toolPayload(t, searched)
	if searchPayload["query"] != "first" || searchPayload["namespace"] != manualsNS {
		t.Fatalf("search payload = %#v", searchPayload)
	}

	invalid, _ := client.call("tools/call", map[string]any{"name": "search", "arguments": map[string]any{"query": ""}})
	if text := toolErrorText(t, invalid); text != "query must be a non-empty string" {
		t.Fatalf("validation error = %q", text)
	}

	missing, _ := client.call("tools/call", map[string]any{"name": "missing", "arguments": map[string]any{}})
	if text := toolErrorText(t, missing); text != "Unknown or disabled RAG tool: missing" {
		t.Fatalf("unknown tool error = %q", text)
	}

	// get_search_result was removed with the queued flow: it is an unknown tool
	// now, and search itself already returned the evidence above.
	removed, _ := client.call("tools/call", map[string]any{
		"name": "get_search_result", "arguments": map[string]any{"result_id": "123e4567-e89b-12d3-a456-426614174000"},
	})
	if text := toolErrorText(t, removed); text != "Unknown or disabled RAG tool: get_search_result" {
		t.Fatalf("get_search_result error = %q", text)
	}

	prompts, _ := client.call("prompts/list", nil)
	if listedPrompts := prompts["result"].(map[string]any)["prompts"].([]any); len(listedPrompts) != 0 {
		t.Fatalf("prompts = %#v", listedPrompts)
	}
}

func TestLegacySessionIsNamespaceBound(t *testing.T) {
	runtime := newFakeRuntime()
	_, server := newTestHTTPHandler(t, runtime, Options{})
	manuals := newRawRPC(t, server.URL, manualsNS)
	manuals.initialize()

	other := newRawRPC(t, server.URL, otherNS)
	other.sessionID = manuals.sessionID
	other.protocolVersion = "2025-11-25"
	_, response := other.call("tools/list", nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign session status = %d, want 404", response.StatusCode)
	}

	// The original namespace session must still work.
	listed, _ := manuals.call("tools/list", nil)
	if _, ok := listed["result"]; !ok {
		t.Fatalf("manuals session broke: %#v", listed)
	}
}

func newModernClient(t *testing.T, server *httptest.Server, namespace string) *sdk.ClientSession {
	t.Helper()
	return newModernClientWithOptions(t, server, namespace, nil)
}

func newModernClientWithOptions(t *testing.T, server *httptest.Server, namespace string, options *sdk.ClientOptions) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, options)
	transport := &sdk.StreamableClientTransport{
		Endpoint: server.URL + "/mcp",
		HTTPClient: &http.Client{
			Transport: &headerTransport{headers: map[string]string{
				"Authorization":   "Bearer " + testToken,
				"X-RAG-Namespace": namespace,
			}},
			Timeout: 15 * time.Second,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		cancel()
	})
	initialized := session.InitializeResult()
	if initialized == nil || initialized.ProtocolVersion != "2026-07-28" {
		t.Fatalf("modern client negotiated %#v", initialized)
	}
	return session
}

func TestModernProtocolLifecycle(t *testing.T) {
	runtime := newFakeRuntime()
	runtime.searchPayload = map[string]any{
		"query":           "modern",
		"namespace":       manualsNS,
		"evidence_status": "found",
		"results":         []any{},
	}
	endpoint, server := newTestHTTPHandler(t, runtime, Options{})
	session := newModernClient(t, server, manualsNS)
	ctx := context.Background()

	if info := session.InitializeResult().ServerInfo; info.Name != "Praxis-RAG" || info.Version != "0.4.0" {
		t.Fatalf("server info = %#v", info)
	}

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(listed.Tools) != 5 || listed.Tools[0].Name != "search" {
		t.Fatalf("tools = %#v", listed.Tools)
	}
	searchSchema := listed.Tools[0].InputSchema.(map[string]any)
	if _, ok := searchSchema["properties"].(map[string]any)["wait_seconds"]; ok {
		t.Fatalf("synchronous search must not expose wait_seconds")
	}

	called, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "search", Arguments: map[string]any{"query": "modern"},
	})
	if err != nil {
		t.Fatalf("call search: %v", err)
	}
	if called.IsError {
		t.Fatalf("search failed: %#v", called.Content)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(called.Content[0].(*sdk.TextContent).Text), &payload); err != nil {
		t.Fatalf("decode search payload: %v", err)
	}
	if payload["query"] != "modern" || payload["namespace"] != manualsNS {
		t.Fatalf("search payload = %#v", payload)
	}

	invalid, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "search", Arguments: map[string]any{"query": ""}})
	if err != nil {
		t.Fatalf("invalid call must be a tool error, got protocol error: %v", err)
	}
	if !invalid.IsError {
		t.Fatalf("validation failure must set IsError")
	}

	missing, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "missing", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("unknown tool must be a tool error, got protocol error: %v", err)
	}
	if !missing.IsError {
		t.Fatalf("unknown tool must set IsError")
	}
	if text := missing.Content[0].(*sdk.TextContent).Text; text != "Unknown or disabled RAG tool: missing" {
		t.Fatalf("unknown tool text = %q", text)
	}

	prompts, err := session.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatalf("list prompts: %v", err)
	}
	if len(prompts.Prompts) != 0 {
		t.Fatalf("prompts = %#v", prompts.Prompts)
	}

	// Catalog mutation is visible to the next list and get.
	store := endpoint.opts.Catalog.(*catalog.MemoryStore)
	namespaceCatalog, err := catalog.NewNamespaceCatalog(store, manualNSForTest())
	if err != nil {
		t.Fatalf("namespace catalog: %v", err)
	}
	if _, err := namespaceCatalog.PutPrompt(ctx, "summary", catalog.PromptDefinition{
		Description: "Summarize",
		Arguments:   []catalog.PromptArgument{{Name: "topic", Required: true}},
		Messages:    []catalog.PromptMessage{{Role: "user", Text: "Summarize {{topic}}"}},
	}); err != nil {
		t.Fatalf("put prompt: %v", err)
	}
	endpoint.PromptsChanged(manualsNS)

	prompts, err = session.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatalf("list prompts after mutation: %v", err)
	}
	if len(prompts.Prompts) != 1 || prompts.Prompts[0].Name != "summary" {
		t.Fatalf("prompts after mutation = %#v", prompts.Prompts)
	}
	prompt, err := session.GetPrompt(ctx, &sdk.GetPromptParams{
		Name: "summary", Arguments: map[string]string{"topic": "{{do_not_execute}}"},
	})
	if err != nil {
		t.Fatalf("get prompt: %v", err)
	}
	if text := prompt.Messages[0].Content.(*sdk.TextContent).Text; text != "Summarize {{do_not_execute}}" {
		t.Fatalf("rendered prompt = %q", text)
	}
	if _, err := session.GetPrompt(ctx, &sdk.GetPromptParams{Name: "summary"}); err == nil {
		t.Fatalf("missing required prompt argument must fail")
	}
}

func manualNSForTest() string { return manualsNS }

func TestModernNamespaceIsolationAndDisable(t *testing.T) {
	runtime := newFakeRuntime()
	runtime.searchPayload = map[string]any{"namespace": otherNS, "evidence_status": "found", "results": []any{}}
	endpoint, server := newTestHTTPHandler(t, runtime, Options{})
	manuals := newModernClient(t, server, manualsNS)
	other := newModernClient(t, server, otherNS)
	ctx := context.Background()

	// Disable search only in the manuals namespace catalog.
	store := endpoint.opts.Catalog.(*catalog.MemoryStore)
	namespaceCatalog, err := catalog.NewNamespaceCatalog(store, manualsNS)
	if err != nil {
		t.Fatalf("namespace catalog: %v", err)
	}
	disabled := false
	if _, err := namespaceCatalog.PatchTool(ctx, searchToolName, catalog.ToolPatch{Enabled: &disabled}, catalog.ToolSettings{}); err != nil {
		t.Fatalf("patch tool: %v", err)
	}

	listed, err := manuals.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list manuals tools: %v", err)
	}
	if len(listed.Tools) != 4 {
		t.Fatalf("manuals tools = %d, want 4", len(listed.Tools))
	}
	blocked, err := manuals.CallTool(ctx, &sdk.CallToolParams{Name: "search", Arguments: map[string]any{"query": "x"}})
	if err != nil {
		t.Fatalf("disabled tool must be a tool error: %v", err)
	}
	if !blocked.IsError {
		t.Fatalf("disabled tool must set IsError")
	}

	otherTools, err := other.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list other tools: %v", err)
	}
	if len(otherTools.Tools) != 5 {
		t.Fatalf("other tools = %d, want 5", len(otherTools.Tools))
	}
	called, err := other.CallTool(ctx, &sdk.CallToolParams{Name: "search", Arguments: map[string]any{"query": "other"}})
	if err != nil || called.IsError {
		t.Fatalf("other search failed: %v %#v", err, called)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(called.Content[0].(*sdk.TextContent).Text), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["namespace"] != otherNS {
		t.Fatalf("other namespace payload = %#v", payload)
	}
}

func TestHTTPTransportSecurity(t *testing.T) {
	runtime := newFakeRuntime()
	_, server := newTestHTTPHandler(t, runtime, Options{})
	initializeBody := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "test", "version": "1"},
		},
	}
	post := func(mutate func(*http.Request)) *http.Response {
		encoded, _ := json.Marshal(initializeBody)
		request, err := http.NewRequest(http.MethodPost, server.URL+"/mcp", bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Authorization", "Bearer "+testToken)
		request.Header.Set("X-RAG-Namespace", manualsNS)
		if mutate != nil {
			mutate(request)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		response.Body.Close()
		return response
	}

	if response := post(nil); response.StatusCode != http.StatusOK {
		t.Fatalf("allowed request status = %d", response.StatusCode)
	}
	if response := post(func(request *http.Request) {
		request.Header.Del("X-RAG-Namespace")
	}); response.StatusCode != http.StatusUnauthorized || response.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("missing auth status = %d challenge = %q",
			response.StatusCode, response.Header.Get("WWW-Authenticate"))
	}
	if response := post(func(request *http.Request) {
		request.Header.Set("X-RAG-Namespace", "../escape")
	}); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid namespace status = %d", response.StatusCode)
	}
	if response := post(func(request *http.Request) {
		request.Host = "evil.example"
	}); response.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("invalid host status = %d", response.StatusCode)
	}
	if response := post(func(request *http.Request) {
		request.Header.Set("Origin", "https://evil.example")
	}); response.StatusCode != http.StatusForbidden {
		t.Fatalf("invalid origin status = %d", response.StatusCode)
	}
	if response := post(func(request *http.Request) {
		request.Header.Set("Origin", "http://127.0.0.1:9999")
	}); response.StatusCode != http.StatusOK {
		t.Fatalf("allowed origin status = %d", response.StatusCode)
	}
	if response := post(func(request *http.Request) {
		request.Header.Set("Content-Type", "text/plain")
	}); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid content type status = %d", response.StatusCode)
	}
}

func TestDeclaredBodyLengthIsRejected(t *testing.T) {
	endpoint := newTestEndpoint(t, newFakeRuntime(), Options{})
	handler, err := NewHTTPHandler(HTTPOptions{
		Endpoint: endpoint,
		NamespaceFromRequest: func(*http.Request) (string, error) {
			return manualsNS, nil
		},
		AllowedHosts:   []string{"127.0.0.1", "127.0.0.1:*"},
		AllowedOrigins: []string{"http://127.0.0.1:*"},
	})
	if err != nil {
		t.Fatalf("NewHTTPHandler: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.ContentLength = maxRequestBodyBytes + 1
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "Request body too large") {
		t.Fatalf("body = %q", recorder.Body.String())
	}
}

func TestNamespaceCallbackForbidden(t *testing.T) {
	endpoint := newTestEndpoint(t, newFakeRuntime(), Options{})
	handler, err := NewHTTPHandler(HTTPOptions{
		Endpoint: endpoint,
		NamespaceFromRequest: func(*http.Request) (string, error) {
			return "", ErrForbidden
		},
		AllowedHosts:   []string{"127.0.0.1:*"},
		AllowedOrigins: []string{"http://127.0.0.1:*"},
	})
	if err != nil {
		t.Fatalf("NewHTTPHandler: %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/mcp", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.StatusCode)
	}
}

func TestLegacyToolListChangedNotification(t *testing.T) {
	runtime := newFakeRuntime()
	endpoint, server := newTestHTTPHandler(t, runtime, Options{})
	client := newRawRPC(t, server.URL, manualsNS)
	client.initialize()

	getRequest, err := http.NewRequest(http.MethodGet, server.URL+"/mcp", nil)
	if err != nil {
		t.Fatalf("new GET: %v", err)
	}
	getRequest.Header.Set("Accept", "text/event-stream")
	getRequest.Header.Set("Authorization", "Bearer "+testToken)
	getRequest.Header.Set("X-RAG-Namespace", manualsNS)
	getRequest.Header.Set("Mcp-Session-Id", client.sessionID)
	getRequest.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	stream, err := client.client.Do(getRequest)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("event stream status = %d", stream.StatusCode)
	}
	defer stream.Body.Close()

	notifications := make(chan string, 8)
	go func() {
		defer stream.Body.Close()
		scanner := bufio.NewScanner(stream.Body)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "notifications/") {
				select {
				case notifications <- line:
				default:
				}
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	endpoint.ToolsChanged(manualsNS)

	select {
	case line := <-notifications:
		if !strings.Contains(line, "notifications/tools/list_changed") {
			t.Fatalf("notification = %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for tools/list_changed notification")
	}
}
