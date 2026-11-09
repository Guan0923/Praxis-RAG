package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PasswordHashPrefix identifies the stored PBKDF2-SHA256 format:
// "pbkdf2_sha256$<iterations>$<salt b64>$<hash b64>".
const PasswordHashPrefix = "pbkdf2_sha256"

const (
	passwordIterations = 210_000
	passwordSaltSize   = 16
	passwordKeySize    = 32
)

// errRandomUnavailable reports a failure of the system entropy source.
var errRandomUnavailable = errors.New("安全随机数不可用")

// randomBytes returns size cryptographically random bytes.
func randomBytes(size int) ([]byte, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return nil, errRandomUnavailable
	}
	return buffer, nil
}

// HashPassword derives a PBKDF2-HMAC-SHA256 hash with a fresh random salt.
func HashPassword(password string) (string, error) {
	salt, err := randomBytes(passwordSaltSize)
	if err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordKeySize)
	if err != nil {
		return "", errRandomUnavailable
	}
	return fmt.Sprintf("%s$%d$%s$%s", PasswordHashPrefix, passwordIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a password against a stored hash in constant time.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != PasswordHashPrefix {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1_000 || iterations > 1_000_000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(expected) == 0 {
		return false
	}
	actual, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1
}
