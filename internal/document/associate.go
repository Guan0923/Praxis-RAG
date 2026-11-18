package document

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The caption/note classification below is shared by the Docling manifest
// association (docling_associate.go). The former native-OOXML association
// (word_reader.WordReader.associate) was removed together with the in-process
// Word parser; Docling's service-side structure plus AssociateDoclingCaptions
// own the caption/note attachment now.

// captionPrefix mirrors the prefix part of word_reader.CAPTION: it captures the
// object kind (图/表/figure/fig/table) and the number. The Python pattern ends
// with r"(?:\b|(?=[\s:：、.．]))"; Go's regexp has no lookaround and its \b is
// ASCII-only, so the trailing boundary is checked by pythonWordBoundaryAfter.
var captionPrefix = regexp.MustCompile(`(?i)^\s*(图|表|figure|fig\.?|table)\s*(\d+(?:[.\-]\d+)*|[一二三四五六七八九十]+)`)

// noteLine mirrors word_reader.NOTE.
var noteLine = regexp.MustCompile(`(?i)^\s*(?:注|说明|来源|资料来源|note|notes|source)\s*[:：.]`)

// captionParts is one successful word_reader.CAPTION.match.
type captionParts struct {
	Kind   string
	Number string
}

// parseCaption ports word_reader.CAPTION.match.
func parseCaption(text string) *captionParts {
	match := captionPrefix.FindStringSubmatchIndex(text)
	if match == nil {
		return nil
	}
	rest := text[match[1]:]
	if rest != "" {
		value, _ := utf8.DecodeRuneInString(rest)
		if isPythonWordRune(value) {
			return nil
		}
	}
	return &captionParts{Kind: text[match[2]:match[3]], Number: text[match[4]:match[5]]}
}

// captionIsTable reports whether a caption kind names a table, mirroring the
// Python `match["kind"].lower() in {"table", "表"}` checks.
func captionIsTable(kind string) bool {
	switch strings.ToLower(kind) {
	case "table", "表":
		return true
	}
	return false
}

// isPythonWordRune reports whether rune counts as \w for Python's re module.
func isPythonWordRune(value rune) bool {
	return value == '_' || unicode.IsLetter(value) || unicode.IsDigit(value) ||
		unicode.Is(unicode.Nl, value) || unicode.Is(unicode.No, value)
}

// sequentialNumbers reports whether numbers is exactly "1".."count".
func sequentialNumbers(numbers []string, count int) bool {
	if len(numbers) != count {
		return false
	}
	seen := make(map[string]struct{}, count)
	for _, number := range numbers {
		seen[number] = struct{}{}
	}
	if len(seen) != count {
		return false
	}
	for position := 1; position <= count; position++ {
		if _, ok := seen[strconv.Itoa(position)]; !ok {
			return false
		}
	}
	return true
}
