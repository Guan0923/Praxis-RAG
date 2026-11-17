package config

import (
	"strings"
	"testing"
)

func TestRagConfigDoclingDefaults(t *testing.T) {
	isolateEnv(t)
	cfg, err := RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.DoclingServiceURL != "" {
		t.Fatalf("DoclingServiceURL = %q, want empty", cfg.DoclingServiceURL)
	}
	if cfg.DoclingServiceTimeout != 300 {
		t.Fatalf("DoclingServiceTimeout = %d, want 300", cfg.DoclingServiceTimeout)
	}
}

func TestRagConfigDoclingServiceURL(t *testing.T) {
	isolateEnv(t)
	cfg, err := RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.DoclingServiceURL != "" {
		t.Fatalf("DoclingServiceURL = %q, want empty", cfg.DoclingServiceURL)
	}

	t.Setenv("RAG_DOCLING_SERVICE_URL", "http://127.0.0.1:8092/")
	cfg, err = RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.DoclingServiceURL != "http://127.0.0.1:8092" {
		t.Fatalf("DoclingServiceURL = %q, want trailing slash trimmed", cfg.DoclingServiceURL)
	}
}

func TestRagConfigRejectsInvalidDoclingServiceURL(t *testing.T) {
	isolateEnv(t)
	for _, invalid := range []string{
		"ftp://docling.internal:8092",
		"docling.internal:8092",
		"http://docling.internal:8092/v1/parse?x=1",
		"http://docling.internal:8092/path#fragment",
	} {
		t.Setenv("RAG_DOCLING_SERVICE_URL", invalid)
		if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_DOCLING_SERVICE_URL") {
			t.Fatalf("URL %q error = %v", invalid, err)
		}
	}
	t.Setenv("RAG_DOCLING_SERVICE_URL", "https://docling.internal:8092")
	cfg, err := RagConfigFromEnv()
	if err != nil {
		t.Fatalf("valid URL error = %v", err)
	}
	if cfg.DoclingServiceURL != "https://docling.internal:8092" {
		t.Fatalf("DoclingServiceURL = %q", cfg.DoclingServiceURL)
	}
}

func TestRagConfigDoclingServiceTimeoutBounds(t *testing.T) {
	isolateEnv(t)
	for _, invalid := range []string{"0", "7201", "abc"} {
		t.Setenv("RAG_DOCLING_SERVICE_TIMEOUT", invalid)
		if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_DOCLING_SERVICE_TIMEOUT") {
			t.Fatalf("timeout %q error = %v", invalid, err)
		}
	}
	t.Setenv("RAG_DOCLING_SERVICE_TIMEOUT", "7200")
	cfg, err := RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.DoclingServiceTimeout != 7200 {
		t.Fatalf("DoclingServiceTimeout = %d, want 7200", cfg.DoclingServiceTimeout)
	}
	t.Setenv("RAG_DOCLING_SERVICE_TIMEOUT", "1")
	cfg, err = RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.DoclingServiceTimeout != 1 {
		t.Fatalf("DoclingServiceTimeout = %d, want 1", cfg.DoclingServiceTimeout)
	}
}
