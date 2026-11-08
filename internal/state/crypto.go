package state

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// EncryptionPrefix marks a portable AES-GCM credential value.
const EncryptionPrefix = "enc:v1:"

// EncryptionKeyEnv is the environment variable holding the shared state key.
const EncryptionKeyEnv = "RAG_STATE_ENCRYPTION_KEY"

const (
	encryptionNonceSize = 12
	encryptionKeySize   = 32
)

// CryptoError mirrors the ModelAPIError raised by rag_mcp/state/crypto.py for
// configuration and credential failures. The messages are kept identical
// because they are surfaced to operators.
type CryptoError struct{ Message string }

// Error implements error.
func (e *CryptoError) Error() string { return e.Message }

// Protect resolves an empty key from the environment like Python's
// `key = key or os.getenv(...)`. It returns "" for an empty value and never
// stores plaintext.
func resolveEncryptionKey(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return strings.TrimSpace(os.Getenv(EncryptionKeyEnv))
}

// Protect encrypts value as "enc:v1:" + base64(12-byte nonce || AES-GCM
// ciphertext). Without a configured key it falls back to the legacy platform
// storage (dpapi: on Windows, private-file: elsewhere) exactly like the Python
// helper.
func Protect(value, key string) (string, error) {
	if value == "" {
		return "", nil
	}
	resolved := resolveEncryptionKey(key)
	if resolved == "" {
		return legacyProtect(value)
	}
	plaintext := []byte(value)
	aead, err := newAEAD(resolved)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, encryptionNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("无法生成加密随机数: %w", err)
	}
	sealed := aead.Seal(nil, nonce, plaintext, nil)
	payload := make([]byte, 0, len(nonce)+len(sealed))
	payload = append(payload, nonce...)
	payload = append(payload, sealed...)
	return EncryptionPrefix + base64.StdEncoding.EncodeToString(payload), nil
}

// Unprotect decrypts a value written by Protect (Go or Python). Legacy dpapi:
// and private-file: records are accepted only on the platform that wrote them.
func Unprotect(value, key string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.HasPrefix(value, EncryptionPrefix) {
		return legacyUnprotect(value)
	}
	resolved := resolveEncryptionKey(key)
	if resolved == "" {
		return "", &CryptoError{Message: "读取 apikey 需要设置 " + EncryptionKeyEnv}
	}
	payload, err := base64.StdEncoding.DecodeString(value[len(EncryptionPrefix):])
	if err != nil {
		return "", err
	}
	aead, err := newAEAD(resolved)
	if err != nil {
		return "", err
	}
	if len(payload) < encryptionNonceSize {
		return "", fmt.Errorf("加密的 apikey 数据不完整")
	}
	plaintext, err := aead.Open(nil, payload[:encryptionNonceSize], payload[encryptionNonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// IsPortable reports whether value is empty or uses the portable enc:v1 format.
func IsPortable(value string) bool {
	return value == "" || strings.HasPrefix(value, EncryptionPrefix)
}

// newAEAD decodes the configured key and builds the AES-GCM cipher.
func newAEAD(keyText string) (cipher.AEAD, error) {
	raw, err := decodeEncryptionKey(keyText)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// decodeEncryptionKey mirrors crypto._decode_key: optional "base64:" prefix,
// url-safe or standard base64, tolerant of missing padding, and exactly 32
// bytes. Any failure reports the configuration error message.
func decodeEncryptionKey(keyText string) ([]byte, error) {
	value := strings.TrimSpace(keyText)
	value = strings.TrimPrefix(value, "base64:")
	// Python translates the url-safe alphabet and then decodes; accept both
	// alphabets like base64.urlsafe_b64decode does.
	value = strings.NewReplacer("-", "+", "_", "/").Replace(value)
	padding := (4 - len(value)%4) % 4
	value += strings.Repeat("=", padding)
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) != encryptionKeySize {
		return nil, &CryptoError{Message: EncryptionKeyEnv + " 必须是 base64 编码的 32 字节密钥"}
	}
	return raw, nil
}

// legacyProtect mirrors crypto._legacy_protect. The Go port does not implement
// Windows DPAPI yet (deferred); it refuses to write a format the Python service
// legacyProtect mirrors crypto._legacy_protect: dpapi: on Windows (current-user
// CryptProtectData) and private-file: elsewhere. The platform split lives in
// crypto_legacy_windows.go / crypto_legacy_other.go.
func legacyProtect(value string) (string, error) {
	return legacyProtectPlatform(value)
}

// legacyUnprotect mirrors crypto._legacy_unprotect: only the platform that
// wrote a legacy value can read it back.
func legacyUnprotect(value string) (string, error) {
	return legacyUnprotectPlatform(value)
}
