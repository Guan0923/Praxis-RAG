package httpapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/auth"
	"github.com/Guan0923/Praxis-RAG/internal/mail"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// fakeAccountProfile is an in-memory AccountProfile for handler tests.
type fakeAccountProfile struct {
	verificationEnabled bool

	registerVal   auth.Login
	registerErr   error
	registerInput auth.RegisterInput

	codeErr error
	codes   []string

	updateVal  *state.User
	updateErr  error
	updateArgs struct {
		userID   string
		username string
		age      *int
		gender   string
	}

	passwordErr error
	passwordArg struct {
		userID          string
		currentPassword string
		newPassword     string
	}

	emailChangeErr error
	emailChangeArg struct {
		userID   string
		newEmail string
		password string
	}

	changeEmailErr error
	changeEmailVal *state.User
	changeEmailArg struct {
		userID   string
		newEmail string
		code     string
	}

	setAvatarErr         error
	setAvatarVal         *state.User
	setAvatarContentType string
	setAvatarData        []byte

	avatarVal *state.UserAvatar
	avatarErr error

	deleteAvatarErr error
	deleteAvatarVal *state.User
}

func (f *fakeAccountProfile) RegistrationVerificationEnabled(context.Context) bool {
	return f.verificationEnabled
}

func (f *fakeAccountProfile) RegisterAccount(_ context.Context, in auth.RegisterInput) (auth.Login, error) {
	f.registerInput = in
	return f.registerVal, f.registerErr
}

func (f *fakeAccountProfile) RequestRegisterCode(_ context.Context, email string) error {
	f.codes = append(f.codes, email)
	return f.codeErr
}

func (f *fakeAccountProfile) UpdateProfile(_ context.Context, userID, username string, age *int, gender string) (*state.User, error) {
	f.updateArgs.userID, f.updateArgs.username, f.updateArgs.age, f.updateArgs.gender = userID, username, age, gender
	return f.updateVal, f.updateErr
}

func (f *fakeAccountProfile) ChangePassword(_ context.Context, userID, currentPassword, newPassword string) error {
	f.passwordArg.userID, f.passwordArg.currentPassword, f.passwordArg.newPassword = userID, currentPassword, newPassword
	return f.passwordErr
}

func (f *fakeAccountProfile) RequestEmailChange(_ context.Context, userID, newEmail, password string) error {
	f.emailChangeArg.userID, f.emailChangeArg.newEmail, f.emailChangeArg.password = userID, newEmail, password
	return f.emailChangeErr
}

func (f *fakeAccountProfile) ChangeEmail(_ context.Context, userID, newEmail, code string) (*state.User, error) {
	f.changeEmailArg.userID, f.changeEmailArg.newEmail, f.changeEmailArg.code = userID, newEmail, code
	return f.changeEmailVal, f.changeEmailErr
}

func (f *fakeAccountProfile) SetAvatar(_ context.Context, userID, contentType string, data []byte) (*state.User, error) {
	f.setAvatarContentType, f.setAvatarData = contentType, data
	return f.setAvatarVal, f.setAvatarErr
}

func (f *fakeAccountProfile) Avatar(context.Context, string) (*state.UserAvatar, error) {
	return f.avatarVal, f.avatarErr
}

func (f *fakeAccountProfile) DeleteAvatar(context.Context, string) (*state.User, error) {
	return f.deleteAvatarVal, f.deleteAvatarErr
}

// fakeMailAdmin is an in-memory MailAdmin for handler tests.
type fakeMailAdmin struct {
	publicVal map[string]any
	publicErr error

	savedVal map[string]any
	saveErr  error
	saved    mail.Settings

	testErr error
	tested  string
}

func (f *fakeMailAdmin) MailPublic(context.Context) (map[string]any, error) {
	return f.publicVal, f.publicErr
}

func (f *fakeMailAdmin) SaveMailSettings(_ context.Context, settings mail.Settings) (map[string]any, error) {
	f.saved = settings
	return f.savedVal, f.saveErr
}

func (f *fakeMailAdmin) SendTestMail(_ context.Context, to string) error {
	f.tested = to
	return f.testErr
}

func newProfileTestServer(t *testing.T, users UserAuth, profile AccountProfile, admin MailAdmin) *Server {
	t.Helper()
	settings := testSettings("")
	settings.AuthEnabled = true
	settings.RegistrationEnabled = true
	settings.SessionTTL = time.Hour
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(settings, nil, nil, logger,
		WithUserAuth(users),
		WithAccountProfile(profile),
		WithMailAdmin(admin),
	)
}

// profileSessionServer wires one authenticated bearer session plus the profile
// and mail fakes.
func profileSessionServer(t *testing.T, token string, profile AccountProfile, admin MailAdmin) *Server {
	t.Helper()
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{token: userSession(token)}}
	return newProfileTestServer(t, users, profile, admin)
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func profileUser() *state.User {
	age := 28
	avatar := "2026-11-13T10:00:00.000000+00:00"
	return &state.User{
		ID: "u-user", Username: "bob", Role: state.RoleUser, Enabled: true,
		CreatedAt: "2026-11-09T10:00:00.000000+00:00", Email: "bob@example.com",
		EmailVerified: true, Age: &age, Gender: "male", AvatarUpdatedAt: &avatar,
	}
}

func TestAuthStatusReportsEmailVerification(t *testing.T) {
	profile := &fakeAccountProfile{verificationEnabled: true}
	server := newProfileTestServer(t, &fakeUserAuth{registration: true}, profile, nil)
	response := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/auth/status", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if body := decodeBody(t, response); body["email_verification_enabled"] != true {
		t.Fatalf("email_verification_enabled = %v", body["email_verification_enabled"])
	}
}

func TestRegisterPassesEmailAndCodeToAccountProfile(t *testing.T) {
	profile := &fakeAccountProfile{registerVal: auth.Login{
		User:      state.User{ID: "u-new", Username: "alice", Role: state.RoleAdmin, Enabled: true},
		Token:     "token-new",
		CSRF:      "csrf-new",
		ExpiresAt: time.Now().Add(time.Hour),
	}}
	server := newProfileTestServer(t, &fakeUserAuth{registration: true}, profile, nil)
	response := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/auth/register", remote: "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"username":"alice","password":"password-one","email":"Alice@Example.com","code":"123456"}`,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if profile.registerInput.Username != "alice" || profile.registerInput.Email != "Alice@Example.com" ||
		profile.registerInput.Code != "123456" {
		t.Fatalf("register input = %+v", profile.registerInput)
	}
}

func TestRegisterCodeMapsUnavailableAndTooFrequent(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"unavailable", &auth.Error{Kind: auth.KindUnavailable, Message: auth.MessageMailUnavailable}, http.StatusServiceUnavailable},
		{"too frequent", &auth.Error{Kind: auth.KindConflict, Message: auth.MessageEmailCodeTooFrequent}, http.StatusTooManyRequests},
		{"invalid email", &auth.Error{Kind: auth.KindValidation, Message: auth.MessageEmailInvalid}, http.StatusUnprocessableEntity},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			profile := &fakeAccountProfile{codeErr: testCase.err}
			server := newProfileTestServer(t, &fakeUserAuth{registration: true}, profile, nil)
			response := performRequest(server.Handler(), testRequest{
				method: "POST", target: "/v1/auth/register/code", remote: "127.0.0.1:5000",
				headers: map[string]string{"Content-Type": "application/json"},
				body:    `{"email":"a@example.com"}`,
			})
			if response.Code != testCase.wantStatus {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestUpdateProfileReturnsExtendedPayload(t *testing.T) {
	profile := &fakeAccountProfile{updateVal: profileUser()}
	server := profileSessionServer(t, "user-token", profile, nil)
	response := performRequest(server.Handler(), testRequest{
		method: "PATCH", target: "/v1/auth/profile", remote: "127.0.0.1:5000",
		headers: withContentType(bearer("user-token")),
		body:    `{"username":"bobby","age":28,"gender":"male"}`,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	body := decodeBody(t, response)
	user, _ := body["user"].(map[string]any)
	if user == nil {
		t.Fatalf("body = %v", body)
	}
	if user["email"] != "bob@example.com" || user["email_verified"] != true ||
		user["age"] != float64(28) || user["gender"] != "male" ||
		user["has_avatar"] != true || user["avatar_updated_at"] == nil {
		t.Fatalf("user payload = %v", user)
	}
	if profile.updateArgs.userID != "u-user" || profile.updateArgs.username != "bobby" ||
		profile.updateArgs.gender != "male" || profile.updateArgs.age == nil || *profile.updateArgs.age != 28 {
		t.Fatalf("update args = %+v", profile.updateArgs)
	}
}

func TestUpdateProfileRequiresSession(t *testing.T) {
	server := newProfileTestServer(t, &fakeUserAuth{sessions: map[string]*auth.SessionUser{}}, &fakeAccountProfile{}, nil)
	response := performRequest(server.Handler(), testRequest{
		method: "PATCH", target: "/v1/auth/profile", remote: "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"username":"bobby","age":null,"gender":""}`,
	})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}

// TestChangePasswordWrongCurrentPasswordIs422 guards the contract rule that a
// wrong current password must never surface as 401 (which would sign the user
// out in the Console).
func TestChangePasswordWrongCurrentPasswordIs422(t *testing.T) {
	profile := &fakeAccountProfile{passwordErr: &auth.Error{
		Kind: auth.KindValidation, Message: auth.MessageCurrentPasswordWrong,
	}}
	server := profileSessionServer(t, "user-token", profile, nil)
	response := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/auth/password", remote: "127.0.0.1:5000",
		headers: withContentType(bearer("user-token")),
		body:    `{"current_password":"wrong-one","new_password":"password-two"}`,
	})
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	body := decodeBody(t, response)
	if body["detail"] != auth.MessageCurrentPasswordWrong {
		t.Fatalf("detail = %v", body["detail"])
	}
}

func TestChangePasswordRejectsShortNewPassword(t *testing.T) {
	profile := &fakeAccountProfile{}
	server := profileSessionServer(t, "user-token", profile, nil)
	response := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/auth/password", remote: "127.0.0.1:5000",
		headers: withContentType(bearer("user-token")),
		body:    `{"current_password":"password-one","new_password":"short"}`,
	})
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if profile.passwordArg.userID != "" {
		t.Fatalf("ChangePassword was called for a short password: %+v", profile.passwordArg)
	}
}

func TestEmailChangeFlowRoutes(t *testing.T) {
	profile := &fakeAccountProfile{changeEmailVal: profileUser()}
	server := profileSessionServer(t, "user-token", profile, nil)
	codeResponse := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/auth/email/change/code", remote: "127.0.0.1:5000",
		headers: withContentType(bearer("user-token")),
		body:    `{"email":"new@example.com","password":"password-one"}`,
	})
	if codeResponse.Code != http.StatusNoContent {
		t.Fatalf("code status = %d body = %s", codeResponse.Code, codeResponse.Body.String())
	}
	if profile.emailChangeArg.newEmail != "new@example.com" || profile.emailChangeArg.userID != "u-user" {
		t.Fatalf("email change args = %+v", profile.emailChangeArg)
	}
	applyResponse := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/auth/email/change", remote: "127.0.0.1:5000",
		headers: withContentType(bearer("user-token")),
		body:    `{"email":"new@example.com","code":"654321"}`,
	})
	if applyResponse.Code != http.StatusOK {
		t.Fatalf("apply status = %d body = %s", applyResponse.Code, applyResponse.Body.String())
	}
	if profile.changeEmailArg.code != "654321" {
		t.Fatalf("change email args = %+v", profile.changeEmailArg)
	}
}

// multipartAvatar builds one avatar multipart body.
func multipartAvatar(t *testing.T, contentType string, data []byte) (string, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="avatar"; filename="avatar.bin"`)
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return writer.FormDataContentType(), buffer.String()
}

var pngAvatarBytes = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}

func TestAvatarUploadReadAndDelete(t *testing.T) {
	profile := &fakeAccountProfile{setAvatarVal: profileUser()}
	server := profileSessionServer(t, "user-token", profile, nil)
	contentType, body := multipartAvatar(t, "image/png", pngAvatarBytes)
	headers := bearer("user-token")
	headers["Content-Type"] = contentType
	upload := performRequest(server.Handler(), testRequest{
		method: "PUT", target: "/v1/auth/avatar", remote: "127.0.0.1:5000",
		headers: headers, body: body,
	})
	if upload.Code != http.StatusOK {
		t.Fatalf("upload status = %d body = %s", upload.Code, upload.Body.String())
	}
	if profile.setAvatarContentType != "image/png" || !bytes.Equal(profile.setAvatarData, pngAvatarBytes) {
		t.Fatalf("SetAvatar(contentType=%q, len=%d)", profile.setAvatarContentType, len(profile.setAvatarData))
	}
	user, _ := decodeBody(t, upload)["user"].(map[string]any)
	if user == nil || user["has_avatar"] != true {
		t.Fatalf("upload payload = %v", user)
	}

	profile.avatarVal = &state.UserAvatar{
		UserID: "u-user", ContentType: "image/png", Size: int64(len(pngAvatarBytes)), Content: pngAvatarBytes,
	}
	read := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/auth/avatar", remote: "127.0.0.1:5000",
		headers: bearer("user-token"),
	})
	if read.Code != http.StatusOK {
		t.Fatalf("read status = %d body = %s", read.Code, read.Body.String())
	}
	if got := read.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("content type = %q", got)
	}
	if got := read.Header().Get("Cache-Control"); got != "private, max-age=300" {
		t.Fatalf("cache control = %q", got)
	}
	if !bytes.Equal(read.Body.Bytes(), pngAvatarBytes) {
		t.Fatalf("avatar bytes = %v", read.Body.Bytes())
	}

	remove := performRequest(server.Handler(), testRequest{
		method: "DELETE", target: "/v1/auth/avatar", remote: "127.0.0.1:5000",
		headers: bearer("user-token"),
	})
	if remove.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body = %s", remove.Code, remove.Body.String())
	}
}

func TestAvatarMissingReturns404(t *testing.T) {
	profile := &fakeAccountProfile{}
	server := profileSessionServer(t, "user-token", profile, nil)
	response := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/auth/avatar", remote: "127.0.0.1:5000",
		headers: bearer("user-token"),
	})
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}

func TestAvatarRejectsNonImagePayload(t *testing.T) {
	profile := &fakeAccountProfile{}
	server := profileSessionServer(t, "user-token", profile, nil)
	contentType, body := multipartAvatar(t, "image/png", []byte("not an image"))
	headers := bearer("user-token")
	headers["Content-Type"] = contentType
	response := performRequest(server.Handler(), testRequest{
		method: "PUT", target: "/v1/auth/avatar", remote: "127.0.0.1:5000",
		headers: headers, body: body,
	})
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if profile.setAvatarData != nil {
		t.Fatal("SetAvatar was called with invalid data")
	}
}

func TestMailSettingsAdminFlow(t *testing.T) {
	admin := &fakeMailAdmin{
		publicVal: map[string]any{"enabled": true, "host": "smtp.example.com", "port": 587, "password_set": true},
		savedVal:  map[string]any{"enabled": true, "host": "smtp2.example.com", "port": 465, "password_set": true},
	}
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"admin-token": adminSession("admin-token")}}
	server := newProfileTestServer(t, users, &fakeAccountProfile{}, admin)

	list := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/admin/mail-settings", remote: "127.0.0.1:5000",
		headers: bearer("admin-token"),
	})
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", list.Code, list.Body.String())
	}
	if _, leaked := decodeBody(t, list)["settings"].(map[string]any)["password"]; leaked {
		t.Fatal("mail settings leaked a password field")
	}

	save := performRequest(server.Handler(), testRequest{
		method: "PUT", target: "/v1/admin/mail-settings", remote: "127.0.0.1:5000",
		headers: withContentType(bearer("admin-token")),
		body:    `{"enabled":true,"host":"smtp2.example.com","port":465,"tls":"tls","username":"bot@example.com","password":"secret","from":"bot@example.com"}`,
	})
	if save.Code != http.StatusOK {
		t.Fatalf("save status = %d body = %s", save.Code, save.Body.String())
	}
	if admin.saved.Host != "smtp2.example.com" || admin.saved.Port != 465 || admin.saved.TLS != "tls" ||
		admin.saved.Password != "secret" {
		t.Fatalf("saved settings = %+v", admin.saved)
	}

	testMail := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/admin/mail-settings/test", remote: "127.0.0.1:5000",
		headers: withContentType(bearer("admin-token")),
		body:    `{"to":"ops@example.com"}`,
	})
	if testMail.Code != http.StatusNoContent {
		t.Fatalf("test status = %d body = %s", testMail.Code, testMail.Body.String())
	}
	if admin.tested != "ops@example.com" {
		t.Fatalf("tested = %q", admin.tested)
	}
}

func TestMailSettingsRequireAdminSession(t *testing.T) {
	admin := &fakeMailAdmin{publicVal: map[string]any{}}
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token")}}
	server := newProfileTestServer(t, users, &fakeAccountProfile{}, admin)
	response := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/admin/mail-settings", remote: "203.0.113.9:5000",
		headers: bearer("user-token"),
	})
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}

func TestMailSettingsUnavailableMaps503(t *testing.T) {
	admin := &fakeMailAdmin{publicErr: &auth.Error{Kind: auth.KindUnavailable, Message: auth.MessageMailUnavailable}}
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"admin-token": adminSession("admin-token")}}
	server := newProfileTestServer(t, users, &fakeAccountProfile{}, admin)
	response := performRequest(server.Handler(), testRequest{
		method: "GET", target: "/v1/admin/mail-settings", remote: "127.0.0.1:5000",
		headers: bearer("admin-token"),
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}

func TestTestMailSendFailureMaps502(t *testing.T) {
	admin := &fakeMailAdmin{testErr: context.DeadlineExceeded}
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"admin-token": adminSession("admin-token")}}
	server := newProfileTestServer(t, users, &fakeAccountProfile{}, admin)
	response := performRequest(server.Handler(), testRequest{
		method: "POST", target: "/v1/admin/mail-settings/test", remote: "127.0.0.1:5000",
		headers: withContentType(bearer("admin-token")),
		body:    `{"to":"ops@example.com"}`,
	})
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}

func TestAvatarRejectsOversizedImage(t *testing.T) {
	profile := &fakeAccountProfile{}
	server := profileSessionServer(t, "user-token", profile, nil)
	oversized := make([]byte, state.MaxAvatarBytes+1)
	copy(oversized, pngAvatarBytes)
	contentType, body := multipartAvatar(t, "image/png", oversized)
	headers := bearer("user-token")
	headers["Content-Type"] = contentType
	response := performRequest(server.Handler(), testRequest{
		method: "PUT", target: "/v1/auth/avatar", remote: "127.0.0.1:5000",
		headers: headers, body: body,
	})
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if profile.setAvatarData != nil {
		t.Fatal("SetAvatar was called with an oversized image")
	}
}

func TestDetectAvatarType(t *testing.T) {
	cases := []struct {
		name     string
		declared string
		data     []byte
		want     string
	}{
		{"png", "image/png", pngAvatarBytes, "image/png"},
		{"jpeg", "image/jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "image/jpeg"},
		{"gif", "image/gif", []byte("GIF89a...."), "image/gif"},
		{"webp", "image/webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp"},
		{"declared mismatch", "image/png", []byte("GIF89a...."), ""},
		{"unsupported type", "image/bmp", []byte("BM\x00\x00"), ""},
		{"empty data", "image/png", nil, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := detectAvatarType(testCase.declared, testCase.data); got != testCase.want {
				t.Fatalf("detectAvatarType(%q) = %q, want %q", testCase.declared, got, testCase.want)
			}
		})
	}
}

// withContentType adds the JSON content type to a header map.
func withContentType(headers map[string]string) map[string]string {
	headers["Content-Type"] = "application/json"
	return headers
}
