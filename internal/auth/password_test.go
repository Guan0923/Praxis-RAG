package auth

import (
	"strings"
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if !strings.HasPrefix(hash, PasswordHashPrefix+"$") {
		t.Fatalf("hash = %q, want the pbkdf2 prefix", hash)
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Fatalf("VerifyPassword() rejected the correct password")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatalf("VerifyPassword() accepted a wrong password")
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Fatalf("VerifyPassword() is not deterministic")
	}
	second, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("HashPassword(second) error = %v", err)
	}
	if second == hash {
		t.Fatalf("two hashes share the salt")
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	cases := []string{
		"",
		"plain-text",
		"pbkdf2_sha256$0$c2FsdA$aGFzaA",
		"pbkdf2_sha256$abc$c2FsdA$aGFzaA",
		"pbkdf2_sha256$210000$$aGFzaA",
		"bcrypt$210000$c2FsdA$aGFzaA",
		"pbkdf2_sha256$210000$c2FsdA",
	}
	for _, encoded := range cases {
		if VerifyPassword(encoded, "password") {
			t.Errorf("VerifyPassword(%q) accepted a malformed hash", encoded)
		}
	}
}

func TestSessionTokenShape(t *testing.T) {
	token, digest, csrf, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken() error = %v", err)
	}
	if len(token) < 40 || strings.ContainsAny(token, "+/=") {
		t.Fatalf("token = %q, want a base64url secret", token)
	}
	if len(digest) != 64 || TokenDigest(token) != digest {
		t.Fatalf("digest = %q, want the sha256 hex of the token", digest)
	}
	if len(csrf) != 64 {
		t.Fatalf("csrf = %q, want 64 hex chars", csrf)
	}
	secondToken, secondDigest, secondCSRF, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken(second) error = %v", err)
	}
	if secondToken == token || secondDigest == digest || secondCSRF == csrf {
		t.Fatalf("session tokens are not unique")
	}
}

func TestIsoTimeMatchesStateFormat(t *testing.T) {
	moment := time.Date(2026, 11, 9, 8, 7, 6, 123456000, time.UTC)
	if got := isoTime(moment); got != "2026-11-09T08:07:06.123456+00:00" {
		t.Fatalf("isoTime() = %q", got)
	}
}
