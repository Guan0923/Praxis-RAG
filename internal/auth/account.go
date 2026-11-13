package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/mail"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// Verification-code lifecycle. A code is six digits, valid for ten minutes,
// gives up after five wrong guesses and cannot be re-requested for the same
// address and purpose within one minute.
const (
	codePurposeRegister    = "register"
	codePurposeEmailChange = "email-change"

	verificationCodeTTL         = 10 * time.Minute
	verificationCodeCooldown    = 60 * time.Second
	verificationCodeAttempts    = 5
	verificationCodeDigits      = 6
	messageAvatarFeatureOffline = "头像功能未启用"
)

// emailPattern is the deliberately simple address check shared by the
// registration, e-mail-change and mail-test flows. The address is lower-cased
// before validation, so storage is normalized.
var emailPattern = regexp.MustCompile(`^[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}$`)

// RegisterInput is the extended registration payload: the e-mail address and
// verification code are required only while the mail server is configured.
type RegisterInput struct {
	Username string
	Password string
	Email    string
	Code     string
}

// RegistrationVerificationEnabled reports whether new accounts must verify an
// e-mail address, which is exactly when a mail server is configured.
func (s *Service) RegistrationVerificationEnabled(ctx context.Context) bool {
	return s.mail != nil && s.mail.Configured(ctx)
}

// RegisterAccount creates an account and starts a session. When e-mail
// verification is enabled the address and code are mandatory and the address is
// marked verified; otherwise the address is optional and stays unverified.
func (s *Service) RegisterAccount(ctx context.Context, input RegisterInput) (Login, error) {
	if !s.registration {
		return Login{}, newError(KindForbidden, MessageRegistrationClosed)
	}
	username := normalizeUsername(input.Username)
	if err := validateUsername(username); err != nil {
		return Login{}, err
	}
	if err := validatePassword(input.Password); err != nil {
		return Login{}, err
	}
	email := normalizeEmail(input.Email)
	verification := s.RegistrationVerificationEnabled(ctx)
	if verification || email != "" {
		if err := validateEmail(email); err != nil {
			return Login{}, err
		}
	}
	if verification && strings.TrimSpace(input.Code) == "" {
		return Login{}, newError(KindValidation, MessageEmailCodeRequired)
	}
	if email != "" {
		existing, err := s.users.ByEmail(ctx, email)
		if err != nil {
			return Login{}, err
		}
		if existing != nil {
			return Login{}, newError(KindConflict, MessageEmailTaken)
		}
	}
	var pending *state.EmailCode
	if verification {
		record, err := s.verifyEmailCode(ctx, email, codePurposeRegister, input.Code)
		if err != nil {
			return Login{}, err
		}
		pending = record
	}
	hash, err := HashPassword(input.Password)
	if err != nil {
		return Login{}, err
	}
	user, err := s.users.CreateWithProfile(ctx, username, hash, email)
	if err != nil {
		var conflict *state.StateConflict
		if errors.As(err, &conflict) {
			if email != "" {
				if existing, lookupErr := s.users.ByEmail(ctx, email); lookupErr == nil && existing != nil {
					return Login{}, newError(KindConflict, MessageEmailTaken)
				}
			}
			return Login{}, newError(KindConflict, MessageUsernameTaken)
		}
		return Login{}, err
	}
	if pending != nil {
		if _, err := s.users.UpdateEmail(ctx, user.ID, email, true); err != nil {
			return Login{}, err
		}
		user.Email = email
		user.EmailVerified = true
		// The account exists now, so the code has done its job.
		if err := s.codes.Consume(ctx, pending.ID); err != nil {
			return Login{}, err
		}
	}
	return s.startSession(ctx, user)
}

// RequestRegisterCode issues a registration verification code for an unused
// address. It is a no-op for disabled registration, missing mail or too-frequent
// requests.
func (s *Service) RequestRegisterCode(ctx context.Context, email string) error {
	if !s.registration {
		return newError(KindForbidden, MessageRegistrationClosed)
	}
	if !s.RegistrationVerificationEnabled(ctx) {
		return newError(KindUnavailable, MessageMailUnavailable)
	}
	email = normalizeEmail(email)
	if err := validateEmail(email); err != nil {
		return err
	}
	existing, err := s.users.ByEmail(ctx, email)
	if err != nil {
		return err
	}
	if existing != nil {
		return newError(KindConflict, MessageEmailTaken)
	}
	return s.issueEmailCode(ctx, email, codePurposeRegister)
}

// UpdateProfile changes the editable profile fields. An empty username keeps the
// current one; a nil age and an empty gender clear those fields.
func (s *Service) UpdateProfile(ctx context.Context, userID, username string, age *int, gender string) (*state.User, error) {
	current, err := s.users.ByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, newError(KindNotFound, MessageUserNotFound)
	}
	normalized := ""
	if strings.TrimSpace(username) != "" {
		normalized = normalizeUsername(username)
		if err := validateUsername(normalized); err != nil {
			return nil, err
		}
	}
	if age != nil && (*age < 1 || *age > 150) {
		return nil, newError(KindValidation, MessageAgeInvalid)
	}
	gender = strings.ToLower(strings.TrimSpace(gender))
	switch gender {
	case "", "male", "female", "other":
	default:
		return nil, newError(KindValidation, MessageGenderInvalid)
	}
	updated, err := s.users.UpdateProfile(ctx, userID, normalized, age, gender)
	if err != nil {
		var conflict *state.StateConflict
		if errors.As(err, &conflict) {
			return nil, newError(KindConflict, MessageUsernameTaken)
		}
		return nil, err
	}
	if updated == nil {
		return nil, newError(KindNotFound, MessageUserNotFound)
	}
	return updated, nil
}

// ChangePassword replaces the password after verifying the current one. A wrong
// current password is a validation problem (422), never an authentication
// failure, so the Console does not sign the user out.
func (s *Service) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	user, err := s.users.ByID(ctx, userID)
	if err != nil {
		return err
	}
	if user == nil {
		return newError(KindNotFound, MessageUserNotFound)
	}
	if err := s.verifyCurrentPassword(ctx, user, currentPassword); err != nil {
		return err
	}
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	encoded, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.users.UpdatePassword(ctx, userID, encoded)
}

// RequestEmailChange verifies the current password and sends a code to the new
// address. The wrong-password case is a validation error for the same
// sign-out-avoidance reason as ChangePassword.
func (s *Service) RequestEmailChange(ctx context.Context, userID, newEmail, password string) error {
	user, err := s.users.ByID(ctx, userID)
	if err != nil {
		return err
	}
	if user == nil {
		return newError(KindNotFound, MessageUserNotFound)
	}
	if err := s.verifyCurrentPassword(ctx, user, password); err != nil {
		return err
	}
	email := normalizeEmail(newEmail)
	if err := validateEmail(email); err != nil {
		return err
	}
	if !s.RegistrationVerificationEnabled(ctx) {
		return newError(KindUnavailable, MessageMailUnavailable)
	}
	existing, err := s.users.ByEmail(ctx, email)
	if err != nil {
		return err
	}
	if existing != nil && existing.ID != userID {
		return newError(KindConflict, MessageEmailTaken)
	}
	return s.issueEmailCode(ctx, email, codePurposeEmailChange)
}

// ChangeEmail applies a verified e-mail address change.
func (s *Service) ChangeEmail(ctx context.Context, userID, newEmail, code string) (*state.User, error) {
	user, err := s.users.ByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, newError(KindNotFound, MessageUserNotFound)
	}
	email := normalizeEmail(newEmail)
	if err := validateEmail(email); err != nil {
		return nil, err
	}
	existing, err := s.users.ByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.ID != userID {
		return nil, newError(KindConflict, MessageEmailTaken)
	}
	pending, err := s.verifyEmailCode(ctx, email, codePurposeEmailChange, code)
	if err != nil {
		return nil, err
	}
	updated, err := s.users.UpdateEmail(ctx, userID, email, true)
	if err != nil {
		var conflict *state.StateConflict
		if errors.As(err, &conflict) {
			return nil, newError(KindConflict, MessageEmailTaken)
		}
		return nil, err
	}
	if !updated {
		return nil, newError(KindNotFound, MessageUserNotFound)
	}
	if err := s.codes.Consume(ctx, pending.ID); err != nil {
		return nil, err
	}
	return s.users.ByID(ctx, userID)
}

// SetAvatar stores the profile image after checking its declared type and size.
// The HTTP layer additionally verifies the magic bytes.
func (s *Service) SetAvatar(ctx context.Context, userID, contentType string, data []byte) (*state.User, error) {
	if s.avatars == nil {
		return nil, newError(KindUnavailable, messageAvatarFeatureOffline)
	}
	user, err := s.users.ByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, newError(KindNotFound, MessageUserNotFound)
	}
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	switch contentType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		return nil, newError(KindValidation, MessageAvatarInvalid)
	}
	if len(data) == 0 || len(data) > state.MaxAvatarBytes {
		return nil, newError(KindValidation, MessageAvatarInvalid)
	}
	if err := s.avatars.Set(ctx, userID, contentType, data); err != nil {
		var valueError *state.ValueError
		if errors.As(err, &valueError) {
			return nil, newError(KindValidation, MessageAvatarInvalid)
		}
		return nil, err
	}
	return s.users.ByID(ctx, userID)
}

// Avatar returns the profile image of one account, or nil when it has none.
func (s *Service) Avatar(ctx context.Context, userID string) (*state.UserAvatar, error) {
	if s.avatars == nil {
		return nil, newError(KindUnavailable, messageAvatarFeatureOffline)
	}
	return s.avatars.Get(ctx, userID)
}

// DeleteAvatar removes the profile image and returns the refreshed account.
func (s *Service) DeleteAvatar(ctx context.Context, userID string) (*state.User, error) {
	if s.avatars == nil {
		return nil, newError(KindUnavailable, messageAvatarFeatureOffline)
	}
	user, err := s.users.ByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, newError(KindNotFound, MessageUserNotFound)
	}
	if _, err := s.avatars.Delete(ctx, userID); err != nil {
		return nil, err
	}
	return s.users.ByID(ctx, userID)
}

// MailPublic returns the Console projection of the mail server settings.
func (s *Service) MailPublic(ctx context.Context) (map[string]any, error) {
	if s.mail == nil {
		return nil, newError(KindUnavailable, MessageMailUnavailable)
	}
	projection, err := s.mail.Public(ctx)
	if err != nil {
		return nil, mapMailError(err)
	}
	return projection, nil
}

// SaveMailSettings validates and stores the mail server settings, returning the
// refreshed projection.
func (s *Service) SaveMailSettings(ctx context.Context, settings mail.Settings) (map[string]any, error) {
	if s.mail == nil {
		return nil, newError(KindUnavailable, MessageMailUnavailable)
	}
	if err := s.mail.Save(ctx, settings); err != nil {
		return nil, mapMailError(err)
	}
	return s.MailPublic(ctx)
}

// SendTestMail sends one test message to confirm the SMTP configuration.
func (s *Service) SendTestMail(ctx context.Context, to string) error {
	if s.mail == nil {
		return newError(KindUnavailable, MessageMailUnavailable)
	}
	to = normalizeEmail(to)
	if err := validateEmail(to); err != nil {
		return err
	}
	if !s.mail.Configured(ctx) {
		return newError(KindUnavailable, MessageMailUnavailable)
	}
	body := "这是一封来自 Praxis-RAG 的测试邮件。\n\n收到本邮件说明 SMTP 邮件服务器配置可用。\n"
	if err := s.mail.Send(ctx, to, "Praxis-RAG 测试邮件", body); err != nil {
		return mapMailError(err)
	}
	return nil
}

// issueEmailCode creates and sends one code, enforcing the per-address cooldown.
func (s *Service) issueEmailCode(ctx context.Context, email, purpose string) error {
	if s.codes == nil || s.mail == nil {
		return newError(KindUnavailable, MessageMailUnavailable)
	}
	now := s.now().UTC()
	active, err := s.codes.LatestActive(ctx, email, purpose, isoTime(now))
	if err != nil {
		return err
	}
	if active != nil && withinCooldown(active.CreatedAt, now) {
		return newError(KindConflict, MessageEmailCodeTooFrequent)
	}
	code, err := randomVerificationCode()
	if err != nil {
		return err
	}
	if err := s.codes.Create(ctx, email, purpose, digestCode(code), isoTime(now.Add(verificationCodeTTL))); err != nil {
		return err
	}
	// Pruning is best effort: a failed cleanup never blocks a valid request.
	_, _ = s.codes.DeleteExpired(ctx, isoTime(now))
	subject, body := verificationMail(purpose, code)
	if err := s.mail.Send(ctx, email, subject, body); err != nil {
		return newError(KindUnavailable, "验证码邮件发送失败，请稍后重试")
	}
	return nil
}

// verifyEmailCode resolves one active code and checks the digest without
// consuming it: the caller consumes it only after its own change succeeded.
func (s *Service) verifyEmailCode(ctx context.Context, email, purpose, provided string) (*state.EmailCode, error) {
	if s.codes == nil {
		return nil, newError(KindUnavailable, MessageMailUnavailable)
	}
	provided = strings.TrimSpace(provided)
	if provided == "" {
		return nil, newError(KindValidation, MessageEmailCodeRequired)
	}
	record, err := s.codes.LatestActive(ctx, email, purpose, s.nowISO())
	if err != nil {
		return nil, err
	}
	if record == nil || record.Attempts >= verificationCodeAttempts {
		return nil, newError(KindValidation, MessageEmailCodeInvalid)
	}
	if subtle.ConstantTimeCompare([]byte(record.CodeDigest), []byte(digestCode(provided))) != 1 {
		_ = s.codes.IncrementAttempts(ctx, record.ID)
		return nil, newError(KindValidation, MessageEmailCodeInvalid)
	}
	return record, nil
}

// verifyCurrentPassword re-reads the password hash of an account and compares it.
func (s *Service) verifyCurrentPassword(ctx context.Context, user *state.User, password string) error {
	_, hash, err := s.users.ByUsername(ctx, user.Username)
	if err != nil {
		return err
	}
	if hash == "" || !VerifyPassword(hash, password) {
		return newError(KindValidation, MessageCurrentPasswordWrong)
	}
	return nil
}

// mapMailError turns a mail-package validation failure into a typed auth error.
func mapMailError(err error) error {
	var validation *mail.ValidationError
	if errors.As(err, &validation) {
		return newError(KindValidation, validation.Message)
	}
	return err
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validateEmail(email string) error {
	if email == "" || len(email) > 254 || !emailPattern.MatchString(email) {
		return newError(KindValidation, MessageEmailInvalid)
	}
	return nil
}

// randomVerificationCode returns a zero-padded six digit code from the system
// entropy source.
func randomVerificationCode() (string, error) {
	limit := big.NewInt(1000000)
	value, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return "", errRandomUnavailable
	}
	return fmt.Sprintf("%0*d", verificationCodeDigits, value.Int64()), nil
}

// digestCode is the stored SHA-256 hex digest of one verification code.
func digestCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// withinCooldown reports whether the previous code was issued less than the
// cooldown ago. Unparsable timestamps allow a new request.
func withinCooldown(createdAt string, now time.Time) bool {
	issued, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return false
	}
	return now.Sub(issued) < verificationCodeCooldown
}

// verificationMail renders the subject and plain-text body for one purpose.
func verificationMail(purpose, code string) (string, string) {
	if purpose == codePurposeEmailChange {
		return "Praxis-RAG 邮箱变更验证码",
			"您的邮箱变更验证码是 " + code + "，10 分钟内有效。\n\n如非本人操作，请忽略本邮件。\n"
	}
	return "Praxis-RAG 注册验证码",
		"您的注册验证码是 " + code + "，10 分钟内有效。\n\n如非本人操作，请忽略本邮件。\n"
}
