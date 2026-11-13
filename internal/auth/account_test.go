package auth

import (
	"bufio"
	"context"
	"encoding/base64"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/mail"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/statetest"
)

// verificationCodePattern extracts the six digit code from a delivered mail.
var verificationCodePattern = regexp.MustCompile(`\b[0-9]{6}\b`)

// fakeSMTPServer is a minimal SMTP server used by the account tests: it speaks
// just enough of the protocol for net/smtp and records the DATA payloads.
type fakeSMTPServer struct {
	address string
	codes   chan string

	mu       sync.Mutex
	messages []string
}

// startFakeSMTPServer listens on loopback and serves SMTP sessions until the
// test ends.
func startFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &fakeSMTPServer{address: listener.Addr().String(), codes: make(chan string, 16)}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go server.serve(connection)
		}
	}()
	return server
}

// serve runs one SMTP conversation.
func (s *fakeSMTPServer) serve(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)
	reply := func(line string) {
		_, _ = writer.WriteString(line + "\r\n")
		_ = writer.Flush()
	}
	reply("220 fake-smtp ready")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			reply("250-fake-smtp")
			reply("250 OK")
		case strings.HasPrefix(command, "MAIL FROM"), strings.HasPrefix(command, "RCPT TO"):
			reply("250 OK")
		case command == "DATA":
			reply("354 End data with <CR><LF>.<CR><LF>")
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
			reply("250 queued")
			s.record(body.String())
		case strings.HasPrefix(command, "AUTH"):
			reply("235 2.7.0 Authentication successful")
		case command == "QUIT":
			reply("221 bye")
			return
		case command == "RSET", command == "NOOP":
			reply("250 OK")
		default:
			reply("250 OK")
		}
	}
}

// record stores one delivered message and publishes its verification code, if
// any. The mail package sends base64 text bodies, so decode before matching.
func (s *fakeSMTPServer) record(message string) {
	s.mu.Lock()
	s.messages = append(s.messages, message)
	s.mu.Unlock()
	_, body, found := strings.Cut(message, "\r\n\r\n")
	if !found {
		return
	}
	text := body
	if decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Split(body, "\r\n"), "")); err == nil {
		text = string(decoded)
	}
	code := verificationCodePattern.FindString(text)
	if code == "" {
		return
	}
	select {
	case s.codes <- code:
	default:
	}
}

// nextCode waits for the next delivered verification code.
func (s *fakeSMTPServer) nextCode(t *testing.T) string {
	t.Helper()
	select {
	case code := <-s.codes:
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("fake SMTP server did not deliver a verification code")
		return ""
	}
}

// messageCount reports how many messages were delivered so far.
func (s *fakeSMTPServer) messageCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}

// accountTestFixture is one auth service backed by the disposable MySQL schema
// and a fake SMTP server.
type accountTestFixture struct {
	service *Service
	stores  *state.Stores
	mail    *mail.Service
	smtp    *fakeSMTPServer
}

// newAccountTestService wires the auth service with mail, verification codes and
// avatars, pointing the stored SMTP settings at the fake server.
func newAccountTestService(t *testing.T) *accountTestFixture {
	t.Helper()
	smtp := startFakeSMTPServer(t)
	url, key := statetest.Connection(t)
	stores, err := state.New(state.StateSettings{
		URL:           url,
		EncryptionKey: key,
		AutoSchema:    true,
		PoolSize:      5,
		MaxOverflow:   10,
		PoolRecycle:   1800,
		PoolTimeout:   30,
	}, true)
	if err != nil {
		t.Fatalf("state.New(MySQL) error = %v", err)
	}
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	mailService := mail.NewService(stores.AppSettings)
	host, portText, err := net.SplitHostPort(smtp.address)
	if err != nil {
		t.Fatalf("splitting fake SMTP address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parsing fake SMTP port: %v", err)
	}
	if err := mailService.Save(context.Background(), mail.Settings{
		Enabled: true,
		Host:    host,
		Port:    port,
		From:    "bot@example.com",
		TLS:     mail.TLSNone,
	}); err != nil {
		t.Fatalf("saving fake SMTP settings: %v", err)
	}
	service := NewService(stores.Users, stores.Sessions, Options{
		RegistrationEnabled: true,
		SessionTTL:          time.Hour,
		Mail:                mailService,
		Codes:               stores.EmailCodes,
		Avatars:             stores.Avatars,
	})
	return &accountTestFixture{service: service, stores: stores, mail: mailService, smtp: smtp}
}

// registerWithEmail runs the full request-code / register flow and returns the
// issued login.
func (f *accountTestFixture) registerWithEmail(t *testing.T, username, password, email string) Login {
	t.Helper()
	ctx := context.Background()
	if err := f.service.RequestRegisterCode(ctx, email); err != nil {
		t.Fatalf("RequestRegisterCode(%q) error = %v", email, err)
	}
	code := f.smtp.nextCode(t)
	login, err := f.service.RegisterAccount(ctx, RegisterInput{
		Username: username,
		Password: password,
		Email:    email,
		Code:     code,
	})
	if err != nil {
		t.Fatalf("RegisterAccount(%q) error = %v", username, err)
	}
	return login
}

// wrongVerificationCode returns a six digit code that differs from the real one.
func wrongVerificationCode(code string) string {
	if code == "000000" {
		return "111111"
	}
	return "000000"
}

func TestRegisterAccountVerifiesEmail(t *testing.T) {
	fixture := newAccountTestService(t)
	ctx := context.Background()
	if !fixture.service.RegistrationVerificationEnabled(ctx) {
		t.Fatal("RegistrationVerificationEnabled() = false with a configured mail server")
	}

	login := fixture.registerWithEmail(t, "alice", "password-one", "Alice@Example.com")
	if login.Token == "" || login.CSRF == "" {
		t.Fatalf("login = %+v", login)
	}
	// The address is normalized to lower case and marked verified.
	if login.User.Email != "alice@example.com" || !login.User.EmailVerified {
		t.Fatalf("registered user = %+v", login.User)
	}
	stored, err := fixture.stores.Users.ByEmail(ctx, "alice@example.com")
	if err != nil || stored == nil || stored.ID != login.User.ID || !stored.EmailVerified {
		t.Fatalf("ByEmail() = %+v, %v", stored, err)
	}
	session, err := fixture.service.Authenticate(ctx, login.Token)
	if err != nil || session == nil || session.User.EmailVerified != true {
		t.Fatalf("Authenticate() = %+v, %v", session, err)
	}

	// A second account cannot take the same address; the conflict is detected
	// before the code is even checked.
	if _, err := fixture.service.RegisterAccount(ctx, RegisterInput{
		Username: "bob", Password: "password-two", Email: "alice@example.com", Code: "123456",
	}); err == nil {
		t.Fatal("duplicate email registration succeeded")
	} else {
		assertAuthError(t, err, KindConflict, MessageEmailTaken)
	}
	// Requesting a code for a registered address is rejected for the same reason.
	assertAuthError(t, fixture.service.RequestRegisterCode(ctx, "alice@example.com"),
		KindConflict, MessageEmailTaken)

	// A duplicate username is still reported, with a code for a fresh address.
	if err := fixture.service.RequestRegisterCode(ctx, "bob@example.com"); err != nil {
		t.Fatalf("RequestRegisterCode(bob) error = %v", err)
	}
	bobCode := fixture.smtp.nextCode(t)
	if _, err := fixture.service.RegisterAccount(ctx, RegisterInput{
		Username: "alice", Password: "password-two", Email: "bob@example.com", Code: bobCode,
	}); err == nil {
		t.Fatal("duplicate username registration succeeded")
	} else {
		assertAuthError(t, err, KindConflict, MessageUsernameTaken)
	}

	// Missing verification data and malformed addresses are validation problems.
	if _, err := fixture.service.RegisterAccount(ctx, RegisterInput{
		Username: "carol", Password: "password-three", Email: "carol@example.com",
	}); err == nil {
		t.Fatal("registration without a code succeeded")
	} else {
		assertAuthError(t, err, KindValidation, MessageEmailCodeRequired)
	}
	if _, err := fixture.service.RegisterAccount(ctx, RegisterInput{
		Username: "carol", Password: "password-three", Email: "not-an-email", Code: "123456",
	}); err == nil {
		t.Fatal("registration with a malformed address succeeded")
	} else {
		assertAuthError(t, err, KindValidation, MessageEmailInvalid)
	}
}

func TestRegisterCodeCooldownAndAttemptLimit(t *testing.T) {
	fixture := newAccountTestService(t)
	ctx := context.Background()

	if err := fixture.service.RequestRegisterCode(ctx, "rate@example.com"); err != nil {
		t.Fatalf("RequestRegisterCode() error = %v", err)
	}
	code := fixture.smtp.nextCode(t)
	if len(code) != verificationCodeDigits {
		t.Fatalf("delivered code = %q", code)
	}
	// A second request inside the one minute cooldown is rejected.
	assertAuthError(t, fixture.service.RequestRegisterCode(ctx, "rate@example.com"),
		KindConflict, MessageEmailCodeTooFrequent)

	// Five wrong guesses exhaust the code.
	wrong := wrongVerificationCode(code)
	for attempt := 1; attempt <= verificationCodeAttempts; attempt++ {
		_, err := fixture.service.RegisterAccount(ctx, RegisterInput{
			Username: "rate-limited", Password: "password-one", Email: "rate@example.com", Code: wrong,
		})
		if err == nil {
			t.Fatalf("wrong attempt %d succeeded", attempt)
		}
		assertAuthError(t, err, KindValidation, MessageEmailCodeInvalid)
	}
	var attempts int
	if err := fixture.stores.DB.QueryRowContext(ctx,
		"SELECT attempts FROM email_verification_codes WHERE email = ? ORDER BY created_at DESC, id DESC LIMIT 1",
		"rate@example.com").Scan(&attempts); err != nil {
		t.Fatalf("reading attempts: %v", err)
	}
	if attempts != verificationCodeAttempts {
		t.Fatalf("stored attempts = %d, want %d", attempts, verificationCodeAttempts)
	}
	// Even the originally delivered code is now unusable.
	if _, err := fixture.service.RegisterAccount(ctx, RegisterInput{
		Username: "rate-limited", Password: "password-one", Email: "rate@example.com", Code: code,
	}); err == nil {
		t.Fatal("exhausted code was accepted")
	} else {
		assertAuthError(t, err, KindValidation, MessageEmailCodeInvalid)
	}

	// A different address is unaffected by the first one's cooldown.
	if err := fixture.service.RequestRegisterCode(ctx, "fresh@example.com"); err != nil {
		t.Fatalf("RequestRegisterCode(fresh) error = %v", err)
	}
}

func TestChangePasswordValidation(t *testing.T) {
	fixture := newAccountTestService(t)
	ctx := context.Background()
	login := fixture.registerWithEmail(t, "alice", "password-one", "alice@example.com")
	userID := login.User.ID

	// The wrong current password must be a validation failure (422), never an
	// authentication failure, so the Console does not sign the user out.
	err := fixture.service.ChangePassword(ctx, userID, "wrong-password", "password-two")
	if err == nil {
		t.Fatal("ChangePassword(wrong current) succeeded")
	}
	assertAuthError(t, err, KindValidation, MessageCurrentPasswordWrong)

	assertAuthError(t, fixture.service.ChangePassword(ctx, userID, "password-one", "short"),
		KindValidation, MessagePasswordInvalid)
	assertAuthError(t, fixture.service.ChangePassword(ctx, "no-such-id", "password-one", "password-two"),
		KindNotFound, MessageUserNotFound)

	if err := fixture.service.ChangePassword(ctx, userID, "password-one", "password-two"); err != nil {
		t.Fatalf("ChangePassword() error = %v", err)
	}
	if _, err := fixture.service.Login(ctx, "alice", "password-one"); err == nil {
		t.Fatal("Login with the old password succeeded")
	} else {
		assertAuthError(t, err, KindUnauthorized, MessageCredentials)
	}
	if _, err := fixture.service.Login(ctx, "alice", "password-two"); err != nil {
		t.Fatalf("Login with the new password error = %v", err)
	}
}

func TestUpdateProfileAndAvatar(t *testing.T) {
	fixture := newAccountTestService(t)
	ctx := context.Background()
	alice := fixture.registerWithEmail(t, "alice", "password-one", "alice@example.com")
	bob := fixture.registerWithEmail(t, "bob", "password-two", "bob@example.com")

	age := 28
	updated, err := fixture.service.UpdateProfile(ctx, alice.User.ID, "alice-renamed", &age, "female")
	if err != nil || updated == nil {
		t.Fatalf("UpdateProfile() = %+v, %v", updated, err)
	}
	if updated.Username != "alice-renamed" || updated.Age == nil || *updated.Age != 28 ||
		updated.Gender != "female" || updated.Email != "alice@example.com" {
		t.Fatalf("updated profile = %+v", updated)
	}
	// nil age and an empty gender clear the optional fields.
	cleared, err := fixture.service.UpdateProfile(ctx, alice.User.ID, "alice-renamed", nil, "")
	if err != nil || cleared == nil || cleared.Age != nil || cleared.Gender != "" {
		t.Fatalf("cleared profile = %+v, %v", cleared, err)
	}

	invalidAge := 0
	_, err = fixture.service.UpdateProfile(ctx, alice.User.ID, "", &invalidAge, "")
	assertAuthError(t, err, KindValidation, MessageAgeInvalid)
	tooOld := 151
	_, err = fixture.service.UpdateProfile(ctx, alice.User.ID, "", &tooOld, "")
	assertAuthError(t, err, KindValidation, MessageAgeInvalid)
	_, err = fixture.service.UpdateProfile(ctx, alice.User.ID, "", nil, "unknown")
	assertAuthError(t, err, KindValidation, MessageGenderInvalid)
	_, err = fixture.service.UpdateProfile(ctx, alice.User.ID, "ab", nil, "")
	assertAuthError(t, err, KindValidation, MessageUsernameInvalid)
	// Renaming onto another account's username conflicts.
	_, err = fixture.service.UpdateProfile(ctx, alice.User.ID, "bob", nil, "")
	assertAuthError(t, err, KindConflict, MessageUsernameTaken)
	_, err = fixture.service.UpdateProfile(ctx, "no-such-id", "ghost", nil, "")
	assertAuthError(t, err, KindNotFound, MessageUserNotFound)

	// Avatar: valid image bytes are stored and reflected on the user.
	avatarBytes := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3}
	withAvatar, err := fixture.service.SetAvatar(ctx, bob.User.ID, "image/png", avatarBytes)
	if err != nil || withAvatar == nil || withAvatar.AvatarUpdatedAt == nil {
		t.Fatalf("SetAvatar() = %+v, %v", withAvatar, err)
	}
	stored, err := fixture.service.Avatar(ctx, bob.User.ID)
	if err != nil || stored == nil || string(stored.Content) != string(avatarBytes) ||
		stored.ContentType != "image/png" {
		t.Fatalf("Avatar() = %+v, %v", stored, err)
	}
	_, err = fixture.service.SetAvatar(ctx, bob.User.ID, "image/bmp", avatarBytes)
	assertAuthError(t, err, KindValidation, MessageAvatarInvalid)
	_, err = fixture.service.SetAvatar(ctx, bob.User.ID, "image/png", make([]byte, state.MaxAvatarBytes+1))
	assertAuthError(t, err, KindValidation, MessageAvatarInvalid)
	withoutAvatar, err := fixture.service.DeleteAvatar(ctx, bob.User.ID)
	if err != nil || withoutAvatar == nil || withoutAvatar.AvatarUpdatedAt != nil {
		t.Fatalf("DeleteAvatar() = %+v, %v", withoutAvatar, err)
	}
}

func TestEmailChangeFlow(t *testing.T) {
	fixture := newAccountTestService(t)
	ctx := context.Background()
	alice := fixture.registerWithEmail(t, "alice", "password-one", "alice@example.com")
	bob := fixture.registerWithEmail(t, "bob", "password-two", "bob@example.com")

	// The wrong current password is a validation failure here too.
	assertAuthError(t, fixture.service.RequestEmailChange(ctx, alice.User.ID, "new@example.com", "wrong"),
		KindValidation, MessageCurrentPasswordWrong)
	assertAuthError(t, fixture.service.RequestEmailChange(ctx, alice.User.ID, "not-an-email", "password-one"),
		KindValidation, MessageEmailInvalid)
	assertAuthError(t, fixture.service.RequestEmailChange(ctx, alice.User.ID, bob.User.Email, "password-one"),
		KindConflict, MessageEmailTaken)

	if err := fixture.service.RequestEmailChange(ctx, alice.User.ID, "new@example.com", "password-one"); err != nil {
		t.Fatalf("RequestEmailChange() error = %v", err)
	}
	code := fixture.smtp.nextCode(t)
	if wrong := wrongVerificationCode(code); wrong != code {
		if _, err := fixture.service.ChangeEmail(ctx, alice.User.ID, "new@example.com", wrong); err == nil {
			t.Fatal("ChangeEmail(wrong code) succeeded")
		} else {
			assertAuthError(t, err, KindValidation, MessageEmailCodeInvalid)
		}
	}
	changed, err := fixture.service.ChangeEmail(ctx, alice.User.ID, "new@example.com", code)
	if err != nil || changed == nil {
		t.Fatalf("ChangeEmail() = %+v, %v", changed, err)
	}
	if changed.Email != "new@example.com" || !changed.EmailVerified {
		t.Fatalf("changed user = %+v", changed)
	}
	if stored, err := fixture.stores.Users.ByEmail(ctx, "new@example.com"); err != nil || stored == nil || stored.ID != alice.User.ID {
		t.Fatalf("ByEmail(new) = %+v, %v", stored, err)
	}
	// The old address is free again.
	if stale, err := fixture.stores.Users.ByEmail(ctx, "alice@example.com"); err != nil || stale != nil {
		t.Fatalf("ByEmail(old) = %+v, %v", stale, err)
	}
	// The same code cannot be replayed.
	if _, err := fixture.service.ChangeEmail(ctx, alice.User.ID, "new@example.com", code); err == nil {
		t.Fatal("ChangeEmail replayed a consumed code")
	}
}

func TestMailSettingsAndTestMail(t *testing.T) {
	fixture := newAccountTestService(t)
	ctx := context.Background()

	settings, err := fixture.service.MailPublic(ctx)
	if err != nil || settings == nil {
		t.Fatalf("MailPublic() = %+v, %v", settings, err)
	}
	if settings["configured"] != true || settings["host"] == "" {
		t.Fatalf("MailPublic() = %+v", settings)
	}
	if _, leaked := settings["password"]; leaked {
		t.Fatalf("MailPublic() leaked a password: %+v", settings)
	}

	saved, err := fixture.service.SaveMailSettings(ctx, mail.Settings{
		Enabled:  true,
		Host:     settings["host"].(string),
		Port:     settings["port"].(int),
		Username: "bot@example.com",
		Password: "smtp-secret",
		From:     "bot@example.com",
		TLS:      mail.TLSNone,
	})
	if err != nil {
		t.Fatalf("SaveMailSettings() error = %v", err)
	}
	if saved["password_set"] != true {
		t.Fatalf("Saved projection = %+v", saved)
	}
	if _, leaked := saved["password"]; leaked {
		t.Fatalf("SaveMailSettings() leaked a password: %+v", saved)
	}
	// The password is protected at rest but round-trips through the service.
	loaded, err := fixture.mail.Load(ctx)
	if err != nil || loaded.Password != "smtp-secret" {
		t.Fatalf("Load() = %+v, %v", loaded, err)
	}
	var raw string
	if err := fixture.stores.DB.QueryRowContext(ctx,
		"SELECT setting_value FROM app_settings WHERE setting_key = ?", "mail").Scan(&raw); err != nil {
		t.Fatalf("reading stored mail settings: %v", err)
	}
	if strings.Contains(raw, "smtp-secret") {
		t.Fatalf("stored mail settings contain the plaintext password: %s", raw)
	}

	_, err = fixture.service.SaveMailSettings(ctx, mail.Settings{
		Enabled: true, Host: "smtp.example.com", Port: 587, From: "bot@example.com", TLS: "ssl",
	})
	assertAuthError(t, err, KindValidation, "加密方式只能是 none、starttls 或 tls")
	if err := fixture.service.SendTestMail(ctx, "ops@example.com"); err != nil {
		t.Fatalf("SendTestMail() error = %v", err)
	}
	if fixture.smtp.messageCount() < 1 {
		t.Fatal("SendTestMail() did not reach the SMTP server")
	}
	assertAuthError(t, fixture.service.SendTestMail(ctx, "not-an-address"), KindValidation, MessageEmailInvalid)
}

func TestUnconfiguredMailKeepsLegacyRegistration(t *testing.T) {
	// Open a second fixture without saving SMTP settings: the stored settings
	// stay absent, so e-mail verification is disabled and the legacy
	// username/password registration still works.
	url, key := statetest.Connection(t)
	stores, err := state.New(state.StateSettings{
		URL:           url,
		EncryptionKey: key,
		AutoSchema:    true,
		PoolSize:      5,
		MaxOverflow:   10,
		PoolRecycle:   1800,
		PoolTimeout:   30,
	}, true)
	if err != nil {
		t.Fatalf("state.New(MySQL) error = %v", err)
	}
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	mailService := mail.NewService(stores.AppSettings)
	service := NewService(stores.Users, stores.Sessions, Options{
		RegistrationEnabled: true,
		SessionTTL:          time.Hour,
		Mail:                mailService,
		Codes:               stores.EmailCodes,
		Avatars:             stores.Avatars,
	})
	ctx := context.Background()

	if service.RegistrationVerificationEnabled(ctx) {
		t.Fatal("RegistrationVerificationEnabled() = true without SMTP settings")
	}
	// The legacy registration path works without an address...
	legacy, err := service.RegisterAccount(ctx, RegisterInput{Username: "legacy", Password: "password-one"})
	if err != nil || legacy.User.Email != "" || legacy.User.EmailVerified {
		t.Fatalf("RegisterAccount(legacy) = %+v, %v", legacy.User, err)
	}
	// ...and an address is optional and stays unverified.
	withEmail, err := service.RegisterAccount(ctx, RegisterInput{
		Username: "optional", Password: "password-two", Email: "Optional@Example.com",
	})
	if err != nil {
		t.Fatalf("RegisterAccount(optional email) error = %v", err)
	}
	if withEmail.User.Email != "optional@example.com" || withEmail.User.EmailVerified {
		t.Fatalf("optional-email user = %+v", withEmail.User)
	}
	// Mail-dependent operations report the missing configuration.
	assertAuthError(t, service.RequestRegisterCode(ctx, "code@example.com"),
		KindUnavailable, MessageMailUnavailable)
	assertAuthError(t, service.SendTestMail(ctx, "ops@example.com"),
		KindUnavailable, MessageMailUnavailable)
	projection, err := service.MailPublic(ctx)
	if err != nil {
		t.Fatalf("MailPublic() error = %v", err)
	}
	if projection["enabled"] != false || projection["configured"] != false || projection["password_set"] != false {
		t.Fatalf("unconfigured projection = %+v", projection)
	}
}
