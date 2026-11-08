package state

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// cipherKeyBytes is a 32-byte key whose standard base64 encoding contains both
// '+' and '/' so the url-safe translation is exercised.
func cipherKeyBytes() []byte {
	key := make([]byte, 32)
	pattern := []byte{0xfb, 0xff, 0xbf}
	for index := range key {
		key[index] = pattern[index%len(pattern)]
	}
	return key
}

func cipherKeyText() string {
	return "base64:" + base64.StdEncoding.EncodeToString(cipherKeyBytes())
}

func TestProtectFormatAndRoundTrip(t *testing.T) {
	isolateStateEnv(t)
	key := cipherKeyText()
	value := "prk_中文 secret-ü"
	protected, err := Protect(value, key)
	if err != nil {
		t.Fatalf("Protect() error = %v", err)
	}
	if !strings.HasPrefix(protected, EncryptionPrefix) {
		t.Fatalf("protected value %q does not start with %q", protected, EncryptionPrefix)
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(protected, EncryptionPrefix))
	if err != nil {
		t.Fatalf("payload is not standard base64: %v", err)
	}
	if len(payload) != encryptionNonceSize+len([]byte(value))+16 {
		t.Fatalf("payload length = %d, want %d", len(payload), encryptionNonceSize+len([]byte(value))+16)
	}
	block, err := aes.NewCipher(cipherKeyBytes())
	if err != nil {
		t.Fatalf("aes.NewCipher() error = %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM() error = %v", err)
	}
	plaintext, err := aead.Open(nil, payload[:encryptionNonceSize], payload[encryptionNonceSize:], nil)
	if err != nil {
		t.Fatalf("AES-GCM open error = %v", err)
	}
	if string(plaintext) != value {
		t.Fatalf("decrypted value = %q, want %q", plaintext, value)
	}
	roundTripped, err := Unprotect(protected, key)
	if err != nil {
		t.Fatalf("Unprotect() error = %v", err)
	}
	if roundTripped != value {
		t.Fatalf("Unprotect() = %q, want %q", roundTripped, value)
	}
}

func TestProtectUsesRandomNonce(t *testing.T) {
	isolateStateEnv(t)
	key := cipherKeyText()
	first, err := Protect("same value", key)
	if err != nil {
		t.Fatalf("Protect() error = %v", err)
	}
	second, err := Protect("same value", key)
	if err != nil {
		t.Fatalf("Protect() error = %v", err)
	}
	if first == second {
		t.Fatal("two protections of the same value produced identical ciphertexts")
	}
}

func TestProtectEmptyValue(t *testing.T) {
	isolateStateEnv(t)
	protected, err := Protect("", cipherKeyText())
	if err != nil || protected != "" {
		t.Fatalf("Protect(\"\") = %q, %v; want empty", protected, err)
	}
	decrypted, err := Unprotect("", cipherKeyText())
	if err != nil || decrypted != "" {
		t.Fatalf("Unprotect(\"\") = %q, %v; want empty", decrypted, err)
	}
}

func TestEncryptionKeyRepresentations(t *testing.T) {
	isolateStateEnv(t)
	raw := cipherKeyBytes()
	representations := map[string]string{
		"standard base64":   base64.StdEncoding.EncodeToString(raw),
		"prefixed base64":   "base64:" + base64.StdEncoding.EncodeToString(raw),
		"url-safe unpadded": base64.RawURLEncoding.EncodeToString(raw),
		"url-safe padded":   base64.URLEncoding.EncodeToString(raw),
		"url-safe prefixed": "base64:" + base64.RawURLEncoding.EncodeToString(raw),
	}
	for name, key := range representations {
		protected, err := Protect("value", key)
		if err != nil {
			t.Fatalf("%s: Protect() error = %v", name, err)
		}
		decrypted, err := Unprotect(protected, key)
		if err != nil {
			t.Fatalf("%s: Unprotect() error = %v", name, err)
		}
		if decrypted != "value" {
			t.Fatalf("%s: Unprotect() = %q", name, decrypted)
		}
	}
}

func TestEncryptionKeyValidationMessages(t *testing.T) {
	isolateStateEnv(t)
	longKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x01}, 16))
	for name, key := range map[string]string{
		"short key":     longKey,
		"invalid bytes": "base64:!!!not-base64!!!",
	} {
		_, err := Protect("value", key)
		if err == nil {
			t.Fatalf("%s: Protect() succeeded, want an error", name)
		}
		var cryptoError *CryptoError
		if !errors.As(err, &cryptoError) {
			t.Fatalf("%s: error %v is not a *CryptoError", name, err)
		}
		if cryptoError.Message != "RAG_STATE_ENCRYPTION_KEY 必须是 base64 编码的 32 字节密钥" {
			t.Fatalf("%s: message = %q", name, cryptoError.Message)
		}
	}
}

func TestUnprotectRequiresKeyForEncV1(t *testing.T) {
	isolateStateEnv(t)
	protected, err := Protect("value", testEncryptionKey)
	if err != nil {
		t.Fatalf("Protect() error = %v", err)
	}
	_, err = Unprotect(protected, "")
	cryptoError := mustBeError[*CryptoError](t, err)
	if cryptoError.Message != "读取 apikey 需要设置 RAG_STATE_ENCRYPTION_KEY" {
		t.Fatalf("message = %q", cryptoError.Message)
	}
}

func TestUnprotectRejectsWrongKeyAndTampering(t *testing.T) {
	isolateStateEnv(t)
	protected, err := Protect("value", cipherKeyText())
	if err != nil {
		t.Fatalf("Protect() error = %v", err)
	}
	otherKey := "base64:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x02}, 32))
	if _, err := Unprotect(protected, otherKey); err == nil {
		t.Fatal("Unprotect() with the wrong key succeeded")
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(protected, EncryptionPrefix))
	if err != nil {
		t.Fatalf("payload decode error = %v", err)
	}
	payload[len(payload)-1] ^= 0xff
	tampered := EncryptionPrefix + base64.StdEncoding.EncodeToString(payload)
	if _, err := Unprotect(tampered, cipherKeyText()); err == nil {
		t.Fatal("Unprotect() of a tampered payload succeeded")
	}
	short := EncryptionPrefix + base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	if _, err := Unprotect(short, cipherKeyText()); err == nil {
		t.Fatal("Unprotect() of a short payload succeeded")
	}
}

func TestIsPortable(t *testing.T) {
	for value, want := range map[string]bool{
		"":                    true,
		"enc:v1:abcdef":       true,
		"dpapi:abcdef":        false,
		"private-file:secret": false,
		"unprotected secret":  false,
	} {
		if got := IsPortable(value); got != want {
			t.Fatalf("IsPortable(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestLegacyProtectPlatformBehavior(t *testing.T) {
	isolateStateEnv(t)
	protected, err := Protect("legacy", "")
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Fatalf("Protect without a key on Windows = %q, want a deferred-DPAPI error", protected)
		}
		var cryptoError *CryptoError
		if !errors.As(err, &cryptoError) {
			t.Fatalf("error %v is not a *CryptoError", err)
		}
	} else {
		if err != nil {
			t.Fatalf("Protect without a key error = %v", err)
		}
		if protected != "private-file:legacy" {
			t.Fatalf("Protected value = %q, want private-file prefix", protected)
		}
		decrypted, err := Unprotect(protected, "")
		if err != nil || decrypted != "legacy" {
			t.Fatalf("Unprotect(%q) = %q, %v", protected, decrypted, err)
		}
	}
}

func TestLegacyUnprotectPlatformBehavior(t *testing.T) {
	isolateStateEnv(t)
	const platformMessage = "apikey 需要由保存它的系统账号读取"
	if runtime.GOOS == "windows" {
		_, err := Unprotect("private-file:secret", "")
		cryptoError := mustBeError[*CryptoError](t, err)
		if cryptoError.Message != platformMessage {
			t.Fatalf("private-file message = %q, want %q", cryptoError.Message, platformMessage)
		}
	} else {
		decrypted, err := Unprotect("private-file:secret", "")
		if err != nil || decrypted != "secret" {
			t.Fatalf("Unprotect(private-file:) = %q, %v", decrypted, err)
		}
		_, err = Unprotect("dpapi:AAAA", "")
		cryptoError := mustBeError[*CryptoError](t, err)
		if cryptoError.Message != platformMessage {
			t.Fatalf("dpapi message = %q, want %q", cryptoError.Message, platformMessage)
		}
	}
	_, err := Unprotect("plain-value", "")
	if cryptoError := mustBeError[*CryptoError](t, err); cryptoError.Message != platformMessage {
		t.Fatalf("unknown legacy message = %q, want %q", cryptoError.Message, platformMessage)
	}
}

func TestUsesEnvironmentKeyByDefault(t *testing.T) {
	isolateStateEnv(t)
	t.Setenv(EncryptionKeyEnv, "  "+cipherKeyText()+"  ")
	protected, err := Protect("value", "")
	if err != nil {
		t.Fatalf("Protect() error = %v", err)
	}
	if !strings.HasPrefix(protected, EncryptionPrefix) {
		t.Fatalf("value %q is not portable; the environment key was ignored", protected)
	}
	decrypted, err := Unprotect(protected, "")
	if err != nil || decrypted != "value" {
		t.Fatalf("Unprotect() = %q, %v", decrypted, err)
	}
}
