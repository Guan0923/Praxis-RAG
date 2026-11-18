package config

// This file tests the LightRAG sidecar configuration: the empty-by-default
// service URL, its http(s) validation, and the bounded call timeout. It only
// covers the variables added for graph retrieval.

import (
	"strings"
	"testing"
	"time"
)

func TestRagConfigLightRAGDefaults(t *testing.T) {
	isolateEnv(t)
	cfg, err := RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	// Unlike the PDF service, the sidecar has no default URL: graph retrieval
	// stays disabled until a deployment configures it.
	if cfg.LightRAGServiceURL != "" {
		t.Fatalf("LightRAGServiceURL = %q, want empty", cfg.LightRAGServiceURL)
	}
	if cfg.LightRAGServiceTimeout != 60*time.Second {
		t.Fatalf("LightRAGServiceTimeout = %v, want 60s", cfg.LightRAGServiceTimeout)
	}
}

func TestRagConfigLightRAGServiceURL(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8093":            "http://127.0.0.1:8093",
		"https://lightrag.internal:8093/":  "https://lightrag.internal:8093",
		" http://lightrag.internal:8093//": "http://lightrag.internal:8093",
	}
	for raw, want := range cases {
		isolateEnv(t)
		t.Setenv("RAG_LIGHTRAG_SERVICE_URL", raw)
		cfg, err := RagConfigFromEnv()
		if err != nil {
			t.Fatalf("RagConfigFromEnv() error = %v", err)
		}
		if cfg.LightRAGServiceURL != want {
			t.Fatalf("LightRAGServiceURL = %q, want %q", cfg.LightRAGServiceURL, want)
		}
	}
}

func TestRagConfigRejectsInvalidLightRAGServiceURL(t *testing.T) {
	isolateEnv(t)
	for _, invalid := range []string{
		"ftp://lightrag.internal:8093",
		"lightrag.internal:8093",
		"http://lightrag.internal:8093/v1/workspaces?x=1",
		"http://lightrag.internal:8093/path#fragment",
	} {
		t.Setenv("RAG_LIGHTRAG_SERVICE_URL", invalid)
		if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_LIGHTRAG_SERVICE_URL") {
			t.Fatalf("URL %q error = %v", invalid, err)
		}
	}
	// A valid URL must leave the timeout default in place.
	t.Setenv("RAG_LIGHTRAG_SERVICE_URL", "https://lightrag.internal:8093")
	cfg, err := RagConfigFromEnv()
	if err != nil {
		t.Fatalf("valid URL error = %v", err)
	}
	if cfg.LightRAGServiceTimeout != 60*time.Second {
		t.Fatalf("LightRAGServiceTimeout = %v, want 60s", cfg.LightRAGServiceTimeout)
	}
}

func TestRagConfigLightRAGServiceTimeoutBounds(t *testing.T) {
	isolateEnv(t)
	for _, invalid := range []string{"0", "601", "abc"} {
		t.Setenv("RAG_LIGHTRAG_SERVICE_TIMEOUT", invalid)
		if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_LIGHTRAG_SERVICE_TIMEOUT") {
			t.Fatalf("timeout %q error = %v", invalid, err)
		}
	}
	for raw, want := range map[string]time.Duration{
		"1":   1 * time.Second,
		"600": 600 * time.Second,
	} {
		t.Setenv("RAG_LIGHTRAG_SERVICE_TIMEOUT", raw)
		cfg, err := RagConfigFromEnv()
		if err != nil {
			t.Fatalf("timeout %q error = %v", raw, err)
		}
		if cfg.LightRAGServiceTimeout != want {
			t.Fatalf("LightRAGServiceTimeout = %v, want %v", cfg.LightRAGServiceTimeout, want)
		}
	}
}
