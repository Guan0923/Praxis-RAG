// Package auth implements Console account authentication: PBKDF2 password
// hashing, opaque session tokens stored as digests, and the registration /
// login / user-management rules.
//
// It deliberately knows nothing about HTTP: the httpapi layer maps its typed
// errors to status codes and manages the session cookie and CSRF header.
package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/mail"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// ErrorKind classifies auth failures for the HTTP layer.
type ErrorKind int

const (
	// KindValidation is a user-fixable input problem (bad username/password).
	KindValidation ErrorKind = iota
	// KindUnauthorized means the credentials or session are not valid.
	KindUnauthorized
	// KindForbidden means the operation is not allowed (registration closed,
	// last-admin protection).
	KindForbidden
	// KindConflict means the username already exists.
	KindConflict
	// KindNotFound means the targeted account does not exist.
	KindNotFound
	// KindUnavailable means a dependency required for the operation (the mail
	// server) is not configured.
	KindUnavailable
)

// Error is a safe, user-facing auth failure.
type Error struct {
	Kind    ErrorKind
	Message string
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

func newError(kind ErrorKind, message string) *Error { return &Error{Kind: kind, Message: message} }

// Message constants keep the wording stable across handlers and tests.
const (
	MessageUsernameInvalid    = "用户名需为 3-64 位字母、数字、点、下划线或横线，且以字母或数字开头"
	MessagePasswordInvalid    = "密码长度需为 8-128 个字符"
	MessageUsernameTaken      = "用户名已存在"
	MessageCredentials        = "用户名或密码错误"
	MessageSessionInvalid     = "登录已失效，请重新登录"
	MessageAccountDisabled    = "该账号已被禁用"
	MessageRegistrationClosed = "注册已关闭，请联系管理员创建账号"
	MessageLastAdmin          = "至少保留一名启用状态的管理员"
	MessageUserNotFound       = "用户不存在"
	MessageRoleInvalid        = "角色只能是 admin、vip 或 user"

	// MessageEmail* and MessageMail* back the e-mail registration / profile
	// flows; their wording is shared with the Console.
	MessageEmailInvalid         = "请输入有效的邮箱地址"
	MessageEmailTaken           = "该邮箱已被其他账号使用"
	MessageEmailCodeRequired    = "请先获取并填写邮箱验证码"
	MessageEmailCodeInvalid     = "验证码无效或已过期"
	MessageEmailCodeTooFrequent = "验证码发送过于频繁，请稍后再试"
	MessageMailUnavailable      = "邮件服务器未配置或未启用，请联系管理员"
	MessageCurrentPasswordWrong = "当前密码不正确"
	MessageAgeInvalid           = "年龄需为 1-150 之间的整数"
	MessageGenderInvalid        = "性别只能是 male、female 或 other"
	MessageAvatarInvalid        = "头像需为 PNG/JPEG/WEBP/GIF 图片且不超过 2MB"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,63}$`)

// Options configures a Service.
type Options struct {
	// RegistrationEnabled allows self-service registration.
	RegistrationEnabled bool
	// SessionTTL is how long an issued session stays valid.
	SessionTTL time.Duration
	// Now overrides the clock in tests.
	Now func() time.Time
	// Mail sends verification and test mail; nil disables e-mail verification.
	Mail *mail.Service
	// Codes persists the e-mail verification codes; nil disables them.
	Codes *state.EmailVerificationRepository
	// Avatars persists profile images; nil disables avatar uploads.
	Avatars *state.UserAvatarRepository
}

// SessionUser is an authenticated account plus its session CSRF token.
type SessionUser struct {
	User state.User
	CSRF string
}

// Login is the result of a successful registration or login.
type Login struct {
	User      state.User
	Token     string
	CSRF      string
	ExpiresAt time.Time
}

// Service authenticates accounts and issues sessions.
type Service struct {
	users        *state.UserRepository
	sessions     *state.UserSessionRepository
	registration bool
	ttl          time.Duration
	now          func() time.Time
	mail         *mail.Service
	codes        *state.EmailVerificationRepository
	avatars      *state.UserAvatarRepository
}

// NewService builds the auth service. A nil clock uses time.Now.
func NewService(users *state.UserRepository, sessions *state.UserSessionRepository, options Options) *Service {
	ttl := options.SessionTTL
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	clock := options.Now
	if clock == nil {
		clock = time.Now
	}
	return &Service{
		users:        users,
		sessions:     sessions,
		registration: options.RegistrationEnabled,
		ttl:          ttl,
		now:          clock,
		mail:         options.Mail,
		codes:        options.Codes,
		avatars:      options.Avatars,
	}
}

// RegistrationEnabled reports whether self-service registration is open.
func (s *Service) RegistrationEnabled() bool { return s.registration }

// Register creates an account and starts a session. The first account becomes
// the administrator (state.UserRepository.Create).
func (s *Service) Register(ctx context.Context, username, password string) (Login, error) {
	if !s.registration {
		return Login{}, newError(KindForbidden, MessageRegistrationClosed)
	}
	username = normalizeUsername(username)
	if err := validateUsername(username); err != nil {
		return Login{}, err
	}
	if err := validatePassword(password); err != nil {
		return Login{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return Login{}, err
	}
	user, err := s.users.Create(ctx, username, hash)
	if err != nil {
		var conflict *state.StateConflict
		if errors.As(err, &conflict) {
			return Login{}, newError(KindConflict, MessageUsernameTaken)
		}
		return Login{}, err
	}
	return s.startSession(ctx, user)
}

// Login verifies a password and starts a session.
func (s *Service) Login(ctx context.Context, username, password string) (Login, error) {
	username = normalizeUsername(username)
	user, hash, err := s.users.ByUsername(ctx, username)
	if err != nil {
		return Login{}, err
	}
	if user == nil || hash == "" || !VerifyPassword(hash, password) {
		return Login{}, newError(KindUnauthorized, MessageCredentials)
	}
	if !user.Enabled {
		return Login{}, newError(KindUnauthorized, MessageAccountDisabled)
	}
	if err := s.users.TouchLogin(ctx, user.ID); err != nil {
		return Login{}, err
	}
	return s.startSession(ctx, *user)
}

// Logout revokes the session behind the supplied token. Unknown tokens are a
// no-op so logout stays idempotent.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	session, err := s.sessions.ByDigest(ctx, TokenDigest(token))
	if err != nil {
		return err
	}
	if session == nil {
		return nil
	}
	return s.sessions.Revoke(ctx, session.ID)
}

// Authenticate resolves a session token to its account and CSRF token.
func (s *Service) Authenticate(ctx context.Context, token string) (*SessionUser, error) {
	if token == "" {
		return nil, newError(KindUnauthorized, MessageSessionInvalid)
	}
	session, err := s.sessions.ByDigest(ctx, TokenDigest(token))
	if err != nil {
		return nil, err
	}
	if session == nil || session.Revoked || session.ExpiresAt <= s.nowISO() {
		return nil, newError(KindUnauthorized, MessageSessionInvalid)
	}
	user, err := s.users.ByID(ctx, session.UserID)
	if err != nil {
		return nil, err
	}
	if user == nil || !user.Enabled {
		return nil, newError(KindUnauthorized, MessageSessionInvalid)
	}
	// Activity tracking is best effort: a failed touch must not log the user out.
	_ = s.sessions.Touch(ctx, session.ID)
	return &SessionUser{User: *user, CSRF: session.CSRF}, nil
}

// ListUsers returns every account.
func (s *Service) ListUsers(ctx context.Context) ([]state.User, error) {
	return s.users.List(ctx)
}

// UpdateUser changes role and enabled state while protecting the last enabled
// administrator.
func (s *Service) UpdateUser(ctx context.Context, userID, role string, enabled bool) (*state.User, error) {
	if !state.IsRole(role) {
		return nil, newError(KindValidation, MessageRoleInvalid)
	}
	current, err := s.users.ByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, newError(KindNotFound, MessageUserNotFound)
	}
	if err := s.guardLastAdmin(ctx, current, role, enabled); err != nil {
		return nil, err
	}
	updated, err := s.users.Update(ctx, userID, role, enabled)
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return nil, newError(KindNotFound, MessageUserNotFound)
	}
	if !updated.Enabled {
		if err := s.sessions.RevokeUserSessions(ctx, updated.ID); err != nil {
			return nil, err
		}
	}
	return updated, nil
}

// DeleteUser removes an account while protecting the last enabled administrator.
func (s *Service) DeleteUser(ctx context.Context, userID string) (bool, error) {
	current, err := s.users.ByID(ctx, userID)
	if err != nil {
		return false, err
	}
	if current == nil {
		return false, newError(KindNotFound, MessageUserNotFound)
	}
	if err := s.guardLastAdmin(ctx, current, state.RoleUser, false); err != nil {
		return false, err
	}
	return s.users.Delete(ctx, userID)
}

// CountUsers exposes the account count (used by the Console bootstrap screen).
func (s *Service) CountUsers(ctx context.Context) (int, error) {
	return s.users.Count(ctx)
}

func (s *Service) startSession(ctx context.Context, user state.User) (Login, error) {
	token, digest, csrf, err := NewSessionToken()
	if err != nil {
		return Login{}, err
	}
	expires := s.now().UTC().Add(s.ttl)
	if _, err := s.sessions.Create(ctx, user.ID, digest, csrf, isoTime(expires)); err != nil {
		return Login{}, err
	}
	return Login{User: user, Token: token, CSRF: csrf, ExpiresAt: expires}, nil
}

func (s *Service) guardLastAdmin(ctx context.Context, current *state.User, nextRole string, nextEnabled bool) error {
	if !current.IsAdmin() || !current.Enabled {
		return nil
	}
	if nextRole == state.RoleAdmin && nextEnabled {
		return nil
	}
	admins, err := s.users.CountEnabledAdmins(ctx)
	if err != nil {
		return err
	}
	if admins <= 1 {
		return newError(KindForbidden, MessageLastAdmin)
	}
	return nil
}

func (s *Service) nowISO() string { return isoTime(s.now().UTC()) }

// isoTime formats a UTC timestamp the same way the state layer stores them.
func isoTime(moment time.Time) string {
	return moment.UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

func normalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

func validateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return newError(KindValidation, MessageUsernameInvalid)
	}
	return nil
}

func validatePassword(password string) error {
	length := len([]rune(password))
	if length < 8 || length > 128 {
		return newError(KindValidation, MessagePasswordInvalid)
	}
	return nil
}
