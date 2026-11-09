package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRagConfigDefaults(t *testing.T) {
	isolateEnv(t)
	cfg, err := RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.Backend != BackendMock {
		t.Fatalf("backend = %q, want mock", cfg.Backend)
	}
	if cfg.Collection != "mini_agent_knowledge_pdf_assets_v1" {
		t.Fatalf("collection = %q", cfg.Collection)
	}
	if cfg.VectorSize != DenseVectorSize {
		t.Fatalf("vector size = %d", cfg.VectorSize)
	}
	if cfg.EmbeddingModel != "BAAI/bge-m3" || cfg.RerankerModel != DefaultRerankerModel {
		t.Fatalf("unexpected model defaults: %+v", cfg)
	}
}

func TestRagConfigRejectsInvalidBackendAndSelectors(t *testing.T) {
	isolateEnv(t)
	t.Setenv("RAG_BACKEND", "sqlite")
	if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_BACKEND must be mock or qdrant") {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = os.Unsetenv("RAG_BACKEND")
	t.Setenv("QDRANT_URL", "http://localhost:6333")
	t.Setenv("QDRANT_PATH", filepath.Join(t.TempDir(), "qdrant"))
	if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "Set only one of QDRANT_URL and QDRANT_PATH") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRagConfigQdrantDefaultsToLocalhost(t *testing.T) {
	isolateEnv(t)
	t.Setenv("RAG_BACKEND", "qdrant")
	cfg, err := RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.QdrantURL != "http://localhost:6333" {
		t.Fatalf("qdrant url = %q", cfg.QdrantURL)
	}
}

func TestRagConfigRejectsWrongVectorSize(t *testing.T) {
	isolateEnv(t)
	t.Setenv("RAG_DENSE_VECTOR_SIZE", "768")
	if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "must have 1024 dimensions") {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Setenv("RAG_DENSE_VECTOR_SIZE", "not-a-number")
	if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_DENSE_VECTOR_SIZE must be an integer") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRagConfigBoundedIntegers(t *testing.T) {
	isolateEnv(t)
	t.Setenv("RAG_RERANKER_BATCH_SIZE", "65")
	if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_RERANKER_BATCH_SIZE must be between 1 and 64") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServiceSettingsDefaults(t *testing.T) {
	home := isolateEnv(t)
	settings, err := ServiceSettingsFromEnv()
	if err != nil {
		t.Fatalf("ServiceSettingsFromEnv() error = %v", err)
	}
	if settings.Host != "127.0.0.1" || settings.Port != 8011 {
		t.Fatalf("unexpected bind settings: %+v", settings)
	}
	if settings.DataRoot != filepath.Join(home, ".praxis-rag", "data") {
		t.Fatalf("data root = %q", settings.DataRoot)
	}
	if len(settings.MCPAllowedHosts) != len(DefaultMCPAllowedHosts) {
		t.Fatalf("allowed hosts = %v", settings.MCPAllowedHosts)
	}
	if settings.Token != "" {
		t.Fatalf("token = %q, want empty", settings.Token)
	}
	if settings.UploadMode != UploadModeSync {
		t.Fatalf("upload mode = %q, want %q", settings.UploadMode, UploadModeSync)
	}
}

func TestServiceSettingsUploadMode(t *testing.T) {
	isolateEnv(t)
	t.Setenv("RAG_UPLOAD_MODE", "async")
	settings, err := ServiceSettingsFromEnv()
	if err != nil {
		t.Fatalf("ServiceSettingsFromEnv() error = %v", err)
	}
	if settings.UploadMode != UploadModeAsync {
		t.Fatalf("upload mode = %q, want %q", settings.UploadMode, UploadModeAsync)
	}
	t.Setenv("RAG_UPLOAD_MODE", "sync")
	if settings, err = ServiceSettingsFromEnv(); err != nil || settings.UploadMode != UploadModeSync {
		t.Fatalf("sync upload mode = %+v, %v", settings, err)
	}
	t.Setenv("RAG_UPLOAD_MODE", "queue")
	if _, err := ServiceSettingsFromEnv(); err == nil || err.Error() != "RAG_UPLOAD_MODE must be sync or async" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServiceSettingsInvitationTTL(t *testing.T) {
	isolateEnv(t)
	settings, err := ServiceSettingsFromEnv()
	if err != nil {
		t.Fatalf("ServiceSettingsFromEnv() error = %v", err)
	}
	if settings.InvitationTTL != 168*time.Hour {
		t.Fatalf("default invitation ttl = %v, want 168h", settings.InvitationTTL)
	}

	t.Setenv("RAG_INVITATION_TTL_HOURS", "24")
	settings, err = ServiceSettingsFromEnv()
	if err != nil {
		t.Fatalf("ServiceSettingsFromEnv() error = %v", err)
	}
	if settings.InvitationTTL != 24*time.Hour {
		t.Fatalf("invitation ttl = %v, want 24h", settings.InvitationTTL)
	}

	t.Setenv("RAG_INVITATION_TTL_HOURS", "0")
	if _, err := ServiceSettingsFromEnv(); err == nil ||
		!strings.Contains(err.Error(), "RAG_INVITATION_TTL_HOURS must be an integer between 1 and 8760") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServiceSettingsRejectsInvalidPort(t *testing.T) {
	isolateEnv(t)
	t.Setenv("RAG_SERVICE_PORT", "abc")
	if _, err := ServiceSettingsFromEnv(); err == nil || err.Error() != "RAG_SERVICE_PORT must be an integer" {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Setenv("RAG_SERVICE_PORT", "70000")
	if _, err := ServiceSettingsFromEnv(); err == nil || err.Error() != "RAG_SERVICE_PORT must be between 1 and 65535" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServiceSettingsRequireTokenAndAllowlistsWhenNotLoopback(t *testing.T) {
	isolateEnv(t)
	t.Setenv("RAG_SERVICE_HOST", "0.0.0.0")
	if _, err := ServiceSettingsFromEnv(); err == nil || err.Error() != "RAG_SERVICE_TOKEN is required when RAG_SERVICE_HOST is not loopback" {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Setenv("RAG_SERVICE_TOKEN", "secret")
	if _, err := ServiceSettingsFromEnv(); err == nil || err.Error() != "RAG_MCP_ALLOWED_HOSTS is required for a non-loopback RAG service" {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Setenv("RAG_MCP_ALLOWED_HOSTS", "example.com")
	if _, err := ServiceSettingsFromEnv(); err == nil || err.Error() != "RAG_MCP_ALLOWED_ORIGINS is required for a non-loopback RAG service" {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Setenv("RAG_MCP_ALLOWED_ORIGINS", "https://example.com")
	settings, err := ServiceSettingsFromEnv()
	if err != nil {
		t.Fatalf("ServiceSettingsFromEnv() error = %v", err)
	}
	if settings.Token != "secret" {
		t.Fatalf("token = %q", settings.Token)
	}
}

func TestServiceSettingsRejectEmptyAllowlists(t *testing.T) {
	isolateEnv(t)
	t.Setenv("RAG_MCP_ALLOWED_HOSTS", " , ")
	if _, err := ServiceSettingsFromEnv(); err == nil || err.Error() != "MCP host and origin allowlists must not be empty" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStateSettingsRequiresMySQLConfiguration(t *testing.T) {
	isolateEnv(t)
	root := filepath.Join(t.TempDir(), "data")
	if _, err := StateSettingsFromEnv(root); err == nil {
		t.Fatal("StateSettingsFromEnv() error = nil, want a configuration error")
	}
}

func TestStateSettingsFromMySQLPartsAndURL(t *testing.T) {
	isolateEnv(t)
	t.Setenv("RAG_MYSQL_HOST", "db.local")
	t.Setenv("RAG_MYSQL_PORT", "3307")
	t.Setenv("RAG_MYSQL_USER", "rag")
	t.Setenv("RAG_MYSQL_PASSWORD", "pw")
	t.Setenv("RAG_MYSQL_DATABASE", "praxis")
	t.Setenv("RAG_STATE_ENCRYPTION_KEY", "base64:key")
	settings, err := StateSettingsFromEnv(t.TempDir())
	if err != nil {
		t.Fatalf("StateSettingsFromEnv() error = %v", err)
	}
	if settings.Host != "db.local" || settings.Port != 3307 || settings.Database != "praxis" {
		t.Fatalf("unexpected mysql settings: %+v", settings)
	}
	description := settings.Describe()
	if description["backend"] != "mysql" || description["host"] != "db.local" || description["database"] != "praxis" {
		t.Fatalf("unexpected describe payload: %+v", description)
	}
	if description["encrypted"] != true {
		t.Fatalf("encrypted = %v", description["encrypted"])
	}
}

func TestStateSettingsExplicitMySQLURL(t *testing.T) {
	isolateEnv(t)
	t.Setenv("RAG_STATE_URL", "mysql+pymysql://rag:pw@127.0.0.1:3306/praxis_rag?charset=utf8mb4")
	settings, err := StateSettingsFromEnv(t.TempDir())
	if err != nil {
		t.Fatalf("StateSettingsFromEnv() error = %v", err)
	}
	if settings.Host != "127.0.0.1" || settings.Database != "praxis_rag" {
		t.Fatalf("unexpected settings: %+v", settings)
	}
}

func TestRagConfigVLMEndpoint(t *testing.T) {
	isolateEnv(t)
	cfg, err := RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.VLMBaseURL != "" || cfg.VLMModel != "" || cfg.VLMTimeout != 120 || cfg.VLMConcurrency != 4 {
		t.Fatalf("vlm defaults = %+v", cfg)
	}

	t.Setenv("RAG_VLM_BASE_URL", "https://llm.internal/v1/")
	t.Setenv("RAG_VLM_API_KEY", "sk-test")
	t.Setenv("RAG_VLM_MODEL", "qwen2.5-vl-7b")
	t.Setenv("RAG_VLM_TIMEOUT", "45")
	t.Setenv("RAG_VLM_CONCURRENCY", "8")
	cfg, err = RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.VLMBaseURL != "https://llm.internal/v1" || cfg.VLMAPIKey != "sk-test" ||
		cfg.VLMModel != "qwen2.5-vl-7b" || cfg.VLMTimeout != 45 || cfg.VLMConcurrency != 8 {
		t.Fatalf("vlm config = %+v", cfg)
	}

	t.Setenv("RAG_VLM_MODEL", "")
	if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_VLM_MODEL is required") {
		t.Fatalf("missing model error = %v", err)
	}
	t.Setenv("RAG_VLM_MODEL", "qwen2.5-vl-7b")

	for _, invalid := range []string{"ftp://llm.internal/v1", "llm.internal/v1", "https://user:pw@llm.internal/v1"} {
		t.Setenv("RAG_VLM_BASE_URL", invalid)
		if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_VLM_BASE_URL") {
			t.Fatalf("base URL %q error = %v", invalid, err)
		}
	}

	t.Setenv("RAG_VLM_BASE_URL", "http://127.0.0.1:8000/v1")
	t.Setenv("RAG_VLM_TIMEOUT", "0")
	if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_VLM_TIMEOUT") {
		t.Fatalf("timeout bound error = %v", err)
	}
}
