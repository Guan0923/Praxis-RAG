// Package payment implements the Alipay computer-website payment gateway
// (alipay.trade.page.pay) used by the Console membership purchase flow.
//
// It deliberately depends on the standard library only: signatures are RSA
// PKCS#1 v1.5 over SHA-256 (RSA2), requests are form-encoded POSTs to the
// Alipay open gateway, and money is always an integer number of cents. No
// third-party SDK and no floating point arithmetic are involved.
package payment

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Gateway endpoints of the Alipay open platform.
const (
	// DefaultGateway is the live Alipay gateway.
	DefaultGateway = "https://openapi.alipay.com/gateway.do"
	// SandboxGateway is the Alipay sandbox gateway selected by
	// RAG_ALIPAY_ENV=sandbox when RAG_ALIPAY_GATEWAY is not set.
	SandboxGateway = "https://openapi-sandbox.dl.alipaydev.com/gateway.do"
)

// Environment names accepted by RAG_ALIPAY_ENV.
const (
	// EnvironmentProduction talks to the live Alipay gateway.
	EnvironmentProduction = "production"
	// EnvironmentSandbox talks to the Alipay sandbox gateway.
	EnvironmentSandbox = "sandbox"
)

// DefaultTimeoutMinutes is the default payment window of one membership order.
const DefaultTimeoutMinutes = 30

// MaxTimeoutMinutes is Alipay's upper bound for timeout_express (15 days).
const MaxTimeoutMinutes = 15 * 24 * 60

// Environment variable names of the Alipay integration. They are part of the
// deployment contract and must stay stable.
const (
	// EnvAppID identifies the Alipay open-platform application.
	EnvAppID = "RAG_ALIPAY_APP_ID"
	// EnvPrivateKey carries the application private key (PEM or base64 DER).
	EnvPrivateKey = "RAG_ALIPAY_PRIVATE_KEY"
	// EnvPublicKey carries the Alipay public key or X.509 certificate.
	EnvPublicKey = "RAG_ALIPAY_PUBLIC_KEY"
	// EnvGateway overrides the Alipay gateway URL.
	EnvGateway = "RAG_ALIPAY_GATEWAY"
	// EnvEnvironment selects production or sandbox defaults.
	EnvEnvironment = "RAG_ALIPAY_ENV"
	// EnvNotifyURL is the asynchronous notification endpoint.
	EnvNotifyURL = "RAG_ALIPAY_NOTIFY_URL"
	// EnvReturnURL is the browser return endpoint after payment.
	EnvReturnURL = "RAG_ALIPAY_RETURN_URL"
	// EnvSellerID is the receiving Alipay PID checked on notifications.
	EnvSellerID = "RAG_ALIPAY_SELLER_ID"
	// EnvTimeoutMinutes is the order payment window in minutes.
	EnvTimeoutMinutes = "RAG_ALIPAY_TIMEOUT_MINUTES"
	// EnvPurchaseEnabled switches the membership purchase entry on.
	EnvPurchaseEnabled = "RAG_MEMBERSHIP_PURCHASE_ENABLED"
)

// ErrNotConfigured reports missing or unusable Alipay payment configuration.
var ErrNotConfigured = errors.New("支付宝支付未配置")

// Settings is the Alipay gateway configuration. It is filled from the
// environment (SettingsFromEnv) or from a deployment-specific source; secrets
// are only kept in memory and are never included in errors or logs.
type Settings struct {
	// AppID is the Alipay open-platform application ID.
	AppID string
	// PrivateKeyPEM is the application private key: PKCS#1/PKCS#8 PEM or bare
	// base64 DER. Literal `\n` sequences are accepted.
	PrivateKeyPEM string
	// PublicKeyPEM is the Alipay public key: PKIX PEM, X.509 certificate PEM or
	// bare base64 DER.
	PublicKeyPEM string
	// Gateway is the Alipay gateway URL; an empty value resolves to the default
	// gateway of Environment.
	Gateway string
	// NotifyURL receives the asynchronous payment notification.
	NotifyURL string
	// ReturnURL receives the browser after payment.
	ReturnURL string
	// SellerID is the receiving Alipay PID (收款方 PID). Notifications must
	// carry this seller_id.
	SellerID string
	// TimeoutMinutes is the payment window written to timeout_express.
	TimeoutMinutes int
	// PurchaseEnabled exposes the "buy membership" entry in the Console.
	PurchaseEnabled bool
	// Environment is EnvironmentProduction or EnvironmentSandbox.
	Environment string
}

// normalized trims free-form values, expands literal `\n` sequences in keys and
// fills the documented defaults so every later step sees the same settings.
func (s Settings) normalized() Settings {
	normalized := s
	normalized.AppID = strings.TrimSpace(normalized.AppID)
	normalized.PrivateKeyPEM = normalizeKeyMaterial(normalized.PrivateKeyPEM)
	normalized.PublicKeyPEM = normalizeKeyMaterial(normalized.PublicKeyPEM)
	normalized.Gateway = strings.TrimSpace(normalized.Gateway)
	normalized.NotifyURL = strings.TrimSpace(normalized.NotifyURL)
	normalized.ReturnURL = strings.TrimSpace(normalized.ReturnURL)
	normalized.SellerID = strings.TrimSpace(normalized.SellerID)
	normalized.Environment = normalizeEnvironment(normalized.Environment)
	if normalized.Environment == "" {
		normalized.Environment = EnvironmentProduction
	}
	if normalized.Gateway == "" {
		normalized.Gateway = defaultGatewayFor(normalized.Environment)
	}
	if normalized.TimeoutMinutes == 0 {
		normalized.TimeoutMinutes = DefaultTimeoutMinutes
	}
	return normalized
}

// normalizeEnvironment maps the accepted spellings onto the two environment
// names. Unknown values are returned trimmed and lower-cased so that Validate
// and SettingsFromEnv can reject them.
func normalizeEnvironment(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "prod", "production", "live", "online":
		return EnvironmentProduction
	case "sandbox":
		return EnvironmentSandbox
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

// defaultGatewayFor returns the gateway belonging to one environment.
func defaultGatewayFor(environment string) string {
	if environment == EnvironmentSandbox {
		return SandboxGateway
	}
	return DefaultGateway
}

// Configured reports whether every credential and callback address is present.
func (s Settings) Configured() bool {
	for _, value := range []string{s.AppID, s.PrivateKeyPEM, s.PublicKeyPEM, s.NotifyURL, s.ReturnURL, s.SellerID} {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

// PurchaseAvailable reports whether the membership purchase entry may be shown
// and used: the integration is fully configured and the feature switch is on.
func (s Settings) PurchaseAvailable() bool {
	return s.Configured() && s.PurchaseEnabled
}

// Validate verifies that the settings are complete and usable. It returns a
// Chinese error that never contains key material.
func (s Settings) Validate() error {
	normalized := s.normalized()
	missing := make([]string, 0, 6)
	required := []struct {
		value string
		name  string
	}{
		{normalized.AppID, "AppID"},
		{normalized.PrivateKeyPEM, "应用私钥"},
		{normalized.PublicKeyPEM, "支付宝公钥"},
		{normalized.NotifyURL, "异步通知地址"},
		{normalized.ReturnURL, "同步跳转地址"},
		{normalized.SellerID, "收款方 PID"},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w：缺少 %s", ErrNotConfigured, strings.Join(missing, "、"))
	}
	if _, err := parsePrivateKey(normalized.PrivateKeyPEM); err != nil {
		return err
	}
	if _, err := parsePublicKey(normalized.PublicKeyPEM); err != nil {
		return err
	}
	if err := validateGatewayURL(normalized.Gateway); err != nil {
		return err
	}
	if err := validateCallbackURL(normalized.NotifyURL, "异步通知地址"); err != nil {
		return err
	}
	if err := validateCallbackURL(normalized.ReturnURL, "同步跳转地址"); err != nil {
		return err
	}
	switch normalized.Environment {
	case EnvironmentProduction, EnvironmentSandbox:
	default:
		return fmt.Errorf("%w：RAG_ALIPAY_ENV 只能是 sandbox 或 production", ErrNotConfigured)
	}
	if normalized.TimeoutMinutes < 1 || normalized.TimeoutMinutes > MaxTimeoutMinutes {
		return fmt.Errorf("%w：RAG_ALIPAY_TIMEOUT_MINUTES 必须是 1 到 %d 之间的整数", ErrNotConfigured, MaxTimeoutMinutes)
	}
	return nil
}

// SettingsFromEnv loads the Alipay settings from the environment. It does not
// require a complete configuration: a deployment that keeps the feature off may
// leave the credentials empty, in which case Configured reports false. Invalid
// individual values (bad boolean, bad integer, malformed gateway or callback
// address) are rejected so a typo never silently reaches the live gateway.
func SettingsFromEnv() (Settings, error) {
	environment, err := environmentSetting()
	if err != nil {
		return Settings{}, err
	}
	timeoutMinutes, err := timeoutMinutesSetting()
	if err != nil {
		return Settings{}, err
	}
	purchaseEnabled, err := purchaseEnabledSetting()
	if err != nil {
		return Settings{}, err
	}
	gateway := optionalEnv(EnvGateway)
	if gateway == "" {
		gateway = defaultGatewayFor(environment)
	} else if err := validateGatewayURL(gateway); err != nil {
		return Settings{}, fmt.Errorf("%s 必须是 http(s) 绝对地址", EnvGateway)
	}
	notifyURL := optionalEnv(EnvNotifyURL)
	if notifyURL != "" {
		if err := validateCallbackURL(notifyURL, "RAG_ALIPAY_NOTIFY_URL"); err != nil {
			return Settings{}, err
		}
	}
	returnURL := optionalEnv(EnvReturnURL)
	if returnURL != "" {
		if err := validateCallbackURL(returnURL, "RAG_ALIPAY_RETURN_URL"); err != nil {
			return Settings{}, err
		}
	}
	return Settings{
		AppID:           optionalEnv(EnvAppID),
		PrivateKeyPEM:   normalizeKeyMaterial(os.Getenv(EnvPrivateKey)),
		PublicKeyPEM:    normalizeKeyMaterial(os.Getenv(EnvPublicKey)),
		Gateway:         gateway,
		NotifyURL:       notifyURL,
		ReturnURL:       returnURL,
		SellerID:        optionalEnv(EnvSellerID),
		TimeoutMinutes:  timeoutMinutes,
		PurchaseEnabled: purchaseEnabled,
		Environment:     environment,
	}, nil
}

// environmentSetting parses RAG_ALIPAY_ENV.
func environmentSetting() (string, error) {
	switch environment := normalizeEnvironment(os.Getenv(EnvEnvironment)); environment {
	case EnvironmentProduction, EnvironmentSandbox:
		return environment, nil
	default:
		return "", fmt.Errorf("%s 只能是 sandbox 或 production", EnvEnvironment)
	}
}

// timeoutMinutesSetting parses RAG_ALIPAY_TIMEOUT_MINUTES.
func timeoutMinutesSetting() (int, error) {
	raw := strings.TrimSpace(os.Getenv(EnvTimeoutMinutes))
	if raw == "" {
		return DefaultTimeoutMinutes, nil
	}
	minutes, err := strconv.Atoi(raw)
	if err != nil || minutes < 1 || minutes > MaxTimeoutMinutes {
		return 0, fmt.Errorf("%s 必须是 1 到 %d 之间的整数", EnvTimeoutMinutes, MaxTimeoutMinutes)
	}
	return minutes, nil
}

// purchaseEnabledSetting parses RAG_MEMBERSHIP_PURCHASE_ENABLED. It defaults to
// false: the membership purchase entry stays hidden unless it is switched on.
func purchaseEnabledSetting() (bool, error) {
	raw, ok := os.LookupEnv(EnvPurchaseEnabled)
	if !ok {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false", "no", "off":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	default:
		return false, fmt.Errorf("%s 必须是 true 或 false", EnvPurchaseEnabled)
	}
}

// optionalEnv returns the trimmed value of an environment variable or "" when
// it is unset or blank.
func optionalEnv(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// validateGatewayURL checks that the Alipay gateway is an http(s) absolute URL.
func validateGatewayURL(raw string) error {
	if !isAbsoluteHTTPURL(raw) {
		return fmt.Errorf("%w：支付宝网关地址必须是 http(s) 绝对地址", ErrNotConfigured)
	}
	return nil
}

// validateCallbackURL checks that one callback address is an http(s) absolute
// URL.
func validateCallbackURL(raw, label string) error {
	if !isAbsoluteHTTPURL(raw) {
		return fmt.Errorf("%w：%s 必须是 http(s) 绝对地址", ErrNotConfigured, label)
	}
	return nil
}

// isAbsoluteHTTPURL reports whether raw parses as an absolute http or https URL
// with a host.
func isAbsoluteHTTPURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Host != ""
}
