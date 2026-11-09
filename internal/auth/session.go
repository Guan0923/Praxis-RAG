package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// TokenDigest returns the SHA-256 hex digest of a session token. Only digests
// are stored, so a database leak cannot replay live sessions.
func TokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewSessionToken returns the client token, its stored digest and the CSRF
// token bound to the session.
func NewSessionToken() (token, digest, csrf string, err error) {
	raw, err := randomBytes(32)
	if err != nil {
		return "", "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	csrfRaw, err := randomBytes(32)
	if err != nil {
		return "", "", "", err
	}
	return token, TokenDigest(token), hex.EncodeToString(csrfRaw), nil
}
