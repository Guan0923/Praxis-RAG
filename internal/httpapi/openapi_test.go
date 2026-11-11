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
	// The OCR engine is a knowledge-base setting and a saved-model kind.
	createProperties, _ := createBody["properties"].(map[string]any)
	if _, present := createProperties["ocr_model"]; !present {
		t.Fatalf("CreateKnowledgeBaseBody.properties missing ocr_model: %v", createProperties)
	}
	updateBody, _ := schemas["UpdateKnowledgeBaseBody"].(map[string]any)
	updateProperties, _ := updateBody["properties"].(map[string]any)
	if _, present := updateProperties["ocr_model"]; !present {
		t.Fatalf("UpdateKnowledgeBaseBody.properties missing ocr_model: %v", updateProperties)
	}
	modelProfileBody, _ := schemas["CreateModelProfileBody"].(map[string]any)
	modelProfileProperties, _ := modelProfileBody["properties"].(map[string]any)
	kind, _ := modelProfileProperties["kind"].(map[string]any)
	kindEnum, _ := kind["enum"].([]any)
	if len(kindEnum) != 5 || kindEnum[4] != "jev" {
		t.Fatalf("CreateModelProfileBody.kind enum = %v", kindEnum)
	}

	// Sharing/invitation surface documented from the same table.
	for _, path := range []string{
		"/v1/users",
		"/v1/knowledge-bases",
		"/v1/knowledge-bases/{namespace}/access",
		"/v1/knowledge-bases/{namespace}/permissions",
		"/v1/knowledge-bases/{namespace}/permissions/{user_id}",
		"/v1/knowledge-bases/{namespace}/invitations",
		"/v1/invitations",
		"/v1/invitations/preview",
		"/v1/invitations/accept",
		"/v1/invitations/decline",
		"/v1/invitations/{invitation_id}",
	} {
		if _, present := paths[path]; !present {
			t.Fatalf("paths missing %s", path)
		}
	}
	// Console batch delete: one literal batch path per resource plus the
	// single-knowledge-base purge route.
	for _, path := range []string{
		"/v1/admin/knowledge-bases/batch-delete",
		"/v1/admin/api-keys/batch-delete",
	} {
		if _, present := paths[path]; !present {
			t.Fatalf("paths missing %s", path)
		}
	}
	purgePath, _ := paths["/v1/admin/knowledge-bases/{namespace}"].(map[string]any)
	if _, present := purgePath["delete"]; !present {
		t.Fatalf("DELETE /v1/admin/knowledge-bases/{namespace} is not documented: %v", purgePath)
	}
	batchKnowledgeBases, _ := schemas["BatchDeleteKnowledgeBasesBody"].(map[string]any)
	if required, _ := batchKnowledgeBases["required"].([]any); len(required) != 1 || required[0] != "namespaces" {
		t.Fatalf("BatchDeleteKnowledgeBasesBody.required = %v", batchKnowledgeBases["required"])
	}
	batchAPIKeys, _ := schemas["BatchDeleteAPIKeysBody"].(map[string]any)
	if required, _ := batchAPIKeys["required"].([]any); len(required) != 1 || required[0] != "ids" {
		t.Fatalf("BatchDeleteAPIKeysBody.required = %v", batchAPIKeys["required"])
	}

	invitationBody, _ := schemas["CreateInvitationBody"].(map[string]any)
	invitationProperties, _ := invitationBody["properties"].(map[string]any)
	permission, _ := invitationProperties["permission"].(map[string]any)
	permissionEnum, _ := permission["enum"].([]any)
	if len(permissionEnum) != 2 || permissionEnum[0] != "read" || permissionEnum[1] != "write" {
		t.Fatalf("CreateInvitationBody.permission enum = %v", permission["enum"])
	}
	tokenBody, _ := schemas["InvitationTokenBody"].(map[string]any)
	if tokenRequired, _ := tokenBody["required"].([]any); len(tokenRequired) != 1 || tokenRequired[0] != "token" {
		t.Fatalf("InvitationTokenBody.required = %v", tokenBody["required"])
	}
	userUpdate, _ := schemas["UserUpdateBody"].(map[string]any)
	userProperties, _ := userUpdate["properties"].(map[string]any)
	role, _ := userProperties["role"].(map[string]any)
	roleEnum, _ := role["enum"].([]any)
	if len(roleEnum) != 3 || roleEnum[0] != "admin" || roleEnum[1] != "vip" || roleEnum[2] != "user" {
		t.Fatalf("UserUpdateBody.role enum = %v", role["enum"])
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
