package httpapi

import (
	"net/http"
	"strings"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
)

func writeBadRuntime(w http.ResponseWriter, err error) {
	if apierrors.IsSnapshotUnavailable(err) {
		writeDetail(w, http.StatusGone, err.Error())
		return
	}
	writeRuntimeError(w, err, true)
}

// artifactDisposition ports the Python filename sanitization and builds a safe
// Content-Disposition header value.
func artifactDisposition(name, mimeType string) (string, string) {
	leaf := strings.ReplaceAll(name, "\\", "/")
	if index := strings.LastIndex(leaf, "/"); index >= 0 {
		leaf = leaf[index+1:]
	}
	leaf = strings.Map(func(character rune) rune {
		if character < 32 || character == 127 {
			return -1
		}
		return character
	}, leaf)
	leaf = strings.TrimSpace(leaf)
	if leaf == "" || leaf == "." || leaf == ".." {
		leaf = "artifact"
	}
	fallback := strings.Map(func(character rune) rune {
		if (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' ||
			character == ' ' || character == '-' {
			return character
		}
		return '_'
	}, leaf)
	safeType := "application/octet-stream"
	if validMIMEType(mimeType) {
		safeType = mimeType
	}
	return `attachment; filename="` + fallback + `"; filename*=UTF-8''` + percentQuote(leaf), safeType
}

func validMIMEType(value string) bool {
	if value == "" {
		return false
	}
	slash := strings.Index(value, "/")
	if slash <= 0 || slash == len(value)-1 {
		return false
	}
	for _, character := range value {
		if (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("!#$&^_.+-/", character) {
			continue
		}
		return false
	}
	return true
}

func percentQuote(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' || character == '~' {
			builder.WriteByte(character)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(hexDigits[character>>4])
		builder.WriteByte(hexDigits[character&0x0f])
	}
	return builder.String()
}
