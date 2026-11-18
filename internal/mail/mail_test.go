package mail

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/statetest"
)

// fakeStore is an in-memory SettingsStore.
type fakeStore struct {
	mu     sync.Mutex
	values map[string]string
}

func newFakeStore() *fakeStore { return &fakeStore{values: map[string]string{}} }

func (f *fakeStore) Get(_ context.Context, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, found := f.values[key]
	return value, found, nil
}

func (f *fakeStore) Put(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = value
	return nil
}

func validSettings() Settings {
	return Settings{
		Enabled: true,
		Host:    "smtp.example.com",
		Port:    587,
		From:    "bot@example.com",
		TLS:     TLSStartTLS,
	}
}

func TestSaveValidatesSettings(t *testing.T) {
	service := NewService(newFakeStore())
	cases := []struct {
		name     string
		settings Settings
		want     string
	}{
		{"port range", Settings{Enabled: true, Host: "smtp.example.com", From: "a@example.com", Port: 70000}, "端口"},
		{"tls mode", Settings{Enabled: true, Host: "smtp.example.com", From: "a@example.com", Port: 587, TLS: "ssl"}, "加密方式"},
		{"host required", Settings{Enabled: true, From: "a@example.com", Port: 587}, "SMTP 服务器地址"},
		{"from required", Settings{Enabled: true, Host: "smtp.example.com", Port: 587}, "发件人地址"},
		{"from format", Settings{Enabled: true, Host: "smtp.example.com", Port: 587, From: "not-an-address"}, "发件人地址格式"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := service.Save(context.Background(), testCase.settings)
			var validation *ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("Save() error = %v, want *ValidationError", err)
			}
			if !strings.Contains(validation.Message, testCase.want) {
				t.Fatalf("message = %q, want it to contain %q", validation.Message, testCase.want)
			}
		})
	}
}

func TestSaveAcceptsPartialSettingsWhileDisabled(t *testing.T) {
	service := NewService(newFakeStore())
	if err := service.Save(context.Background(), Settings{Enabled: false, Port: 25, TLS: TLSNone}); err != nil {
		t.Fatalf("Save(disabled partial) error = %v", err)
	}
	if service.Configured(context.Background()) {
		t.Fatal("Configured() = true for disabled settings")
	}
}

func TestPublicNeverLeaksPassword(t *testing.T) {
	store := newFakeStore()
	service := NewService(store, statetest.TestEncryptionKey)
	settings := validSettings()
	settings.Username = "bot@example.com"
	settings.Password = "smtp-secret"
	if err := service.Save(context.Background(), settings); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	raw := store.values[settingsKey]
	var stored storedSettings
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatalf("stored settings are not JSON: %v", err)
	}
	if !strings.HasPrefix(stored.Password, state.EncryptionPrefix) {
		t.Fatalf("stored password is not enc:v1 protected: %s", stored.Password)
	}
	if strings.Contains(raw, "smtp-secret") {
		t.Fatalf("stored settings contain the plaintext password: %s", raw)
	}
	public, err := service.Public(context.Background())
	if err != nil {
		t.Fatalf("Public() error = %v", err)
	}
	if _, found := public["password"]; found {
		t.Fatalf("Public() returned a password field: %v", public)
	}
	if public["password_set"] != true {
		t.Fatalf("password_set = %v, want true", public["password_set"])
	}
	if public["configured"] != true || public["host"] != "smtp.example.com" {
		t.Fatalf("Public() = %v", public)
	}
	loaded, err := service.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Password != "smtp-secret" {
		t.Fatalf("Load() password = %q, want the saved value", loaded.Password)
	}
}

func TestSaveWithoutPasswordKeepsStoredOne(t *testing.T) {
	service := NewService(newFakeStore(), statetest.TestEncryptionKey)
	ctx := context.Background()
	settings := validSettings()
	settings.Password = "smtp-secret"
	if err := service.Save(ctx, settings); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	next := validSettings()
	next.Host = "smtp2.example.com"
	next.Password = ""
	if err := service.Save(ctx, next); err != nil {
		t.Fatalf("Save(without password) error = %v", err)
	}
	loaded, err := service.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Password != "smtp-secret" || loaded.Host != "smtp2.example.com" {
		t.Fatalf("Load() = %+v", loaded)
	}
}

// startFakeSMTP serves one minimal SMTP session and returns its address and a
// channel carrying the DATA payload.
func startFakeSMTP(t *testing.T) (string, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received := make(chan string, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
		reader := bufio.NewReader(connection)
		writer := bufio.NewWriter(connection)
		writeLine := func(line string) {
			_, _ = writer.WriteString(line + "\r\n")
			_ = writer.Flush()
		}
		writeLine("220 fake-smtp ready")
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			command := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
				writeLine("250-fake-smtp")
				writeLine("250 OK")
			case strings.HasPrefix(command, "MAIL FROM"), strings.HasPrefix(command, "RCPT TO"):
				writeLine("250 OK")
			case command == "DATA":
				writeLine("354 End data with <CR><LF>.<CR><LF>")
				var body strings.Builder
				for {
					dataLine, dataErr := reader.ReadString('\n')
					if dataErr != nil {
						return
					}
					if strings.TrimRight(dataLine, "\r\n") == "." {
						break
					}
					body.WriteString(dataLine)
				}
				writeLine("250 queued")
				received <- body.String()
			case command == "QUIT":
				writeLine("221 bye")
				return
			default:
				writeLine("250 OK")
			}
		}
	}()
	return listener.Addr().String(), received
}

func TestSendDeliversToFakeSMTP(t *testing.T) {
	address, received := startFakeSMTP(t)
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	service := NewService(newFakeStore())
	settings := Settings{
		Enabled: true,
		Host:    host,
		Port:    port,
		From:    "bot@example.com",
		TLS:     TLSNone,
	}
	if err := service.Save(context.Background(), settings); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := service.Send(context.Background(), "user@example.com", "测试主题", "验证码 123456"); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	select {
	case message := <-received:
		headers, body, found := strings.Cut(message, "\r\n\r\n")
		if !found {
			t.Fatalf("message has no header separator: %q", message)
		}
		if !strings.Contains(strings.ToLower(headers), "subject: =?utf-8?") {
			t.Fatalf("subject is not encoded: %q", headers)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Split(body, "\r\n"), ""))
		if err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if string(decoded) != "验证码 123456" {
			t.Fatalf("body = %q", decoded)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fake SMTP server did not receive the message")
	}
}

func TestSendRequiresConfiguredServer(t *testing.T) {
	service := NewService(newFakeStore())
	err := service.Send(context.Background(), "user@example.com", "subject", "body")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Send() error = %v, want ErrNotConfigured", err)
	}
}

func TestSendRejectsInvalidRecipient(t *testing.T) {
	service := NewService(newFakeStore())
	if err := service.Save(context.Background(), validSettings()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	err := service.Send(context.Background(), "not-an-address", "subject", "body")
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Send() error = %v, want *ValidationError", err)
	}
}

func TestStartTLSRequiresServerSupport(t *testing.T) {
	address, _ := startFakeSMTP(t)
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split address: %v", err)
	}
	port, _ := strconv.Atoi(portText)
	service := NewService(newFakeStore())
	settings := Settings{Enabled: true, Host: host, Port: port, From: "bot@example.com", TLS: TLSStartTLS}
	if err := service.Save(context.Background(), settings); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	err = service.Send(context.Background(), "user@example.com", "subject", "body")
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("Send() error = %v, want a STARTTLS failure", err)
	}
}
