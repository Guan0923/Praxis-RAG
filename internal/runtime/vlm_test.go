package runtime

import (
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
)

// TestUploadVLMUsesKnowledgeBaseProvider verifies the Console settings win over
// the service environment, and that an unset knowledge-base provider falls back
// to RAG_VLM_* so existing deployments keep working.
func TestUploadVLMUsesKnowledgeBaseProvider(t *testing.T) {
	service, stores := newKBService(t, "http://127.0.0.1:1")
	service.config.VLMBaseURL = "http://env-vlm:9000/v1"
	service.config.VLMAPIKey = "env-key"
	service.config.VLMModel = "env-vlm"
	service.config.VLMTimeout = 45
	service.config.VLMConcurrency = 3

	stored, err := kbconfig.StoredConnectionWithKey(modelapi.ModelConnection{
		BaseURL: "http://kb-vlm:8000/v1", APIKey: "kb-key",
	}, stores.Settings.EncryptionKey)
	if err != nil {
		t.Fatalf("StoredConnectionWithKey() error = %v", err)
	}
	settings := map[string]any{"vlm": stored, "vlm_model": "kb-vlm"}

	resolved, err := service.uploadVLM(settings)
	if err != nil {
		t.Fatalf("uploadVLM() error = %v", err)
	}
	if resolved.BaseURL != "http://kb-vlm:8000/v1" || resolved.Model != "kb-vlm" || resolved.APIKey != "kb-key" {
		t.Fatalf("knowledge-base VLM = %+v", resolved)
	}
	if resolved.Timeout.Seconds() != 45 || resolved.Concurrency != 3 {
		t.Fatalf("timeout/concurrency = %v/%d", resolved.Timeout, resolved.Concurrency)
	}

	fallback, err := service.uploadVLM(map[string]any{
		"vlm":       map[string]any{"base_url": "", "protected_key": ""},
		"vlm_model": "",
	})
	if err != nil {
		t.Fatalf("uploadVLM(fallback) error = %v", err)
	}
	if fallback.BaseURL != "http://env-vlm:9000/v1" || fallback.Model != "env-vlm" || fallback.APIKey != "env-key" {
		t.Fatalf("environment fallback = %+v", fallback)
	}
}
