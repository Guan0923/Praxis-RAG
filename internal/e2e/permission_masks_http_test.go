package e2e

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// TestPermissionMaskAcceptanceScenarios covers the five documented
// acceptance scenarios of the operation-bitmask permission model end to end
// through REST: administrator defaults with an explicit file deny, deny
// priority in both directions, the create/update split, and read denial on
// the original-document surface.
func TestPermissionMaskAcceptanceScenarios(t *testing.T) {
	h := newHarness(t, "")
	handler := permissionsHandler(t, h)

	admin := newConsoleClient(t, handler)
	admin.register("mask-admin", permissionsAdminPassword)
	member := newConsoleClient(t, handler)
	member.register("mask-user", permissionsPlainPassword)

	createLegacyKnowledgeBase(t, admin, handler, "default", h.ModelURL)
	recorder, _ := admin.json(http.MethodPost, "/v1/knowledge-bases", "", map[string]any{
		"namespace":       "mask",
		"embedding":       map[string]any{"base_url": h.ModelURL, "apikey": ""},
		"embedding_model": "embed-a",
		"reranker":        map[string]any{"base_url": h.ModelURL, "apikey": ""},
		"reranker_model":  "rank-a",
		"strategy":        "character",
		"chunk_size":      120,
		"chunk_overlap":   20,
	})
	requireStatus(t, recorder, http.StatusCreated, "admin creates knowledge base")

	// Two documents exist in the knowledge base.
	for _, name := range []string{"A.pdf.md", "B.pdf.md"} {
		body, contentType := restMultipart(t, []restUploadFile{{name: name, content: documentText}})
		recorder = admin.do(http.MethodPost, "/v1/document-batches", "mask", body, contentType)
		requireStatus(t, recorder, http.StatusAccepted, "admin uploads "+name)
		waitRestBatchSucceeded(t, h, recorder)
	}

	// A rule carrying only a deny takes effect immediately (setting the deny
	// needs no invitation acceptance).
	recorder, payload := admin.json(http.MethodPut, "/v1/knowledge-bases/mask/permissions/"+member.id, "",
		map[string]any{"document_name": "", "allow_mask": 0, "deny_mask": 8})
	requireStatus(t, recorder, http.StatusOK, "deny-only rule applies immediately")
	denyOnly, _ := payload["permission"].(map[string]any)
	if denyOnly["deny_mask"] != float64(8) {
		t.Fatalf("deny-only rule = %v", denyOnly)
	}
	// A file-scoped rule with the create bit is rejected.
	recorder, _ = admin.json(http.MethodPut, "/v1/knowledge-bases/mask/permissions/"+member.id, "",
		map[string]any{"document_name": "A.pdf.md", "allow_mask": 2})
	requireStatus(t, recorder, http.StatusUnprocessableEntity, "file rule with create bit rejected")

	// Scenario 1: the administrator keeps the four default operations; an
	// explicit file-level deny of delete removes exactly that operation for
	// that file only.
	recorder, payload = admin.json(http.MethodPut, "/v1/knowledge-bases/mask/permissions/"+admin.id, "",
		map[string]any{"document_name": "A.pdf.md", "allow_mask": 5, "deny_mask": 8})
	requireStatus(t, recorder, http.StatusOK, "admin sets own file rule")
	rule, _ := payload["permission"].(map[string]any)
	if rule["allow_mask"] != float64(5) || rule["deny_mask"] != float64(8) {
		t.Fatalf("admin rule view = %v", rule)
	}
	// The admin can still read and update A.pdf (its default full allow is
	// merged) but not delete it; B.pdf keeps deletion.
	recorder, payload = admin.json(http.MethodGet, "/v1/knowledge-bases/mask/access", "", nil)
	requireStatus(t, recorder, http.StatusOK, "admin access view")
	scope, _ := payload["scope"].(map[string]any)
	if scope["default_allow"] != float64(15) {
		t.Fatalf("admin default allow = %v", scope["default_allow"])
	}
	fileDeny, _ := scope["file_deny"].(map[string]any)
	if fileDeny["A.pdf.md"] != float64(8) {
		t.Fatalf("admin file deny = %v", fileDeny)
	}
	recorder = admin.do(http.MethodGet, "/v1/documents/A.pdf.md/content", "mask", nil, "")
	requireStatus(t, recorder, http.StatusOK, "admin reads denied-delete file")
	recorder, _ = admin.json(http.MethodDelete, "/v1/documents/A.pdf.md", "mask", nil)
	requireStatus(t, recorder, http.StatusForbidden, "admin cannot delete the denied file")
	recorder = admin.do(http.MethodDelete, "/v1/documents/B.pdf.md", "mask", nil, "")
	requireStatus(t, recorder, http.StatusOK, "admin still deletes the other file")

	// B.pdf.md is gone; re-admit it so the later scenarios have two documents
	// again (the name no longer exists, so admission is accepted).
	bBody, bType := restMultipart(t, []restUploadFile{{name: "B.pdf.md", content: documentText}})
	recorder = admin.do(http.MethodPost, "/v1/document-batches", "mask", bBody, bType)
	requireStatus(t, recorder, http.StatusAccepted, "admin re-uploads B.pdf.md")
	waitRestBatchSucceeded(t, h, recorder)

	// A.pdf.md was never deleted (only its deletion was denied), so it stays
	// indexed; clearing the deny restores the full default for later rounds.
	recorder, _ = admin.json(http.MethodPost, "/v1/knowledge-bases/mask/permissions/"+admin.id+"/clear-deny", "",
		map[string]any{"document_name": "A.pdf.md"})
	requireStatus(t, recorder, http.StatusOK, "admin clears own deny")

	// Scenario 2: whole allow + file deny, whole deny + file allow — deny wins
	// both ways; revoking the allow keeps the deny effective.
	recorder, _ = admin.json(http.MethodPut, "/v1/knowledge-bases/mask/permissions/"+member.id, "",
		map[string]any{"document_name": "", "allow_mask": 1, "deny_mask": 0})
	requireStatus(t, recorder, http.StatusOK, "member gets whole read")
	recorder, _ = admin.json(http.MethodPut, "/v1/knowledge-bases/mask/permissions/"+member.id, "",
		map[string]any{"document_name": "A.pdf.md", "allow_mask": 0, "deny_mask": 1})
	requireStatus(t, recorder, http.StatusOK, "member file deny read")
	recorder = member.do(http.MethodGet, "/v1/documents/A.pdf.md/content", "mask", nil, "")
	requireStatus(t, recorder, http.StatusForbidden, "file deny restricts the whole-library read")
	recorder = member.do(http.MethodGet, "/v1/documents/B.pdf.md/content", "mask", nil, "")
	requireStatus(t, recorder, http.StatusOK, "other files stay readable under the whole allow")

	// The other direction: a whole-library deny beats a file allow for the
	// same operation — the file allow cannot lift the library-wide denial.
	recorder, _ = admin.json(http.MethodPut, "/v1/knowledge-bases/mask/permissions/"+member.id, "",
		map[string]any{"document_name": "", "allow_mask": 0, "deny_mask": 1})
	requireStatus(t, recorder, http.StatusOK, "whole deny read")
	recorder, _ = admin.json(http.MethodPut, "/v1/knowledge-bases/mask/permissions/"+member.id, "",
		map[string]any{"document_name": "B.pdf.md", "allow_mask": 1, "deny_mask": 0})
	requireStatus(t, recorder, http.StatusOK, "file allow read")
	recorder = member.do(http.MethodGet, "/v1/documents/B.pdf.md/content", "mask", nil, "")
	requireStatus(t, recorder, http.StatusForbidden, "whole deny beats the file allow for the same operation")
	// Revoke the allow: the deny keeps refusing.
	recorder = admin.do(http.MethodDelete, "/v1/knowledge-bases/mask/permissions/"+member.id+"?document_name=B.pdf.md", "mask", nil, "")
	requireStatus(t, recorder, http.StatusNoContent, "revoke allow of one file rule")
	recorder = member.do(http.MethodGet, "/v1/documents/B.pdf.md/content", "mask", nil, "")
	requireStatus(t, recorder, http.StatusForbidden, "deny outlives the revoked allow")

	// Scenario 3: create-only uploads new files but cannot overwrite; the
	// update-only account replaces its granted file but cannot delete.
	recorder, _ = admin.json(http.MethodPut, "/v1/knowledge-bases/mask/permissions/"+member.id, "",
		map[string]any{"document_name": "", "allow_mask": 2, "deny_mask": 0})
	requireStatus(t, recorder, http.StatusOK, "create-only rule")
	newBody, newType := restMultipart(t, []restUploadFile{{name: "fresh.md", content: documentText}})
	recorder = member.do(http.MethodPost, "/v1/document-batches", "mask", newBody, newType)
	requireStatus(t, recorder, http.StatusAccepted, "create-only uploads a new file")
	waitRestBatchSucceeded(t, h, recorder)
	// Overwriting the existing A.pdf.md is refused.
	overwriteBody, overwriteType := restMultipart(t, []restUploadFile{{name: "A.pdf.md", content: documentText}})
	recorder = member.do(http.MethodPost, "/v1/document-batches", "mask", overwriteBody, overwriteType)
	requireStatus(t, recorder, http.StatusForbidden, "create-only cannot overwrite an existing file")

	recorder, _ = admin.json(http.MethodPut, "/v1/knowledge-bases/mask/permissions/"+member.id, "",
		map[string]any{"document_name": "A.pdf.md", "allow_mask": 4, "deny_mask": 0})
	requireStatus(t, recorder, http.StatusOK, "update-only rule")
	replaceBody, replaceType := maskReplacementMultipart(t, "replacement.md", documentText+" v2")
	recorder = member.do(http.MethodPost, "/v1/documents/A.pdf.md/replace", "mask", []byte(replaceBody), replaceType)
	requireStatus(t, recorder, http.StatusOK, "update-only replaces its granted file")
	recorder = member.do(http.MethodDelete, "/v1/documents/A.pdf.md", "mask", nil, "")
	requireStatus(t, recorder, http.StatusForbidden, "update-only cannot delete")
	recorder = member.do(http.MethodGet, "/v1/documents/A.pdf.md/content", "mask", nil, "")
	requireStatus(t, recorder, http.StatusForbidden, "update-only has no read bit")

	// Scenario 4 (REST surface): a member denied every read is stopped at the
	// knowledge-base gate; a member whose read survives only for granted
	// files sees just those, and the denied file is refused on the
	// original-document surface and retrieval.
	recorder, _ = admin.json(http.MethodPut, "/v1/knowledge-bases/mask/permissions/"+member.id, "",
		map[string]any{"document_name": "", "allow_mask": 0, "deny_mask": 1})
	requireStatus(t, recorder, http.StatusOK, "whole deny read for scenario 4")
	recorder, _ = member.json(http.MethodGet, "/v1/documents", "mask", nil)
	requireStatus(t, recorder, http.StatusForbidden, "member denied every read cannot list documents")

	// Whole read allow + file deny: the member still reads (the file allow
	// survives), but the denied file is filtered from the list and refused on
	// the content surface and retrieval.
	recorder, _ = admin.json(http.MethodPut, "/v1/knowledge-bases/mask/permissions/"+member.id, "",
		map[string]any{"document_name": "", "allow_mask": 1, "deny_mask": 0})
	requireStatus(t, recorder, http.StatusOK, "whole read allow")
	recorder, _ = admin.json(http.MethodPut, "/v1/knowledge-bases/mask/permissions/"+member.id, "",
		map[string]any{"document_name": "B.pdf.md", "allow_mask": 0, "deny_mask": 1})
	requireStatus(t, recorder, http.StatusOK, "file deny read")
	recorder, payload = member.json(http.MethodGet, "/v1/documents", "mask", nil)
	requireStatus(t, recorder, http.StatusOK, "restricted member lists documents")
	documents, _ := payload["documents"].([]any)
	if len(documents) != 2 {
		t.Fatalf("restricted member must see exactly the two allowed documents: %v", documents)
	}
	if !containsDocument(payload["documents"], "A.pdf.md") || containsDocument(payload["documents"], "B.pdf.md") {
		t.Fatalf("the denied document must be filtered from the visible list: %v", documents)
	}
	recorder = member.do(http.MethodGet, "/v1/documents/A.pdf.md/content", "mask", nil, "")
	requireStatus(t, recorder, http.StatusOK, "allowed document content is readable")
	recorder = member.do(http.MethodGet, "/v1/documents/B.pdf.md/content", "mask", nil, "")
	requireStatus(t, recorder, http.StatusForbidden, "denied document content is refused")
	// Retrieval is restricted (whole read + file deny): the restricted search
	// narrows to the readable set instead of leaking the denied document's
	// evidence.
	searchPayload := map[string]any{"query": documentText[:20], "retrieval": "bm25", "rerank": "none"}
	recorder, payload = member.json(http.MethodPost, "/v1/search", "mask", searchPayload)
	requireStatus(t, recorder, http.StatusOK, "restricted retrieval runs with the narrowed set")
	results, _ := payload["results"].([]any)
	for _, raw := range results {
		item, _ := raw.(map[string]any)
		if source, _ := item["source"].(string); source == "B.pdf.md" {
			t.Fatalf("denied document leaked into retrieval: %v", item)
		}
	}
}

// waitRestBatchSucceeded runs the document worker until every item of one
// admitted batch succeeded (the e2e harness processes jobs synchronously
// through the real worker path).
func waitRestBatchSucceeded(t *testing.T, h *harness, recorder *httptest.ResponseRecorder) {
	t.Helper()
	for _, artifactID := range restBatchArtifactIDs(t, decodeRESTBody(t, recorder)) {
		h.trackDocumentArtifact(artifactID)
		runDocumentWorkerUntil(t, h, "mask batch document indexed", func() bool {
			job, err := h.Stores.DocumentJobs.Get(context.Background(), artifactID, nil)
			return err == nil && job != nil && job.Status == state.DocumentJobSucceeded
		})
	}
}

// restBatchArtifactIDs extracts the artifact ids of one admitted batch.
func restBatchArtifactIDs(t *testing.T, payload map[string]any) []string {
	t.Helper()
	items, _ := payload["items"].([]any)
	ids := make([]string, 0, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		artifactID, _ := item["artifact_id"].(string)
		if artifactID != "" {
			ids = append(ids, artifactID)
		}
	}
	if len(ids) == 0 {
		t.Fatalf("admitted batch has no artifact ids: %v", payload)
	}
	return ids
}

// maskReplacementMultipart builds the replace endpoint's single "file" part.
func maskReplacementMultipart(t *testing.T, filename, content string) (string, string) {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatalf("write content: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buffer.String(), writer.FormDataContentType()
}
