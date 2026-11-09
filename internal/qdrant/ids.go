package qdrant

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
)

// pointNamespace is the fixed UUID namespace used by rag_mcp.storage.qdrant_store.
var pointNamespace = [16]byte{
	0xf8, 0x53, 0x4b, 0x63, 0x11, 0x2c, 0x4e, 0xdc,
	0x97, 0x07, 0xc2, 0x82, 0x4a, 0xa5, 0x3c, 0x73,
}

// uuid5 computes the RFC 4122 version-5 UUID for name in the given namespace.
func uuid5(namespace [16]byte, name []byte) string {
	hasher := sha1.New()
	hasher.Write(namespace[:])
	hasher.Write(name)
	sum := hasher.Sum(nil)
	var value [16]byte
	copy(value[:], sum[:16])
	value[6] = (value[6] & 0x0f) | 0x50
	value[8] = (value[8] & 0x3f) | 0x80
	var text [36]byte
	hex.Encode(text[0:8], value[0:4])
	text[8] = '-'
	hex.Encode(text[9:13], value[4:6])
	text[13] = '-'
	hex.Encode(text[14:18], value[6:8])
	text[18] = '-'
	hex.Encode(text[19:23], value[8:10])
	text[23] = '-'
	hex.Encode(text[24:36], value[10:16])
	return string(text[:])
}

// versionPointID derives the deterministic point id for one staged version.
// Python builds it as uuid5(namespace, json.dumps([...])) with json.dumps
// defaults (", " separators and ensure_ascii=True), which this reproduces.
func (s *Store) versionPointID(recordType, itemID, version, documentID string) string {
	var namespace any
	if s.namespace != "" {
		namespace = s.namespace
	}
	name := pythonJSONList([]any{"version", namespace, documentID, version, recordType, itemID})
	return uuid5(pointNamespace, []byte(name))
}

// pointID derives the legacy (unversioned) point id: uuid5(namespace, scope).
func (s *Store) pointID(itemID string) string {
	return uuid5(pointNamespace, []byte(s.namespace+"\x00"+itemID))
}

// pythonJSONList renders a list exactly like json.dumps with default options
// (which is what the Python store feeds to uuid.uuid5).
func pythonJSONList(values []any) string {
	var builder strings.Builder
	builder.WriteByte('[')
	for index, value := range values {
		if index > 0 {
			builder.WriteString(", ")
		}
		switch typed := value.(type) {
		case nil:
			builder.WriteString("null")
		case string:
			builder.WriteString(pythonJSONString(typed))
		default:
			panic("pythonJSONList only supports strings and null")
		}
	}
	builder.WriteByte(']')
	return builder.String()
}

// pythonJSONString escapes one string with json.dumps(ensure_ascii=True)
// escaping rules (including surrogate pairs for astral code points).
func pythonJSONString(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\b':
			builder.WriteString(`\b`)
		case '\f':
			builder.WriteString(`\f`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if char < 0x20 {
				builder.WriteString(`\u00`)
				builder.WriteByte(lowerHex(byte(char) >> 4))
				builder.WriteByte(lowerHex(byte(char) & 0x0f))
				continue
			}
			if char <= 0x7E {
				builder.WriteRune(char)
				continue
			}
			if char <= 0xFFFF {
				writeUnicodeEscape(&builder, uint16(char))
				continue
			}
			scalar := char - 0x10000
			high := uint16(0xD800 + (scalar >> 10))
			low := uint16(0xDC00 + (scalar & 0x3FF))
			writeUnicodeEscape(&builder, high)
			writeUnicodeEscape(&builder, low)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

func writeUnicodeEscape(builder *strings.Builder, value uint16) {
	const hexDigits = "0123456789abcdef"
	builder.WriteString(`\u`)
	builder.WriteByte(hexDigits[(value>>12)&0xF])
	builder.WriteByte(hexDigits[(value>>8)&0xF])
	builder.WriteByte(hexDigits[(value>>4)&0xF])
	builder.WriteByte(hexDigits[value&0xF])
}

func lowerHex(value byte) byte {
	if value < 10 {
		return '0' + value
	}
	return 'a' + (value - 10)
}
