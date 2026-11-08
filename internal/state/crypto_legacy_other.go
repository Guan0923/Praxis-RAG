//go:build !windows

package state

import "strings"

// legacyProtectPlatform mirrors crypto._legacy_protect on non-Windows hosts.
func legacyProtectPlatform(value string) (string, error) {
	return "private-file:" + value, nil
}

// legacyUnprotectPlatform mirrors crypto._legacy_unprotect on non-Windows hosts:
// private-file: values are readable, dpapi: values require the Windows account.
func legacyUnprotectPlatform(value string) (string, error) {
	switch {
	case strings.HasPrefix(value, "private-file:"):
		return strings.TrimPrefix(value, "private-file:"), nil
	default:
		return "", &CryptoError{Message: "apikey 需要由保存它的系统账号读取"}
	}
}
