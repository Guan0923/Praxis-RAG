package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Guan0923/xinghe_rag/internal/resources"
)

// documentPagesText produces exactly two paragraph pages under
// resources.PageCharacters (8000): 5002 + 5000 characters.
func documentPagesText() string {
	return strings.Repeat("a", 5000) + "\n\n" + strings.Repeat("b", 5000)
}

func resourcePayload(t *testing.T, result *sdk.ReadResourceResult) map[string]any {
	t.Helper()
	if result == nil || len(result.Contents) == 0 {
		t.Fatalf("empty resource result: %#v", result)
	}
	contents := result.Contents[0]
	if contents.MIMEType != "application/json" {
		t.Fatalf("mime type = %q", contents.MIMEType)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(contents.Text), &payload); err != nil {
		t.Fatalf("resource payload is not JSON: %v (%q)", err, contents.Text)
	}
	return payload
}

func TestModernResourcesAndDocumentPages(t *testing.T) {
	runtime := newFakeRuntime()
	runtime.listDocuments = []DocumentRecord{{"name": "manual.txt"}, {"name": "guide.md"}}
	runtime.readDocument = DocumentContent{Text: documentPagesText(), Version: "v1"}
	_, server := newTestHTTPHandler(t, runtime, Options{})
	session := newModernClient(t, server, manualsNS)
	ctx := context.Background()

	capabilities := session.InitializeResult().Capabilities
	if capabilities.Resources == nil || !capabilities.Resources.ListChanged || !capabilities.Resources.Subscribe {
		t.Fatalf("resources capabilities = %#v", capabilities.Resources)
	}

	listed, err := session.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("list resources: %v", err)
	}
	if len(listed.Resources) != 2 {
		t.Fatalf("resources = %d, want 2", len(listed.Resources))
	}
	first := listed.Resources[0]
	if first.URI != resources.DocumentURI(manualsNS, "manual.txt") || first.Name != "manual.txt" ||
		first.MIMEType != "application/json" || first.Description != resourceDescription {
		t.Fatalf("resource = %#v", first)
	}

	templates, err := session.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	if len(templates.ResourceTemplates) != 2 ||
		templates.ResourceTemplates[0].URITemplate != "rag://knowledge/manuals/{name}{?page,version}" ||
		templates.ResourceTemplates[1].URITemplate != "rag://artifacts/{namespace}/{artifact_id}{?offset,length}" {
		t.Fatalf("templates = %#v", templates.ResourceTemplates)
	}

	uri := resources.DocumentURI(manualsNS, "manual.txt")
	firstPage, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("read page 1: %v", err)
	}
	page1 := resourcePayload(t, firstPage)
	if page1["uri"] != uri || page1["page"] != float64(1) || page1["total_pages"] != float64(2) ||
		page1["version"] != "v1" || page1["truncated"] != true || page1["has_more"] != true {
		t.Fatalf("page 1 payload = %#v", page1)
	}
	nextURI, ok := page1["next_uri"].(string)
	if !ok || !strings.Contains(nextURI, "page=2") || !strings.Contains(nextURI, "version=v1") {
		t.Fatalf("next_uri = %#v", page1["next_uri"])
	}
	secondPage, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: nextURI})
	if err != nil {
		t.Fatalf("read page 2: %v", err)
	}
	page2 := resourcePayload(t, secondPage)
	if page2["page"] != float64(2) || page2["text"] != strings.Repeat("b", 5000) || page2["next_uri"] != nil {
		t.Fatalf("page 2 payload = %#v", page2)
	}
}

func TestModernArtifactResource(t *testing.T) {
	artifactID := "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a"
	runtime := newFakeRuntime()
	runtime.artifactPayload = map[string]any{
		"artifact_id": artifactID,
		"offset":      0,
		"length":      3,
		"data":        "YWJj",
		"next_offset": 3,
	}
	_, server := newTestHTTPHandler(t, runtime, Options{})
	session := newModernClient(t, server, manualsNS)
	ctx := context.Background()

	uri := artifactURI(manualsNS, artifactID) + "?offset=0&length=3"
	result, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	payload := resourcePayload(t, result)
	if payload["data"] != "YWJj" {
		t.Fatalf("artifact payload = %#v", payload)
	}
	wantNext := artifactURI(manualsNS, artifactID) + "?offset=3&length=3"
	if payload["next_uri"] != wantNext {
		t.Fatalf("next_uri = %#v, want %q", payload["next_uri"], wantNext)
	}
	if len(runtime.artifactCalls) != 1 {
		t.Fatalf("artifact calls = %d", len(runtime.artifactCalls))
	}
	if call := runtime.artifactCalls[0]; call.ArtifactID != artifactID || call.Offset != 0 || call.Length != 3 ||
		call.Namespace != manualsNS {
		t.Fatalf("artifact call = %#v", call)
	}

	// Defaults are offset=0 length=65536.
	if _, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: artifactURI(manualsNS, artifactID)}); err != nil {
		t.Fatalf("read artifact with defaults: %v", err)
	}
	if call := runtime.artifactCalls[1]; call.Offset != 0 || call.Length != 65536 {
		t.Fatalf("default artifact call = %#v", call)
	}
}

// TestModernResourceErrorsMatchSEP2575 verifies the modern wire contract for
// resource application errors: INVALID_PARAMS (-32602) with HTTP 400, as
// required by SEP-2575. Raw HTTP is used because the Go SDK client tears down
// the session on non-2xx responses (see NOTES.md).
func TestModernResourceErrorsMatchSEP2575(t *testing.T) {
	artifactID := "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a"
	runtime := newFakeRuntime()
	runtime.listDocuments = []DocumentRecord{{"name": "manual.txt"}}
	runtime.readDocument = DocumentContent{Text: "hello world", Version: "v1"}
	_, server := newTestHTTPHandler(t, runtime, Options{})
	documentURI := resources.DocumentURI(manualsNS, "manual.txt")

	cases := []struct {
		name      string
		uri       string
		wantError string
	}{
		{"page two without version", documentURI + "?page=2", "Read the document URI first to obtain a versioned next-page URI"},
		{"stale version", documentURI + "?page=2&version=stale", "Document changed; read its document URI again"},
		{"missing page", documentURI + "?page=999&version=v1", "Page does not exist"},
		{"foreign namespace", resources.DocumentURI(otherNS, "manual.txt"), "Document URI is outside this namespace"},
		{"bad page number", documentURI + "?page=0", "Invalid page number"},
		{"artifact bad uuid", "rag://artifacts/manuals/not-a-uuid", "Expected a dashed UUID string"},
		{"artifact foreign namespace", "rag://artifacts/other/" + artifactID, "Artifact URI is outside this namespace or non-canonical"},
		{"artifact leading zero", artifactURI(manualsNS, artifactID) + "?offset=00&length=1", "Artifact chunk parameters must be canonical integers"},
		{"artifact zero length", artifactURI(manualsNS, artifactID) + "?offset=0&length=0", "length must be between 1 and 1048576 bytes"},
		{"artifact duplicate key", artifactURI(manualsNS, artifactID) + "?offset=1&offset=2", "Invalid artifact chunk parameters"},
		{"artifact unknown key", artifactURI(manualsNS, artifactID) + "?bogus=1", "Invalid artifact chunk parameters"},
		{"artifact empty query", artifactURI(manualsNS, artifactID) + "?", "Invalid artifact chunk parameters"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, payload := modernRawCall(t, server, manualsNS, "resources/read", map[string]any{
				"uri": testCase.uri,
			})
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
			errorPayload, ok := payload["error"].(map[string]any)
			if !ok {
				t.Fatalf("missing JSON-RPC error: %#v", payload)
			}
			if code, _ := errorPayload["code"].(float64); code != -32602 {
				t.Fatalf("error code = %v, want -32602", errorPayload["code"])
			}
			if message, _ := errorPayload["message"].(string); !strings.Contains(message, testCase.wantError) {
				t.Fatalf("error = %q, want %q", message, testCase.wantError)
			}
		})
	}

	// Runtime failures are also INVALID_PARAMS tool-level errors.
	runtime.artifactErr = errors.New("Artifact expired")
	status, payload := modernRawCall(t, server, manualsNS, "resources/read", map[string]any{
		"uri": artifactURI(manualsNS, artifactID),
	})
	if status != http.StatusBadRequest {
		t.Fatalf("artifact runtime error status = %d, want 400", status)
	}
	if message, _ := payload["error"].(map[string]any)["message"].(string); !strings.Contains(message, "Artifact expired") {
		t.Fatalf("artifact runtime error = %#v", payload)
	}

	// subscriptions/listen with a page URI is rejected before acknowledging.
	status, payload = modernRawCall(t, server, manualsNS, "subscriptions/listen", map[string]any{
		"notifications": map[string]any{"resourceSubscriptions": []any{documentURI + "?page=1"}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("listen validation status = %d, want 400", status)
	}
	if message, _ := payload["error"].(map[string]any)["message"].(string); !strings.Contains(message, "Subscribe to the document URI") {
		t.Fatalf("listen validation error = %#v", payload)
	}
}

// TestLegacyResourceSubscriptions exercises resources/list, templates, read and
// resources/subscribe + notifications over the legacy (stateful) transport,
// including the unsubscribe filter.
func TestLegacyResourceSubscriptions(t *testing.T) {
	runtime := newFakeRuntime()
	runtime.listDocuments = []DocumentRecord{{"name": "manual.txt"}}
	runtime.readDocument = DocumentContent{Text: "hello world", Version: "v1"}
	endpoint, server := newTestHTTPHandler(t, runtime, Options{})
	client := newRawRPC(t, server.URL, manualsNS)
	client.initialize()
	uri := resources.DocumentURI(manualsNS, "manual.txt")

	listed, _ := client.call("resources/list", nil)
	resourcesList := listed["result"].(map[string]any)["resources"].([]any)
	if len(resourcesList) != 1 || resourcesList[0].(map[string]any)["uri"] != uri {
		t.Fatalf("resources/list = %#v", resourcesList)
	}
	templates, _ := client.call("resources/templates/list", nil)
	if len(templates["result"].(map[string]any)["resourceTemplates"].([]any)) != 2 {
		t.Fatalf("resources/templates/list = %#v", templates)
	}
	read, _ := client.call("resources/read", map[string]any{"uri": uri})
	contents := read["result"].(map[string]any)["contents"].([]any)
	if len(contents) != 1 || contents[0].(map[string]any)["mimeType"] != "application/json" {
		t.Fatalf("resources/read = %#v", read)
	}
	var page map[string]any
	if err := json.Unmarshal([]byte(contents[0].(map[string]any)["text"].(string)), &page); err != nil {
		t.Fatalf("page payload: %v", err)
	}
	if page["version"] != "v1" || page["page"] != float64(1) {
		t.Fatalf("page = %#v", page)
	}
	// Legacy application errors are JSON-RPC errors with an HTTP 200 response.
	badRead, badResponse := client.call("resources/read", map[string]any{"uri": uri + "?page=2"})
	if badResponse.StatusCode != http.StatusOK {
		t.Fatalf("legacy error status = %d, want 200", badResponse.StatusCode)
	}
	if errorPayload, ok := badRead["error"].(map[string]any); !ok || errorPayload["code"] != float64(-32602) {
		t.Fatalf("legacy error payload = %#v", badRead)
	}

	// Subscription validation.
	invalidSubscriptions := []struct {
		name      string
		uri       string
		wantError string
	}{
		{"page uri", uri + "?page=1", "Subscribe to the document URI, not a page URI"},
		{"missing document", resources.DocumentURI(manualsNS, "missing.txt"), "Document does not exist"},
		{"artifact uri", artifactURI(manualsNS, "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a"), "Invalid document URI"},
		{"foreign namespace", resources.DocumentURI(otherNS, "manual.txt"), "Document URI is outside this namespace"},
	}
	for _, testCase := range invalidSubscriptions {
		payload, _ := client.call("resources/subscribe", map[string]any{"uri": testCase.uri})
		errorPayload, ok := payload["error"].(map[string]any)
		if !ok {
			t.Fatalf("%s: expected a JSON-RPC error, got %#v", testCase.name, payload)
		}
		if message, _ := errorPayload["message"].(string); !strings.Contains(message, testCase.wantError) {
			t.Fatalf("%s: error = %q, want %q", testCase.name, message, testCase.wantError)
		}
	}

	subscribed, _ := client.call("resources/subscribe", map[string]any{"uri": uri})
	if _, hasError := subscribed["error"]; hasError {
		t.Fatalf("subscribe failed: %#v", subscribed)
	}
	notifications, cleanup := openLegacyEventStream(t, client)
	defer cleanup()

	// Adding a document proves the stream is live (list_changed only; the
	// notification machinery of the SDK attaches the standalone stream lazily).
	endpoint.DocumentsChanged(manualsNS,
		[]DocumentRecord{{"name": "manual.txt", "size": 1}},
		[]DocumentRecord{{"name": "manual.txt", "size": 1}, {"name": "new.txt", "size": 0}})
	waitLegacyNotification(t, notifications, "notifications/resources/list_changed", 3*time.Second)

	endpoint.DocumentsChanged(manualsNS,
		[]DocumentRecord{{"name": "manual.txt", "size": 1}, {"name": "new.txt", "size": 0}},
		[]DocumentRecord{{"name": "manual.txt", "size": 2}, {"name": "new.txt", "size": 0}})
	waitLegacyNotification(t, notifications, "notifications/resources/updated", 3*time.Second)

	// After unsubscribe the updated notification is filtered out.
	if payload, _ := client.call("resources/unsubscribe", map[string]any{"uri": uri}); payload["error"] != nil {
		t.Fatalf("unsubscribe failed: %#v", payload)
	}
	endpoint.DocumentsChanged(manualsNS,
		[]DocumentRecord{{"name": "manual.txt", "size": 2}, {"name": "new.txt", "size": 0}},
		[]DocumentRecord{{"name": "manual.txt", "size": 3}, {"name": "new.txt", "size": 0}})
	remaining := drainLegacyNotifications(t, notifications, 700*time.Millisecond)
	for _, method := range remaining {
		if method == "notifications/resources/updated" {
			t.Fatalf("unsubscribed session received resources/updated")
		}
	}
}

func TestDocumentToolsPublishResourceNotifications(t *testing.T) {
	runtime := newFakeRuntime()
	runtime.listDocuments = []DocumentRecord{{"name": "a.txt", "size": float64(1)}}
	runtime.readDocument = DocumentContent{Text: "first", Version: "v1"}
	events := make(chan string, 32)
	deliver := func(event string) {
		select {
		case events <- event:
		default:
		}
	}
	clientOptions := &sdk.ClientOptions{
		ResourceListChangedHandler: func(context.Context, *sdk.ResourceListChangedRequest) {
			deliver("list_changed")
		},
		ResourceUpdatedHandler: func(_ context.Context, request *sdk.ResourceUpdatedNotificationRequest) {
			deliver("updated:" + request.Params.URI)
		},
	}
	_, server := newTestHTTPHandler(t, runtime, Options{})
	session := newModernClientWithOptions(t, server, manualsNS, clientOptions)
	ctx := context.Background()
	uri := resources.DocumentURI(manualsNS, "a.txt")
	if err := session.Subscribe(ctx, &sdk.SubscribeParams{URI: uri}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	runtime.writePayload = map[string]any{
		"status":    "updated",
		"document":  map[string]any{"name": "a.txt", "size": float64(2)},
		"documents": []any{map[string]any{"name": "a.txt", "size": float64(2)}},
	}
	updated, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "update_document", Arguments: map[string]any{"name": "a.txt", "content": "replacement"},
	})
	if err != nil || updated.IsError {
		t.Fatalf("update_document failed: %v %#v", err, updated)
	}
	waitEvent(t, events, "updated:"+uri, 3*time.Second)
	waitEvent(t, events, "list_changed", 3*time.Second)

	// A no-op write must not publish notifications.
	runtime.listDocuments = []DocumentRecord{{"name": "a.txt", "size": float64(2)}}
	runtime.writePayload = map[string]any{
		"status":    "unchanged",
		"document":  map[string]any{"name": "a.txt", "size": float64(2)},
		"documents": []any{map[string]any{"name": "a.txt", "size": float64(2)}},
	}
	if _, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "update_document", Arguments: map[string]any{"name": "a.txt", "content": "replacement"},
	}); err != nil {
		t.Fatalf("noop update_document failed: %v", err)
	}
	if unexpected := drainEvents(t, events, 300*time.Millisecond); len(unexpected) != 0 {
		t.Fatalf("noop write published %v", unexpected)
	}

	// Deleting the document publishes list_changed and its resources/updated.
	runtime.deletePayload = map[string]any{
		"deleted":   map[string]any{"name": "a.txt", "size": float64(2)},
		"documents": []any{},
	}
	deleted, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "delete_document", Arguments: map[string]any{"name": "a.txt"},
	})
	if err != nil || deleted.IsError {
		t.Fatalf("delete_document failed: %v %#v", err, deleted)
	}
	waitEvent(t, events, "updated:"+uri, 3*time.Second)
	waitEvent(t, events, "list_changed", 3*time.Second)
}

func TestModernResourceSubscriptions(t *testing.T) {
	runtime := newFakeRuntime()
	runtime.listDocuments = []DocumentRecord{{"name": "manual.txt"}}
	events := make(chan string, 32)
	deliver := func(event string) {
		select {
		case events <- event:
		default:
		}
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, &sdk.ClientOptions{
		ResourceListChangedHandler: func(context.Context, *sdk.ResourceListChangedRequest) {
			deliver("list_changed")
		},
		ResourceUpdatedHandler: func(_ context.Context, request *sdk.ResourceUpdatedNotificationRequest) {
			deliver("updated:" + request.Params.URI)
		},
	})
	endpoint := newTestEndpoint(t, runtime, Options{})
	server := httptest.NewServer(newTestHandlerFor(t, endpoint))
	defer server.Close()
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

	uri := resources.DocumentURI(manualsNS, "manual.txt")
	if err := session.Subscribe(ctx, &sdk.SubscribeParams{URI: uri}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	endpoint.DocumentsChanged(manualsNS,
		[]DocumentRecord{{"name": "manual.txt", "size": 1}},
		[]DocumentRecord{{"name": "manual.txt", "size": 2}})
	waitEvent(t, events, "updated:"+uri, 3*time.Second)
	waitEvent(t, events, "list_changed", 3*time.Second)

	// Identical records publish nothing.
	endpoint.DocumentsChanged(manualsNS,
		[]DocumentRecord{{"name": "manual.txt", "size": 2}},
		[]DocumentRecord{{"name": "manual.txt", "size": 2}})
	if unexpected := drainEvents(t, events, 300*time.Millisecond); len(unexpected) != 0 {
		t.Fatalf("noop change published %v", unexpected)
	}

	// Deletion publishes the removed document's updated notification.
	endpoint.DocumentsChanged(manualsNS,
		[]DocumentRecord{{"name": "manual.txt", "size": 2}},
		nil)
	waitEvent(t, events, "updated:"+uri, 3*time.Second)
}

func waitEvent(t *testing.T, events <-chan string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case event := <-events:
			if event == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

func drainEvents(t *testing.T, events <-chan string, duration time.Duration) []string {
	t.Helper()
	var received []string
	deadline := time.After(duration)
	for {
		select {
		case event := <-events:
			received = append(received, event)
		case <-deadline:
			return received
		}
	}
}

// modernRawCall sends one SEP-2575 request over raw HTTP. It returns the HTTP
// status and the JSON-RPC response (either JSON or the single SSE data event).
func modernRawCall(t *testing.T, server *httptest.Server, namespace, method string, params map[string]any) (int, map[string]any) {
	t.Helper()
	fullParams := map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		},
	}
	for key, value := range params {
		fullParams[key] = value
	}
	encoded, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": fullParams,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/mcp", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("X-RAG-Namespace", namespace)
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", method)
	if uri, ok := params["uri"].(string); ok && method == "resources/read" {
		request.Header.Set("Mcp-Name", uri)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer response.Body.Close()
	raw, err := readAllTest(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	payload := decodeTestPayload(response.Header.Get("Content-Type"), raw)
	return response.StatusCode, payload
}

func readAllTest(reader interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buffer bytes.Buffer
	_, err := buffer.ReadFrom(reader)
	return buffer.Bytes(), err
}

func decodeTestPayload(contentType string, raw []byte) map[string]any {
	if strings.HasPrefix(contentType, "application/json") {
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		return payload
	}
	var last map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &payload); err == nil {
			last = payload
		}
	}
	return last
}

// legacyEventStream reads JSON-RPC notification methods from the legacy
// standalone SSE stream.
type legacyEventStream struct {
	events chan string
}

func openLegacyEventStream(t *testing.T, client *rawRPC) (*legacyEventStream, func()) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, client.baseURL+"/mcp", nil)
	if err != nil {
		t.Fatalf("new GET: %v", err)
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("X-RAG-Namespace", client.namespace)
	request.Header.Set("Mcp-Session-Id", client.sessionID)
	request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	response, err := client.client.Do(request)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("event stream status = %d", response.StatusCode)
	}
	done := make(chan struct{})
	events := make(chan string, 32)
	go func() {
		defer response.Body.Close()
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data:")
			if !ok {
				continue
			}
			var message map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &message); err != nil {
				continue
			}
			method, _ := message["method"].(string)
			if !strings.HasPrefix(method, "notifications/") {
				continue
			}
			select {
			case events <- method:
			case <-done:
				return
			}
		}
	}()
	cleanup := func() {
		close(done)
		_ = response.Body.Close()
	}
	return &legacyEventStream{events: events}, cleanup
}

func waitLegacyNotification(t *testing.T, stream *legacyEventStream, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case method := <-stream.events:
			if method == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

func drainLegacyNotifications(t *testing.T, stream *legacyEventStream, duration time.Duration) []string {
	t.Helper()
	var received []string
	deadline := time.After(duration)
	for {
		select {
		case method := <-stream.events:
			received = append(received, method)
		case <-deadline:
			return received
		}
	}
}
