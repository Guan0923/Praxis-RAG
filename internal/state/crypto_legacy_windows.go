//go:build windows

package state

import (
	"encoding/base64"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dataBlob mirrors Win32 DATA_BLOB. It must keep the exact layout because the
// pointer is handed to crypt32.
type dataBlob struct {
	size uint32
	data *byte
}

var (
	crypt32DLL             = windows.NewLazySystemDLL("crypt32.dll")
	cryptProtectDataProc   = crypt32DLL.NewProc("CryptProtectData")
	cryptUnprotectDataProc = crypt32DLL.NewProc("CryptUnprotectData")
)

// cryptProtectUIForbidden matches the flag Python's ctypes call passes (1).
const cryptProtectUIForbidden = 0x1

// dpapiProtect mirrors crypto._dpapi(value): CryptProtectData with the current
// user's default protection, no optional entropy, and UI forbidden.
func dpapiProtect(value []byte) ([]byte, error) {
	var input dataBlob
	if len(value) > 0 {
		input.size = uint32(len(value))
		input.data = &value[0]
	}
	var output dataBlob
	result, _, _ := cryptProtectDataProc.Call(
		uintptr(unsafe.Pointer(&input)),
		0, // szDataDescr
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&output)),
	)
	if result == 0 {
		return nil, &CryptoError{Message: "无法使用当前系统账号保护或读取 apikey"}
	}
	defer func() {
		if output.data != nil {
			_, _ = windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.data))))
		}
	}()
	if output.size == 0 || output.data == nil {
		return []byte{}, nil
	}
	plain := make([]byte, output.size)
	copy(plain, unsafe.Slice(output.data, output.size))
	return plain, nil
}

// dpapiUnprotect mirrors crypto._dpapi(value, decrypt=True).
func dpapiUnprotect(value []byte) ([]byte, error) {
	var input dataBlob
	if len(value) > 0 {
		input.size = uint32(len(value))
		input.data = &value[0]
	}
	var output dataBlob
	result, _, _ := cryptUnprotectDataProc.Call(
		uintptr(unsafe.Pointer(&input)),
		0, // ppszDataDescr (not requested)
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&output)),
	)
	if result == 0 {
		return nil, &CryptoError{Message: "无法使用当前系统账号保护或读取 apikey"}
	}
	defer func() {
		if output.data != nil {
			_, _ = windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.data))))
		}
	}()
	if output.size == 0 || output.data == nil {
		return []byte{}, nil
	}
	plain := make([]byte, output.size)
	copy(plain, unsafe.Slice(output.data, output.size))
	return plain, nil
}

// legacyProtectPlatform mirrors crypto._legacy_protect on Windows.
func legacyProtectPlatform(value string) (string, error) {
	protected, err := dpapiProtect([]byte(value))
	if err != nil {
		return "", err
	}
	return "dpapi:" + base64.StdEncoding.EncodeToString(protected), nil
}

// legacyUnprotectPlatform mirrors crypto._legacy_unprotect on Windows: dpapi:
// values are readable, private-file: values are rejected as foreign.
func legacyUnprotectPlatform(value string) (string, error) {
	switch {
	case strings.HasPrefix(value, "dpapi:"):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "dpapi:"))
		if err != nil {
			return "", err
		}
		plain, err := dpapiUnprotect(raw)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	case strings.HasPrefix(value, "private-file:"):
		return "", &CryptoError{Message: "apikey 需要由保存它的系统账号读取"}
	default:
		return "", &CryptoError{Message: "apikey 需要由保存它的系统账号读取"}
	}
}
