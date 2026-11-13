package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/auth"
	"github.com/Guan0923/xinghe_rag/internal/mail"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// AccountProfile is the personal-profile facade behind the Console profile
// dialog and the e-mail registration flow. It is deliberately separate from
// UserAuth so the existing login/user-management fakes keep compiling.
type AccountProfile interface {
	RegistrationVerificationEnabled(ctx context.Context) bool
	RegisterAccount(ctx context.Context, in auth.RegisterInput) (auth.Login, error)
	RequestRegisterCode(ctx context.Context, email string) error
	UpdateProfile(ctx context.Context, userID, username string, age *int, gender string) (*state.User, error)
	ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error
	RequestEmailChange(ctx context.Context, userID, newEmail, password string) error
	ChangeEmail(ctx context.Context, userID, newEmail, code string) (*state.User, error)
	SetAvatar(ctx context.Context, userID, contentType string, data []byte) (*state.User, error)
	Avatar(ctx context.Context, userID string) (*state.UserAvatar, error)
	DeleteAvatar(ctx context.Context, userID string) (*state.User, error)
}

// MailAdmin is the administrator SMTP settings facade.
type MailAdmin interface {
	MailPublic(ctx context.Context) (map[string]any, error)
	SaveMailSettings(ctx context.Context, settings mail.Settings) (map[string]any, error)
	SendTestMail(ctx context.Context, to string) error
}

const (
	// maxAvatarUploadBytes bounds the multipart envelope: the image itself is
	// capped at state.MaxAvatarBytes and this leaves room for the part headers.
	maxAvatarUploadBytes = state.MaxAvatarBytes + 64<<10
	// codeRequestLimit and codeRequestWindow throttle verification-code requests
	// per client address.
	codeRequestLimit  = 5
	codeRequestWindow = time.Minute
	// minProfilePasswordLength mirrors the auth service password rule.
	minProfilePasswordLength = 8
)

// profileBody is the PATCH /v1/auth/profile payload. All three fields are sent
// by the Console; a null age or an empty gender clears the field.
type profileBody struct {
	Username string `json:"username"`
	Age      *int   `json:"age"`
	Gender   string `json:"gender"`
}

// passwordBody is the POST /v1/auth/password payload.
type passwordBody struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// registerCodeBody is the POST /v1/auth/register/code payload.
type registerCodeBody struct {
	Email string `json:"email"`
}

// emailChangeCodeBody is the POST /v1/auth/email/change/code payload.
type emailChangeCodeBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// emailChangeBody is the POST /v1/auth/email/change payload.
type emailChangeBody struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

// mailTestBody is the POST /v1/admin/mail-settings/test payload.
type mailTestBody struct {
	To string `json:"to"`
}

// mailSettingsBody is the PUT /v1/admin/mail-settings payload.
type mailSettingsBody struct {
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
	TLS      string `json:"tls"`
}

// rateLimiter bounds requests per key over a rolling window.
type rateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newRateLimiter() *rateLimiter { return &rateLimiter{hits: make(map[string][]time.Time)} }

// allow records one request and reports whether it fits inside the window.
func (l *rateLimiter) allow(key string, limit int, window time.Duration) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := make([]time.Time, 0, len(l.hits[key])+1)
	for _, hit := range l.hits[key] {
		if now.Sub(hit) < window {
			recent = append(recent, hit)
		}
	}
	if len(recent) >= limit {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	return true
}

// registerProfileRoutes mounts the personal-profile, avatar, registration-code
// and mail-settings endpoints.
func (s *Server) registerProfileRoutes() {
	s.router.add("POST", "/v1/auth/register/code", authNone, s.handleAuthRegisterCode)
	s.router.add("PATCH", "/v1/auth/profile", authSession, s.handleUpdateProfile)
	s.router.add("POST", "/v1/auth/password", authSession, s.handleChangePassword)
	s.router.add("POST", "/v1/auth/email/change/code", authSession, s.handleRequestEmailChange)
	s.router.add("POST", "/v1/auth/email/change", authSession, s.handleChangeEmail)
	s.router.add("PUT", "/v1/auth/avatar", authSession, s.handlePutAvatar)
	s.router.add("GET", "/v1/auth/avatar", authSession, s.handleGetAvatar)
	s.router.add("DELETE", "/v1/auth/avatar", authSession, s.handleDeleteAvatar)
	s.router.add("GET", "/v1/admin/mail-settings", authAdmin, s.handleGetMailSettings)
	s.router.add("PUT", "/v1/admin/mail-settings", authAdmin, s.handlePutMailSettings)
	s.router.add("POST", "/v1/admin/mail-settings/test", authAdmin, s.handleTestMail)
}

// requireAccount reports whether the personal-profile service is wired.
func (s *Server) requireAccount(w http.ResponseWriter) bool {
	if s.account == nil {
		writeDetail(w, http.StatusServiceUnavailable, "个人信息功能未启用")
		return false
	}
	return true
}

// requireMailAdmin reports whether the mail administration service is wired.
func (s *Server) requireMailAdmin(w http.ResponseWriter) bool {
	if s.mailAdmin == nil {
		writeDetail(w, http.StatusServiceUnavailable, "邮件设置功能未启用")
		return false
	}
	return true
}

func (s *Server) handleAuthRegisterCode(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeDetail(w, http.StatusNotFound, "认证未启用")
		return
	}
	if !s.requireAccount(w) {
		return
	}
	// Registration codes only exist while self-service registration is open.
	if !s.users.RegistrationEnabled() {
		writeDetail(w, http.StatusForbidden, auth.MessageRegistrationClosed)
		return
	}
	var body registerCodeBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	if !s.allowCodeRequest(clientAddress(r)) {
		w.Header().Set("Retry-After", "60")
		writeDetail(w, http.StatusTooManyRequests, auth.MessageEmailCodeTooFrequent)
		return
	}
	if err := s.account.RequestRegisterCode(r.Context(), body.Email); err != nil {
		writeAccountError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccount(w) {
		return
	}
	session, ok := sessionUserFromContext(r.Context())
	if !ok || session == nil {
		writeDetail(w, http.StatusUnauthorized, auth.MessageSessionInvalid)
		return
	}
	var body profileBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	updated, err := s.account.UpdateProfile(r.Context(), session.User.ID, body.Username, body.Age, body.Gender)
	if err != nil {
		writeAccountError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"user": userPayload(*updated)})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccount(w) {
		return
	}
	session, ok := sessionUserFromContext(r.Context())
	if !ok || session == nil {
		writeDetail(w, http.StatusUnauthorized, auth.MessageSessionInvalid)
		return
	}
	var body passwordBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	if len([]rune(body.NewPassword)) < minProfilePasswordLength {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Loc: []any{"body", "new_password"}, Msg: auth.MessagePasswordInvalid, Type: "value_error",
		}})
		return
	}
	if err := s.account.ChangePassword(r.Context(), session.User.ID, body.CurrentPassword, body.NewPassword); err != nil {
		writeAccountError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRequestEmailChange(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccount(w) {
		return
	}
	session, ok := sessionUserFromContext(r.Context())
	if !ok || session == nil {
		writeDetail(w, http.StatusUnauthorized, auth.MessageSessionInvalid)
		return
	}
	var body emailChangeCodeBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	if !s.allowCodeRequest("email-change|" + clientAddress(r)) {
		w.Header().Set("Retry-After", "60")
		writeDetail(w, http.StatusTooManyRequests, auth.MessageEmailCodeTooFrequent)
		return
	}
	if err := s.account.RequestEmailChange(r.Context(), session.User.ID, body.Email, body.Password); err != nil {
		writeAccountError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleChangeEmail(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccount(w) {
		return
	}
	session, ok := sessionUserFromContext(r.Context())
	if !ok || session == nil {
		writeDetail(w, http.StatusUnauthorized, auth.MessageSessionInvalid)
		return
	}
	var body emailChangeBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	updated, err := s.account.ChangeEmail(r.Context(), session.User.ID, body.Email, body.Code)
	if err != nil {
		writeAccountError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"user": userPayload(*updated)})
}

func (s *Server) handlePutAvatar(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccount(w) {
		return
	}
	session, ok := sessionUserFromContext(r.Context())
	if !ok || session == nil {
		writeDetail(w, http.StatusUnauthorized, auth.MessageSessionInvalid)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarUploadBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		writeDetail(w, http.StatusUnprocessableEntity, auth.MessageAvatarInvalid)
		return
	}
	var (
		contentType string
		data        []byte
	)
	for {
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		if partErr != nil {
			writeDetail(w, http.StatusUnprocessableEntity, auth.MessageAvatarInvalid)
			return
		}
		if part.FormName() != "avatar" {
			_, _ = io.Copy(io.Discard, part)
			continue
		}
		data, partErr = io.ReadAll(io.LimitReader(part, int64(state.MaxAvatarBytes)+1))
		if partErr != nil || len(data) > state.MaxAvatarBytes {
			writeDetail(w, http.StatusUnprocessableEntity, auth.MessageAvatarInvalid)
			return
		}
		contentType = detectAvatarType(part.Header.Get("Content-Type"), data)
		if contentType == "" {
			writeDetail(w, http.StatusUnprocessableEntity, auth.MessageAvatarInvalid)
			return
		}
		break
	}
	if data == nil || contentType == "" {
		writeDetail(w, http.StatusUnprocessableEntity, auth.MessageAvatarInvalid)
		return
	}
	updated, err := s.account.SetAvatar(r.Context(), session.User.ID, contentType, data)
	if err != nil {
		writeAccountError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"user": userPayload(*updated)})
}

func (s *Server) handleGetAvatar(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccount(w) {
		return
	}
	session, ok := sessionUserFromContext(r.Context())
	if !ok || session == nil {
		writeDetail(w, http.StatusUnauthorized, auth.MessageSessionInvalid)
		return
	}
	avatar, err := s.account.Avatar(r.Context(), session.User.ID)
	if err != nil {
		writeAccountError(w, err)
		return
	}
	if avatar == nil {
		w.Header().Set("Cache-Control", "no-store")
		writeDetail(w, http.StatusNotFound, "未设置头像")
		return
	}
	w.Header().Set("Content-Type", avatar.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(avatar.Content)))
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(avatar.Content)
}

func (s *Server) handleDeleteAvatar(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccount(w) {
		return
	}
	session, ok := sessionUserFromContext(r.Context())
	if !ok || session == nil {
		writeDetail(w, http.StatusUnauthorized, auth.MessageSessionInvalid)
		return
	}
	if _, err := s.account.DeleteAvatar(r.Context(), session.User.ID); err != nil {
		writeAccountError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetMailSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireMailAdmin(w) {
		return
	}
	settings, err := s.mailAdmin.MailPublic(r.Context())
	if err != nil {
		writeAccountError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

func (s *Server) handlePutMailSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireMailAdmin(w) {
		return
	}
	var body mailSettingsBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	settings, err := s.mailAdmin.SaveMailSettings(r.Context(), mail.Settings{
		Enabled:  body.Enabled,
		Host:     body.Host,
		Port:     body.Port,
		Username: body.Username,
		Password: body.Password,
		From:     body.From,
		TLS:      body.TLS,
	})
	if err != nil {
		writeAccountError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

func (s *Server) handleTestMail(w http.ResponseWriter, r *http.Request) {
	if !s.requireMailAdmin(w) {
		return
	}
	var body mailTestBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	if err := s.mailAdmin.SendTestMail(r.Context(), body.To); err != nil {
		var authError *auth.Error
		if errors.As(err, &authError) {
			writeAuthError(w, err)
			return
		}
		// A configured server that still refuses the message is an upstream
		// gateway failure, not a validation problem.
		writeDetail(w, http.StatusBadGateway, "测试邮件发送失败："+err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// writeAccountError maps account/mail failures to HTTP status codes, adding the
// too-frequent special case for verification codes.
func writeAccountError(w http.ResponseWriter, err error) {
	var authError *auth.Error
	if errors.As(err, &authError) && authError.Message == auth.MessageEmailCodeTooFrequent {
		w.Header().Set("Retry-After", "60")
		writeDetail(w, http.StatusTooManyRequests, authError.Message)
		return
	}
	writeAuthError(w, err)
}

// allowCodeRequest applies the per-client verification-code throttle.
func (s *Server) allowCodeRequest(key string) bool {
	if s.accountLimiter == nil {
		return true
	}
	return s.accountLimiter.allow(key, codeRequestLimit, codeRequestWindow)
}

// clientAddress returns the request peer address without its port.
func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return strings.ToLower(strings.TrimSpace(host))
}

// detectAvatarType accepts only a declared image type that the magic bytes
// confirm, so a renamed executable cannot be stored as an avatar.
func detectAvatarType(declared string, data []byte) string {
	mediaType, _, err := mime.ParseMediaType(declared)
	if err != nil {
		return ""
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	switch mediaType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		return ""
	}
	if sniffed := sniffAvatarType(data); sniffed == mediaType {
		return sniffed
	}
	return ""
}

// sniffAvatarType identifies PNG/JPEG/WEBP/GIF from the leading bytes.
func sniffAvatarType(data []byte) string {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}
