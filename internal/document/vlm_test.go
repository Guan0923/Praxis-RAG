package document

import (
	"testing"
	"time"
)

func TestVLMSettingsEnv(t *testing.T) {
	if env := (VLMSettings{}).Env(); env != nil {
		t.Fatalf("unconfigured Env() = %v", env)
	}
	env := VLMSettings{
		BaseURL:     "http://vlm.test/v1",
		APIKey:      "secret",
		Model:       "test-vlm",
		Timeout:     45 * time.Second,
		Concurrency: 3,
	}.Env()
	want := map[string]string{
		"RAG_VLM_BASE_URL":    "http://vlm.test/v1",
		"RAG_VLM_API_KEY":     "secret",
		"RAG_VLM_MODEL":       "test-vlm",
		"RAG_VLM_TIMEOUT":     "45",
		"RAG_VLM_CONCURRENCY": "3",
	}
	if len(env) != len(want) {
		t.Fatalf("Env() = %v", env)
	}
	for key, value := range want {
		if env[key] != value {
			t.Fatalf("Env()[%q] = %q, want %q", key, env[key], value)
		}
	}
}
