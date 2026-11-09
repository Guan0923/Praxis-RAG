// Package resources implements the stable document resource URIs and bounded
// paragraph pages exposed over MCP. It is a port of rag_mcp/resources.py.
package resources

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// PageCharacters is the maximum number of characters in one paragraph page.
const PageCharacters = 8000

var (
	formulaBlock = regexp.MustCompile(`(?m)^\$\$[ \t]*\n[\s\S]*?^\$\$[ \t]*(?:\n|$)`)
	pagePattern  = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)
)

// percentQuote mirrors Python urllib.parse.quote(value, safe=""): every byte
// outside the unreserved set is percent-encoded with uppercase hex digits.
func percentQuote(value string) string {
	const hex = "0123456789ABCDEF"
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'A' && character <= 'Z') ||
			(character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' || character == '~' {
			builder.WriteByte(character)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(hex[character>>4])
		builder.WriteByte(hex[character&0x0f])
	}
	return builder.String()
}

// DocumentURI returns the canonical resource URI for one document.
func DocumentURI(namespace, name string) string {
	return fmt.Sprintf("rag://knowledge/%s/%s", percentQuote(namespace), percentQuote(name))
}

// ParseURI validates a document resource URI and returns its page selection.
func ParseURI(namespace, uri string) (string, int, *string, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return "", 0, nil, fmt.Errorf("Invalid document URI")
	}
	escapedPath := parsed.EscapedPath()
	parts := strings.Split(escapedPath, "/")
	if parsed.Scheme != "rag" || parsed.Host != "knowledge" || parsed.Fragment != "" || len(parts) != 3 {
		return "", 0, nil, fmt.Errorf("Invalid document URI")
	}
	uriNamespace, err := url.PathUnescape(parts[1])
	if err != nil {
		return "", 0, nil, fmt.Errorf("Invalid document URI")
	}
	name, err := url.PathUnescape(parts[2])
	if err != nil {
		return "", 0, nil, fmt.Errorf("Invalid document URI")
	}
	if uriNamespace != namespace || name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\\x00") {
		return "", 0, nil, fmt.Errorf("Document URI is outside this namespace")
	}
	if parsed.Scheme+"://"+parsed.Host+escapedPath != DocumentURI(namespace, name) {
		return "", 0, nil, fmt.Errorf("Non-canonical document URI")
	}

	query, err := parseStrictQuery(parsed.RawQuery)
	if err != nil {
		return "", 0, nil, err
	}
	for key := range query {
		if key != "page" && key != "version" {
			return "", 0, nil, fmt.Errorf("Invalid page parameters")
		}
	}
	page := 1
	if values, ok := query["page"]; ok {
		if len(values) != 1 || !pagePattern.MatchString(values[0]) {
			return "", 0, nil, fmt.Errorf("Invalid page number")
		}
		page, _ = strconv.Atoi(values[0])
	}
	var version *string
	if values, ok := query["version"]; ok {
		if len(values) != 1 || values[0] == "" {
			return "", 0, nil, fmt.Errorf("Empty document version")
		}
		value := values[0]
		version = &value
	}
	if page > 1 && version == nil {
		return "", 0, nil, fmt.Errorf("Read the document URI first to obtain a versioned next-page URI")
	}
	return name, page, version, nil
}

func parseStrictQuery(raw string) (map[string][]string, error) {
	result := map[string][]string{}
	if raw == "" {
		return result, nil
	}
	for _, segment := range strings.Split(raw, "&") {
		key, value, ok := strings.Cut(segment, "=")
		if !ok {
			return nil, fmt.Errorf("Invalid page parameters")
		}
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			return nil, fmt.Errorf("Invalid page parameters")
		}
		decodedValue, err := url.QueryUnescape(value)
		if err != nil {
			return nil, fmt.Errorf("Invalid page parameters")
		}
		result[decodedKey] = append(result[decodedKey], decodedValue)
	}
	return result, nil
}

type span struct{ start, end int }

// ParagraphPages splits text into pages of at most PageCharacters code points,
// preferring paragraph boundaries and never splitting a protected formula.
func ParagraphPages(text string, protectInlineMath bool) []string {
	runes := []rune(text)
	formulas := formulaRanges(text)
	if protectInlineMath {
		formulas = append(formulas, inlineMathRanges(runes)...)
		sortSpans(formulas)
	}

	pages := make([]string, 0, 4)
	start := 0
	for start < len(runes) {
		if len(runes)-start <= PageCharacters {
			pages = append(pages, string(runes[start:]))
			break
		}
		window := runes[start : start+PageCharacters]
		end := start + PageCharacters
		if index := lastParagraphBreak(window); index >= 0 {
			end = start + index + 2
		}
		for _, formula := range formulas {
			if formula.start < end && end < formula.end {
				if formula.start > start {
					end = formula.start
				} else {
					end = formula.end
				}
				break
			}
		}
		pages = append(pages, string(runes[start:end]))
		start = end
	}
	if len(pages) == 0 {
		return []string{""}
	}
	return pages
}

func lastParagraphBreak(window []rune) int {
	for index := len(window) - 2; index >= 0; index-- {
		if window[index] == '\n' && window[index+1] == '\n' {
			return index
		}
	}
	return -1
}

func formulaRanges(text string) []span {
	matches := formulaBlock.FindAllStringIndex(text, -1)
	ranges := make([]span, 0, len(matches))
	for _, match := range matches {
		ranges = append(ranges, span{
			start: utf8.RuneCountInString(text[:match[0]]),
			end:   utf8.RuneCountInString(text[:match[1]]),
		})
	}
	return ranges
}

// inlineMathRanges implements (?<!\$)\$(?!\$)[^$]+\$(?!\$) over runes, since
// Go's RE2 engine has no lookaround.
func inlineMathRanges(runes []rune) []span {
	result := make([]span, 0)
	for index := 0; index < len(runes); {
		if runes[index] != '$' {
			index++
			continue
		}
		if index > 0 && runes[index-1] == '$' {
			index++
			continue
		}
		if index+1 < len(runes) && runes[index+1] == '$' {
			index += 2
			continue
		}
		closing := index + 1
		for closing < len(runes) && runes[closing] != '$' {
			closing++
		}
		if closing >= len(runes) {
			break
		}
		if closing+1 < len(runes) && runes[closing+1] == '$' {
			index = closing + 2
			continue
		}
		if closing > index+1 {
			result = append(result, span{start: index, end: closing + 1})
			index = closing + 1
			continue
		}
		index++
	}
	return result
}

func sortSpans(spans []span) {
	for index := 1; index < len(spans); index++ {
		for inner := index; inner > 0 && spans[inner].start < spans[inner-1].start; inner-- {
			spans[inner], spans[inner-1] = spans[inner-1], spans[inner]
		}
	}
}

// ResourcePage renders one paragraph page payload for MCP read_resource.
func ResourcePage(namespace, name, text, version string, page int, expected *string) (map[string]any, error) {
	if expected != nil && *expected != version {
		return nil, fmt.Errorf("Document changed; read its document URI again")
	}
	protect := strings.HasSuffix(strings.ToLower(name), ".doc") || strings.HasSuffix(strings.ToLower(name), ".docx")
	pages := ParagraphPages(text, protect)
	if page > len(pages) {
		return nil, fmt.Errorf("Page does not exist")
	}
	uri := DocumentURI(namespace, name)
	hasMore := page < len(pages)
	var nextURI any
	if hasMore {
		nextURI = uri + "?" + url.Values{"page": {strconv.Itoa(page + 1)}, "version": {version}}.Encode()
	}
	return map[string]any{
		"uri":         uri,
		"text":        pages[page-1],
		"page":        page,
		"total_pages": len(pages),
		"version":     version,
		"truncated":   hasMore,
		"has_more":    hasMore,
		"next_uri":    nextURI,
	}, nil
}
