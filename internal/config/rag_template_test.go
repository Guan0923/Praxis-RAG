package config

import "testing"

// TestRagConfigDefaultKBTemplate pins RAG_DEFAULT_KB_TEMPLATE: the tenant
// self-create flow clones the namespace named here and falls back to "default".
func TestRagConfigDefaultKBTemplate(t *testing.T) {
	isolateEnv(t)
	cfg, err := RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.DefaultKBTemplate != "default" {
		t.Fatalf("DefaultKBTemplate = %q, want default", cfg.DefaultKBTemplate)
	}

	t.Setenv("RAG_DEFAULT_KB_TEMPLATE", "  kb-template  ")
	cfg, err = RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.DefaultKBTemplate != "kb-template" {
		t.Fatalf("DefaultKBTemplate = %q, want kb-template", cfg.DefaultKBTemplate)
	}

	t.Setenv("RAG_DEFAULT_KB_TEMPLATE", "   ")
	cfg, err = RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.DefaultKBTemplate != "default" {
		t.Fatalf("blank DefaultKBTemplate = %q, want default fallback", cfg.DefaultKBTemplate)
	}
}
