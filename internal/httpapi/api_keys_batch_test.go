package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// createAPIKeyForBatchTest issues one credential through the trusted local
// management surface and returns its id plus plaintext secret.
func createAPIKeyForBatchTest(t *testing.T, handler http.Handler, name string) (string, string) {
	t.Helper()
	response := performRequest(handler, testRequest{
		method:  "POST",
		target:  "/v1/admin/api-keys",
		remote:  "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"name":"` + name + `","namespaces":["alpha"]}`,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create %s status = %d body = %s", name, response.Code, response.Body.String())
	}
	body := decodeBody(t, response)
	id, _ := body["id"].(string)
	secret, _ := body["key"].(string)
	if id == "" || secret == "" {
		t.Fatalf("create %s body = %v", name, body)
	}
	return id, secret
}

// apiKeyAuthenticates reports whether one plaintext credential still passes the
// tenant authentication path; an unexpected status fails the test.
func apiKeyAuthenticates(t *testing.T, handler http.Handler, secret string) bool {
	t.Helper()
	response := performRequest(handler, testRequest{
		method:  "GET",
		target:  "/v1/chunking/strategies",
		headers: map[string]string{"Authorization": "Bearer " + secret, "X-RAG-Namespace": "alpha"},
	})
	switch response.Code {
	case http.StatusOK:
		return true
	case http.StatusUnauthorized:
		return false
	default:
		t.Fatalf("authenticate status = %d body = %s", response.Code, response.Body.String())
		return false
	}
}

// batchDeleteStringList decodes one JSON string array from a decoded body.
func batchDeleteStringList(t *testing.T, value any) []string {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("value is not a list: %v", value)
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("list item is not a string: %v", item)
		}
		result = append(result, text)
	}
	return result
}

// batchDeleteBody renders {"ids":[...]} for the batch-delete endpoint.
func batchDeleteBody(t *testing.T, ids ...string) string {
	t.Helper()
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		quoted = append(quoted, `"`+id+`"`)
	}
	return `{"ids":[` + strings.Join(quoted, ",") + `]}`
}

func TestAPIKeyBatchDeleteAgainstSQLiteState(t *testing.T) {
	handler := newStateBackedServer(t).Handler()
	firstID, firstSecret := createAPIKeyForBatchTest(t, handler, "batch-one")
	secondID, secondSecret := createAPIKeyForBatchTest(t, handler, "batch-two")
	thirdID, thirdSecret := createAPIKeyForBatchTest(t, handler, "batch-three")

	response := performRequest(handler, testRequest{
		method:  "POST",
		target:  "/v1/admin/api-keys/batch-delete",
		remote:  "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    batchDeleteBody(t, firstID, secondID, "missing-key-id"),
	})
	if response.Code != http.StatusOK {
		t.Fatalf("batch delete status = %d body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	body := decodeBody(t, response)
	deleted := batchDeleteStringList(t, body["deleted"])
	if len(deleted) != 2 || deleted[0] != firstID || deleted[1] != secondID {
		t.Fatalf("deleted = %v", deleted)
	}
	notFound := batchDeleteStringList(t, body["not_found"])
	if len(notFound) != 1 || notFound[0] != "missing-key-id" {
		t.Fatalf("not_found = %v", notFound)
	}
	failed, ok := body["failed"].([]any)
	if !ok || len(failed) != 0 {
		t.Fatalf("failed = %v", body["failed"])
	}
	if body["deleted_count"] != float64(2) {
		t.Fatalf("deleted_count = %v", body["deleted_count"])
	}

	if apiKeyAuthenticates(t, handler, firstSecret) {
		t.Fatal("deleted key still authenticates")
	}
	if apiKeyAuthenticates(t, handler, secondSecret) {
		t.Fatal("deleted key still authenticates")
	}
	if !apiKeyAuthenticates(t, handler, thirdSecret) {
		t.Fatal("remaining key no longer authenticates")
	}

	// A repeated id is deduplicated: one delete, no spurious not_found entry.
	duplicate := performRequest(handler, testRequest{
		method:  "POST",
		target:  "/v1/admin/api-keys/batch-delete",
		remote:  "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    batchDeleteBody(t, thirdID, thirdID),
	})
	if duplicate.Code != http.StatusOK {
		t.Fatalf("dedupe status = %d body = %s", duplicate.Code, duplicate.Body.String())
	}
	duplicateBody := decodeBody(t, duplicate)
	if got := batchDeleteStringList(t, duplicateBody["deleted"]); len(got) != 1 || got[0] != thirdID {
		t.Fatalf("dedupe deleted = %v", got)
	}
	if got := batchDeleteStringList(t, duplicateBody["not_found"]); len(got) != 0 {
		t.Fatalf("dedupe not_found = %v", got)
	}
	if duplicateBody["deleted_count"] != float64(1) {
		t.Fatalf("dedupe deleted_count = %v", duplicateBody["deleted_count"])
	}
	if apiKeyAuthenticates(t, handler, thirdSecret) {
		t.Fatal("deleted key still authenticates")
	}

	// Deleting the same id again reports it as missing, never as deleted.
	again := performRequest(handler, testRequest{
		method:  "POST",
		target:  "/v1/admin/api-keys/batch-delete",
		remote:  "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    batchDeleteBody(t, thirdID),
	})
	if again.Code != http.StatusOK {
		t.Fatalf("redelete status = %d body = %s", again.Code, again.Body.String())
	}
	againBody := decodeBody(t, again)
	if got := batchDeleteStringList(t, againBody["deleted"]); len(got) != 0 {
		t.Fatalf("redelete deleted = %v", got)
	}
	if got := batchDeleteStringList(t, againBody["not_found"]); len(got) != 1 || got[0] != thirdID {
		t.Fatalf("redelete not_found = %v", got)
	}
	if againBody["deleted_count"] != float64(0) {
		t.Fatalf("redelete deleted_count = %v", againBody["deleted_count"])
	}
}

func TestAPIKeyBatchDeleteValidation(t *testing.T) {
	handler := newStateBackedServer(t).Handler()
	manyIDs := make([]string, 0, 101)
	for index := 0; index < 101; index++ {
		manyIDs = append(manyIDs, fmt.Sprintf("key-%03d", index))
	}
	for _, testCase := range []struct{ name, body string }{
		{"missing ids", `{}`},
		{"null ids", `{"ids":null}`},
		{"empty ids", `{"ids":[]}`},
		{"too many ids", batchDeleteBody(t, manyIDs...)},
		{"empty id", `{"ids":[""]}`},
		{"unknown field", `{"ids":["key-1"],"extra":true}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response := performRequest(handler, testRequest{
				method:  "POST",
				target:  "/v1/admin/api-keys/batch-delete",
				remote:  "127.0.0.1:5000",
				headers: map[string]string{"Content-Type": "application/json"},
				body:    testCase.body,
			})
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
			details, ok := decodeBody(t, response)["detail"].([]any)
			if !ok || len(details) == 0 {
				t.Fatalf("detail = %v", decodeBody(t, response)["detail"])
			}
		})
	}

	// Pin the FastAPI-shaped detail items for two representative failures.
	emptyIDs := performRequest(handler, testRequest{
		method:  "POST",
		target:  "/v1/admin/api-keys/batch-delete",
		remote:  "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"ids":[]}`,
	})
	emptyDetail, ok := decodeBody(t, emptyIDs)["detail"].([]any)[0].(map[string]any)
	if !ok {
		t.Fatalf("empty ids detail = %v", emptyIDs.Body.String())
	}
	if emptyDetail["type"] != "too_short" || emptyDetail["msg"] != "List should have at least 1 item after validation, not 0" {
		t.Fatalf("empty ids detail = %v", emptyDetail)
	}
	if loc, _ := emptyDetail["loc"].([]any); len(loc) != 2 || loc[0] != "body" || loc[1] != "ids" {
		t.Fatalf("empty ids loc = %v", emptyDetail["loc"])
	}

	unknownField := performRequest(handler, testRequest{
		method:  "POST",
		target:  "/v1/admin/api-keys/batch-delete",
		remote:  "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"ids":["key-1"],"extra":true}`,
	})
	unknownDetail, ok := decodeBody(t, unknownField)["detail"].([]any)[0].(map[string]any)
	if !ok {
		t.Fatalf("unknown field detail = %v", unknownField.Body.String())
	}
	if unknownDetail["type"] != "extra_forbidden" {
		t.Fatalf("unknown field detail = %v", unknownDetail)
	}
}

func TestAPIKeyBatchDeleteWithoutStore(t *testing.T) {
	handler := newTestServer(t, testSettings(""), nil).Handler()
	response := performRequest(handler, testRequest{
		method:  "POST",
		target:  "/v1/admin/api-keys/batch-delete",
		remote:  "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"ids":["key-1"]}`,
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if detail := decodeBody(t, response)["detail"]; detail != "API key store is not available" {
		t.Fatalf("detail = %v", detail)
	}
}
