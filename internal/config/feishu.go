package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultFeishuAPIBaseURL is the Feishu Open Platform API origin.
const DefaultFeishuAPIBaseURL = "https://open.feishu.cn"

// DefaultFeishuAccountsBaseURL is the Feishu OAuth origin.
const DefaultFeishuAccountsBaseURL = "https://accounts.feishu.cn"

// FeishuSettings configures the optional Feishu (Lark) import integration.
//
// AppID and AppSecret belong to the deployment. Each Console account then
// authorizes its own Feishu user and the integration calls the Open Platform
// with that account's user_access_token. When AppID, AppSecret and RedirectURI
// are all empty the integration is disabled; a partially configured trio is a
// startup error so a typo cannot silently disable the feature.
type FeishuSettings struct {
	AppID           string
	AppSecret       string
	RedirectURI     string
	APIBaseURL      string
	AccountsBaseURL string
	// Timeout bounds one Feishu HTTP call.
	Timeout time.Duration
}

// Enabled reports whether the deployment configured the integration.
func (s FeishuSettings) Enabled() bool {
	return s.AppID != "" && s.AppSecret != "" && s.RedirectURI != ""
}

// FeishuSettingsFromEnv loads the optional Feishu integration configuration.
// Secrets stay in the process environment; only AppID/redirect/base URLs are
// expected in config.toml.
func FeishuSettingsFromEnv() (FeishuSettings, error) {
	appID := strings.TrimSpace(os.Getenv("RAG_FEISHU_APP_ID"))
	appSecret := strings.TrimSpace(os.Getenv("RAG_FEISHU_APP_SECRET"))
	redirectURI := strings.TrimSpace(os.Getenv("RAG_FEISHU_REDIRECT_URI"))
	if appID == "" && appSecret == "" && redirectURI == "" {
		return FeishuSettings{
			APIBaseURL:      DefaultFeishuAPIBaseURL,
			AccountsBaseURL: DefaultFeishuAccountsBaseURL,
			Timeout:         20 * time.Second,
		}, nil
	}
	if appID == "" || appSecret == "" || redirectURI == "" {
		return FeishuSettings{}, fmt.Errorf("RAG_FEISHU_APP_ID、RAG_FEISHU_APP_SECRET 和 RAG_FEISHU_REDIRECT_URI 必须同时配置")
	}
	if err := validateAbsoluteHTTPURL("RAG_FEISHU_REDIRECT_URI", redirectURI); err != nil {
		return FeishuSettings{}, err
	}
	apiBase := strings.TrimRight(strings.TrimSpace(os.Getenv("RAG_FEISHU_API_BASE_URL")), "/")
	if apiBase == "" {
		apiBase = DefaultFeishuAPIBaseURL
	}
	if err := validateAbsoluteHTTPURL("RAG_FEISHU_API_BASE_URL", apiBase); err != nil {
		return FeishuSettings{}, err
	}
	accountsBase := strings.TrimRight(strings.TrimSpace(os.Getenv("RAG_FEISHU_ACCOUNTS_BASE_URL")), "/")
	if accountsBase == "" {
		accountsBase = DefaultFeishuAccountsBaseURL
	}
	if err := validateAbsoluteHTTPURL("RAG_FEISHU_ACCOUNTS_BASE_URL", accountsBase); err != nil {
		return FeishuSettings{}, err
	}
	timeout := 20 * time.Second
	if raw := strings.TrimSpace(os.Getenv("RAG_FEISHU_TIMEOUT_SECONDS")); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 || seconds > 300 {
			return FeishuSettings{}, fmt.Errorf("RAG_FEISHU_TIMEOUT_SECONDS must be an integer between 1 and 300")
		}
		timeout = time.Duration(seconds) * time.Second
	}
	return FeishuSettings{
		AppID:           appID,
		AppSecret:       appSecret,
		RedirectURI:     redirectURI,
		APIBaseURL:      apiBase,
		AccountsBaseURL: accountsBase,
		Timeout:         timeout,
	}, nil
}

// validateAbsoluteHTTPURL accepts an absolute http(s) URL without a fragment.
func validateAbsoluteHTTPURL(name, value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("%s must be an absolute http(s) URL", name)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s must be an absolute http(s) URL", name)
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("%s must not contain a fragment", name)
	}
	return nil
}
