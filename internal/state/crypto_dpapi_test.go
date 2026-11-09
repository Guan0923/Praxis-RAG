package state

import (
	"encoding/base64"
	"os"
	"runtime"
	"strings"
	"testing"
)

const platformLegacyMessage = "apikey 需要由保存它的系统账号读取"

// TestLegacyProtectPlatformBehavior covers the platform-split legacy protection:
// Windows uses current-user DPAPI, every other platform uses private-file:.
func TestLegacyProtectPlatformBehavior(t *testing.T) {
	isolateStateEnv(t)
	key := os.Getenv(EncryptionKeyEnv)
	if key != "" {
		t.Fatalf("test environment still has %s set: %q", EncryptionKeyEnv, key)
	}
	protected, err := Protect("legacy-secret", "")
	if err != nil {
		t.Fatalf("Protect() error = %v", err)
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(protected, "dpapi:") {
			t.Fatalf("protected value = %q, want the dpapi: prefix", protected)
		}
		if _, decodeErr := base64.StdEncoding.DecodeString(strings.TrimPrefix(protected, "dpapi:")); decodeErr != nil {
			t.Fatalf("dpapi payload is not base64: %v", decodeErr)
		}
		decrypted, err := Unprotect(protected, "")
		if err != nil || decrypted != "legacy-secret" {
			t.Fatalf("Unprotect(dpapi) = %q, %v", decrypted, err)
		}
		// A second protection re-encrypts but still round-trips.
		second, err := Protect("legacy-secret", "")
		if err != nil || !strings.HasPrefix(second, "dpapi:") {
			t.Fatalf("second Protect() = %q, %v", second, err)
		}
		if second == protected {
			t.Fatal("DPAPI protection is deterministic; expected fresh entropy")
		}
	} else {
		if protected != "private-file:legacy-secret" {
			t.Fatalf("protected value = %q, want private-file:", protected)
		}
		decrypted, err := Unprotect(protected, "")
		if err != nil || decrypted != "legacy-secret" {
			t.Fatalf("Unprotect(private-file) = %q, %v", decrypted, err)
		}
	}
}

func TestLegacyUnprotectPlatformBehavior(t *testing.T) {
	isolateStateEnv(t)
	if runtime.GOOS == "windows" {
		_, err := Unprotect("private-file:secret", "")
		cryptoError := mustBeError[*CryptoError](t, err)
		if cryptoError.Message != platformLegacyMessage {
			t.Fatalf("private-file message = %q, want %q", cryptoError.Message, platformLegacyMessage)
		}
		_, err = Unprotect("dpapi:not-base64!!!", "")
		if err == nil {
			t.Fatal("Unprotect() of an invalid dpapi payload succeeded")
		}
	} else {
		_, err := Unprotect("dpapi:AAAA", "")
		cryptoError := mustBeError[*CryptoError](t, err)
		if cryptoError.Message != platformLegacyMessage {
			t.Fatalf("dpapi message = %q, want %q", cryptoError.Message, platformLegacyMessage)
		}
		decrypted, err := Unprotect("private-file:secret", "")
		if err != nil || decrypted != "secret" {
			t.Fatalf("Unprotect(private-file) = %q, %v", decrypted, err)
		}
	}
	_, err := Unprotect("plain-value", "")
	if cryptoError := mustBeError[*CryptoError](t, err); cryptoError.Message != platformLegacyMessage {
		t.Fatalf("unknown legacy message = %q, want %q", cryptoError.Message, platformLegacyMessage)
	}
}
