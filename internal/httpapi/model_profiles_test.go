package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/auth"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
)

// fakeModelProfileRuntime is an in-memory ModelProfileRuntime for handler tests.
type fakeModelProfileRuntime struct {
	listPayload []map[string]any
	listErr     error

	getID      string
	getPayload map[string]any
	getErr     error

	createInput   ModelProfileInput
	createPayload map[string]any
	createErr     error

	updateID      string
	updateInput   ModelProfileInput
	updatePayload map[string]any
	updateErr     error

	deleteID      string
	deletePayload bool
	deleteErr     error

	keyID      string
	keyPayload string
	keyErr     error

	modelsID      string
	modelsPayload []string
	modelsErr     error

	// viewerUserID / viewerIsAdmin record the identity each handler derived from
	// the session, so the isolation wiring is asserted without a real database.
	viewerUserID  string
	viewerIsAdmin bool
}

func (f *fakeModelProfileRuntime) recordViewer(viewerUserID string, isAdmin bool) {
	f.viewerUserID, f.viewerIsAdmin = viewerUserID, isAdmin
}

func (f *fakeModelProfileRuntime) ListModelProfiles(_ context.Context, viewerUserID string, isAdmin bool) ([]map[string]any, error) {
	f.recordViewer(viewerUserID, isAdmin)
	return f.listPayload, f.listErr
}

func (f *fakeModelProfileRuntime) ModelProfile(_ context.Context, viewerUserID string, isAdmin bool, id string) (map[string]any, error) {
	f.recordViewer(viewerUserID, isAdmin)
	f.getID = id
	return f.getPayload, f.getErr
}

func (f *fakeModelProfileRuntime) CreateModelProfile(_ context.Context, viewerUserID string, isAdmin bool, input ModelProfileInput) (map[string]any, error) {
	f.recordViewer(viewerUserID, isAdmin)
	f.createInput = input
	return f.createPayload, f.createErr
}

func (f *fakeModelProfileRuntime) UpdateModelProfile(_ context.Context, viewerUserID string, isAdmin bool, id string, input ModelProfileInput) (map[string]any, error) {
	f.recordViewer(viewerUserID, isAdmin)
	f.updateID = id
	f.updateInput = input
	return f.updatePayload, f.updateErr
}

func (f *fakeModelProfileRuntime) DeleteModelProfile(_ context.Context, viewerUserID string, isAdmin bool, id string) (bool, error) {
	f.recordViewer(viewerUserID, isAdmin)
	f.deleteID = id
	return f.deletePayload, f.deleteErr
}

func (f *fakeModelProfileRuntime) ModelProfileKey(_ context.Context, viewerUserID string, isAdmin bool, id string) (string, error) {
	f.recordViewer(viewerUserID, isAdmin)
	f.keyID = id
	return f.keyPayload, f.keyErr
}

func (f *fakeModelProfileRuntime) ModelProfileModels(_ context.Context, viewerUserID string, isAdmin bool, id string) ([]string, error) {
	f.recordViewer(viewerUserID, isAdmin)
	f.modelsID = id
	return f.modelsPayload, f.modelsErr
}

func newModelProfileServer(t *testing.T, runtime *fakeModelProfileRuntime) http.Handler {
	t.Helper()
	server := New(testSettings(""), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithModelProfileRuntime(runtime),
	)
	return server.Handler()
}

func TestModelProfileAdminListDetailsKeyAndModels(t *testing.T) {
	runtime := &fakeModelProfileRuntime{
		listPayload: []map[string]any{{
			"id": "p1", "kind": "embedding", "name": "bge-m3",
			"base_url": "http://model.local/v1", "model": "BAAI/bge-m3",
			"api_key_set": true, "vector_size": 1024,
		}},
		getPayload:    map[string]any{"id": "p1", "kind": "embedding", "name": "bge-m3"},
		keyPayload:    "private-key",
		modelsPayload: []string{"embed-a", "embed-b"},
	}
	handler := newModelProfileServer(t, runtime)

	listed := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/model-profiles", remote: "127.0.0.1:5000",
	})
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", listed.Code, listed.Body.String())
	}
	if got := listed.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("list Cache-Control = %q", got)
	}
	if items, ok := decodeBody(t, listed)["model_profiles"].([]any); !ok || len(items) != 1 {
		t.Fatalf("model_profiles = %v", decodeBody(t, listed)["model_profiles"])
	}

	details := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/model-profiles/p1", remote: "127.0.0.1:5000",
	})
	if details.Code != http.StatusOK {
		t.Fatalf("details status = %d body = %s", details.Code, details.Body.String())
	}
	if profile, ok := decodeBody(t, details)["profile"].(map[string]any); !ok || profile["id"] != "p1" {
		t.Fatalf("details = %v", decodeBody(t, details)["profile"])
	}
	if runtime.getID != "p1" {
		t.Fatalf("captured id = %q", runtime.getID)
	}
	if got := details.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("details Cache-Control = %q", got)
	}

	key := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/model-profiles/p1/key", remote: "127.0.0.1:5000",
	})
	if key.Code != http.StatusOK {
		t.Fatalf("key status = %d body = %s", key.Code, key.Body.String())
	}
	if body := decodeBody(t, key); body["apikey"] != "private-key" {
		t.Fatalf("key = %v", body)
	}
	if runtime.keyID != "p1" {
		t.Fatalf("key captured id = %q", runtime.keyID)
	}
	if got := key.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("key Cache-Control = %q", got)
	}

	models := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/model-profiles/p1/models", remote: "127.0.0.1:5000",
		body: `{}`,
	})
	if models.Code != http.StatusOK {
		t.Fatalf("models status = %d body = %s", models.Code, models.Body.String())
	}
	if values, ok := decodeBody(t, models)["models"].([]any); !ok || len(values) != 2 {
		t.Fatalf("models = %v", decodeBody(t, models)["models"])
	}
	if runtime.modelsID != "p1" {
		t.Fatalf("models captured id = %q", runtime.modelsID)
	}
	// The Console may send no body at all: the stored profile is authoritative.
	withoutBody := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/model-profiles/p1/models", remote: "127.0.0.1:5000",
	})
	if withoutBody.Code != http.StatusOK {
		t.Fatalf("models without body status = %d body = %s", withoutBody.Code, withoutBody.Body.String())
	}
	badBody := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/model-profiles/p1/models", remote: "127.0.0.1:5000",
		body: `{"unknown":true}`,
	})
	if badBody.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown models body status = %d", badBody.Code)
	}
}

func TestModelProfileAdminCreate(t *testing.T) {
	runtime := &fakeModelProfileRuntime{
		createPayload: map[string]any{"id": "p1", "kind": "embedding", "name": "bge-m3"},
	}
	handler := newModelProfileServer(t, runtime)

	created := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/model-profiles", remote: "127.0.0.1:5000",
		body: `{"kind":"embedding","name":"bge-m3","base_url":"http://model.local/v1",` +
			`"apikey":"private-key","model":"BAAI/bge-m3"}`,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", created.Code, created.Body.String())
	}
	if profile, ok := decodeBody(t, created)["profile"].(map[string]any); !ok || profile["id"] != "p1" {
		t.Fatalf("create body = %v", decodeBody(t, created))
	}
	if got := created.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("create Cache-Control = %q", got)
	}
	input := runtime.createInput
	if input.Kind != "embedding" || input.Name != "bge-m3" ||
		input.BaseURL != "http://model.local/v1" || input.Model != "BAAI/bge-m3" ||
		input.APIKey != "private-key" || !input.HasAPIKey {
		t.Fatalf("create input = %+v", input)
	}

	// A body without apikey reports HasAPIKey=false so the runtime keeps an
	// existing key when the base URL is unchanged.
	withoutKey := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/model-profiles", remote: "127.0.0.1:5000",
		body: `{"kind":"reranker","name":"rank","base_url":"http://model.local/v1","model":"rank-a"}`,
	})
	if withoutKey.Code != http.StatusCreated {
		t.Fatalf("create without key status = %d body = %s", withoutKey.Code, withoutKey.Body.String())
	}
	if input := runtime.createInput; input.HasAPIKey || input.APIKey != "" {
		t.Fatalf("create without key input = %+v", input)
	}

	// JEV (TypeSafe System One) configurations are saved like VLM profiles: the
	// endpoint and model id are entered by hand, and the kind is not selectable
	// for knowledge bases.
	jev := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/model-profiles", remote: "127.0.0.1:5000",
		body: `{"kind":"jev","name":"system-one","base_url":"https://api.typesafe.ai/v1",` +
			`"apikey":"jev-key","model":"jev-latest"}`,
	})
	if jev.Code != http.StatusCreated {
		t.Fatalf("jev create status = %d body = %s", jev.Code, jev.Body.String())
	}
	if input := runtime.createInput; input.Kind != "jev" || input.Name != "system-one" ||
		input.BaseURL != "https://api.typesafe.ai/v1" || input.Model != "jev-latest" ||
		input.APIKey != "jev-key" || !input.HasAPIKey {
		t.Fatalf("jev create input = %+v", input)
	}

	runtime.createErr = &apierrors.KnowledgeConflictError{Message: kbconfig.MessageModelProfileExists}
	conflict := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/model-profiles", remote: "127.0.0.1:5000",
		body: `{"kind":"embedding","name":"bge-m3","base_url":"http://model.local/v1","model":"BAAI/bge-m3"}`,
	})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d body = %s", conflict.Code, conflict.Body.String())
	}
	if detail := decodeBody(t, conflict)["detail"]; detail != kbconfig.MessageModelProfileExists {
		t.Fatalf("conflict detail = %v", detail)
	}

	runtime.createErr = &apierrors.KnowledgeValidationError{Message: kbconfig.MessageModelKindInvalid}
	invalid := performRequest(handler, testRequest{
		method: "POST", target: "/v1/admin/model-profiles", remote: "127.0.0.1:5000",
		body: `{"kind":"embedding","name":"bge-m3","base_url":"bad url","model":"BAAI/bge-m3"}`,
	})
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("validation status = %d body = %s", invalid.Code, invalid.Body.String())
	}
}

func TestModelProfileAdminPatchAndDelete(t *testing.T) {
	runtime := &fakeModelProfileRuntime{
		updatePayload: map[string]any{"id": "p1", "name": "renamed", "model": "BAAI/bge-m3"},
		deletePayload: true,
	}
	handler := newModelProfileServer(t, runtime)

	patch := performRequest(handler, testRequest{
		method: "PATCH", target: "/v1/admin/model-profiles/p1", remote: "127.0.0.1:5000",
		body: `{"name":"renamed"}`,
	})
	if patch.Code != http.StatusOK {
		t.Fatalf("patch status = %d body = %s", patch.Code, patch.Body.String())
	}
	if runtime.updateID != "p1" || runtime.updateInput.Name != "renamed" || runtime.updateInput.HasAPIKey {
		t.Fatalf("patch captured = %q %+v", runtime.updateID, runtime.updateInput)
	}

	// An explicit (even empty) apikey clears/replaces the stored key.
	clearKey := performRequest(handler, testRequest{
		method: "PATCH", target: "/v1/admin/model-profiles/p1", remote: "127.0.0.1:5000",
		body: `{"apikey":""}`,
	})
	if clearKey.Code != http.StatusOK {
		t.Fatalf("patch key status = %d body = %s", clearKey.Code, clearKey.Body.String())
	}
	if input := runtime.updateInput; !input.HasAPIKey || input.APIKey != "" {
		t.Fatalf("patch key input = %+v", input)
	}

	// A missing profile is a 404 on the by-id routes.
	runtime.updateErr = &apierrors.KnowledgeValidationError{Message: kbconfig.MessageModelProfileMissing}
	missing := performRequest(handler, testRequest{
		method: "PATCH", target: "/v1/admin/model-profiles/missing", remote: "127.0.0.1:5000",
		body: `{"name":"renamed"}`,
	})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing patch status = %d body = %s", missing.Code, missing.Body.String())
	}
	if detail := decodeBody(t, missing)["detail"]; detail != kbconfig.MessageModelProfileMissing {
		t.Fatalf("missing patch detail = %v", detail)
	}

	// Provider failures on update stay 400 validation errors.
	runtime.updateErr = &apierrors.KnowledgeValidationError{Message: "HTTP 401：认证失败，请检查 apikey"}
	provider := performRequest(handler, testRequest{
		method: "PATCH", target: "/v1/admin/model-profiles/p1", remote: "127.0.0.1:5000",
		body: `{"base_url":"http://other.local/v1"}`,
	})
	if provider.Code != http.StatusBadRequest {
		t.Fatalf("provider patch status = %d body = %s", provider.Code, provider.Body.String())
	}

	deleted := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/admin/model-profiles/p1", remote: "127.0.0.1:5000",
	})
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body = %s", deleted.Code, deleted.Body.String())
	}
	if runtime.deleteID != "p1" {
		t.Fatalf("delete captured id = %q", runtime.deleteID)
	}

	runtime.deletePayload = false
	missingDelete := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/admin/model-profiles/missing", remote: "127.0.0.1:5000",
	})
	if missingDelete.Code != http.StatusNotFound {
		t.Fatalf("missing delete status = %d body = %s", missingDelete.Code, missingDelete.Body.String())
	}
	if detail := decodeBody(t, missingDelete)["detail"]; detail != kbconfig.MessageModelProfileMissing {
		t.Fatalf("missing delete detail = %v", detail)
	}
}

func TestModelProfileAdminValidationParity(t *testing.T) {
	runtime := &fakeModelProfileRuntime{}
	handler := newModelProfileServer(t, runtime)
	longText := func(size int) string { return strings.Repeat("x", size) }

	cases := []struct {
		name     string
		patch    bool
		body     string
		location string
		wantType string
	}{
		{"missing kind", false, `{"name":"n","model":"m"}`, "kind", "missing"},
		{"invalid kind", false, `{"kind":"sql","name":"n","model":"m"}`, "kind", "literal_error"},
		{"missing name", false, `{"kind":"embedding","model":"m"}`, "name", "missing"},
		{"empty name", false, `{"kind":"embedding","name":"","model":"m"}`, "name", "string_too_short"},
		{"long name", false, `{"kind":"embedding","name":"` + longText(81) + `","model":"m"}`, "name", "string_too_long"},
		{"missing model", false, `{"kind":"embedding","name":"n"}`, "model", "missing"},
		{"long model", false, `{"kind":"embedding","name":"n","model":"` + longText(513) + `"}`, "model", "string_too_long"},
		{"long base_url", false, `{"kind":"embedding","name":"n","model":"m","base_url":"` + longText(2049) + `"}`, "base_url", "string_too_long"},
		{"extra create field", false, `{"kind":"embedding","name":"n","model":"m","unknown":true}`, "unknown", "extra_forbidden"},
		{"patch empty name", true, `{"name":""}`, "name", "string_too_short"},
		{"patch long name", true, `{"name":"` + longText(81) + `"}`, "name", "string_too_long"},
		{"patch long model", true, `{"model":"` + longText(513) + `"}`, "model", "string_too_long"},
		{"patch long base_url", true, `{"base_url":"` + longText(2049) + `"}`, "base_url", "string_too_long"},
		{"patch immutable kind", true, `{"kind":"embedding"}`, "kind", "extra_forbidden"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			method, target := "POST", "/v1/admin/model-profiles"
			if testCase.patch {
				method, target = "PATCH", "/v1/admin/model-profiles/p1"
			}
			response := performRequest(handler, testRequest{
				method: method, target: target, remote: "127.0.0.1:5000", body: testCase.body,
			})
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
			detail, _ := decodeBody(t, response)["detail"].([]any)
			if len(detail) == 0 {
				t.Fatalf("missing detail: %s", response.Body.String())
			}
			first, _ := detail[0].(map[string]any)
			location, _ := first["loc"].([]any)
			if len(location) != 2 || location[0] != "body" || location[1] != testCase.location {
				t.Fatalf("loc = %v, want body.%s", first["loc"], testCase.location)
			}
			if first["type"] != testCase.wantType {
				t.Fatalf("type = %v, want %s", first["type"], testCase.wantType)
			}
		})
	}
	if runtime.createInput.Kind != "" {
		t.Fatalf("invalid create reached the runtime: %+v", runtime.createInput)
	}
	if runtime.updateID != "" {
		t.Fatalf("invalid patch reached the runtime: %q", runtime.updateID)
	}
}

func TestModelProfileAdminRequiresRuntime(t *testing.T) {
	server := New(testSettings(""), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := server.Handler()
	for _, request := range []testRequest{
		{method: "GET", target: "/v1/admin/model-profiles", remote: "127.0.0.1:5000"},
		{method: "POST", target: "/v1/admin/model-profiles", remote: "127.0.0.1:5000", body: `{}`},
		{method: "GET", target: "/v1/admin/model-profiles/p1", remote: "127.0.0.1:5000"},
		{method: "DELETE", target: "/v1/admin/model-profiles/p1", remote: "127.0.0.1:5000"},
	} {
		response := performRequest(handler, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s status = %d, want 503", request.method, request.target, response.Code)
		}
	}
}

func TestModelProfileAdminRequiresAdmin(t *testing.T) {
	settings := testSettings("")
	settings.AuthEnabled = true
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		"admin-token": adminSession("admin-token"),
		"user-token":  userSession("user-token"),
	}}
	runtime := &fakeModelProfileRuntime{listPayload: []map[string]any{}}
	handler := New(settings, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithModelProfileRuntime(runtime),
		WithUserAuth(users),
	).Handler()

	// A normal Console account cannot manage model configurations.
	forbidden := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/model-profiles", remote: "127.0.0.1:5000",
		headers: cookieFor("user-token"),
	})
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("user status = %d body = %s", forbidden.Code, forbidden.Body.String())
	}
	if detail := decodeBody(t, forbidden)["detail"]; detail != "需要管理员权限" {
		t.Fatalf("user detail = %v", detail)
	}

	admin := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/model-profiles", remote: "127.0.0.1:5000",
		headers: cookieFor("admin-token"),
	})
	if admin.Code != http.StatusOK {
		t.Fatalf("admin status = %d body = %s", admin.Code, admin.Body.String())
	}

	// Remote callers keep the legacy local-admin boundary.
	remote := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/model-profiles", remote: "10.0.0.9:5000",
	})
	if remote.Code != http.StatusForbidden || decodeBody(t, remote)["detail"] != "仅允许本机直接访问管理接口" {
		t.Fatalf("remote = %d %s", remote.Code, remote.Body.String())
	}
}
