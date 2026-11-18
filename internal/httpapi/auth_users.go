package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/auth"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

const (
	// sessionCookieName carries the opaque login session.
	sessionCookieName = "rag_session"
	// csrfHeaderName must echo the session CSRF token on cookie-authenticated
	// mutating requests.
	csrfHeaderName = "X-CSRF-Token"
	// apiKeyPrefix mirrors the "prk_" secrets issued by the API-key store; a
	// bearer token with this prefix is never treated as a session token.
	apiKeyPrefix = "prk_"
	// loginAttemptLimit and loginAttemptWindow bound password guessing.
	loginAttemptLimit  = 5
	loginAttemptWindow = 5 * time.Minute
)

// UserAuth is the account/session facade behind the Console login flow and the
// user-management endpoints.
type UserAuth interface {
	RegistrationEnabled() bool
	Register(ctx context.Context, username, password string) (auth.Login, error)
	Login(ctx context.Context, username, password string) (auth.Login, error)
	Logout(ctx context.Context, token string) error
	Authenticate(ctx context.Context, token string) (*auth.SessionUser, error)
	ListUsers(ctx context.Context) ([]state.User, error)
	UpdateUser(ctx context.Context, userID, role string, enabled bool) (*state.User, error)
	DeleteUser(ctx context.Context, userID string) (bool, error)
}

// authBody is the registration/login request payload. Email and Code are only
// used by the extended registration flow; login ignores them.
type authBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email"`
	Code     string `json:"code"`
}

// userUpdateBody is the user-management patch payload.
type userUpdateBody struct {
	Role    string `json:"role"`
	Enabled *bool  `json:"enabled"`
}

// loginLimiter counts recent failures per client/username pair.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
}

type loginAttempt struct {
	count int
	until time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: make(map[string]loginAttempt)}
}

// allow reports whether the key may attempt a login, plus the retry delay.
func (l *loginLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	attempt, found := l.attempts[key]
	if !found {
		return true, 0
	}
	if time.Now().After(attempt.until) {
		delete(l.attempts, key)
		return true, 0
	}
	if attempt.count >= loginAttemptLimit {
		return false, time.Until(attempt.until)
	}
	return true, 0
}

// fail records one failed attempt.
func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	attempt, found := l.attempts[key]
	if !found || time.Now().After(attempt.until) {
		attempt = loginAttempt{until: time.Now().Add(loginAttemptWindow)}
	}
	attempt.count++
	l.attempts[key] = attempt
}

// reset clears the counter after a successful login.
func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

// registerUserAuthRoutes mounts the login/user-management endpoints.
func (s *Server) registerUserAuthRoutes() {
	s.router.add("GET", "/v1/auth/status", authNone, s.handleAuthStatus)
	s.router.add("POST", "/v1/auth/register", authNone, s.handleAuthRegister)
	s.router.add("POST", "/v1/auth/login", authNone, s.handleAuthLogin)
	s.router.add("POST", "/v1/auth/logout", authNone, s.handleAuthLogout)
	s.router.add("GET", "/v1/auth/me", authNone, s.handleAuthMe)
	s.router.add("GET", "/v1/admin/users", authAdmin, s.handleListUsers)
	s.router.add("PATCH", "/v1/admin/users/{user_id}", authAdmin, s.handleUpdateUser)
	s.router.add("DELETE", "/v1/admin/users/{user_id}", authAdmin, s.handleDeleteUser)
}

// authEnabled reports whether the login flow is active.
func (s *Server) authEnabled() bool { return s.settings.AuthEnabled && s.users != nil }

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	payload := map[string]any{
		"auth_enabled":               s.authEnabled(),
		"registration_enabled":       false,
		"email_verification_enabled": s.registrationVerificationEnabled(r.Context()),
		"authenticated":              false,
	}
	if !s.authEnabled() {
		// Legacy mode: the Console keeps its token/loopback behaviour.
		payload["authenticated"] = true
		writeJSON(w, http.StatusOK, payload)
		return
	}
	payload["registration_enabled"] = s.users.RegistrationEnabled()
	if user, _ := s.requestSessionUser(r); user != nil {
		payload["authenticated"] = true
		payload["user"] = userPayload(user.User)
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeDetail(w, http.StatusNotFound, "认证未启用")
		return
	}
	if !s.users.RegistrationEnabled() {
		writeDetail(w, http.StatusForbidden, auth.MessageRegistrationClosed)
		return
	}
	body, ok := s.decodeAuthBody(w, r)
	if !ok {
		return
	}
	login, err := s.registerAccount(r.Context(), body)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	s.writeLogin(w, http.StatusCreated, login)
}

// registerAccount routes the registration to the profile-aware service when it
// is wired and keeps the legacy username/password path otherwise.
func (s *Server) registerAccount(ctx context.Context, body authBody) (auth.Login, error) {
	if s.account == nil {
		return s.users.Register(ctx, body.Username, body.Password)
	}
	return s.account.RegisterAccount(ctx, auth.RegisterInput{
		Username: body.Username,
		Password: body.Password,
		Email:    body.Email,
		Code:     body.Code,
	})
}

// registrationVerificationEnabled reports whether the extended registration
// requires a verified e-mail address.
func (s *Server) registrationVerificationEnabled(ctx context.Context) bool {
	return s.account != nil && s.account.RegistrationVerificationEnabled(ctx)
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeDetail(w, http.StatusNotFound, "认证未启用")
		return
	}
	body, ok := s.decodeAuthBody(w, r)
	if !ok {
		return
	}
	key := loginAttemptKey(r, body.Username)
	if allowed, retry := s.limiter.allow(key); !allowed {
		writeDetail(w, http.StatusTooManyRequests,
			"登录失败次数过多，请在 "+strconv.Itoa(int(retry.Seconds())+1)+" 秒后重试")
		return
	}
	login, err := s.users.Login(r.Context(), body.Username, body.Password)
	if err != nil {
		var authError *auth.Error
		if errors.As(err, &authError) && authError.Kind == auth.KindUnauthorized {
			s.limiter.fail(key)
		}
		writeAuthError(w, err)
		return
	}
	s.limiter.reset(key)
	s.writeLogin(w, http.StatusOK, login)
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	token, _ := s.sessionTokenFromRequest(r)
	if s.authEnabled() && token != "" {
		_ = s.users.Logout(r.Context(), token)
	}
	s.clearSessionCookie(w)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.authEnabled() {
		writeDetail(w, http.StatusUnauthorized, "认证未启用")
		return
	}
	session, _ := s.requestSessionUser(r)
	if session == nil {
		writeDetail(w, http.StatusUnauthorized, auth.MessageSessionInvalid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       userPayload(session.User),
		"csrf_token": session.CSRF,
	})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if s.users == nil {
		writeDetail(w, http.StatusServiceUnavailable, "用户管理未启用")
		return
	}
	users, err := s.users.ListUsers(r.Context())
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, err.Error())
		return
	}
	payload := make([]map[string]any, 0, len(users))
	for _, user := range users {
		payload = append(payload, userPayload(user))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"users": payload})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	if s.users == nil {
		writeDetail(w, http.StatusServiceUnavailable, "用户管理未启用")
		return
	}
	var body userUpdateBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	role := strings.TrimSpace(body.Role)
	if role == "" {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{
			{Loc: []any{"body", "role"}, Msg: "Field required", Type: "missing"},
		})
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	updated, err := s.users.UpdateUser(r.Context(), pathParam(r, "user_id"), role, enabled)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"user": userPayload(*updated)})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if s.users == nil {
		writeDetail(w, http.StatusServiceUnavailable, "用户管理未启用")
		return
	}
	deleted, err := s.users.DeleteUser(r.Context(), pathParam(r, "user_id"))
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if !deleted {
		writeDetail(w, http.StatusNotFound, auth.MessageUserNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// decodeAuthBody decodes and shape-checks a registration/login payload.
func (s *Server) decodeAuthBody(w http.ResponseWriter, r *http.Request) (authBody, bool) {
	var body authBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return authBody{}, false
	}
	return body, true
}

// writeLogin persists the cookie and returns the client-visible session data.
func (s *Server) writeLogin(w http.ResponseWriter, status int, login auth.Login) {
	s.setSessionCookie(w, login.Token, login.ExpiresAt)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]any{
		"user":       userPayload(login.User),
		"csrf_token": login.CSRF,
		"expires_at": login.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.settings.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.settings.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// sessionTokenFromRequest returns the session token from the cookie, or from a
// bearer header that cannot be an API key.
func (s *Server) sessionTokenFromRequest(r *http.Request) (string, bool) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		return cookie.Value, true
	}
	authorization := r.Header.Get("Authorization")
	if strings.HasPrefix(authorization, "Bearer ") {
		token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
		if token != "" && !strings.HasPrefix(token, apiKeyPrefix) && token != s.settings.Token {
			return token, false
		}
	}
	return "", false
}

// requestSessionUser authenticates the request session, and reports whether the
// token arrived in the session cookie (which requires CSRF validation on
// mutating requests).
func (s *Server) requestSessionUser(r *http.Request) (*auth.SessionUser, bool) {
	if !s.authEnabled() {
		return nil, false
	}
	token, viaCookie := s.sessionTokenFromRequest(r)
	if token == "" {
		return nil, false
	}
	user, err := s.users.Authenticate(r.Context(), token)
	if err != nil {
		return nil, false
	}
	return user, viaCookie
}

// validateCSRF enforces the double-submit token for cookie-authenticated
// mutating requests. Bearer-authenticated requests are exempt because browsers
// do not attach that header automatically.
func (s *Server) validateCSRF(r *http.Request, user *auth.SessionUser, viaCookie bool) bool {
	if !viaCookie {
		return true
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	provided := strings.TrimSpace(r.Header.Get(csrfHeaderName))
	return provided != "" && secureEqual(provided, user.CSRF)
}

// loginAttemptKey scopes the limiter to one client address and username.
func loginAttemptKey(r *http.Request, username string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return strings.ToLower(strings.TrimSpace(host)) + "|" + strings.ToLower(strings.TrimSpace(username))
}

// userPayload is the public JSON shape of an account. Profile fields with no
// value are omitted; email and gender are always present so the Console can
// render the form without guessing.
func userPayload(user state.User) map[string]any {
	payload := map[string]any{
		"id":             user.ID,
		"username":       user.Username,
		"role":           user.Role,
		"enabled":        user.Enabled,
		"created_at":     user.CreatedAt,
		"email":          user.Email,
		"email_verified": user.EmailVerified,
		"gender":         user.Gender,
		"has_avatar":     user.AvatarUpdatedAt != nil,
	}
	if user.LastLoginAt != nil {
		payload["last_login_at"] = *user.LastLoginAt
	}
	if user.Age != nil {
		payload["age"] = *user.Age
	}
	if user.AvatarUpdatedAt != nil {
		payload["avatar_updated_at"] = *user.AvatarUpdatedAt
	}
	return payload
}

// writeAuthError maps typed auth failures to HTTP status codes.
func writeAuthError(w http.ResponseWriter, err error) {
	var authError *auth.Error
	if !errors.As(err, &authError) {
		writeDetail(w, http.StatusInternalServerError, "认证服务异常")
		return
	}
	switch authError.Kind {
	case auth.KindValidation:
		writeDetail(w, http.StatusUnprocessableEntity, authError.Message)
	case auth.KindUnauthorized:
		writeDetail(w, http.StatusUnauthorized, authError.Message)
	case auth.KindForbidden:
		writeDetail(w, http.StatusForbidden, authError.Message)
	case auth.KindConflict:
		writeDetail(w, http.StatusConflict, authError.Message)
	case auth.KindNotFound:
		writeDetail(w, http.StatusNotFound, authError.Message)
	case auth.KindUnavailable:
		writeDetail(w, http.StatusServiceUnavailable, authError.Message)
	default:
		writeDetail(w, http.StatusInternalServerError, "认证服务异常")
	}
}
