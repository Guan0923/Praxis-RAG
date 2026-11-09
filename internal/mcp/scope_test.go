package mcp

// Scope enforcement tests: httpapi resolves access.ScopeSet per authenticated
// request and puts it in the request context; internal/mcp must enforce the same
// visibility on tools and resources through both MCP protocol eras, and keep the
// historical unrestricted behavior for management credentials (no scope).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Guan0923/xinghe_rag/internal/access"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/resources"
)

const (
	// scopeTestHeader selects one scope fixture for a request, mimicking the
	// context scope httpapi attaches after authentication.
	scopeTestHeader = "X-Test-Scope"

	artifactFixtureID = "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a"
)

// testWholeScope is the owner/administrator scope: everything, read and write.
func testWholeScope() access.ScopeSet {
	return access.ScopeSet{
		WholeRead:      true,
		WholeWrite:     true,
		ReadDocuments:  map[string]struct{}{},
		WriteDocuments: map[string]struct{}{},
	}
}

// testFileScope is a file-level grant: only the named documents.
func testFileScope(read, write []string) access.ScopeSet {
	scope := access.NewScopeSet()
	for _, name := range read {
		scope.ReadDocuments[name] = struct{}{}
	}
	for _, name := range write {
		scope.WriteDocuments[name] = struct{}{}
	}
	return scope
}

// newScopeTestServer builds the /mcp handler with an outer adapter that attaches
// the fixture scope to the request context, exactly like httpapi does.
func newScopeTestServer(t *testing.T, runtime Runtime, options Options, scopes map[string]access.ScopeSet) (*Endpoint, *httptest.Server) {
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
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := r.Header.Get(scopeTestHeader); key != "" {
			scope, ok := scopes[key]
			if !ok {
				// Unknown fixture: deny everything rather than falling back to
				// the unrestricted default.
				scope = access.NewScopeSet()
			}
			r = r.WithContext(access.ContextWithScope(r.Context(), scope))
		}
		handler.ServeHTTP(w, r)
	})
	server := httptest.NewServer(wrapped)
	t.Cleanup(server.Close)
	return endpoint, server
}

// doScoped sends one raw JSON-RPC message with the fixture scope attached.
func (r *rawRPC) doScoped(scopeKey string, payload map[string]any, extraHeaders map[string]string) (*http.Response, map[string]any) {
	r.t.Helper()
	headers := make(map[string]string, len(extraHeaders)+1)
	for name, value := range extraHeaders {
		headers[name] = value
	}
	if scopeKey != "" {
		headers[scopeTestHeader] = scopeKey
	}
	return r.do(payload, headers)
}

// initializeScoped opens a legacy session whose initialize request carries the
// given scope (the stateful transport runs later handlers on this context).
func initializeScoped(t *testing.T, rpc *rawRPC, scopeKey string) {
	t.Helper()
	rpc.nextID++
	response, payload := rpc.doScoped(scopeKey, map[string]any{
		"jsonrpc": "2.0", "id": rpc.nextID, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "1"},
		},
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d", response.StatusCode)
	}
	if payload == nil {
		t.Fatalf("initialize returned no payload")
	}
	rpc.sessionID = response.Header.Get("Mcp-Session-Id")
	rpc.protocolVersion = "2025-11-25"
	if rpc.sessionID == "" {
		t.Fatalf("initialize did not set Mcp-Session-Id")
	}
	if initialized := rpc.notify("notifications/initialized"); initialized.StatusCode != http.StatusAccepted {
		t.Fatalf("initialized status = %d, want 202", initialized.StatusCode)
	}
}

// callToolScoped performs one tools/call with the fixture scope.
func callToolScoped(t *testing.T, rpc *rawRPC, scopeKey, name string, arguments map[string]any) map[string]any {
	t.Helper()
	rpc.nextID++
	_, payload := rpc.doScoped(scopeKey, map[string]any{
		"jsonrpc": "2.0", "id": rpc.nextID, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": arguments},
	}, nil)
	return payload
}

func TestScopeSearchRestrictsDocumentIDs(t *testing.T) {
	runtime := newFakeRuntime()
	scopes := map[string]access.ScopeSet{
		"full":  testWholeScope(),
		"files": testFileScope([]string{"a.md"}, []string{"a.md"}),
		"none":  access.NewScopeSet(),
	}
	_, server := newScopeTestServer(t, runtime, Options{}, scopes)
	rpc := newRawRPC(t, server.URL, manualsNS)

	// The session is initialized by a whole-knowledge-base caller: a later
	// file-level request must not inherit the initializing context scope. This is
	// the core reason the HTTP adapter transports the scope per request.
	initializeScoped(t, rpc, "full")

	if payload := callToolScoped(t, rpc, "files", searchToolName, map[string]any{"query": "hello"}); len(runtime.searchCalls) != 1 {
		t.Fatalf("restricted search did not reach the runtime: %#v", payload)
	}
	if got := runtime.searchCalls[0].DocumentIDs; !reflect.DeepEqual(got, []string{"doc:a.md"}) {
		t.Fatalf("restricted DocumentIDs = %#v, want [doc:a.md]", got)
	}
	if got := runtime.resolveCalls[0]; !reflect.DeepEqual(got, []string{"a.md"}) {
		t.Fatalf("resolved names = %#v, want [a.md]", got)
	}

	callToolScoped(t, rpc, "full", searchToolName, map[string]any{"query": "hello"})
	if got := runtime.searchCalls[1].DocumentIDs; got != nil {
		t.Fatalf("whole-knowledge-base DocumentIDs = %#v, want nil", got)
	}

	callToolScoped(t, rpc, "none", searchToolName, map[string]any{"query": "hello"})
	if got := runtime.searchCalls[2].DocumentIDs; got == nil || len(got) != 0 {
		t.Fatalf("no-access DocumentIDs = %#v, want non-nil empty", got)
	}

	// Without any scope (API key / service token) the historical behavior stays.
	callToolScoped(t, rpc, "", searchToolName, map[string]any{"query": "hello"})
	if got := runtime.searchCalls[3].DocumentIDs; got != nil {
		t.Fatalf("management credential DocumentIDs = %#v, want nil", got)
	}
}

func TestScopeSearchCannotBeForgedByClientHeader(t *testing.T) {
	runtime := newFakeRuntime()
	scopes := map[string]access.ScopeSet{
		"files": testFileScope([]string{"a.md"}, nil),
	}
	_, server := newScopeTestServer(t, runtime, Options{}, scopes)
	rpc := newRawRPC(t, server.URL, manualsNS)
	initializeScoped(t, rpc, "files")

	// A client-supplied internal scope header must be overwritten by the adapter.
	rpc.nextID++
	_, payload := rpc.doScoped("files", map[string]any{
		"jsonrpc": "2.0", "id": rpc.nextID, "method": "tools/call",
		"params": map[string]any{"name": searchToolName, "arguments": map[string]any{"query": "x"}},
	}, map[string]string{scopeHeaderName: encodeScopeHeader(testWholeScope())})
	if payload == nil || len(runtime.searchCalls) != 1 {
		t.Fatalf("spoofed search did not reach the runtime: %#v", payload)
	}
	if got := runtime.searchCalls[0].DocumentIDs; !reflect.DeepEqual(got, []string{"doc:a.md"}) {
		t.Fatalf("spoofed DocumentIDs = %#v, want [doc:a.md]", got)
	}
}

func TestScopeQueuedSearchRejected(t *testing.T) {
	runtime := newFakeRuntime()
	scopes := map[string]access.ScopeSet{
		"full":  testWholeScope(),
		"files": testFileScope([]string{"a.md"}, nil),
	}
	_, server := newScopeTestServer(t, runtime, Options{QueuedSearch: true}, scopes)
	rpc := newRawRPC(t, server.URL, manualsNS)
	initializeScoped(t, rpc, "full")

	rejected := callToolScoped(t, rpc, "files", searchToolName, map[string]any{"query": "x", "wait_seconds": 0})
	if text := toolErrorText(t, rejected); text != messageRestrictedSearchQueued {
		t.Fatalf("queued tool error = %q", text)
	}
	if len(runtime.submitted) != 0 {
		t.Fatalf("restricted queued search must not be submitted: %#v", runtime.submitted)
	}

	admitted := callToolScoped(t, rpc, "full", searchToolName, map[string]any{"query": "x", "wait_seconds": 0})
	if admitted == nil || len(runtime.submitted) != 1 {
		t.Fatalf("whole-knowledge-base queued search must be admitted: %#v", admitted)
	}
}

func TestScopeGetSearchResultFiltersResults(t *testing.T) {
	runtime := newFakeRuntime()
	runtime.searchResults = []map[string]any{{
		"result_id": searchResult,
		"namespace": manualsNS,
		"status":    "succeeded",
		"results": []any{
			map[string]any{"rank": 1, "document_id": "doc:a.md", "chunk_id": "c1", "text": "A"},
			map[string]any{"rank": 2, "document_id": "doc:b.md", "chunk_id": "c2", "text": "B"},
		},
	}}
	endpoint := newTestEndpoint(t, runtime, Options{QueuedSearch: true})
	entry := entryByName(t, endpoint, getSearchResultToolName)
	request := contracts.SearchResultRequest{ResultID: searchResult, Page: 1, PageSize: 10}

	restricted, err := entry.execute(
		access.ContextWithScope(context.Background(), testFileScope([]string{"a.md"}, nil)), manualsNS, request)
	if err != nil {
		t.Fatalf("restricted get_search_result: %v", err)
	}
	results, _ := restricted["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("restricted results = %#v", restricted["results"])
	}
	first, _ := results[0].(map[string]any)
	if first["document_id"] != "doc:a.md" || first["rank"] != 1 {
		t.Fatalf("restricted result = %#v", first)
	}

	unrestricted, err := entry.execute(context.Background(), manualsNS, request)
	if err != nil {
		t.Fatalf("unrestricted get_search_result: %v", err)
	}
	if results, _ := unrestricted["results"].([]any); len(results) != 2 {
		t.Fatalf("unrestricted results = %#v", unrestricted["results"])
	}

	// In-process runtimes may build []map[string]any payloads instead of the
	// JSON-decoded []any shape.
	runtime.searchResults = []map[string]any{{
		"status": "succeeded",
		"results": []map[string]any{
			{"rank": 1, "document_id": "doc:a.md"},
			{"rank": 2, "document_id": "doc:b.md"},
		},
	}}
	typed, err := entry.execute(
		access.ContextWithScope(context.Background(), testFileScope([]string{"a.md"}, nil)), manualsNS, request)
	if err != nil {
		t.Fatalf("typed restricted get_search_result: %v", err)
	}
	typedResults, _ := typed["results"].([]any)
	if len(typedResults) != 1 || typedResults[0].(map[string]any)["document_id"] != "doc:a.md" {
		t.Fatalf("typed restricted results = %#v", typed["results"])
	}
}

func TestScopeResourcesFiltered(t *testing.T) {
	runtime := newFakeRuntime()
	runtime.listDocuments = []DocumentRecord{
		{"name": "a.md"}, {"name": "b.md"}, {"name": "c.md"},
	}
	runtime.readDocument = DocumentContent{Text: "hello world", Version: "v1"}
	endpoint := newTestEndpoint(t, runtime, Options{})

	fileScope := access.ContextWithScope(context.Background(), testFileScope([]string{"a.md"}, nil))
	restricted, err := endpoint.listResources(fileScope, manualsNS, nil)
	if err != nil {
		t.Fatalf("restricted listResources: %v", err)
	}
	if len(restricted.Resources) != 1 || restricted.Resources[0].Name != "a.md" {
		t.Fatalf("restricted resources = %#v", restricted.Resources)
	}

	unrestricted, err := endpoint.listResources(context.Background(), manualsNS, nil)
	if err != nil {
		t.Fatalf("unrestricted listResources: %v", err)
	}
	if len(unrestricted.Resources) != 3 {
		t.Fatalf("management credential resources = %d, want 3", len(unrestricted.Resources))
	}

	// Reading an ungranted document resource is refused like the REST routes.
	if _, err := endpoint.readResource(fileScope, manualsNS, &sdk.ReadResourceParams{
		URI: resources.DocumentURI(manualsNS, "b.md"),
	}); err == nil || !strings.Contains(err.Error(), access.MessageKnowledgeAccess) {
		t.Fatalf("ungranted document read error = %v", err)
	}
	result, err := endpoint.readResource(fileScope, manualsNS, &sdk.ReadResourceParams{
		URI: resources.DocumentURI(manualsNS, "a.md"),
	})
	if err != nil || len(result.Contents) != 1 {
		t.Fatalf("granted document read = %#v, %v", result, err)
	}

	// Artifacts embed their source document name and follow the same grant.
	runtime.artifactPayload = map[string]any{
		"artifact_id": artifactFixtureID, "name": "b.md", "encoding": "base64",
		"data": "aGk=", "total_bytes": 2, "offset": 0, "next_offset": nil,
	}
	if _, err := endpoint.readResource(fileScope, manualsNS, &sdk.ReadResourceParams{
		URI: artifactURI(manualsNS, artifactFixtureID),
	}); err == nil || !strings.Contains(err.Error(), access.MessageKnowledgeAccess) {
		t.Fatalf("ungranted artifact read error = %v", err)
	}
	runtime.artifactPayload["name"] = "a.md"
	artifact, err := endpoint.readResource(fileScope, manualsNS, &sdk.ReadResourceParams{
		URI: artifactURI(manualsNS, artifactFixtureID),
	})
	if err != nil || len(artifact.Contents) != 1 {
		t.Fatalf("granted artifact read = %#v, %v", artifact, err)
	}

	// Resource subscriptions are limited to readable documents as well.
	if err := endpoint.validateSubscription(fileScope, manualsNS, resources.DocumentURI(manualsNS, "b.md")); err == nil {
		t.Fatalf("ungranted subscription must fail")
	}
	if err := endpoint.validateSubscription(fileScope, manualsNS, resources.DocumentURI(manualsNS, "a.md")); err != nil {
		t.Fatalf("granted subscription: %v", err)
	}
}

func TestScopeDocumentWritesRequirePerFileGrant(t *testing.T) {
	runtime := newFakeRuntime()
	runtime.listDocuments = []DocumentRecord{{"name": "a.md"}, {"name": "b.md"}}
	runtime.addPayload = map[string]any{"artifact_id": artifactFixtureID}
	runtime.writePayload = map[string]any{"documents": []any{}}
	runtime.deletePayload = map[string]any{
		"deleted":   map[string]any{"name": "b.md"},
		"documents": []any{map[string]any{"name": "a.md"}},
	}
	scopes := map[string]access.ScopeSet{
		"full":  testWholeScope(),
		"files": testFileScope([]string{"a.md"}, []string{"a.md"}),
		"none":  access.NewScopeSet(),
	}
	_, server := newScopeTestServer(t, runtime, Options{}, scopes)
	rpc := newRawRPC(t, server.URL, manualsNS)
	initializeScoped(t, rpc, "full")

	cases := []struct {
		name      string
		scopeKey  string
		tool      string
		arguments map[string]any
	}{
		{"add other file", "files", addDocumentToolName, map[string]any{"name": "b.md", "content": "x"}},
		{"update other file", "files", updateDocumentToolName, map[string]any{"name": "b.md", "content": "x"}},
		{"delete other file", "files", deleteDocumentToolName, map[string]any{"name": "b.md"}},
		{"add without access", "none", addDocumentToolName, map[string]any{"name": "a.md", "content": "x"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			payload := callToolScoped(t, rpc, testCase.scopeKey, testCase.tool, testCase.arguments)
			if text := toolErrorText(t, payload); text != messageDocumentWriteRequired {
				t.Fatalf("write tool error = %q, want %q", text, messageDocumentWriteRequired)
			}
		})
	}
	if len(runtime.addCalls) != 0 || len(runtime.writeCalls) != 0 || len(runtime.deleteCalls) != 0 {
		t.Fatalf("denied writes reached the runtime: add=%v write=%v delete=%v",
			runtime.addCalls, runtime.writeCalls, runtime.deleteCalls)
	}

	// The explicitly granted file may still be written.
	granted := callToolScoped(t, rpc, "files", addDocumentToolName, map[string]any{"name": "a.md", "content": "x"})
	if payload := toolPayload(t, granted); payload["artifact_id"] != artifactFixtureID {
		t.Fatalf("granted add payload = %#v", payload)
	}
	if !reflect.DeepEqual(runtime.addCalls, []string{"a.md"}) {
		t.Fatalf("granted add calls = %#v", runtime.addCalls)
	}

	// Management credentials (no scope) keep the historical unrestricted writes.
	management := callToolScoped(t, rpc, "", deleteDocumentToolName, map[string]any{"name": "b.md"})
	if payload := toolPayload(t, management); payload["status"] != "deleted" {
		t.Fatalf("management delete payload = %#v", payload)
	}
	if !reflect.DeepEqual(runtime.deleteCalls, []string{"b.md"}) {
		t.Fatalf("management delete calls = %#v", runtime.deleteCalls)
	}
}

func TestScopeModernStatelessSearch(t *testing.T) {
	runtime := newFakeRuntime()
	scopes := map[string]access.ScopeSet{
		"files": testFileScope([]string{"a.md"}, nil),
	}
	_, server := newScopeTestServer(t, runtime, Options{}, scopes)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search",` +
		`"arguments":{"query":"hello"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientCapabilities":{}}}}`
	request, err := http.NewRequest(http.MethodPost, server.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("X-RAG-Namespace", manualsNS)
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", "search")
	request.Header.Set(scopeTestHeader, "files")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	payload := decodeJSONRPCResponse(t, response)
	if payload == nil {
		t.Fatalf("stateless search returned no payload")
	}
	if len(runtime.searchCalls) != 1 {
		t.Fatalf("stateless search did not reach the runtime")
	}
	if got := runtime.searchCalls[0].DocumentIDs; !reflect.DeepEqual(got, []string{"doc:a.md"}) {
		t.Fatalf("stateless DocumentIDs = %#v", got)
	}
}

func TestScopeResourcesOverHTTP(t *testing.T) {
	runtime := newFakeRuntime()
	runtime.listDocuments = []DocumentRecord{{"name": "a.md"}, {"name": "b.md"}}
	runtime.readDocument = DocumentContent{Text: "hello world", Version: "v1"}
	scopes := map[string]access.ScopeSet{
		"full":  testWholeScope(),
		"files": testFileScope([]string{"a.md"}, nil),
	}
	_, server := newScopeTestServer(t, runtime, Options{}, scopes)
	rpc := newRawRPC(t, server.URL, manualsNS)
	initializeScoped(t, rpc, "full")

	resourceNames := func(payload map[string]any) []string {
		t.Helper()
		result, _ := payload["result"].(map[string]any)
		items, _ := result["resources"].([]any)
		names := make([]string, 0, len(items))
		for _, item := range items {
			name, _ := item.(map[string]any)["name"].(string)
			names = append(names, name)
		}
		return names
	}

	_, restricted := rpc.doScoped("files", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "resources/list",
	}, nil)
	if names := resourceNames(restricted); !reflect.DeepEqual(names, []string{"a.md"}) {
		t.Fatalf("restricted resources = %#v", names)
	}
	_, unrestricted := rpc.doScoped("full", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "resources/list",
	}, nil)
	if names := resourceNames(unrestricted); !reflect.DeepEqual(names, []string{"a.md", "b.md"}) {
		t.Fatalf("whole-knowledge-base resources = %#v", names)
	}

	_, denied := rpc.doScoped("files", map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "resources/read",
		"params": map[string]any{"uri": resources.DocumentURI(manualsNS, "b.md")},
	}, nil)
	errorObject, _ := denied["error"].(map[string]any)
	if errorObject == nil {
		t.Fatalf("ungranted resources/read must be a protocol error: %#v", denied)
	}
	if message, _ := errorObject["message"].(string); message != access.MessageKnowledgeAccess {
		t.Fatalf("ungranted resources/read message = %q", message)
	}

	_, allowed := rpc.doScoped("files", map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "resources/read",
		"params": map[string]any{"uri": resources.DocumentURI(manualsNS, "a.md")},
	}, nil)
	if _, ok := allowed["result"].(map[string]any); !ok {
		t.Fatalf("granted resources/read = %#v", allowed)
	}
}

// TestScopeResolveFailureIsToolError ensures a broken permission lookup never
// degrades into an unrestricted search.
func TestScopeResolveFailureIsToolError(t *testing.T) {
	runtime := newFakeRuntime()
	runtime.resolveErr = context.DeadlineExceeded
	endpoint := newTestEndpoint(t, runtime, Options{})
	entry := entryByName(t, endpoint, searchToolName)

	request, err := contracts.SearchRequestFromArguments(map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("search request: %v", err)
	}
	ctx := access.ContextWithScope(context.Background(), testFileScope([]string{"a.md"}, nil))
	if _, err := entry.execute(ctx, manualsNS, request); err == nil {
		t.Fatalf("resolve failure must fail the search")
	}
	if len(runtime.searchCalls) != 0 {
		t.Fatalf("resolve failure must not run an unrestricted search: %#v", runtime.searchCalls)
	}
}

// TestScopeHeaderRoundTrip pins the internal wire form: the adapter writes it on
// every request and every field survives the round trip.
func TestScopeHeaderRoundTrip(t *testing.T) {
	scope := testFileScope([]string{"a.md", "c.md"}, []string{"b.md"})
	encoded := encodeScopeHeader(scope)
	decoded, present := decodeScopeHeader(encoded)
	if !present {
		t.Fatalf("scope header must report a present scope")
	}
	if decoded.ReadsWhole() || decoded.WritesWhole() {
		t.Fatalf("decoded scope is unexpectedly whole: %#v", decoded)
	}
	if !decoded.CanReadDocument("a.md") || !decoded.CanReadDocument("c.md") || !decoded.CanWriteDocument("b.md") {
		t.Fatalf("decoded scope lost grants: %#v", decoded)
	}
	if !decoded.CanReadDocument("b.md") || decoded.CanWriteDocument("a.md") || decoded.CanReadDocument("z.md") {
		t.Fatalf("decoded scope grants too much: %#v", decoded)
	}
	if _, present := decodeScopeHeader(scopeHeaderNone); present {
		t.Fatalf("none marker must not report a scope")
	}
	if _, present := decodeScopeHeader(""); present {
		t.Fatalf("empty header must not report a scope")
	}
	if tampered, present := decodeScopeHeader("!!!not-base64!!!"); !present || tampered.AllowsAnyRead() || tampered.HasAnyWrite() {
		t.Fatalf("tampered header must deny everything: %#v present=%v", tampered, present)
	}
}
