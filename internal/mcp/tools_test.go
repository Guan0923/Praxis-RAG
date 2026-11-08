package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Guan0923/xinghe_rag/internal/catalog"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
)

func entryByName(t *testing.T, endpoint *Endpoint, name string) toolEntry {
	t.Helper()
	for _, entry := range endpoint.entries {
		if entry.name == name {
			return entry
		}
	}
	t.Fatalf("tool %q is not registered", name)
	return toolEntry{}
}

func TestToolRegistryShape(t *testing.T) {
	endpoint := newTestEndpoint(t, newFakeRuntime(), Options{QueuedSearch: true})
	wantNames := []string{
		"search", "get_search_result", "add_document", "update_document", "delete_document", "get_document_status",
	}
	if len(endpoint.entries) != len(wantNames) {
		t.Fatalf("tools = %d, want %d", len(endpoint.entries), len(wantNames))
	}
	for index, name := range wantNames {
		if endpoint.entries[index].name != name {
			t.Fatalf("tool[%d] = %q, want %q", index, endpoint.entries[index].name, name)
		}
	}

	search := entryByName(t, endpoint, searchToolName).definition
	if !strings.Contains(search.Description, "Searches are queued.") {
		t.Fatalf("queued search description missing suffix: %q", search.Description)
	}
	schema := search.InputSchema.(map[string]any)
	properties := schema["properties"].(map[string]any)
	wait := properties["wait_seconds"].(map[string]any)
	if wait["default"] != 10 || wait["minimum"] != 0 || wait["maximum"] != 30 || wait["type"] != "number" {
		t.Fatalf("wait_seconds schema = %#v", wait)
	}
	if required := schema["required"].([]string); len(required) != 1 || required[0] != "query" {
		t.Fatalf("required = %#v", schema["required"])
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("search must reject additional properties")
	}
	annotations := search.Annotations
	if !annotations.ReadOnlyHint || !annotations.IdempotentHint || annotations.DestructiveHint == nil ||
		*annotations.DestructiveHint || annotations.OpenWorldHint == nil || *annotations.OpenWorldHint {
		t.Fatalf("search annotations = %#v", annotations)
	}

	plain := newTestEndpoint(t, newFakeRuntime(), Options{})
	plainProperties := entryByName(t, plain, searchToolName).definition.InputSchema.(map[string]any)["properties"].(map[string]any)
	if _, ok := plainProperties["wait_seconds"]; ok {
		t.Fatalf("synchronous search must not expose wait_seconds")
	}

	add := entryByName(t, endpoint, addDocumentToolName).definition
	addSchema := add.InputSchema.(map[string]any)
	if _, ok := addSchema["oneOf"]; !ok {
		t.Fatalf("add_document must declare oneOf content/content_base64")
	}
	if add.Annotations.DestructiveHint == nil || *add.Annotations.DestructiveHint {
		t.Fatalf("add_document must not be destructive")
	}
	if add.Annotations.ReadOnlyHint {
		t.Fatalf("add_document must not be read-only")
	}
	update := entryByName(t, endpoint, updateDocumentToolName).definition
	if update.Annotations.DestructiveHint == nil || !*update.Annotations.DestructiveHint {
		t.Fatalf("update_document must be destructive")
	}
	deleteTool := entryByName(t, endpoint, deleteDocumentToolName).definition
	deleteSchema := deleteTool.InputSchema.(map[string]any)
	if _, ok := deleteSchema["oneOf"]; ok {
		t.Fatalf("delete_document must not declare oneOf")
	}
	status := entryByName(t, endpoint, documentStatusToolName).definition
	if !status.Annotations.ReadOnlyHint || !status.Annotations.IdempotentHint {
		t.Fatalf("get_document_status annotations = %#v", status.Annotations)
	}
}

func TestParseDocumentArguments(t *testing.T) {
	validPDF := base64.StdEncoding.EncodeToString([]byte("%PDF-1.7 minimal"))
	cases := []struct {
		name      string
		arguments map[string]any
		writing   bool
		wantError string
		wantName  string
		wantSize  int
	}{
		{"dash dash escape", map[string]any{"name": "../escape.txt", "content": "x"}, true, "name must be an exact filename, not a path", "", 0},
		{"windows device", map[string]any{"name": "CON.txt", "content": "x"}, true, "name must be an exact filename, not a path", "", 0},
		{"unsupported suffix", map[string]any{"name": "a.exe", "content": "x"}, true, "Supported files: .txt, .md, .pdf, .docx, .doc", "", 0},
		{"empty content", map[string]any{"name": "a.txt", "content": ""}, true, "File must be non-empty and no larger than 10 MiB", "", 0},
		{"unknown namespace argument", map[string]any{"name": "a.txt", "content": "x", "namespace": "other"}, true, "Unknown document arguments", "", 0},
		{"both content forms", map[string]any{"name": "a.txt", "content": "x", "content_base64": "eA=="}, true, "Provide exactly one of content or content_base64", "", 0},
		{"bad base64", map[string]any{"name": "a.txt", "content_base64": "not base64!"}, true, "content_base64 is not valid Base64", "", 0},
		{"empty base64", map[string]any{"name": "a.txt", "content_base64": ""}, true, "File must be non-empty and no larger than 10 MiB", "", 0},
		{"pdf needs base64", map[string]any{"name": "a.pdf", "content": "x"}, true, "PDF, DOCX and DOC require content_base64", "", 0},
		{"path argument", map[string]any{"name": "a.txt", "path": "C:/private.txt"}, true, "Unknown document arguments", "", 0},
		{"too large content", map[string]any{"name": "a.txt", "content": strings.Repeat("x", MaxFileBytes+1)}, true, "content must be text no larger than 10 MiB", "", 0},
		{"non string name", map[string]any{"name": 7, "content": "x"}, true, "name must be an exact filename, not a path", "", 0},
		{"idempotency empty", map[string]any{"name": "a.txt", "content": "x", "idempotency_key": ""}, true, "idempotency_key must be a string of 1 to 128 characters", "", 0},
		{"idempotency null", map[string]any{"name": "a.txt", "content": "x", "idempotency_key": nil}, true, "", "a.txt", 1},
		{"valid text", map[string]any{"name": "a.txt", "content": "hello"}, true, "", "a.txt", 5},
		{"valid base64 pdf", map[string]any{"name": "manual.pdf", "content_base64": validPDF}, true, "", "manual.pdf", len("%PDF-1.7 minimal")},
		{"delete name only", map[string]any{"name": "a.txt"}, false, "", "a.txt", 0},
		{"delete rejects content", map[string]any{"name": "a.txt", "content": "x"}, false, "Unknown document arguments", "", 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request, err := parseDocumentArguments(testCase.arguments, testCase.writing)
			if testCase.wantError != "" {
				if err == nil || err.Error() != testCase.wantError {
					t.Fatalf("error = %v, want %q", err, testCase.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if request.name != testCase.wantName {
				t.Fatalf("name = %q, want %q", request.name, testCase.wantName)
			}
			if len(request.content) != testCase.wantSize {
				t.Fatalf("content length = %d, want %d", len(request.content), testCase.wantSize)
			}
		})
	}
}

func TestParseDocumentStatusArguments(t *testing.T) {
	valid := "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a"
	request, err := parseDocumentStatusArguments(map[string]any{"artifact_id": valid})
	if err != nil || request.artifactID != valid {
		t.Fatalf("valid artifact_id rejected: %v", err)
	}
	for arguments, want := range map[string]string{
		"empty":          "Provide only artifact_id",
		"extra":          "Provide only artifact_id",
		"uppercase uuid": "artifact_id must be a canonical UUID",
		"not a uuid":     "artifact_id must be a canonical UUID",
	} {
		t.Run(arguments, func(t *testing.T) {
			values := map[string]any{"artifact_id": valid}
			switch arguments {
			case "empty":
				values = map[string]any{}
			case "extra":
				values["page"] = 1
			case "uppercase uuid":
				values["artifact_id"] = strings.ToUpper(valid)
			case "not a uuid":
				values["artifact_id"] = "abc"
			}
			if _, err := parseDocumentStatusArguments(values); err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	}
}

func TestSanitizeDocumentNameAndReserved(t *testing.T) {
	if sanitizeDocumentName("a<b>.txt") != "a_b_.txt" {
		t.Fatalf("sanitize = %q", sanitizeDocumentName("a<b>.txt"))
	}
	if sanitizeDocumentName("  trailing. ") != "trailing" {
		t.Fatalf("sanitize trim = %q", sanitizeDocumentName("  trailing. "))
	}
	if !windowsReservedPath("CON") || !windowsReservedPath("con.txt") || !windowsReservedPath("CONOUT$.x") {
		t.Fatalf("windows reserved detection failed")
	}
	for _, name := range []string{"normal.txt", ".hidden.txt", " CON", "COM0", "LPT10", "a.txt"} {
		if windowsReservedPath(name) {
			t.Fatalf("%q must not be reserved", name)
		}
	}
}

func TestPaginate(t *testing.T) {
	cases := []struct {
		cursor    string
		length    int
		wantStart int
		wantEnd   int
		wantNext  string
		wantError string
	}{
		{"", 60, 0, 50, "50", ""},
		{"0", 60, 0, 50, "50", ""},
		{"50", 60, 50, 60, "", ""},
		{"60", 60, 60, 60, "", ""},
		{"61", 60, 0, 0, "", "List cursor is out of range"},
		{"abc", 60, 0, 0, "", "Invalid list cursor"},
		{"-1", 60, 0, 0, "", "Invalid list cursor"},
		{"1234567890", 60, 0, 0, "", "Invalid list cursor"},
		{"", 0, 0, 0, "", ""},
	}
	for _, testCase := range cases {
		start, end, next, err := paginate(testCase.length, testCase.cursor)
		if testCase.wantError != "" {
			if err == nil || err.Error() != testCase.wantError {
				t.Fatalf("cursor %q: error = %v, want %q", testCase.cursor, err, testCase.wantError)
			}
			continue
		}
		if err != nil {
			t.Fatalf("cursor %q: unexpected error %v", testCase.cursor, err)
		}
		if start != testCase.wantStart || end != testCase.wantEnd || next != testCase.wantNext {
			t.Fatalf("cursor %q = (%d,%d,%q), want (%d,%d,%q)",
				testCase.cursor, start, end, next, testCase.wantStart, testCase.wantEnd, testCase.wantNext)
		}
	}
}

func TestRenderPayloadExtractsImageAssets(t *testing.T) {
	imageData := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	payload := map[string]any{
		"results": []any{
			map[string]any{
				"text":  "first",
				"asset": map[string]any{"type": "image", "mime_type": "image/png", "data": imageData},
			},
			map[string]any{"text": "second"},
		},
	}
	result := renderPayload(payload)
	if len(result.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(result.Content))
	}
	if result.IsError {
		t.Fatalf("render must not set IsError")
	}
	text, ok := result.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("first content = %T, want text", result.Content[0])
	}
	var public map[string]any
	if err := json.Unmarshal([]byte(text.Text), &public); err != nil {
		t.Fatalf("text block is not JSON: %v", err)
	}
	assets := public["results"].([]any)
	imageAsset := assets[0].(map[string]any)["asset"].(map[string]any)
	if _, found := imageAsset["data"]; found {
		t.Fatalf("image data must be removed from the text payload")
	}
	if imageAsset["content_block"] != float64(1) {
		t.Fatalf("content_block = %v, want 1", imageAsset["content_block"])
	}
	secondAsset, hasAsset := assets[1].(map[string]any)["asset"]
	if hasAsset {
		t.Fatalf("non-image result must stay untouched: %v", secondAsset)
	}
	imageBlock, ok := result.Content[1].(*sdk.ImageContent)
	if !ok {
		t.Fatalf("second content = %T, want image", result.Content[1])
	}
	if string(imageBlock.Data) != "png-bytes" || imageBlock.MIMEType != "image/png" {
		t.Fatalf("image = %q %q", imageBlock.Data, imageBlock.MIMEType)
	}

	// Rendering must not mutate the runtime's payload.
	original := payload["results"].([]any)[0].(map[string]any)["asset"].(map[string]any)
	if _, found := original["data"]; !found {
		t.Fatalf("render mutated the runtime payload")
	}
}

func TestQueuedSearchWaitSemantics(t *testing.T) {
	t.Run("zero wait returns only result id", func(t *testing.T) {
		runtime := newFakeRuntime()
		runtime.searchResults = []map[string]any{{"status": "queued"}}
		endpoint := newTestEndpoint(t, runtime, Options{QueuedSearch: true})
		payload, err := endpoint.executeQueuedSearch(context.Background(), "manuals", contracts.QueuedSearchRequest{
			Search:      contracts.SearchRequest{Query: "manual"},
			WaitSeconds: 0,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(payload) != 1 || payload["result_id"] != runtime.submitResultID {
			t.Fatalf("payload = %#v", payload)
		}
		if runtime.getResultCalls != 0 {
			t.Fatalf("zero wait must not poll results")
		}
		if len(runtime.submitted) != 1 || len(runtime.namespaces) != 1 || runtime.namespaces[0] != "manuals" {
			t.Fatalf("submission = %#v namespaces = %#v", runtime.submitted, runtime.namespaces)
		}
	})

	t.Run("completion within wait returns results", func(t *testing.T) {
		runtime := newFakeRuntime()
		runtime.searchResults = []map[string]any{
			{"status": "queued"},
			{"status": "succeeded", "query": "manual", "results": []any{}},
		}
		endpoint := newTestEndpoint(t, runtime, Options{QueuedSearch: true})
		payload, err := endpoint.executeQueuedSearch(context.Background(), "manuals", contracts.QueuedSearchRequest{
			Search:      contracts.SearchRequest{Query: "manual"},
			WaitSeconds: 1,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if payload["status"] != "succeeded" || payload["query"] != "manual" {
			t.Fatalf("payload = %#v", payload)
		}
	})

	t.Run("wait expiry degrades to result id", func(t *testing.T) {
		runtime := newFakeRuntime()
		runtime.searchResults = []map[string]any{{"status": "queued"}}
		endpoint := newTestEndpoint(t, runtime, Options{QueuedSearch: true})
		started := time.Now()
		payload, err := endpoint.executeQueuedSearch(context.Background(), "manuals", contracts.QueuedSearchRequest{
			Search:      contracts.SearchRequest{Query: "manual"},
			WaitSeconds: 0.05,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if time.Since(started) > time.Second {
			t.Fatalf("wait was not bounded")
		}
		if len(payload) != 1 || payload["result_id"] != runtime.submitResultID {
			t.Fatalf("payload = %#v", payload)
		}
		if len(runtime.submitted) != 1 {
			t.Fatalf("submissions = %d, want 1", len(runtime.submitted))
		}
	})

	t.Run("submission error is a tool error", func(t *testing.T) {
		runtime := newFakeRuntime()
		runtime.submitErr = errors.New("Search task exceeded its execution deadline")
		endpoint := newTestEndpoint(t, runtime, Options{QueuedSearch: true})
		if _, err := endpoint.executeQueuedSearch(context.Background(), "manuals", contracts.QueuedSearchRequest{
			Search:      contracts.SearchRequest{Query: "manual"},
			WaitSeconds: 1,
		}); err == nil || !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestDocumentOperationResults(t *testing.T) {
	validUUID := "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a"
	t.Run("update adds uri and drops documents", func(t *testing.T) {
		runtime := newFakeRuntime()
		runtime.writePayload = map[string]any{
			"status":    "updated",
			"document":  map[string]any{"name": "manual.txt"},
			"documents": []any{map[string]any{"name": "manual.txt"}},
		}
		endpoint := newTestEndpoint(t, runtime, Options{})
		payload, err := endpoint.executeDocumentOperation(context.Background(), "manuals", updateDocumentToolName, documentRequest{
			name: "manual.txt", content: []byte("replacement"),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if payload["uri"] != "rag://knowledge/manuals/manual.txt" {
			t.Fatalf("uri = %v", payload["uri"])
		}
		if _, found := payload["documents"]; found {
			t.Fatalf("documents must not leak into the tool result")
		}
		if payload["status"] != "updated" {
			t.Fatalf("status = %v", payload["status"])
		}
	})

	t.Run("delete renames deleted record", func(t *testing.T) {
		runtime := newFakeRuntime()
		runtime.listDocuments = []DocumentSummary{{Name: "manual.txt"}}
		runtime.deletePayload = map[string]any{
			"deleted":   map[string]any{"name": "manual.txt"},
			"documents": []any{},
		}
		endpoint := newTestEndpoint(t, runtime, Options{})
		payload, err := endpoint.executeDocumentOperation(context.Background(), "manuals", deleteDocumentToolName, documentRequest{
			name: "manual.txt",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if payload["status"] != "deleted" {
			t.Fatalf("status = %v", payload["status"])
		}
		if _, found := payload["deleted"]; found {
			t.Fatalf("deleted key must be renamed")
		}
		document, ok := payload["document"].(map[string]any)
		if !ok || document["name"] != "manual.txt" {
			t.Fatalf("document = %#v", payload["document"])
		}
		if payload["uri"] != "rag://knowledge/manuals/manual.txt" {
			t.Fatalf("uri = %v", payload["uri"])
		}
	})

	t.Run("delete requires an existing document", func(t *testing.T) {
		runtime := newFakeRuntime()
		endpoint := newTestEndpoint(t, runtime, Options{})
		if _, err := endpoint.executeDocumentOperation(context.Background(), "manuals", deleteDocumentToolName, documentRequest{
			name: "missing.txt",
		}); err == nil || err.Error() != "Document does not exist: missing.txt" {
			t.Fatalf("error = %v", err)
		}
		if len(runtime.deleteCalls) != 0 {
			t.Fatalf("missing document must not reach the runtime delete")
		}
	})

	t.Run("add conflict message", func(t *testing.T) {
		runtime := newFakeRuntime()
		runtime.addErr = ErrUploadConflict
		endpoint := newTestEndpoint(t, runtime, Options{})
		_, err := endpoint.executeDocumentOperation(context.Background(), "manuals", addDocumentToolName, documentRequest{
			name: "manual.txt", content: []byte("x"),
		})
		want := "Document conflicts with an existing filename, queued upload or idempotency key."
		if err == nil || err.Error() != want {
			t.Fatalf("error = %v, want %q", err, want)
		}
	})

	t.Run("get document status passes payload through", func(t *testing.T) {
		runtime := newFakeRuntime()
		runtime.statusPayload = map[string]any{
			"artifact_id": validUUID, "status": "queued", "stage": "queued", "error": nil,
		}
		endpoint := newTestEndpoint(t, runtime, Options{})
		entry := entryByName(t, endpoint, documentStatusToolName)
		input, err := entry.validate(map[string]any{"artifact_id": validUUID})
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		payload, err := entry.execute(context.Background(), "manuals", input)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if payload["status"] != "queued" || payload["artifact_id"] != validUUID {
			t.Fatalf("payload = %#v", payload)
		}
	})
}

func TestDisabledToolIsHiddenAndRejected(t *testing.T) {
	store := catalog.NewMemoryStore()
	namespaceCatalog, err := catalog.NewNamespaceCatalog(store, "manuals")
	if err != nil {
		t.Fatalf("NewNamespaceCatalog: %v", err)
	}
	disabled := false
	if _, err := namespaceCatalog.PatchTool(context.Background(), searchToolName, catalog.ToolPatch{Enabled: &disabled}, catalog.ToolSettings{}); err != nil {
		t.Fatalf("PatchTool: %v", err)
	}
	runtime := newFakeRuntime()
	endpoint := newTestEndpoint(t, runtime, Options{QueuedSearch: true, Catalog: store})

	result, err := endpoint.listTools(context.Background(), "manuals", nil)
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	for _, tool := range result.Tools {
		if tool.Name == searchToolName {
			t.Fatalf("disabled search tool must not be listed")
		}
	}
	if len(result.Tools) != 5 {
		t.Fatalf("listed %d tools, want 5", len(result.Tools))
	}

	entry := entryByName(t, endpoint, searchToolName)
	handler := endpoint.toolHandler("manuals", entry)
	response, err := handler(context.Background(), &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Name: searchToolName}})
	if err != nil {
		t.Fatalf("disabled tool must be a tool error, got protocol error %v", err)
	}
	if response == nil || !response.IsError {
		t.Fatalf("disabled tool result = %#v", response)
	}
	text, ok := response.Content[0].(*sdk.TextContent)
	if !ok || text.Text != "Unknown or disabled RAG tool: search" {
		t.Fatalf("disabled tool text = %#v", response.Content[0])
	}
}
