package config

import (
	"strings"
	"testing"
)

func TestRagConfigPDFDefaults(t *testing.T) {
	isolateEnv(t)
	cfg, err := RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.PDFEngine != PDFEngineGo {
		t.Fatalf("PDFEngine = %q, want %q", cfg.PDFEngine, PDFEngineGo)
	}
	if cfg.PDFServiceURL != "" {
		t.Fatalf("PDFServiceURL = %q, want empty", cfg.PDFServiceURL)
	}
	if cfg.PDFServiceTimeout != 900 {
		t.Fatalf("PDFServiceTimeout = %d, want 900", cfg.PDFServiceTimeout)
	}
}

func TestRagConfigPDFEngineResolution(t *testing.T) {
	cases := []struct {
		name       string
		engine     string
		serviceURL string
		wantEngine string
		wantURL    string
	}{
		{name: "url_selects_docling", serviceURL: "https://docling.internal:8091", wantEngine: PDFEngineDocling, wantURL: "https://docling.internal:8091"},
		{name: "explicit_go_with_url", engine: "go", serviceURL: "https://docling.internal", wantEngine: PDFEngineGo, wantURL: "https://docling.internal"},
		{name: "explicit_docling_defaults_url", engine: "docling", wantEngine: PDFEngineDocling, wantURL: DefaultPDFServiceURL},
		{name: "explicit_docling_keeps_url", engine: "docling", serviceURL: "http://127.0.0.1:9000/", wantEngine: PDFEngineDocling, wantURL: "http://127.0.0.1:9000"},
		{name: "uppercase_engine", engine: "DOCLING", wantEngine: PDFEngineDocling, wantURL: DefaultPDFServiceURL},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			isolateEnv(t)
			if testCase.engine != "" {
				t.Setenv("RAG_PDF_ENGINE", testCase.engine)
			}
			if testCase.serviceURL != "" {
				t.Setenv("RAG_PDF_SERVICE_URL", testCase.serviceURL)
			}
			cfg, err := RagConfigFromEnv()
			if err != nil {
				t.Fatalf("RagConfigFromEnv() error = %v", err)
			}
			if cfg.PDFEngine != testCase.wantEngine {
				t.Fatalf("PDFEngine = %q, want %q", cfg.PDFEngine, testCase.wantEngine)
			}
			if cfg.PDFServiceURL != testCase.wantURL {
				t.Fatalf("PDFServiceURL = %q, want %q", cfg.PDFServiceURL, testCase.wantURL)
			}
		})
	}
}

func TestRagConfigRejectsInvalidPDFEngine(t *testing.T) {
	isolateEnv(t)
	t.Setenv("RAG_PDF_ENGINE", "poppler")
	if _, err := RagConfigFromEnv(); err == nil ||
		err.Error() != "RAG_PDF_ENGINE must be go or docling" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRagConfigRejectsInvalidPDFServiceURL(t *testing.T) {
	isolateEnv(t)
	for _, invalid := range []string{
		"ftp://docling.internal:8091",
		"docling.internal:8091",
		"http://docling.internal:8091/v1/parse?x=1",
		"http://docling.internal:8091/path#fragment",
	} {
		t.Setenv("RAG_PDF_SERVICE_URL", invalid)
		if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_PDF_SERVICE_URL") {
			t.Fatalf("URL %q error = %v", invalid, err)
		}
	}
	t.Setenv("RAG_PDF_SERVICE_URL", "https://docling.internal:8091")
	if _, err := RagConfigFromEnv(); err != nil {
		t.Fatalf("valid URL error = %v", err)
	}
}

func TestRagConfigPDFServiceTimeoutBounds(t *testing.T) {
	isolateEnv(t)
	for _, invalid := range []string{"0", "7201", "abc"} {
		t.Setenv("RAG_PDF_SERVICE_TIMEOUT", invalid)
		if _, err := RagConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_PDF_SERVICE_TIMEOUT") {
			t.Fatalf("timeout %q error = %v", invalid, err)
		}
	}
	t.Setenv("RAG_PDF_SERVICE_TIMEOUT", "7200")
	cfg, err := RagConfigFromEnv()
	if err != nil {
		t.Fatalf("RagConfigFromEnv() error = %v", err)
	}
	if cfg.PDFServiceTimeout != 7200 {
		t.Fatalf("PDFServiceTimeout = %d, want 7200", cfg.PDFServiceTimeout)
	}
}
