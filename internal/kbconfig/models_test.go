package kbconfig

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
)

func TestModelsForUsesDraftConnection(t *testing.T) {
	url, calls := newFakeModelServer(t, func(modelCall) (int, string) {
		return http.StatusOK, `{"data":[{"id":"embed-a"},{"id":"rank-a"}]}`
	})
	models, err := ModelsFor(context.Background(), "embedding", nil, map[string]any{
		"base_url": url + "/v1",
		"apikey":   "private-key",
	}, nil)
	if err != nil {
		t.Fatalf("ModelsFor() error: %v", err)
	}
	if len(models) != 2 || models[0] != "embed-a" || models[1] != "rank-a" {
		t.Fatalf("models = %v", models)
	}
	recorded := calls()
	if len(recorded) != 1 {
		t.Fatalf("model calls = %d", len(recorded))
	}
	if recorded[0].Method != http.MethodGet || recorded[0].Path != "/v1/models" {
		t.Errorf("request = %s %s", recorded[0].Method, recorded[0].Path)
	}
	if recorded[0].Authorization != "Bearer private-key" {
		t.Errorf("Authorization = %q", recorded[0].Authorization)
	}
}

func TestModelsForKeepsStoredKeyForSameBaseURL(t *testing.T) {
	t.Setenv("RAG_STATE_ENCRYPTION_KEY", testEncryptionKey)
	url, calls := newFakeModelServer(t, func(modelCall) (int, string) {
		return http.StatusOK, `{"data":[{"id":"embed-a"}]}`
	})
	current := testStoredSettings(t, url+"/v1", "stored-key")
	models, err := ModelsFor(context.Background(), "embedding", current, map[string]any{"base_url": url + "/v1"}, nil)
	if err != nil {
		t.Fatalf("ModelsFor() error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %v", models)
	}
	if calls()[0].Authorization != "Bearer stored-key" {
		t.Errorf("Authorization = %q, want stored key reuse", calls()[0].Authorization)
	}

	// A different base URL drops the stored key (Python draft_connection rule).
	if _, err := ModelsFor(context.Background(), "embedding", current,
		map[string]any{"base_url": url + "/other"}, nil); err != nil {
		t.Fatalf("ModelsFor() error: %v", err)
	}
	recorded := calls()
	if recorded[len(recorded)-1].Authorization != "" {
		t.Errorf("changed base_url must drop the stored key, got %q", recorded[len(recorded)-1].Authorization)
	}
}

func TestModelsForExplicitKeyOverridesStoredKey(t *testing.T) {
	t.Setenv("RAG_STATE_ENCRYPTION_KEY", testEncryptionKey)
	url, calls := newFakeModelServer(t, func(modelCall) (int, string) {
		return http.StatusOK, `{"data":[{"id":"embed-a"}]}`
	})
	current := testStoredSettings(t, url, "stored-key")
	if _, err := ModelsFor(context.Background(), "embedding", current, map[string]any{"apikey": "draft-key"}, nil); err != nil {
		t.Fatalf("ModelsFor() error: %v", err)
	}
	if calls()[0].Authorization != "Bearer draft-key" {
		t.Errorf("Authorization = %q", calls()[0].Authorization)
	}
}

func TestModelsForUsesInjectedFactory(t *testing.T) {
	factoryCalls := 0
	factory := func(connection modelapi.ModelConnection) ModelLister {
		factoryCalls++
		if connection.BaseURL != "http://host:8080" || connection.APIKey != "k" {
			t.Errorf("factory connection = %+v", connection)
		}
		return staticLister{models: []string{"injected"}}
	}
	models, err := ModelsFor(context.Background(), "reranker", nil, map[string]any{
		"base_url": "http://host:8080", "apikey": "k",
	}, factory)
	if err != nil {
		t.Fatalf("ModelsFor() error: %v", err)
	}
	if factoryCalls != 1 || len(models) != 1 || models[0] != "injected" {
		t.Fatalf("factory calls=%d models=%v", factoryCalls, models)
	}
}

type staticLister struct {
	models []string
}

func (l staticLister) Models(context.Context) ([]string, error) { return l.models, nil }

func TestModelsForErrors(t *testing.T) {
	if _, err := ModelsFor(context.Background(), "sql", nil, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "unknown model kind") {
		t.Errorf("unknown kind error = %v", err)
	}
	if _, err := ModelsFor(context.Background(), "embedding", nil,
		map[string]any{"base_url": "ftp://host"}, nil); err == nil ||
		err.Error() != "base_url 必须是 HTTP 或 HTTPS 地址，不能包含账号、查询参数或片段" {
		t.Errorf("draft validation error = %v", err)
	}
	if _, err := ModelsFor(context.Background(), "embedding", nil,
		map[string]any{"base_url": "http://host", "apikey": "a\r\nb"}, nil); err == nil ||
		err.Error() != "apikey 格式无效" {
		t.Errorf("apikey validation error = %v", err)
	}

	url, _ := newFakeModelServer(t, func(modelCall) (int, string) {
		return http.StatusUnauthorized, `{"detail":"private upstream error"}`
	})
	_, err := ModelsFor(context.Background(), "embedding", nil, map[string]any{"base_url": url}, nil)
	if err == nil || err.Error() != "HTTP 401：认证失败，请检查 apikey" {
		t.Errorf("upstream error = %v", err)
	}
}

func TestModelsForCurrentSettingsError(t *testing.T) {
	current := map[string]any{"vector_size": 3} // missing embedding_model and provider
	if _, err := ModelsFor(context.Background(), "embedding", current, nil, nil); err == nil ||
		!strings.Contains(err.Error(), `missing "embedding_model"`) {
		t.Errorf("corrupt current settings error = %v", err)
	}
}
func TestModelsForToleratesMissingModelEndpoint(t *testing.T) {
	factory := func(modelapi.ModelConnection) ModelLister {
		return staticErrLister{err: &modelapi.ModelAPIError{
			Message: "HTTP 404：接口或模型不存在，请检查地址和模型名称", Status: 404,
		}}
	}
	models, err := ModelsFor(context.Background(), "reranker", nil, map[string]any{
		"base_url": "http://host:8080",
	}, factory)
	if err != nil {
		t.Fatalf("ModelsFor() error = %v, want a tolerant empty list", err)
	}
	if len(models) != 0 {
		t.Fatalf("models = %v, want empty", models)
	}

	factory = func(modelapi.ModelConnection) ModelLister {
		return staticErrLister{err: &modelapi.ModelAPIError{
			Message: "HTTP 401：认证失败，请检查 apikey", Status: 401,
		}}
	}
	if _, err := ModelsFor(context.Background(), "reranker", nil, map[string]any{
		"base_url": "http://host:8080",
	}, factory); err == nil {
		t.Fatalf("a 401 must still surface as an error")
	}
}

type staticErrLister struct{ err error }

func (l staticErrLister) Models(context.Context) ([]string, error) { return nil, l.err }
