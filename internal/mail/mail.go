// Package mail implements the Console-editable SMTP mail server configuration
// and the outbound verification / test mail delivery.
//
// It depends only on the standard library: the SMTP conversation uses
// net/smtp, and the stored password is protected with state.Protect so it uses
// the same enc:v1: credential format as provider and API keys.
package mail

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// settingsKey is the app_settings row that holds the SMTP configuration.
const settingsKey = "mail"

// sendTimeout bounds one SMTP conversation (connect, EHLO, auth and DATA).
const sendTimeout = 10 * time.Second

// Supported TLS modes.
const (
	TLSNone     = "none"
	TLSStartTLS = "starttls"
	TLSDirect   = "tls"
)

// ErrNotConfigured reports that no usable mail server is configured.
var ErrNotConfigured = errors.New("邮件服务器未配置或未启用，请联系管理员")

// ValidationError is a user-fixable mail-settings problem.
type ValidationError struct{ Message string }

// Error implements error.
func (e *ValidationError) Error() string { return e.Message }

func newValidationError(message string) *ValidationError { return &ValidationError{Message: message} }

// Settings is the SMTP configuration. Password is plaintext and only exists in
// memory (and briefly while sending); it is never returned by Public.
type Settings struct {
	Enabled  bool
	Host     string
	Port     int
	Username string
	Password string
	From     string
	TLS      string
}

// normalized trims free-form values and fills the conventional defaults so the
// Console form and the stored row stay comparable.
func (s Settings) normalized() Settings {
	normalized := s
	normalized.Host = strings.TrimSpace(normalized.Host)
	normalized.Username = strings.TrimSpace(normalized.Username)
	normalized.From = strings.TrimSpace(normalized.From)
	normalized.TLS = strings.ToLower(strings.TrimSpace(normalized.TLS))
	if normalized.TLS == "" {
		normalized.TLS = TLSStartTLS
	}
	if normalized.Port == 0 {
		normalized.Port = 587
	}
	return normalized
}

// validate enforces the fields the Console form can fix. Partial settings are
// allowed while the switch is off so an administrator can prepare them.
func (s Settings) validate() error {
	if s.Port < 1 || s.Port > 65535 {
		return newValidationError("端口需为 1-65535 之间的整数")
	}
	switch s.TLS {
	case TLSNone, TLSStartTLS, TLSDirect:
	default:
		return newValidationError("加密方式只能是 none、starttls 或 tls")
	}
	if !s.Enabled {
		return nil
	}
	if s.Host == "" {
		return newValidationError("请填写 SMTP 服务器地址")
	}
	if s.From == "" {
		return newValidationError("请填写发件人地址")
	}
	if _, err := mail.ParseAddress(s.From); err != nil {
		return newValidationError("发件人地址格式不正确")
	}
	return nil
}

// configured reports whether the settings are complete enough to send mail.
func (s Settings) configured() bool {
	return s.Enabled && s.Host != "" && s.From != ""
}

// SettingsStore is the persistence surface required by Service; the state
// AppSettingsRepository satisfies it.
type SettingsStore interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Put(ctx context.Context, key, value string) error
}

// storedSettings is the persisted JSON shape. Password always holds the
// protected value (enc:v1: or a platform legacy format), never plaintext.
type storedSettings struct {
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
	TLS      string `json:"tls"`
}

// Service reads, validates and uses the SMTP configuration.
type Service struct {
	store         SettingsStore
	encryptionKey string
}

// NewService builds the mail service. The optional encryption key should be the
// resolved state key; an empty key keeps the legacy state.Protect fallback (the
// environment key, then the platform legacy format). A nil store keeps every
// method inert so a deployment without the state layer does not panic.
func NewService(store SettingsStore, encryptionKey ...string) *Service {
	key := ""
	if len(encryptionKey) > 0 {
		key = encryptionKey[0]
	}
	return &Service{store: store, encryptionKey: key}
}

// loadStored reads the raw stored row, keeping the protected password value.
func (s *Service) loadStored(ctx context.Context) (storedSettings, bool, error) {
	if s == nil || s.store == nil {
		return storedSettings{}, false, nil
	}
	raw, found, err := s.store.Get(ctx, settingsKey)
	if err != nil {
		return storedSettings{}, false, err
	}
	if !found || strings.TrimSpace(raw) == "" {
		return storedSettings{}, false, nil
	}
	var stored storedSettings
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return storedSettings{}, false, newValidationError("邮件服务器配置已损坏，请重新保存")
	}
	return stored, true, nil
}

// Load returns the configured settings with the password decrypted. An
// unconfigured deployment returns the zero settings.
func (s *Service) Load(ctx context.Context) (Settings, error) {
	stored, found, err := s.loadStored(ctx)
	if err != nil {
		return Settings{}, err
	}
	if !found {
		return Settings{}.normalized(), nil
	}
	password, err := state.Unprotect(stored.Password, s.encryptionKey)
	if err != nil {
		return Settings{}, fmt.Errorf("邮件服务器密码无法解密：%w", err)
	}
	return Settings{
		Enabled:  stored.Enabled,
		Host:     stored.Host,
		Port:     stored.Port,
		Username: stored.Username,
		Password: password,
		From:     stored.From,
		TLS:      stored.TLS,
	}.normalized(), nil
}

// Save validates and stores the settings. An empty password keeps the stored
// one so the Console can render a "password saved" placeholder.
func (s *Service) Save(ctx context.Context, settings Settings) error {
	if s == nil || s.store == nil {
		return newValidationError("邮件服务器配置不可用")
	}
	normalized := settings.normalized()
	if err := normalized.validate(); err != nil {
		return err
	}
	stored, found, err := s.loadStored(ctx)
	if err != nil {
		return err
	}
	if !found {
		stored = storedSettings{}
	}
	if normalized.Password != "" {
		protected, protectErr := state.Protect(normalized.Password, s.encryptionKey)
		if protectErr != nil {
			return fmt.Errorf("邮件服务器密码无法加密：%w", protectErr)
		}
		stored.Password = protected
	}
	stored.Enabled = normalized.Enabled
	stored.Host = normalized.Host
	stored.Port = normalized.Port
	stored.Username = normalized.Username
	stored.From = normalized.From
	stored.TLS = normalized.TLS
	payload, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("邮件服务器配置无法序列化：%w", err)
	}
	return s.store.Put(ctx, settingsKey, string(payload))
}

// Configured reports whether mail delivery is enabled and complete.
func (s *Service) Configured(ctx context.Context) bool {
	settings, err := s.Load(ctx)
	if err != nil {
		return false
	}
	return settings.configured()
}

// Public returns the Console projection. The password is replaced by a boolean
// flag and is never included, neither as plaintext nor as a protected value.
func (s *Service) Public(ctx context.Context) (map[string]any, error) {
	settings, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"enabled":      settings.Enabled,
		"host":         settings.Host,
		"port":         settings.Port,
		"tls":          settings.TLS,
		"username":     settings.Username,
		"password_set": settings.Password != "",
		"from":         settings.From,
		"configured":   settings.configured(),
	}, nil
}

// Send delivers one text/plain message. The error names the failing stage but
// never includes the password.
func (s *Service) Send(ctx context.Context, to, subject, body string) error {
	settings, err := s.Load(ctx)
	if err != nil {
		return err
	}
	if !settings.configured() {
		return ErrNotConfigured
	}
	recipient, err := mail.ParseAddress(strings.TrimSpace(to))
	if err != nil {
		return newValidationError("收件人地址格式不正确")
	}
	sender, err := mail.ParseAddress(settings.From)
	if err != nil {
		return newValidationError("发件人地址格式不正确")
	}
	client, err := dialSMTP(settings)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("发送邮件失败：%w", ctxErr)
	}
	if settings.Username != "" {
		auth := smtp.PlainAuth("", settings.Username, settings.Password, settings.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP 认证失败：%w", err)
		}
	}
	if err := client.Mail(sender.Address); err != nil {
		return fmt.Errorf("SMTP 发件人被拒绝：%w", err)
	}
	if err := client.Rcpt(recipient.Address); err != nil {
		return fmt.Errorf("SMTP 收件人被拒绝：%w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP 无法开始写入邮件：%w", err)
	}
	if _, err := writer.Write([]byte(buildMessage(settings.From, recipient.Address, subject, body))); err != nil {
		_ = writer.Close()
		return fmt.Errorf("SMTP 邮件内容写入失败：%w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("SMTP 邮件无法提交：%w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("SMTP 会话结束失败：%w", err)
	}
	return nil
}

// dialSMTP opens the transport for the configured TLS mode. Every mode is
// bounded by sendTimeout and reports its own stage on failure.
func dialSMTP(settings Settings) (*smtp.Client, error) {
	address := net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port))
	deadline := time.Now().Add(sendTimeout)
	if settings.TLS == TLSDirect {
		dialer := &net.Dialer{Timeout: sendTimeout, Deadline: deadline}
		connection, err := tls.DialWithDialer(dialer, "tcp", address, &tls.Config{
			ServerName: settings.Host,
			MinVersion: tls.VersionTLS12,
		})
		if err != nil {
			return nil, fmt.Errorf("连接邮件服务器失败：%w", err)
		}
		_ = connection.SetDeadline(deadline)
		client, err := smtp.NewClient(connection, settings.Host)
		if err != nil {
			_ = connection.Close()
			return nil, fmt.Errorf("SMTP 会话建立失败：%w", err)
		}
		return client, nil
	}
	connection, err := net.DialTimeout("tcp", address, sendTimeout)
	if err != nil {
		return nil, fmt.Errorf("连接邮件服务器失败：%w", err)
	}
	_ = connection.SetDeadline(deadline)
	client, err := smtp.NewClient(connection, settings.Host)
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("SMTP 会话建立失败：%w", err)
	}
	if settings.TLS == TLSStartTLS {
		if supported, _ := client.Extension("STARTTLS"); !supported {
			_ = client.Close()
			return nil, errors.New("邮件服务器不支持 STARTTLS")
		}
		if err := client.StartTLS(&tls.Config{ServerName: settings.Host, MinVersion: tls.VersionTLS12}); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("STARTTLS 握手失败：%w", err)
		}
	}
	return client, nil
}

// buildMessage renders one base64 UTF-8 text/plain message so Chinese subjects
// and bodies survive strict SMTP relays.
func buildMessage(from, to, subject, body string) string {
	encodedSubject := subject
	for _, character := range subject {
		if character > 127 {
			encodedSubject = mime.BEncoding.Encode("UTF-8", subject)
			break
		}
	}
	var builder strings.Builder
	builder.WriteString("From: " + from + "\r\n")
	builder.WriteString("To: " + to + "\r\n")
	builder.WriteString("Subject: " + encodedSubject + "\r\n")
	builder.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	builder.WriteString("MIME-Version: 1.0\r\n")
	builder.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	builder.WriteString("Content-Transfer-Encoding: base64\r\n")
	builder.WriteString("\r\n")
	builder.WriteString(base64.StdEncoding.EncodeToString([]byte(body)))
	builder.WriteString("\r\n")
	return builder.String()
}
