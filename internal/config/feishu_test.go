package config

import (
	"strings"
	"testing"
	"time"
)

func TestFeishuSettingsDisabledWhenUnset(t *testing.T) {
	t.Setenv("RAG_FEISHU_APP_ID", "")
	t.Setenv("RAG_FEISHU_APP_SECRET", "")
	t.Setenv("RAG_FEISHU_REDIRECT_URI", "")
	settings, err := FeishuSettingsFromEnv()
	if err != nil {
		t.Fatalf("FeishuSettingsFromEnv() error = %v", err)
	}
	if settings.Enabled() {
		t.Fatal("empty Feishu settings must be disabled")
	}
	if settings.APIBaseURL != DefaultFeishuAPIBaseURL || settings.AccountsBaseURL != DefaultFeishuAccountsBaseURL {
		t.Fatalf("default base URLs = %q / %q", settings.APIBaseURL, settings.AccountsBaseURL)
	}
	if settings.Timeout != 20*time.Second {
		t.Fatalf("default timeout = %v", settings.Timeout)
	}
}

func TestFeishuSettingsRequiresCompleteTrio(t *testing.T) {
	t.Setenv("RAG_FEISHU_APP_ID", "cli_test")
	t.Setenv("RAG_FEISHU_APP_SECRET", "")
	t.Setenv("RAG_FEISHU_REDIRECT_URI", "https://rag.example.com/v1/feishu/oauth/callback")
	_, err := FeishuSettingsFromEnv()
	if err == nil || !strings.Contains(err.Error(), "必须同时配置") {
		t.Fatalf("error = %v", err)
	}
}

func TestFeishuSettingsLoadsValues(t *testing.T) {
	t.Setenv("RAG_FEISHU_APP_ID", "cli_test")
	t.Setenv("RAG_FEISHU_APP_SECRET", "secret")
	t.Setenv("RAG_FEISHU_REDIRECT_URI", "https://rag.example.com/v1/feishu/oauth/callback")
	t.Setenv("RAG_FEISHU_API_BASE_URL", "https://open.feishu.cn/")
	t.Setenv("RAG_FEISHU_ACCOUNTS_BASE_URL", "https://accounts.feishu.cn/")
	t.Setenv("RAG_FEISHU_TIMEOUT_SECONDS", "45")
	settings, err := FeishuSettingsFromEnv()
	if err != nil {
		t.Fatalf("FeishuSettingsFromEnv() error = %v", err)
	}
	if !settings.Enabled() {
		t.Fatal("complete settings must be enabled")
	}
	if settings.APIBaseURL != "https://open.feishu.cn" || settings.AccountsBaseURL != "https://accounts.feishu.cn" {
		t.Fatalf("trimmed base URLs = %q / %q", settings.APIBaseURL, settings.AccountsBaseURL)
	}
	if settings.Timeout != 45*time.Second {
		t.Fatalf("timeout = %v", settings.Timeout)
	}
}

func TestFeishuSettingsRejectsInvalidValues(t *testing.T) {
	t.Setenv("RAG_FEISHU_APP_ID", "cli_test")
	t.Setenv("RAG_FEISHU_APP_SECRET", "secret")
	t.Setenv("RAG_FEISHU_REDIRECT_URI", "/relative/callback")
	if _, err := FeishuSettingsFromEnv(); err == nil {
		t.Fatal("relative redirect URI must be rejected")
	}

	t.Setenv("RAG_FEISHU_REDIRECT_URI", "https://rag.example.com/callback")
	t.Setenv("RAG_FEISHU_TIMEOUT_SECONDS", "0")
	if _, err := FeishuSettingsFromEnv(); err == nil {
		t.Fatal("zero timeout must be rejected")
	}
}
