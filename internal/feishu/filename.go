package feishu

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// reservedFilenameCharacters mirrors the Windows-safe replacement applied by the
// existing upload pipeline (state.windowsSanitizeName).
var reservedFilenameCharacters = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

// maxFilenameBytes is the document-name ceiling enforced by document_jobs.
const maxFilenameBytes = 200

// DocumentFilename turns one Feishu document title into a safe Markdown upload
// filename. The result satisfies the existing upload validation: no reserved
// characters, no leading/trailing dots or spaces, at most 200 bytes, and a
// supported ".md" suffix.
func DocumentFilename(title string) string {
	name := reservedFilenameCharacters.ReplaceAllString(strings.TrimSpace(title), "_")
	name = strings.Trim(name, " .")
	if name == "" {
		name = "飞书文档"
	}
	if strings.HasSuffix(strings.ToLower(name), ".md") {
		name = name[:len(name)-len(".md")]
	}
	name = strings.TrimRight(name, " .")
	if name == "" {
		name = "飞书文档"
	}
	if reservedBaseName(name) {
		name = "_" + name
	}
	// Leave room for the suffix and keep the cut on a rune boundary.
	limit := maxFilenameBytes - len(".md")
	for len(name) > limit {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	name = strings.TrimRight(name, " .")
	if name == "" {
		name = "飞书文档"
	}
	return name + ".md"
}

// reservedBaseName reports Windows device names that must not be used as a file
// stem (CON, PRN, AUX, NUL, COM1..9, LPT1..9).
func reservedBaseName(name string) bool {
	base := name
	if index := strings.Index(base, "."); index >= 0 {
		base = base[:index]
	}
	upper := strings.ToUpper(strings.TrimSpace(base))
	switch upper {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) {
		return upper[3] >= '1' && upper[3] <= '9'
	}
	return false
}
