package lightrag

// This file tests the sidecar transport against an httptest fake service: the
// happy path of every endpoint, the error-kind mapping, the response bounds and
// Content-Type check, cancellation and URL escaping. It deliberately touches
// nothing else of the client.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// sidecarCall records the last request received by the fake sidecar.
type sidecarCall struct {
	method      string
	path        string
	requestURI  string
	escapedPath string
	authorize   string
	contentType string
	body        []byte
}

// newSidecar starts a fake sidecar that records the request and delegates the
// reply to respond.
func newSidecar(t *testing.T, respond func(http.ResponseWriter, *http.Request)) (*httptest.Server, *sidecarCall) {
	t.Helper()
	call := &sidecarCall{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call.method = r.Method
		call.path = r.URL.Path
		call.requestURI = r.URL.RequestURI()
		call.escapedPath = r.URL.EscapedPath()
		call.authorize = r.Header.Get("Authorization")
		call.contentType = r.Header.Get("Content-Type")
		call.body, _ = io.ReadAll(r.Body)
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	return server, call
}

// writeJSON writes one JSON reply with the matching Content-Type.
func writeJSON(w http.ResponseWriter, status int, payload string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, payload)
}

// newTestClient builds a client against server. The trailing slash exercises
// base URL normalization.
func newTestClient(t *testing.T, server *httptest.Server, token string) *Client {
	t.Helper()
	client, err := NewClient(Options{BaseURL: server.URL + "/", Token: token})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

// assertErrorKind checks err is a classified *Error of the wanted kind and
// returns it for further assertions.
func assertErrorKind(t *testing.T, err error, kind ErrorKind) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %q", kind)
	}
	var sidecarError *Error
	if !errors.As(err, &sidecarError) {
		t.Fatalf("error type = %T, want *lightrag.Error: %v", err, err)
	}
	if sidecarError.Kind != kind {
		t.Fatalf("error kind = %q, want %q (message %q)", sidecarError.Kind, kind, sidecarError.Message)
	}
	return sidecarError
}

// decodeBody decodes a recorded request body into a generic JSON object.
func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("request body is not a JSON object: %v (%s)", err, body)
	}
	return decoded
}

func TestClientHealth(t *testing.T) {
	server, call := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		// The charset parameter must not defeat the JSON content-type check.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok","service":"lightrag-sidecar","version":"0.3.0",`+
			`"upstream":{"package":"lightrag-hku","pinned":"1.4.9","installed":"1.4.9"},`+
			`"workspaces":2,"config_error":""}`)
	})
	client := newTestClient(t, server, "secret-token")
	status, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if call.method != http.MethodGet || call.path != healthPath {
		t.Fatalf("health request = %s %s", call.method, call.path)
	}
	// The readiness probe is unauthenticated so a supervisor can poll it.
	if call.authorize != "" {
		t.Fatalf("health request sent Authorization %q", call.authorize)
	}
	if status.Status != "ok" || status.Service != "lightrag-sidecar" || status.Workspaces != 2 {
		t.Fatalf("health status = %+v", status)
	}
	if status.Upstream.Package != "lightrag-hku" || status.Upstream.Pinned != "1.4.9" {
		t.Fatalf("health upstream = %+v", status.Upstream)
	}
}

func TestClientEnsureWorkspace(t *testing.T) {
	server, call := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"namespace":"kb-acme","workspace":"rag_kb-acme",`+
			`"created":true,"rebuilt":false}`)
	})
	client := newTestClient(t, server, "secret-token")
	dimension, maxOutputTokens, timeoutSeconds := 1024, 2048, 90
	status, err := client.EnsureWorkspace(context.Background(), WorkspaceConfig{
		Namespace:         " kb-acme ",
		ConfigFingerprint: "fp-1",
		Language:          "zh",
		Embedding: ModelEndpoint{
			BaseURL: "http://embed.internal/v1", Model: "bge-m3", APIKey: "embed-key",
			Dimension: &dimension,
		},
		ExtractLLM: LLMEndpoint{
			BaseURL: "http://llm.internal/v1", Model: "qwen2.5-14b", APIKey: "llm-key",
			MaxOutputTokens: maxOutputTokens, TimeoutSeconds: timeoutSeconds,
		},
		KeywordLLM: LLMEndpoint{BaseURL: "http://llm.internal/v1", Model: "qwen2.5-14b"},
	})
	if err != nil {
		t.Fatalf("EnsureWorkspace() error = %v", err)
	}
	if call.method != http.MethodPost || call.path != workspacesPath {
		t.Fatalf("workspace request = %s %s", call.method, call.path)
	}
	if call.authorize != "Bearer secret-token" {
		t.Fatalf("Authorization = %q", call.authorize)
	}
	if call.contentType != "application/json" {
		t.Fatalf("Content-Type = %q", call.contentType)
	}
	sent := decodeBody(t, call.body)
	if sent["namespace"] != "kb-acme" || sent["language"] != "zh" ||
		sent["config_fingerprint"] != "fp-1" {
		t.Fatalf("workspace body = %v", sent)
	}
	// The embedding half sends base_url/model/api_key/dimension only, the LLM
	// halves send the LLM limits only.
	embedding, ok := sent["embedding"].(map[string]any)
	if !ok {
		t.Fatalf("embedding body = %v", sent["embedding"])
	}
	if embedding["dimension"] != float64(dimension) || embedding["api_key"] != "embed-key" {
		t.Fatalf("embedding body = %v", embedding)
	}
	if _, exists := embedding["max_output_tokens"]; exists {
		t.Fatalf("embedding body leaked an LLM field: %v", embedding)
	}
	extract, ok := sent["extract_llm"].(map[string]any)
	if !ok {
		t.Fatalf("extract_llm body = %v", sent["extract_llm"])
	}
	if extract["max_output_tokens"] != float64(maxOutputTokens) ||
		extract["timeout_seconds"] != float64(timeoutSeconds) {
		t.Fatalf("extract_llm body = %v", extract)
	}
	if _, exists := extract["dimension"]; exists {
		t.Fatalf("extract_llm body leaked the embedding dimension: %v", extract)
	}
	if status.Namespace != "kb-acme" || status.Workspace != "rag_kb-acme" || !status.Created ||
		status.Rebuilt {
		t.Fatalf("workspace status = %+v", status)
	}
}

func TestClientEnsureWorkspaceRejected(t *testing.T) {
	server, _ := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, `{"detail":"language must be zh or en"}`)
	})
	client := newTestClient(t, server, "")
	_, err := client.EnsureWorkspace(context.Background(), WorkspaceConfig{Namespace: "kb-acme"})
	sidecarError := assertErrorKind(t, err, ErrorKindInvalid)
	if sidecarError.Status != http.StatusBadRequest {
		t.Fatalf("error status = %d", sidecarError.Status)
	}
	// The server detail must reach the caller so an operator can fix the call.
	if !strings.Contains(sidecarError.Message, "language must be zh or en") {
		t.Fatalf("error message = %q", sidecarError.Message)
	}
}

func TestClientIngestChunks(t *testing.T) {
	server, call := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"document_id":"doc-1","accepted":2,"skipped":1,`+
			`"entities":5,"relations":3}`)
	})
	client := newTestClient(t, server, "secret-token")
	result, err := client.IngestChunks(context.Background(), "kb-acme", IngestRequest{
		DocumentID:   "doc-1",
		DocumentName: "manual.pdf",
		Chunks: []Chunk{{
			ChunkID: "doc-1:1", Text: "First paragraph.", Page: 2, Heading: "Intro",
			UploadVersion: 3,
		}},
	})
	if err != nil {
		t.Fatalf("IngestChunks() error = %v", err)
	}
	if call.method != http.MethodPost || call.path != workspacesPath+"/kb-acme/chunks" {
		t.Fatalf("ingest request = %s %s", call.method, call.path)
	}
	sent := decodeBody(t, call.body)
	chunks, ok := sent["chunks"].([]any)
	if !ok || len(chunks) != 1 {
		t.Fatalf("ingest chunks = %v", sent["chunks"])
	}
	chunk, ok := chunks[0].(map[string]any)
	if !ok {
		t.Fatalf("ingest chunk = %v", chunks[0])
	}
	if chunk["chunk_id"] != "doc-1:1" || chunk["upload_version"] != float64(3) {
		t.Fatalf("ingest chunk = %v", chunk)
	}
	if result.DocumentID != "doc-1" || result.Accepted != 2 || result.Skipped != 1 ||
		result.Entities != 5 || result.Relations != 3 {
		t.Fatalf("ingest result = %+v", result)
	}
}

func TestClientQuery(t *testing.T) {
	server, call := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"contexts":[{"chunk_id":"doc-1:1","document_id":"doc-1",`+
			`"text":"RRF fuses BM25 and dense candidates.","score":0.875,`+
			`"entities":["RRF","BM25"],`+
			`"relations":[{"source":"RRF","target":"BM25","description":"fuses"}]}],`+
			`"debug":{"mode":"hybrid","entities":2,"relations":1,"chunks":1}}`)
	})
	client := newTestClient(t, server, "secret-token")
	result, err := client.Query(context.Background(), "kb-acme", QueryRequest{
		Query: "RRF 怎么融合？", Mode: "hybrid", TopK: 5,
		Filters: QueryFilters{DocumentIDs: []string{"doc-1"}}, IncludeEntities: true,
	})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if call.method != http.MethodPost || call.path != workspacesPath+"/kb-acme/query" {
		t.Fatalf("query request = %s %s", call.method, call.path)
	}
	sent := decodeBody(t, call.body)
	if sent["query"] != "RRF 怎么融合？" || sent["mode"] != "hybrid" ||
		sent["top_k"] != float64(5) || sent["include_entities"] != true {
		t.Fatalf("query body = %v", sent)
	}
	filters, ok := sent["filters"].(map[string]any)
	if !ok {
		t.Fatalf("query filters = %v", sent["filters"])
	}
	if ids, ok := filters["document_ids"].([]any); !ok || len(ids) != 1 || ids[0] != "doc-1" {
		t.Fatalf("query filters = %v", filters)
	}
	if len(result.Contexts) != 1 {
		t.Fatalf("query contexts = %+v", result.Contexts)
	}
	first := result.Contexts[0]
	if first.ChunkID != "doc-1:1" || first.DocumentID != "doc-1" || first.Score != 0.875 {
		t.Fatalf("query context = %+v", first)
	}
	if len(first.Entities) != 2 || first.Entities[0] != "RRF" {
		t.Fatalf("query entities = %+v", first.Entities)
	}
	if len(first.Relations) != 1 || first.Relations[0].Target != "BM25" ||
		first.Relations[0].Description != "fuses" {
		t.Fatalf("query relations = %+v", first.Relations)
	}
	if result.Debug.Mode != "hybrid" || result.Debug.Entities != 2 ||
		result.Debug.Relations != 1 || result.Debug.Chunks != 1 {
		t.Fatalf("query debug = %+v", result.Debug)
	}
}

func TestClientDeleteDocumentAndClearWorkspace(t *testing.T) {
	server, call := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	client := newTestClient(t, server, "secret-token")
	if err := client.DeleteDocument(context.Background(), "kb-acme", "doc-1"); err != nil {
		t.Fatalf("DeleteDocument() error = %v", err)
	}
	if call.method != http.MethodDelete ||
		call.path != workspacesPath+"/kb-acme/documents/doc-1" {
		t.Fatalf("delete request = %s %s", call.method, call.path)
	}
	if call.authorize != "Bearer secret-token" {
		t.Fatalf("delete Authorization = %q", call.authorize)
	}
	if err := client.ClearWorkspace(context.Background(), "kb-acme"); err != nil {
		t.Fatalf("ClearWorkspace() error = %v", err)
	}
	if call.method != http.MethodDelete || call.path != workspacesPath+"/kb-acme" {
		t.Fatalf("clear request = %s %s", call.method, call.path)
	}
}

func TestClientUnauthorizedResponse(t *testing.T) {
	server, _ := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		// A careless service may echo the credential it rejected; the client
		// must not copy it into an error message.
		writeJSON(w, http.StatusUnauthorized, `{"detail":"invalid sidecar token: wrong-token"}`)
	})
	client := newTestClient(t, server, "wrong-token")
	_, err := client.Query(context.Background(), "kb-acme", QueryRequest{Query: "q"})
	sidecarError := assertErrorKind(t, err, ErrorKindUnauthorized)
	if sidecarError.Status != http.StatusUnauthorized {
		t.Fatalf("error status = %d", sidecarError.Status)
	}
	if !strings.Contains(sidecarError.Message, "invalid sidecar token") {
		t.Fatalf("error message = %q", sidecarError.Message)
	}
	if strings.Contains(sidecarError.Message, "wrong-token") {
		t.Fatalf("error message leaked the sidecar token: %q", sidecarError.Message)
	}
}

func TestClientUnavailableResponse(t *testing.T) {
	server, _ := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "graph store offline", http.StatusServiceUnavailable)
	})
	client := newTestClient(t, server, "")
	_, err := client.Health(context.Background())
	sidecarError := assertErrorKind(t, err, ErrorKindUnavailable)
	if sidecarError.Status != http.StatusServiceUnavailable {
		t.Fatalf("error status = %d", sidecarError.Status)
	}
}

func TestClientRejectsOversizedResponse(t *testing.T) {
	original := responseLimit
	responseLimit = 64
	t.Cleanup(func() { responseLimit = original })
	server, _ := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":"ok","service":"lightrag-sidecar",`+
			`"version":"0.3.0","upstream":{"package":"lightrag-hku","pinned":"1.4.9",`+
			`"installed":"1.4.9"},"workspaces":2,"config_error":""}`)
	})
	client := newTestClient(t, server, "")
	if _, err := client.Health(context.Background()); err == nil {
		t.Fatal("Health() accepted an oversized response")
	} else {
		assertErrorKind(t, err, ErrorKindUnavailable)
		if !strings.Contains(err.Error(), "字节上限") {
			t.Fatalf("error message = %q", err.Error())
		}
	}
}

func TestClientRejectsNonJSONResponse(t *testing.T) {
	server, _ := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<html>upstream proxy error</html>")
	})
	client := newTestClient(t, server, "")
	_, err := client.Health(context.Background())
	sidecarError := assertErrorKind(t, err, ErrorKindUnavailable)
	if !strings.Contains(sidecarError.Message, "非 JSON") {
		t.Fatalf("error message = %q", sidecarError.Message)
	}
}

func TestClientContextCancellation(t *testing.T) {
	server, _ := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":"ok"}`)
	})
	client := newTestClient(t, server, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Health(ctx)
	assertErrorKind(t, err, ErrorKindUnavailable)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want errors.Is(context.Canceled)", err)
	}
}

func TestClientEscapesNamespaceAndDocumentID(t *testing.T) {
	server, call := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"contexts":[],"debug":{}}`)
	})
	client := newTestClient(t, server, "")
	namespace := "团队/知识 库"
	if _, err := client.Query(context.Background(), namespace, QueryRequest{Query: "问题"}); err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	want := workspacesPath + "/" + url.PathEscape(namespace) + "/query"
	if call.escapedPath != want || call.requestURI != want {
		t.Fatalf("query path = %q (URI %q), want %q", call.escapedPath, call.requestURI, want)
	}
	// The encoded namespace must arrive as exactly one path segment: the
	// decoded slash is part of the namespace, not a separator.
	if call.path != workspacesPath+"/"+namespace+"/query" {
		t.Fatalf("decoded query path = %q", call.path)
	}
	if err := client.DeleteDocument(context.Background(), namespace, "doc/../1"); err != nil {
		t.Fatalf("DeleteDocument() error = %v", err)
	}
	documentPath := workspacesPath + "/" + url.PathEscape(namespace) + "/documents/" +
		url.PathEscape("doc/../1")
	if call.escapedPath != documentPath {
		t.Fatalf("document path = %q, want %q", call.escapedPath, documentPath)
	}
}

func TestNewClientValidatesOptions(t *testing.T) {
	invalid := map[string]string{
		"empty":        "",
		"blank":        "   ",
		"not_a_url":    "lightrag:8093",
		"wrong_scheme": "ftp://127.0.0.1:8093",
		"with_query":   "http://127.0.0.1:8093/v1?token=leak",
		"with_frag":    "http://127.0.0.1:8093/#frag",
		"credentials":  "http://user:secret@127.0.0.1:8093",
	}
	for name, baseURL := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := NewClient(Options{BaseURL: baseURL}); err == nil {
				t.Fatalf("NewClient(%q) accepted an invalid base URL", baseURL)
			} else {
				assertErrorKind(t, err, ErrorKindInvalid)
			}
		})
	}
	// An empty base URL must name the environment variable that configures it.
	_, err := NewClient(Options{BaseURL: ""})
	if err == nil || !strings.Contains(err.Error(), "RAG_LIGHTRAG_SERVICE_URL") {
		t.Fatalf("empty base URL error = %v", err)
	}
	// A non-positive timeout falls back to the default, and the base URL loses
	// its trailing slash so the fixed paths are appended exactly once.
	client, err := NewClient(Options{BaseURL: " https://lightrag.internal:8093/ "})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client.httpClient.Timeout != DefaultTimeout {
		t.Fatalf("client timeout = %v, want %v", client.httpClient.Timeout, DefaultTimeout)
	}
	if client.BaseURL() != "https://lightrag.internal:8093" {
		t.Fatalf("base URL = %q", client.BaseURL())
	}
	custom := &http.Client{}
	client, err = NewClient(Options{BaseURL: "http://127.0.0.1:8093", Timeout: -1, HTTPClient: custom})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client.httpClient != custom {
		t.Fatal("NewClient() replaced the provided HTTP client")
	}
}

func TestClientRejectsEmptyArguments(t *testing.T) {
	called := false
	server, _ := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeJSON(w, http.StatusOK, `{}`)
	})
	client := newTestClient(t, server, "")
	cases := map[string]func() error{
		"ensure_workspace": func() error {
			_, err := client.EnsureWorkspace(context.Background(), WorkspaceConfig{Namespace: "  "})
			return err
		},
		"ingest_without_namespace": func() error {
			_, err := client.IngestChunks(context.Background(), "", IngestRequest{DocumentID: "doc-1"})
			return err
		},
		"ingest_without_document": func() error {
			_, err := client.IngestChunks(context.Background(), "kb-acme", IngestRequest{})
			return err
		},
		"delete_without_document": func() error {
			return client.DeleteDocument(context.Background(), "kb-acme", "")
		},
		"query_without_text": func() error {
			_, err := client.Query(context.Background(), "kb-acme", QueryRequest{Query: " "})
			return err
		},
		"clear_without_namespace": func() error {
			return client.ClearWorkspace(context.Background(), "")
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			assertErrorKind(t, call(), ErrorKindInvalid)
		})
	}
	// Local validation must fail before any request reaches the sidecar.
	if called {
		t.Fatal("an invalid argument still sent a request to the sidecar")
	}
}

// TestClientGraphCleanupContract pins the three cleanup-locator methods:
// Inventory pages documents under an advancing cursor, DocumentRevision
// requires the sidecar's revision field, and PruneRevision accepts only the
// terminal outcomes — anything else means the graph changed mid-cleanup.
func TestClientGraphCleanupContract(t *testing.T) {
	server, call := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK,
			`{"documents":[{"document_id":"doc-2","publication_version":"v2","source_hash":"h2"}]}`)
	})
	client := newTestClient(t, server, "secret-token")
	documents, err := client.Inventory(context.Background(), "kb-acme", "doc-1")
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	if len(documents) != 1 || documents[0].ID != "doc-2" ||
		documents[0].Version != "v2" || documents[0].Hash != "h2" {
		t.Fatalf("Inventory() = %+v", documents)
	}
	if call.method != http.MethodGet || call.path != workspacesPath+"/kb-acme/inventory" {
		t.Fatalf("inventory request = %s %s", call.method, call.path)
	}
	if !strings.Contains(call.requestURI, "after=doc-1") {
		t.Fatalf("inventory query = %q, want the after cursor", call.requestURI)
	}

	// A page that does not advance past the cursor is rejected.
	regressedServer, _ := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK,
			`{"documents":[{"document_id":"doc-0","publication_version":"v1","source_hash":"h1"}]}`)
	})
	regressedClient := newTestClient(t, regressedServer, "secret-token")
	if _, err := regressedClient.Inventory(context.Background(), "kb-acme", "doc-1"); err == nil ||
		!strings.Contains(err.Error(), "invalid graph inventory cursor") {
		t.Fatalf("Inventory(regressed cursor) = %v, want the cursor error", err)
	}

	revisionServer, revisionCall := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"revision":"rev-9"}`)
	})
	revisionClient := newTestClient(t, revisionServer, "secret-token")
	revision, err := revisionClient.DocumentRevision(context.Background(), "kb-acme", "doc-1")
	if err != nil || revision != "rev-9" {
		t.Fatalf("DocumentRevision() = %q, %v", revision, err)
	}
	if revisionCall.method != http.MethodGet ||
		revisionCall.path != workspacesPath+"/kb-acme/documents/doc-1/revision" {
		t.Fatalf("revision request = %s %s", revisionCall.method, revisionCall.path)
	}
	missingServer, _ := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{}`)
	})
	missingClient := newTestClient(t, missingServer, "secret-token")
	if _, err := missingClient.DocumentRevision(context.Background(), "kb-acme", "doc-1"); err == nil ||
		!strings.Contains(err.Error(), "missing graph revision") {
		t.Fatalf("DocumentRevision(missing) = %v, want the missing-revision error", err)
	}

	for _, outcome := range []string{"absent", "deleted", "superseded"} {
		pruneServer, _ := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `{"outcome":"`+outcome+`"}`)
		})
		pruneClient := newTestClient(t, pruneServer, "secret-token")
		if err := pruneClient.PruneRevision(context.Background(), "kb-acme", "doc-1",
			"rev-9", "pub-1", true, nil); err != nil {
			t.Fatalf("PruneRevision(%s) error = %v", outcome, err)
		}
	}
	changedServer, _ := newSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"outcome":"replaced"}`)
	})
	changedClient := newTestClient(t, changedServer, "secret-token")
	if err := changedClient.PruneRevision(context.Background(), "kb-acme", "doc-1",
		"rev-9", "pub-1", true, nil); err == nil ||
		!strings.Contains(err.Error(), "graph document changed during cleanup") {
		t.Fatalf("PruneRevision(changed) = %v, want the mid-cleanup change error", err)
	}
}
