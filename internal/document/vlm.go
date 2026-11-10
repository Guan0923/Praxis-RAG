package document

import (
	"strings"
	"time"
)

// VLMSettings configures the remote OpenAI-compatible vision-language endpoint
// used to describe uncaptioned Word pictures. All inference happens through
// this HTTP endpoint; no local model weights and no Python worker are involved.
type VLMSettings struct {
	// BaseURL is the OpenAI-compatible endpoint, for example
	// "https://llm.internal/v1". Empty means the remote VLM is not configured.
	BaseURL string
	// APIKey is sent as "Authorization: Bearer <key>" when non-empty.
	APIKey string
	// Model is the model name the endpoint expects, for example "qwen2.5-vl-7b".
	Model string
	// Timeout bounds one VLM request. Zero means the caller default.
	Timeout time.Duration
	// Concurrency is the number of simultaneous VLM requests a worker may issue.
	Concurrency int
}

// Configured reports whether a remote endpoint is configured.
func (v VLMSettings) Configured() bool {
	return strings.TrimSpace(v.BaseURL) != ""
}
