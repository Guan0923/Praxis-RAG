package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestOpenAPIAndDocsRoutes(t *testing.T) {
	server := New(testSettings(""), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := server.Handler()

	response := performRequest(handler, testRequest{method: "GET", target: "/openapi.json", remote: "127.0.0.1:5000"})
	if response.Code != http.StatusOK {
		t.Fatalf("openapi status = %d body = %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("openapi content type = %q", contentType)
	}
	var document map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatalf("openapi is not JSON: %v", err)
	}
	info, _ := document["info"].(map[string]any)
	if info["title"] != "Praxis-RAG" {
		t.Fatalf("info.title = %v", info["title"])
	}
	paths, _ := document["paths"].(map[string]any)
	if _, present := paths["/v1/search"]; !present {
		t.Fatalf("paths missing /v1/search: %v", paths)
	}
	components, _ := document["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	searchBody, _ := schemas["SearchBody"].(map[string]any)
	searchProperties, _ := searchBody["properties"].(map[string]any)
	rerank, _ := searchProperties["rerank"].(map[string]any)
	enum, _ := rerank["enum"].([]any)
	if len(enum) != 2 || enum[0] != "none" || enum[1] != "cross_encoder" {
		t.Fatalf("SearchBody.rerank enum = %v", rerank["enum"])
	}
	createBody, _ := schemas["CreateKnowledgeBaseBody"].(map[string]any)
	required, _ := createBody["required"].([]any)
	containsEmbedding := false
	for _, value := range required {
		if value == "embedding" {
			containsEmbedding = true
		}
	}
	if !containsEmbedding {
		t.Fatalf("CreateKnowledgeBaseBody.required = %v", required)
	}

	// The document is generated deterministically from one table.
	second := performRequest(handler, testRequest{method: "GET", target: "/openapi.json", remote: "127.0.0.1:5000"})
	if second.Body.String() != response.Body.String() {
		t.Fatal("openapi document is not deterministic")
	}

	docs := performRequest(handler, testRequest{method: "GET", target: "/docs", remote: "127.0.0.1:5000"})
	if docs.Code != http.StatusOK {
		t.Fatalf("docs status = %d", docs.Code)
	}
	if contentType := docs.Header().Get("Content-Type"); !strings.Contains(contentType, "text/html") {
		t.Fatalf("docs content type = %q", contentType)
	}
	if !strings.Contains(docs.Body.String(), "/openapi.json") {
		t.Fatal("docs page does not reference /openapi.json")
	}
}
