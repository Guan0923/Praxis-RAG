package document

import (
	"strconv"
	"strings"
	"time"
)

// VLMSettings configures the remote OpenAI-compatible vision-language endpoint
// used for formula recognition and picture descriptions.
//
// Layout, table structure and OCR still run locally through Docling; only the
// VLM stages leave the machine. The Python worker reads these values from its
// environment, so the API key never appears in a process command line.
type VLMSettings struct {
	// BaseURL is the OpenAI-compatible endpoint, for example
	// "https://llm.internal/v1". Empty means the remote VLM is not configured.
	BaseURL string
	// APIKey is sent as "Authorization: Bearer <key>" when non-empty.
	APIKey string
	// Model is the model name the endpoint expects, for example "qwen2.5-vl-7b".
	Model string
	// Timeout bounds one VLM request. Zero means the worker default.
	Timeout time.Duration
	// Concurrency is the number of simultaneous VLM requests Docling may issue.
	Concurrency int
}

// Configured reports whether a remote endpoint is configured.
func (v VLMSettings) Configured() bool {
	return strings.TrimSpace(v.BaseURL) != ""
}

// Env renders the settings as the environment variables the Python workers read.
// It returns nil when no endpoint is configured so callers can skip the merge.
func (v VLMSettings) Env() map[string]string {
	if !v.Configured() {
		return nil
	}
	env := map[string]string{
		"RAG_VLM_BASE_URL": strings.TrimSpace(v.BaseURL),
		"RAG_VLM_MODEL":    strings.TrimSpace(v.Model),
	}
	if v.APIKey != "" {
		env["RAG_VLM_API_KEY"] = v.APIKey
	}
	if v.Timeout > 0 {
		env["RAG_VLM_TIMEOUT"] = strconv.Itoa(int(v.Timeout / time.Second))
	}
	if v.Concurrency > 0 {
		env["RAG_VLM_CONCURRENCY"] = strconv.Itoa(v.Concurrency)
	}
	return env
}

// mergeInto copies the configured VLM variables into a worker environment.
func (v VLMSettings) mergeInto(env map[string]string) {
	for key, value := range v.Env() {
		env[key] = value
	}
}
