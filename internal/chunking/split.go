package chunking

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/Guan0923/Praxis-RAG/internal/document"
)

// Range is one half-open [Start, End) span measured in runes, matching Python
// string indices (chunk_strategies returns character offsets).
type Range struct {
	Start int
	End   int
}

// TokenBudget ports chunk_strategies.TokenBudget over an injectable
// TokenCounter. It owns token counts and overlap windows so callers can keep
// chunk boundaries reproducible.
type TokenBudget struct {
	counter TokenCounter
	size    int
	overlap int
	maximum int
}

// NewTokenBudget validates chunk_size against the counter's model limit, like
// TokenBudget.__init__ (the error message matches the Python one).
func NewTokenBudget(counter TokenCounter, config Config) (*TokenBudget, error) {
	maximum := tokenMaximum(counter)
	if maximum < 1 {
		maximum = 1
	}
	if config.ChunkSize > maximum {
		return nil, fmt.Errorf("块长度不能超过模型允许的 %d token", maximum)
	}
	return &TokenBudget{
		counter: counter,
		size:    config.ChunkSize,
		overlap: config.ChunkOverlap,
		maximum: maximum,
	}, nil
}

// Count returns the token count of text.
func (b *TokenBudget) Count(text string) int {
	return b.counter.Count(text)
}

// Maximum is min(model_max_length, 8192) - special tokens.
func (b *TokenBudget) Maximum() int { return b.maximum }

// countRunes counts tokens of the document.Text rune window [start, end).
func (b *TokenBudget) countRunes(runes []rune, start, end int) int {
	if start >= end {
		return 0
	}
	return b.counter.Count(string(runes[start:end]))
}

// countRunesStripped counts tokens of the stripped rune window, mirroring
// `self.count(text[start:stop].strip())`.
func (b *TokenBudget) countRunesStripped(runes []rune, start, end int) int {
	if start >= end {
		return 0
	}
	return b.counter.Count(strings.TrimSpace(string(runes[start:end])))
}

// tokenOffsets returns the token start offsets of runes[start:end] relative to
// start. Counters without TokenOffsetter get offsets derived from prefix counts.
func (b *TokenBudget) tokenOffsets(runes []rune, start, end int) []int {
	if start >= end {
		return nil
	}
	window := string(runes[start:end])
	if offsetter, ok := b.counter.(TokenOffsetter); ok {
		offsets := offsetter.TokenStartOffsets(window)
		if len(offsets) == b.counter.Count(window) {
			return offsets
		}
	}
	return scannedTokenOffsets(b.counter, window)
}

// scannedTokenOffsets derives token start offsets by scanning prefix counts.
// It is O(n^2) in the probe window but only used for counters that do not
// implement TokenOffsetter.
func scannedTokenOffsets(counter TokenCounter, text string) []int {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	total := counter.Count(text)
	offsets := make([]int, 0, total)
	previous := 0
	for index := 1; index <= len(runes); index++ {
		count := counter.Count(string(runes[:index]))
		for previous < count && len(offsets) < total {
			offset := index - 1
			if offset < 0 {
				offset = 0
			}
			offsets = append(offsets, offset)
			previous++
		}
	}
	for len(offsets) < total {
		offsets = append(offsets, len(runes)-1)
	}
	return offsets
}

// suffix ports TokenBudget.suffix: the start of a trailing overlap that keeps
// at most `overlap` tokens and never starts before start+1.
func (b *TokenBudget) suffix(runes []rune, start, end int) int {
	if b.overlap == 0 {
		return end
	}
	offsets := b.tokenOffsets(runes, start, end)
	candidate := end
	if len(offsets) > 0 {
		index := len(offsets) - b.overlap
		if index < 0 {
			index = 0
		}
		candidate = start + offsets[index]
	}
	if candidate < start+1 {
		candidate = start + 1
	}
	for candidate < end && b.countRunes(runes, candidate, end) > b.overlap {
		candidate++
	}
	return candidate
}

// windows ports TokenBudget.windows: token-budgeted windows with overlap. The
// probe bounds work per window even for a multi-million-character sentence.
func (b *TokenBudget) windows(runes []rune, start, end int) ([]Range, error) {
	result := make([]Range, 0, 8)
	for start < end {
		probe := start + maxInt(128, b.size*8)
		if probe > end {
			probe = end
		}
		offsets := b.tokenOffsets(runes, start, probe)
		for len(offsets) <= b.size && probe < end {
			probe = start + (probe-start)*2
			if probe > end {
				probe = end
			}
			offsets = b.tokenOffsets(runes, start, probe)
		}
		stop := probe
		if len(offsets) > b.size {
			stop = start + offsets[b.size]
		}
		if stop < start+1 {
			stop = start + 1
		}
		for stop > start && b.countRunesStripped(runes, start, stop) > b.size {
			stop--
		}
		if stop == start {
			return nil, fmt.Errorf("块长度过小，无法容纳一个完整字符")
		}
		result = append(result, Range{Start: start, End: stop})
		if stop >= end {
			break
		}
		start = b.suffix(runes, start, stop)
	}
	return result, nil
}

// SentenceRanges ports chunk_strategies.sentence_ranges over text[start:end]
// (rune offsets). It splits on CJK/Latin sentence terminators and keeps
// complete trailing closing quotes while ignoring common abbreviations.
func SentenceRanges(text string, start, end int) []Range {
	runes := []rune(text)
	return sentenceRangesRunes(runes, start, end)
}

func sentenceRangesRunes(runes []rune, start, end int) []Range {
	if start < 0 {
		start = 0
	}
	if end > len(runes) {
		end = len(runes)
	}
	result := make([]Range, 0, 8)
	cursor := start
	index := start
	for index < end {
		value := runes[index]
		if isSentenceTerminator(value) {
			stop := index + 1
			for stop < end && isSentenceTerminator(runes[stop]) {
				stop++
			}
			for stop < end && isClosingQuote(runes[stop]) {
				stop++
			}
			result = append(result, Range{Start: cursor, End: stop})
			cursor = stop
			index = stop
			continue
		}
		if value == '.' {
			stop := index + 1
			for stop < end && isClosingQuote(runes[stop]) {
				stop++
			}
			next := stop
			if (next == end || unicode.IsSpace(runes[next])) && !abbreviatedDot(runes, cursor, index, stop) {
				result = append(result, Range{Start: cursor, End: stop})
				cursor = stop
			}
			index = stop
			continue
		}
		index++
	}
	if cursor < end {
		result = append(result, Range{Start: cursor, End: end})
	}
	filtered := result[:0]
	for _, candidate := range result {
		if strings.TrimSpace(string(runes[candidate.Start:candidate.End])) != "" {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func isSentenceTerminator(value rune) bool {
	switch value {
	case '。', '！', '？', '!', '?':
		return true
	}
	return false
}

func isClosingQuote(value rune) bool {
	switch value {
	case '”', '’', '"', '\'', '）', ')':
		return true
	}
	return false
}

// abbreviatedDot mirrors the `_ABBREVIATION.search(...)` guard: only a plain
// "." at the end of the 9-rune window can match the `\.$` anchored pattern, so
// a dot followed by a closing quote is never treated as an abbreviation.
func abbreviatedDot(runes []rune, cursor, dotIndex, stop int) bool {
	if stop != dotIndex+1 {
		return false
	}
	windowStart := dotIndex - 8
	if windowStart < cursor {
		windowStart = cursor
	}
	window := []rune(string(runes[windowStart:stop]))
	if len(window) == 0 || window[len(window)-1] != '.' {
		return false
	}
	body := window[:len(window)-1]
	for _, abbreviation := range []string{"prof", "mrs", "etc", "mr", "ms", "dr", "sr", "jr", "vs", "e.g", "i.e"} {
		if len(body) < len(abbreviation) {
			continue
		}
		suffix := strings.ToLower(string(body[len(body)-len(abbreviation):]))
		if suffix != abbreviation {
			continue
		}
		if boundaryBefore(body, len(body)-len(abbreviation)) {
			return true
		}
	}
	if len(body) > 0 {
		last := body[len(body)-1]
		if (last >= 'A' && last <= 'Z') || (last >= 'a' && last <= 'z') {
			if boundaryBefore(body, len(body)-1) {
				return true
			}
		}
	}
	return false
}

// boundaryBefore reports whether Python's \b holds immediately before
// body[index]. Python uses Unicode word characters; this mirrors that at the
// start of the sliced window, where only a word first character forms a
// boundary.
func boundaryBefore(body []rune, index int) bool {
	if index <= 0 {
		return isUnicodeWordRune(body[0])
	}
	return isUnicodeWordRune(body[index-1]) != isUnicodeWordRune(body[index])
}

func isUnicodeWordRune(value rune) bool {
	return value == '_' || unicode.IsLetter(value) || unicode.IsDigit(value) ||
		unicode.Is(unicode.Nl, value) || unicode.Is(unicode.No, value)
}

// ParagraphRanges ports chunk_strategies.paragraph_ranges: split on blank lines
// (`\n[ \t]*\n+`) inside text[start:end].
func ParagraphRanges(text string, start, end int) []Range {
	return paragraphRangesRunes([]rune(text), start, end)
}

func paragraphRangesRunes(runes []rune, start, end int) []Range {
	if start < 0 {
		start = 0
	}
	if end > len(runes) {
		end = len(runes)
	}
	ranges := make([]Range, 0, 8)
	cursor := start
	index := start
	for index < end {
		if runes[index] != '\n' {
			index++
			continue
		}
		stop := index + 1
		for stop < end && (runes[stop] == ' ' || runes[stop] == '\t') {
			stop++
		}
		if stop >= end || runes[stop] != '\n' {
			index++
			continue
		}
		for stop < end && runes[stop] == '\n' {
			stop++
		}
		if strings.TrimSpace(string(runes[cursor:index])) != "" {
			ranges = append(ranges, Range{Start: cursor, End: index})
		}
		cursor = stop
		index = stop
	}
	if strings.TrimSpace(string(runes[cursor:end])) != "" {
		ranges = append(ranges, Range{Start: cursor, End: end})
	}
	return ranges
}

// structureKey is the comparable form of the Python tuple keys in
// structure_ranges.
type structureKey struct {
	kind      string
	section   int
	page      int
	table     int
	paragraph int
	headings  string
}

// StructureRanges ports chunk_strategies.structure_ranges: consecutive spans
// that belong to the same page (PDF), heading section (Markdown/Word), table or
// paragraph are merged into one range.
func StructureRanges(content document.Content, suffix string) []Range {
	groups := make([]Range, 0, len(content.Spans))
	var last *structureKey
	section := 0
	for _, span := range content.Spans {
		block := span.Block
		if block.Kind == document.KindHeading {
			section++
		}
		var key structureKey
		switch {
		case suffix == document.SuffixPDF:
			key = structureKey{kind: "page", page: block.Page}
		case (suffix == document.SuffixMD || suffix == document.SuffixDOCX ||
			suffix == document.SuffixDOC || suffix == document.SuffixHTML ||
			suffix == document.SuffixHTM) && len(block.Headings) > 0:
			key = structureKey{kind: "section", section: section, headings: strings.Join(block.Headings, "\x00"), table: block.Table}
		case block.Table != 0:
			key = structureKey{kind: "table", table: block.Table}
		default:
			key = structureKey{kind: "paragraph", paragraph: span.Paragraph}
		}
		if last != nil && *last == key {
			groups[len(groups)-1].End = span.End
			continue
		}
		groups = append(groups, Range{Start: span.Start, End: span.End})
		captured := key
		last = &captured
	}
	return groups
}

// SplitContent ports chunk_strategies.split_content. It returns the final chunk
// ranges for one strategy over content.Text.
func SplitContent(content document.Content, suffix string, config Config, budget *TokenBudget) ([]Range, error) {
	if strings.TrimSpace(content.Text) == "" {
		return nil, nil
	}
	runes := []rune(content.Text)
	if config.Strategy == "token" {
		return budget.windows(runes, 0, len(runes))
	}
	var initial []Range
	switch config.Strategy {
	case "sentence":
		initial = sentenceRangesRunes(runes, 0, len(runes))
	case "structure":
		initial = StructureRanges(content, suffix)
	default:
		initial = paragraphRangesRunes(runes, 0, len(runes))
	}
	type unit struct {
		start  int
		end    int
		window bool
	}
	units := make([]unit, 0, len(initial))
	for _, candidate := range initial {
		if budget.countRunes(runes, candidate.Start, candidate.End) <= budget.size {
			units = append(units, unit{start: candidate.Start, end: candidate.End})
			continue
		}
		for _, paragraph := range paragraphRangesRunes(runes, candidate.Start, candidate.End) {
			if config.Strategy != "sentence" && budget.countRunes(runes, paragraph.Start, paragraph.End) <= budget.size {
				units = append(units, unit{start: paragraph.Start, end: paragraph.End})
				continue
			}
			for _, sentence := range sentenceRangesRunes(runes, paragraph.Start, paragraph.End) {
				if budget.countRunes(runes, sentence.Start, sentence.End) <= budget.size {
					units = append(units, unit{start: sentence.Start, end: sentence.End})
					continue
				}
				windows, err := budget.windows(runes, sentence.Start, sentence.End)
				if err != nil {
					return nil, err
				}
				for _, window := range windows {
					units = append(units, unit{start: window.Start, end: window.End, window: true})
				}
			}
		}
	}
	chunks := make([]Range, 0, 16)
	var current *Range
	for _, item := range units {
		if current != nil && item.start >= current.End && budget.countRunesStripped(runes, current.Start, item.end) <= budget.size {
			current.End = item.end
			continue
		}
		if current != nil {
			chunks = append(chunks, *current)
			if !item.window && item.start >= current.End && budget.overlap > 0 {
				// Complete trailing sentences only; never split a sentence just
				// for overlap.
				for _, sentence := range sentenceRangesRunes(runes, current.Start, current.End) {
					if sentence.Start <= current.Start {
						continue
					}
					if budget.countRunesStripped(runes, sentence.Start, current.End) <= budget.overlap &&
						budget.countRunesStripped(runes, sentence.Start, item.end) <= budget.size {
						item.start = sentence.Start
						break
					}
				}
			}
		}
		current = &Range{Start: item.start, End: item.end}
	}
	if current != nil {
		chunks = append(chunks, *current)
	}
	return chunks, nil
}

// SourceMetadata ports chunk_strategies.source_metadata: the chunk source
// location payload stored in chunk metadata.
func SourceMetadata(content document.Content, start, end int) map[string]any {
	covered := make([]document.Span, 0, 8)
	for _, span := range content.Spans {
		if span.Start < end && span.End > start {
			covered = append(covered, span)
		}
	}
	locations := make([]map[string]any, 0, len(covered))
	pages := make([]int, 0, 4)
	pageSeen := make(map[int]struct{}, 4)
	headingPaths := make([][]string, 0, 4)
	headingSeen := make(map[string]struct{}, 4)
	for _, span := range covered {
		locationStart := maxInt(start, span.Start)
		locationEnd := minInt(end, span.End)
		headings := make([]string, len(span.Block.Headings))
		copy(headings, span.Block.Headings)
		locations = append(locations, map[string]any{
			"start":        locationStart,
			"end":          locationEnd,
			"paragraph":    span.Paragraph,
			"page":         optionalInt(span.Block.Page),
			"heading_path": headings,
			"kind":         span.Block.Kind,
			"table":        optionalInt(span.Block.Table),
		})
		if span.Block.Page != 0 {
			pageSeen[span.Block.Page] = struct{}{}
		}
		if len(span.Block.Headings) > 0 {
			key := strings.Join(span.Block.Headings, "\x00")
			if _, ok := headingSeen[key]; !ok {
				headingSeen[key] = struct{}{}
				headingPaths = append(headingPaths, append([]string(nil), span.Block.Headings...))
			}
		}
	}
	for page := range pageSeen {
		pages = append(pages, page)
	}
	sort.Ints(pages)
	return map[string]any{
		"start":         start,
		"end":           end,
		"pages":         pages,
		"heading_paths": headingPaths,
		"locations":     locations,
	}
}

// optionalInt renders Python None for zero sentinels (page/table "no value").
func optionalInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}
