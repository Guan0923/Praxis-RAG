package document

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// NativePDFParser parses PDF text in-process with a pure-Go PDF reader. It is
// the default PDF engine; a remote Docling service can be selected instead
// through DoclingServiceParser. It does not start processes and does not
// require Python or Docling.
// The output is the manifest contract defined in pdf.go, restricted to ordered
// text blocks. Layout models, table structure, OCR and formula/picture assets
// are not produced by this engine.
type NativePDFParser struct{}

// PDFNativeEngine tags manifests produced by the in-process parser.
// Parser["engine"] records the implementation.
const PDFNativeEngine = "go-native"

// pdfLine is one visual line of a page: a baseline, the dominant font size and
// the horizontal span of its text runs.
type pdfLine struct {
	y     float64
	size  float64
	x0    float64
	x1    float64
	items []pdfTextItem
	text  string
}

// pdfTextItem is one decoded text run with its position on the page.
type pdfTextItem struct {
	x, y, w, size float64
	s             string
}

// ParseInto extracts ordered text blocks from the PDF and writes the manifest
// into staging.
func (p *NativePDFParser) ParseInto(ctx context.Context, source, staging string) (PDFManifest, error) {
	started := time.Now()
	name := filepath.Base(source)
	if err := ValidatePDFFile(source, name); err != nil {
		return PDFManifest{}, &PDFParseError{Message: err.Error()}
	}
	file, reader, err := openNativePDF(source)
	if err != nil {
		return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF 解析失败：%s", name)}
	}
	defer file.Close()

	pageCount := reader.NumPage()
	if pageCount > MaxPDFPages {
		return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF 页数不能超过 %d 页：%s", MaxPDFPages, name)}
	}
	pages := make([][]pdfLine, 0, pageCount)
	for number := 1; number <= pageCount; number++ {
		if err := ctx.Err(); err != nil {
			return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF 解析已取消：%s", name)}
		}
		pages = append(pages, extractNativePage(reader.Page(number)))
	}
	repeated := repeatedLineTexts(pages, pageCount)

	blocks := make([]PDFBlock, 0, pageCount*4)
	warnings := make([]PDFWarning, 0, 4)
	characters := 0
	for number, lines := range pages {
		kept := make([]pdfLine, 0, len(lines))
		for _, line := range lines {
			if dropNativeLine(line, pageCount, repeated) {
				continue
			}
			kept = append(kept, line)
		}
		if len(kept) == 0 {
			warnings = append(warnings, PDFWarning{
				Page:    number + 1,
				Kind:    "no_text",
				Count:   1,
				Message: "页面没有可提取的文本（扫描件需要 OCR）",
			})
			continue
		}
		for _, paragraph := range nativeParagraphs(kept) {
			order := len(blocks)
			blocks = append(blocks, PDFBlock{
				Ref:  fmt.Sprintf("go-p%d-b%d", number+1, order),
				Kind: "text",
				Page: number + 1,
				// Order is document wide so consumers can sort blocks without
				// re-reading the page number.
				Order: order,
				Text:  paragraph.text,
			})
			characters += utf8.RuneCountInString(paragraph.text)
		}
	}
	if len(blocks) == 0 {
		return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf(
			"PDF 没有可索引的内容：%s（未提取到文本，扫描件需要 OCR 后再导入）", name)}
	}
	if characters > MaxExtractedCharacters {
		return PDFManifest{}, &PDFParseError{Message: "Parsed PDF exceeds the extracted text limit"}
	}
	manifest := PDFManifest{
		Version:  PDFParserVersion,
		Blocks:   blocks,
		Assets:   []PDFAsset{},
		Warnings: warnings,
		Parser: map[string]any{
			"engine":  PDFNativeEngine,
			"library": "github.com/ledongthuc/pdf",
		},
		Metrics: map[string]any{
			"pages":      pageCount,
			"blocks":     len(blocks),
			"characters": characters,
			"elapsed_ms": time.Since(started).Milliseconds(),
		},
	}
	if err := writeNativePDFManifest(staging, manifest); err != nil {
		return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF 解析失败：%s", name)}
	}
	return manifest, nil
}

// Pages extracts bounded per-page plain text for the Reader/chunk_document
// paths, mirroring the external parser's page grouping.
func (p *NativePDFParser) Pages(ctx context.Context, source string) ([]Page, error) {
	name := filepath.Base(source)
	if err := ValidatePDFFile(source, name); err != nil {
		return nil, err
	}
	file, reader, err := openNativePDF(source)
	if err != nil {
		return nil, readErrf("PDF 解析失败：%s", name)
	}
	defer file.Close()
	pageCount := reader.NumPage()
	if pageCount > MaxPDFPages {
		return nil, readErrf("PDF 页数不能超过 %d 页：%s", MaxPDFPages, name)
	}
	result := make([]Page, 0, pageCount)
	for number := 1; number <= pageCount; number++ {
		if err := ctx.Err(); err != nil {
			return nil, readErrf("PDF 解析已取消：%s", name)
		}
		lines := extractNativePage(reader.Page(number))
		paragraphs := nativeParagraphs(lines)
		parts := make([]string, len(paragraphs))
		for index, paragraph := range paragraphs {
			parts[index] = paragraph.text
		}
		result = append(result, Page{Number: number, Text: strings.Join(parts, "\n\n")})
	}
	return result, nil
}

func openNativePDF(source string) (*os.File, *pdf.Reader, error) {
	file, reader, err := pdf.Open(source)
	if err != nil {
		return nil, nil, err
	}
	return file, reader, nil
}

func writeNativePDFManifest(staging string, manifest PDFManifest) error {
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("PDF artifact directory is not writable: %v", err)
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(staging, "manifest.json"), encoded, 0o644)
}

// extractNativePage reads one page's content runs, groups them into visual
// lines and renders each line with inferred word spaces.
func extractNativePage(page pdf.Page) []pdfLine {
	content := page.Content()
	items := make([]pdfTextItem, 0, len(content.Text))
	for _, text := range content.Text {
		if text.S == "" {
			continue
		}
		size := math.Abs(text.FontSize)
		// Rotated text (for example margin watermarks) has a near-zero
		// horizontal scale in the text matrix; it is not body text.
		if size < 0.75 {
			continue
		}
		sanitized := sanitizeNativePDFText(text.S)
		if sanitized == "" {
			continue
		}
		items = append(items, pdfTextItem{
			x:    text.X,
			y:    text.Y,
			w:    text.W,
			size: size,
			s:    sanitized,
		})
	}
	if len(items) == 0 {
		return nil
	}
	// Keep the content-stream order: many CJK PDFs position glyphs with
	// zero-width runs, so only the stream order is trustworthy for them. Lines
	// are grouped by baseline; per-line X sorting happens below only when the
	// reader reports real glyph widths.
	lines := make([]pdfLine, 0, len(items)/20+1)
	for _, item := range items {
		placed := false
		for index := range lines {
			line := &lines[index]
			tolerance := 0.45 * math.Max(item.size, line.size)
			if tolerance < 1 {
				tolerance = 1
			}
			if math.Abs(line.y-item.y) <= tolerance {
				line.items = append(line.items, item)
				if item.size > line.size {
					line.size = item.size
				}
				placed = true
				break
			}
		}
		if !placed {
			lines = append(lines, pdfLine{y: item.y, size: item.size, items: []pdfTextItem{item}})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].y > lines[j].y })
	result := make([]pdfLine, 0, len(lines))
	for _, line := range lines {
		if nativePositionsReliable(line.items) {
			sort.SliceStable(line.items, func(i, j int) bool { return line.items[i].x < line.items[j].x })
		}
		text, x0, x1 := nativeLineText(line)
		if text == "" {
			continue
		}
		line.text, line.x0, line.x1 = text, x0, x1
		result = append(result, line)
	}
	return result
}

// nativePositionsReliable reports whether the reader exposed glyph widths for
// every run of this line. CJK CID fonts frequently come back with zero widths
// and near-identical X coordinates; their content-stream order is the reading
// order, while sorting by X would scramble them. Mixed lines keep the stream
// order too, because the zero-width runs carry no usable position.
func nativePositionsReliable(items []pdfTextItem) bool {
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if item.w <= 0.5 {
			return false
		}
	}
	return true
}

// nativeLineText joins the runs of one line, inserting a space when the gap
// between two runs is wide enough to be a word boundary. Runs are decoded per
// rune by the reader, so the previous run's position and width give the
// expected pen position exactly.
func nativeLineText(line pdfLine) (string, float64, float64) {
	var builder strings.Builder
	var previous pdfTextItem
	first := true
	for _, item := range line.items {
		if item.s == "" {
			continue
		}
		if !first && nativePositioned(previous) && nativePositioned(item) {
			if item.s == previous.s && math.Abs(item.x-previous.x) < 0.3 {
				// Doubled glyph used to fake bold text; keep one copy.
				continue
			}
			if nativeNeedsSpace(previous, item, line.size) {
				builder.WriteByte(' ')
			}
		}
		builder.WriteString(item.s)
		previous = item
		first = false
	}
	text := strings.Join(strings.Fields(builder.String()), " ")
	text = nativeDotLeader.ReplaceAllString(text, " ")
	text = strings.TrimSpace(strings.Join(strings.Fields(text), " "))
	if text == "" {
		return "", 0, 0
	}
	return text, line.items[0].x, line.items[len(line.items)-1].x + line.items[len(line.items)-1].w
}

// nativeDotLeader matches table-of-contents dot leaders and long separator
// runs, which carry no meaning for retrieval.
var nativeDotLeader = regexp.MustCompile(`(?:[.·•]\s*){4,}|(?:…\s*){2,}|(?:\s-\s*){4,}`)

// nativePositioned reports whether the reader supplied a real glyph advance
// for this run, which is what makes gap-based word spacing meaningful.
func nativePositioned(item pdfTextItem) bool {
	return item.w > 0.5
}

// nativeClosingPunctuation and nativeOpeningPunctuation suppress spaces that a
// gap heuristic would otherwise insert around punctuation.
const (
	nativeClosingPunctuation = ".,;:!?%)]}>”’\"，。；：！？）】》」』、"
	nativeOpeningPunctuation = "([{<“‘\"（【《「『"
)

func nativeNeedsSpace(previous, current pdfTextItem, size float64) bool {
	prevLast := lastRuneOf(previous.s)
	nextFirst := firstRuneOf(current.s)
	if prevLast == 0 || nextFirst == 0 {
		return false
	}
	if unicode.IsSpace(prevLast) || unicode.IsSpace(nextFirst) {
		return false
	}
	if strings.ContainsRune(nativeClosingPunctuation, nextFirst) ||
		strings.ContainsRune(nativeOpeningPunctuation, prevLast) {
		return false
	}
	gap := current.x - (previous.x + previous.w)
	if gap <= 0 {
		return false
	}
	if isCJK(prevLast) && isCJK(nextFirst) {
		return gap > 1.6*math.Max(size, 1)
	}
	return gap > 0.19*math.Max(size, 1)
}

// nativeParagraph groups consecutive lines into paragraphs. Paragraph breaks
// are inferred from the vertical pitch, font size changes and headings.
type nativeParagraphBlock struct {
	text string
}

func nativeParagraphs(lines []pdfLine) []nativeParagraphBlock {
	if len(lines) == 0 {
		return nil
	}
	bodySize := dominantLineSize(lines)
	pitch := medianLinePitch(lines)
	result := make([]nativeParagraphBlock, 0, len(lines)/4+1)
	current := ""
	flush := func() {
		if current == "" {
			return
		}
		result = append(result, nativeParagraphBlock{text: current})
		current = ""
	}
	previousHeading := false
	for index, line := range lines {
		heading := bodySize > 0 && line.size >= bodySize*1.12
		flushOnGap := false
		if index > 0 {
			gap := lines[index-1].y - line.y
			sizeChange := math.Abs(line.size-lines[index-1].size) > 0.22*math.Max(line.size, lines[index-1].size)
			flushOnGap = (pitch > 0 && gap > 1.45*pitch) || sizeChange || previousHeading
		}
		if heading {
			if flushOnGap || current != "" {
				flush()
			}
			result = append(result, nativeParagraphBlock{text: line.text})
			previousHeading = true
			continue
		}
		if flushOnGap {
			flush()
		}
		current = joinParagraphLines(current, line.text)
		previousHeading = false
	}
	flush()
	return result
}

// dominantLineSize returns the most common line size weighted by line count.
func dominantLineSize(lines []pdfLine) float64 {
	counts := map[float64]int{}
	for _, line := range lines {
		key := math.Round(line.size*2) / 2
		counts[key]++
	}
	best, bestCount := 0.0, -1
	keys := make([]float64, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Float64s(keys)
	for _, key := range keys {
		if counts[key] > bestCount {
			best, bestCount = key, counts[key]
		}
	}
	return best
}

// medianLinePitch returns the median vertical distance between adjacent lines.
func medianLinePitch(lines []pdfLine) float64 {
	if len(lines) < 2 {
		return 0
	}
	gaps := make([]float64, 0, len(lines)-1)
	for index := 1; index < len(lines); index++ {
		gap := lines[index-1].y - lines[index].y
		maximum := 2.5 * math.Max(lines[index].size, lines[index-1].size)
		if gap > 0.5 && (maximum <= 0 || gap <= maximum) {
			gaps = append(gaps, gap)
		}
	}
	if len(gaps) == 0 {
		return 0
	}
	sort.Float64s(gaps)
	return gaps[len(gaps)/2]
}

// repeatedLineTexts counts normalized line texts across pages so running
// headers and footers can be removed.
func repeatedLineTexts(pages [][]pdfLine, pageCount int) map[string]int {
	counts := map[string]int{}
	seen := map[string]bool{}
	for _, lines := range pages {
		seen = make(map[string]bool, len(lines))
		for _, line := range lines {
			key := normalizeRepeatedLine(line.text)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			counts[key]++
		}
	}
	threshold := pageCount / 3
	if threshold < 3 {
		threshold = 3
	}
	result := make(map[string]int, len(counts))
	for key, count := range counts {
		if count >= threshold {
			result[key] = count
		}
	}
	return result
}

// normalizeRepeatedLine lowercases text and folds digits so page numbers and
// dated headers compare equal across pages.
func normalizeRepeatedLine(text string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(strings.TrimSpace(text)) {
		if unicode.IsDigit(char) {
			builder.WriteByte('#')
			continue
		}
		if unicode.IsSpace(char) {
			builder.WriteByte(' ')
			continue
		}
		builder.WriteRune(char)
	}
	return strings.Join(strings.Fields(builder.String()), " ")
}

// nativePageNumberLine matches standalone page numbers such as "3", "- 12 -",
// "Page 4 of 20" or "4 / 20". Text that merely starts with a number ("1
// Introduction") must not match.
var nativePageNumberLine = regexp.MustCompile(`(?i)^(?:page\s*|p\.?\s*)?[-–—]?\s*\d{1,4}\s*(?:[-–—]\s*|\s*/\s*\d{1,4}|\s+of\s+\d{1,4})?$`)

// dropNativeLine reports whether a line is a page number or a running
// header/footer, which pollute retrieval text but carry no document meaning.
func dropNativeLine(line pdfLine, pageCount int, repeated map[string]int) bool {
	if line.text == "" {
		return true
	}
	key := normalizeRepeatedLine(line.text)
	if repeated[key] > 0 && utf8.RuneCountInString(line.text) <= 120 {
		return true
	}
	return pageCount > 1 && nativePageNumberLine.MatchString(strings.TrimSpace(line.text))
}

// sanitizeNativePDFText normalizes ligatures, non-breaking spaces, soft
// hyphens and control characters that PDF fonts commonly emit.
func sanitizeNativePDFText(text string) string {
	var builder strings.Builder
	for _, char := range text {
		switch char {
		case '\uFB00':
			builder.WriteString("ff")
		case '\uFB01':
			builder.WriteString("fi")
		case '\uFB02':
			builder.WriteString("fl")
		case '\uFB03':
			builder.WriteString("ffi")
		case '\uFB04':
			builder.WriteString("ffl")
		case '\uFB05', '\uFB06':
			builder.WriteString("st")
		case '\u00AD', '\u200B', '\u200C', '\u200D', '\uFEFF', '\uFFFD':
			continue
		case '\u00A0', '\t', '\r', '\n':
			builder.WriteByte(' ')
		default:
			if char < 0x20 || char == 0x7F {
				continue
			}
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

func lastRuneOf(text string) rune {
	if text == "" {
		return 0
	}
	char, _ := utf8.DecodeLastRuneInString(text)
	return char
}

func firstRuneOf(text string) rune {
	for _, char := range text {
		return char
	}
	return 0
}

func isCJK(char rune) bool {
	return unicode.Is(unicode.Han, char) || unicode.Is(unicode.Hangul, char) ||
		unicode.Is(unicode.Hiragana, char) || unicode.Is(unicode.Katakana, char) ||
		unicode.Is(unicode.Bopomofo, char)
}

// joinParagraphLines concatenates two paragraph lines, removing a hyphen when
// the word was split at the line break.
func joinParagraphLines(previous, next string) string {
	if previous == "" {
		return next
	}
	if next == "" {
		return previous
	}
	last := lastRuneOf(previous)
	if last == '-' {
		first := firstRuneOf(next)
		if unicode.IsLower(first) {
			return strings.TrimSuffix(previous, "-") + next
		}
	}
	if isCJK(last) || isCJK(firstRuneOf(next)) {
		return previous + next
	}
	return previous + " " + next
}
