package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateEnv clears the process environment for the test and restores it after.
func isolateEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	// Capture temp-dir variables before clearing so later t.TempDir calls keep
	// working on Windows.
	tempVars := map[string]string{
		"TEMP": os.Getenv("TEMP"), "TMP": os.Getenv("TMP"), "TMPDIR": os.Getenv("TMPDIR"),
	}
	original := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, entry := range original {
			key, value, ok := strings.Cut(entry, "=")
			if ok {
				_ = os.Setenv(key, value)
			}
		}
	})
	os.Clearenv()
	setHomeEnv(t, home, tempVars)
	return home
}

func setHomeEnv(t *testing.T, home string, tempVars map[string]string) {
	t.Helper()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	for key, value := range tempVars {
		if value != "" {
			t.Setenv(key, value)
		}
	}
}

func TestFirstStartCreatesConfigWithoutOverwritingEdits(t *testing.T) {
	home := isolateEnv(t)

	path, err := LoadStartupConfig()
	if err != nil {
		t.Fatalf("LoadStartupConfig() error = %v", err)
	}
	expected := filepath.Join(home, ".praxis-rag", "config.toml")
	if path != expected {
		t.Fatalf("path = %q, want %q", path, expected)
	}
	if got := os.Getenv("RAG_BACKEND"); got != "qdrant" {
		t.Fatalf("RAG_BACKEND = %q, want qdrant", got)
	}

	edited := strings.Replace(DefaultConfig, `"8011"`, `"8099"`, 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatalf("write edited config: %v", err)
	}

	// A new process starts with a clean environment again.
	tempVars := map[string]string{
		"TEMP": os.Getenv("TEMP"), "TMP": os.Getenv("TMP"), "TMPDIR": os.Getenv("TMPDIR"),
	}
	os.Clearenv()
	setHomeEnv(t, home, tempVars)
	if _, err := LoadStartupConfig(); err != nil {
		t.Fatalf("second LoadStartupConfig() error = %v", err)
	}
	if got := os.Getenv("RAG_SERVICE_PORT"); got != "8099" {
		t.Fatalf("RAG_SERVICE_PORT = %q, want 8099", got)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(content) != edited {
		t.Fatalf("LoadStartupConfig rewrote an existing file")
	}
}

func TestInvalidConfigIsRejected(t *testing.T) {
	isolateEnv(t)
	cases := []string{
		"[environment\n",
		"[environment]\nRAG_SERVICE_PORRT = \"8011\"",
		"[environment]\nRAG_SERVICE_PORT = 8011",
	}
	for _, content := range cases {
		t.Run(content, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			t.Setenv("RAG_CONFIG_FILE", path)
			if _, err := LoadStartupConfig(); err == nil {
				t.Fatalf("LoadStartupConfig() accepted invalid config %q", content)
			}
		})
	}
}

func TestDocumentWorkerSettingsCanBeLoadedFromConfig(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `[environment]
RAG_DOCUMENT_TOPIC = "documents"
RAG_DOCUMENT_GROUP = "document_workers"
RAG_DOCUMENT_DLQ_GROUP = "document_workers_dlq"
RAG_DOCUMENT_BROKER_MAX_RETRIES = "24"
RAG_RESULT_TTL_SECONDS = "7200"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("RAG_CONFIG_FILE", path)
	if _, err := LoadStartupConfig(); err != nil {
		t.Fatalf("LoadStartupConfig() error = %v", err)
	}
	for key, expected := range map[string]string{
		"RAG_DOCUMENT_TOPIC":              "documents",
		"RAG_DOCUMENT_GROUP":              "document_workers",
		"RAG_DOCUMENT_DLQ_GROUP":          "document_workers_dlq",
		"RAG_DOCUMENT_BROKER_MAX_RETRIES": "24",
		"RAG_RESULT_TTL_SECONDS":          "7200",
	} {
		if got := os.Getenv(key); got != expected {
			t.Fatalf("%s = %q, want %q", key, got, expected)
		}
	}
}

func TestDoclingServiceSettingsCanBeLoadedFromConfig(t *testing.T) {
	isolateEnv(t)
	// deploy/config.container.toml validates [environment] against the observed
	// keys, so the Docling settings must be accepted there.
	for _, key := range []string{"RAG_DOCLING_SERVICE_URL", "RAG_DOCLING_SERVICE_TIMEOUT"} {
		if _, allowed := allowedConfigKeys[key]; !allowed {
			t.Fatalf("allowedConfigKeys is missing %s", key)
		}
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `[environment]
RAG_DOCLING_SERVICE_URL = "http://127.0.0.1:8092"
RAG_DOCLING_SERVICE_TIMEOUT = "300"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("RAG_CONFIG_FILE", path)
	if _, err := LoadStartupConfig(); err != nil {
		t.Fatalf("LoadStartupConfig() error = %v", err)
	}
	for key, expected := range map[string]string{
		"RAG_DOCLING_SERVICE_URL":     "http://127.0.0.1:8092",
		"RAG_DOCLING_SERVICE_TIMEOUT": "300",
	} {
		if got := os.Getenv(key); got != expected {
			t.Fatalf("%s = %q, want %q", key, got, expected)
		}
	}
}

func TestLegacyDocumentTaskSettingsAreAcceptedButIgnored(t *testing.T) {
	isolateEnv(t)
	legacy := []string{
		"RAG_DOCUMENT_MAX_ATTEMPTS", "RAG_DOCUMENT_LEASE_SECONDS", "RAG_DOCUMENT_RETRY_SECONDS",
		"RAG_DOCUMENT_DISPATCH_LEASE_SECONDS", "RAG_DOCUMENT_DISPATCH_RETRY_SECONDS",
		"RAG_DOCUMENT_DISPATCH_MAX_RETRY_SECONDS", "RAG_DOCUMENT_DISPATCH_ALERT_SECONDS",
	}
	var builder strings.Builder
	builder.WriteString("[environment]\n")
	for _, key := range legacy {
		builder.WriteString(key + " = \"5\"\n")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("RAG_CONFIG_FILE", path)
	if _, err := LoadStartupConfig(); err != nil {
		t.Fatalf("LoadStartupConfig() error = %v", err)
	}
	for _, key := range legacy {
		if got := os.Getenv(key); got != "" {
			t.Fatalf("legacy %s leaked into environment as %q", key, got)
		}
	}
}

func TestEnvironmentVariablesOverrideConfigFile(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[environment]\nRAG_SERVICE_PORT = \"8099\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("RAG_CONFIG_FILE", path)
	t.Setenv("RAG_SERVICE_PORT", "9100")
	if _, err := LoadStartupConfig(); err != nil {
		t.Fatalf("LoadStartupConfig() error = %v", err)
	}
	if got := os.Getenv("RAG_SERVICE_PORT"); got != "9100" {
		t.Fatalf("RAG_SERVICE_PORT = %q, want the existing environment value 9100", got)
	}
}

func TestExplicitQdrantSelectorIsNotOverriddenByConfig(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	content := "[environment]\nQDRANT_URL = \"http://from-file:6333\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("RAG_CONFIG_FILE", path)
	t.Setenv("QDRANT_PATH", filepath.Join(t.TempDir(), "qdrant"))
	if _, err := LoadStartupConfig(); err != nil {
		t.Fatalf("LoadStartupConfig() error = %v", err)
	}
	if got := os.Getenv("QDRANT_URL"); got != "" {
		t.Fatalf("QDRANT_URL = %q, want it not imported from the file", got)
	}
}
