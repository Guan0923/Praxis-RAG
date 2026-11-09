package state

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// isoTimestampFormat is the subset of datetime.isoformat() used for the stored
// created_at/updated_at strings: UTC with microseconds and a +00:00 offset.
const isoTimestampFormat = "2006-01-02T15:04:05.000000+00:00"

// nowISO mirrors repositories._now().
func nowISO() string {
	return time.Now().UTC().Format(isoTimestampFormat)
}

// marshalJSON mirrors json.dumps(value, ensure_ascii=False): compact UTF-8 with
// no HTML escaping. Python's default ", "/": " separators are not reproduced
// because every stored value is read back through a JSON parser.
func marshalJSON(value any) (string, error) {
	var builder strings.Builder
	encoder := json.NewEncoder(&builder)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(builder.String(), "\n"), nil
}

// decodeJSON mirrors repositories._object: an empty stored value falls back to
// the supplied default; invalid stored JSON raises StateError with the same
// message as Python.
func decodeJSON(raw string, fallback any) (any, error) {
	if raw == "" {
		return fallback, nil
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, newStateError("Stored JSON state is invalid")
	}
	return value, nil
}

// decodeObject mirrors _object(value, {}) for a stored JSON object.
func decodeObject(raw string) (map[string]any, error) {
	value, err := decodeJSON(raw, map[string]any{})
	if err != nil {
		return nil, err
	}
	if value == nil {
		return map[string]any{}, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, newStateError("Stored JSON state is invalid")
	}
	return object, nil
}

// decodeStringList decodes a stored JSON array of strings.
func decodeStringList(raw string) ([]string, error) {
	value, err := decodeJSON(raw, []any{})
	if err != nil {
		return nil, err
	}
	items, ok := value.([]any)
	if !ok {
		return nil, newStateError("Stored JSON state is invalid")
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, newStateError("Stored JSON state is invalid")
		}
		result = append(result, text)
	}
	return result, nil
}

// marshalStringList mirrors json.dumps(namespaces).
func marshalStringList(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	return marshalJSON(values)
}

// sha256Hex reproduces hashlib.sha256(value.encode()).hexdigest().
func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// sha256Sum reproduces hashlib.sha256(content).hexdigest() for bytes.
func sha256Sum(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// tokenURLSafe reproduces secrets.token_urlsafe(size): base64url of size random
// bytes without padding.
func tokenURLSafe(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("无法生成安全随机数: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// randomHex reproduces secrets.token_hex(size): 2*size lowercase hex chars.
func randomHex(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("无法生成安全随机数: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// uuid4Hex reproduces uuid4().hex (32 lowercase hex chars with version/variant
// bits set) for cache outbox event ids.
func uuid4Hex() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("无法生成安全随机数: %w", err)
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	return hex.EncodeToString(buffer), nil
}

// unixSeconds reproduces time.time(): fractional seconds since the epoch.
func unixSeconds() float64 {
	return float64(time.Now().UnixNano()) / float64(time.Second)
}

// requireSetting reads a mandatory string setting from a knowledge-base
// settings object. Python raises KeyError/TypeError here; the Go layer reports
// a plain error that callers map to their own validation message.
func requireSetting(settings map[string]any, name string) (string, error) {
	raw, ok := settings[name]
	if !ok {
		return "", fmt.Errorf("knowledge base settings require %q", name)
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("knowledge base settings %q must be a string", name)
	}
	return value, nil
}

// uuidNamespaceURL is RFC 4122 UUID.NAMESPACE_URL, used by uuid5 identities.
var uuidNamespaceURL = [16]byte{
	0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// uuid5 reproduces uuid.uuid5(uuid.NAMESPACE_URL, name).
func uuid5(namespace [16]byte, name string) string {
	hasher := sha1.New()
	_, _ = hasher.Write(namespace[:])
	_, _ = hasher.Write([]byte(name))
	var value [16]byte
	copy(value[:], hasher.Sum(nil)[:16])
	value[6] = (value[6] & 0x0f) | 0x50
	value[8] = (value[8] & 0x3f) | 0x80
	return formatUUID(value)
}

// uuid4String reproduces str(uuid.uuid4()): lowercase dashed UUIDv4.
func uuid4String() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("无法生成安全随机数: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	var fixed [16]byte
	copy(fixed[:], value)
	return formatUUID(fixed), nil
}

func formatUUID(value [16]byte) string {
	const hexDigits = "0123456789abcdef"
	buffer := make([]byte, 36)
	position := 0
	for index, char := range value {
		if index == 4 || index == 6 || index == 8 || index == 10 {
			buffer[position] = '-'
			position++
		}
		buffer[position] = hexDigits[char>>4]
		buffer[position+1] = hexDigits[char&0x0f]
		position += 2
	}
	return string(buffer)
}

// canonicalUUID reports whether value is the canonical lowercase dashed form,
// mirroring `str(uuid.UUID(value)) == value` (result_snapshots.canonical_uuid
// and ArtifactRegistry.read).
func canonicalUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index := 0; index < len(value); index++ {
		switch index {
		case 8, 13, 18, 23:
			if value[index] != '-' {
				return false
			}
		default:
			char := value[index]
			if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
				return false
			}
		}
	}
	return true
}

// quotePython reproduces urllib.parse.quote(value, safe=""): every byte outside
// A-Z a-z 0-9 - . _ ~ becomes an uppercase %XX escape.
func quotePython(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		char := value[index]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '.' || char == '_' || char == '~' {
			builder.WriteByte(char)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(hexDigits[char>>4])
		builder.WriteByte(hexDigits[char&0x0f])
	}
	return builder.String()
}

// errorTypeName reproduces type(exc).__name__ for sanitized last_error columns.
func errorTypeName(err error) string {
	if err == nil {
		return ""
	}
	name := reflect.TypeOf(err).String()
	if index := strings.LastIndex(name, "."); index >= 0 {
		name = name[index+1:]
	}
	name = strings.TrimPrefix(name, "*")
	if name == "" {
		return "Error"
	}
	return name
}
