package chunking

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Guan0923/xinghe_rag/internal/document"
)

// Chunk is a stable, storage-neutral unit produced from one source document,
// mirroring rag_mcp.chunking.DocumentChunk.
type Chunk struct {
	DocumentID string
	ChunkID    string
	Text       string
	Title      string
	Source     string
	Metadata   map[string]any
}

// ChunkOptions configures ChunkDocument. Zero ChunkSize/Overlap fall back to
// the Python defaults (700/100); to request a zero overlap use an explicit
// character Config.
type ChunkOptions struct {
	// SourceRoot makes Source a root-relative slash path instead of a base name.
	SourceRoot string
	// SourceName overrides the computed source name.
	SourceName string
	// ChunkSize is the legacy character chunk size (default 700).
	ChunkSize int
	// Overlap is the legacy character overlap (default 100).
	Overlap int
	// Config selects a strategy. nil means the legacy character splitter.
	Config *Config
	// TokenizerModel is recorded in chunk metadata (default "BAAI/bge-m3").
	TokenizerModel string
	// Counter overrides the TokenCounter (default DefaultCounter).
	Counter TokenCounter
	// Reader overrides document extraction (nil uses the in-process readers).
	Reader *document.Reader
	// Context bounds external parser work.
	Context context.Context
}

// defaultTokenizerModel mirrors chunk_document's tokenizer_model default.
const defaultTokenizerModel = "BAAI/bge-m3"

var (
	horizontalSpace = regexp.MustCompile(`[ \t]+`)
	characterBreaks = map[rune]struct{}{
		'\n': {}, '。': {}, '！': {}, '？': {}, '!': {}, '?': {}, '；': {}, ';': {},
	}
)

// DiscoverDocuments ports rag_mcp.chunking.discover_documents: a single
// supported file or every supported file below a directory in deterministic
// order.
func DiscoverDocuments(path string) ([]string, error) {
	resolved := resolvePath(path)
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("Document path does not exist: %s", resolved)
	}
	if info.IsDir() {
		found := make([]string, 0, 16)
		walkErr := filepath.WalkDir(resolved, func(candidate string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if document.IsSupportedSuffix(filepath.Ext(candidate)) {
				found = append(found, candidate)
			}
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("Document path does not exist: %s", resolved)
		}
		sort.Slice(found, func(left, right int) bool {
			return pythonCaseFold(filepath.ToSlash(found[left])) < pythonCaseFold(filepath.ToSlash(found[right]))
		})
		return found, nil
	}
	suffix := strings.ToLower(filepath.Ext(resolved))
	if !document.IsSupportedSuffix(suffix) {
		display := suffix
		if display == "" {
			display = "<none>"
		}
		return nil, fmt.Errorf("Unsupported document type: %s", display)
	}
	return []string{resolved}, nil
}

// ChunkDocument ports rag_mcp.chunking.chunk_document: extract one supported
// document and split it into stable chunks.
func ChunkDocument(path string, options ChunkOptions) ([]Chunk, error) {
	config := options.Config
	if config != nil && config.Strategy != "character" {
		return tokenChunks(path, options, *config)
	}
	chunkSize := options.ChunkSize
	if chunkSize == 0 {
		chunkSize = 700
	}
	overlap := options.Overlap
	if overlap == 0 {
		overlap = 100
	}
	if config != nil {
		chunkSize, overlap = config.ChunkSize, config.ChunkOverlap
	}
	if chunkSize < 100 {
		return nil, fmt.Errorf("chunk_size must be at least 100 characters")
	}
	if overlap < 0 || overlap >= chunkSize {
		return nil, fmt.Errorf("overlap must be non-negative and smaller than chunk_size")
	}
	characterConfig, err := NewConfig("character", chunkSize, overlap)
	if err != nil {
		return nil, err
	}
	resolved := resolvePath(path)
	content, err := readerFor(options).ReadDocumentContext(contextOrBackground(options.Context), resolved)
	if err != nil {
		return nil, err
	}
	text := normalizeText(content.OriginalText)
	if text == "" {
		return nil, nil
	}
	source := options.SourceName
	if source == "" {
		source = sourceName(resolved, options.SourceRoot)
	}
	documentID := StableID("doc", pythonCaseFold(source))
	title := titleOf(text, fileStem(resolved))
	normalized := document.Content{Text: text, Spans: relocateSpans(content.Spans, text)}
	suffix := strings.ToLower(filepath.Ext(resolved))
	tokenizerModel := tokenizerModelOf(options)
	ranges := splitCharacterRanges([]rune(text), chunkSize, overlap)
	textRunes := []rune(text)
	chunks := make([]Chunk, 0, len(ranges))
	chunkingMetadata := characterConfig.Metadata(tokenizerModel)
	for index, span := range ranges {
		piece := string(textRunes[span.Start:span.End])
		chunkID := StableID("chunk", fmt.Sprintf("%s\x00%d\x00%s", documentID, index, piece))
		metadata := map[string]any{
			"chunk_index":     index,
			"document_suffix": suffix,
			"chunking":        cloneMetadata(chunkingMetadata),
		}
		for key, value := range SourceMetadata(normalized, span.Start, span.End) {
			metadata[key] = value
		}
		chunks = append(chunks, Chunk{
			DocumentID: documentID,
			ChunkID:    chunkID,
			Text:       piece,
			Title:      title,
			Source:     source,
			Metadata:   metadata,
		})
	}
	return chunks, nil
}

// tokenChunks ports rag_mcp.chunking._token_chunks.
func tokenChunks(path string, options ChunkOptions, config Config) ([]Chunk, error) {
	validated, err := NewConfig(config.Strategy, config.ChunkSize, config.ChunkOverlap)
	if err != nil {
		return nil, err
	}
	resolved := resolvePath(path)
	content, err := readerFor(options).ReadDocumentContext(contextOrBackground(options.Context), resolved)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(content.Text) == "" {
		return nil, nil
	}
	budget, err := NewTokenBudget(counterFor(options), validated)
	if err != nil {
		return nil, err
	}
	source := options.SourceName
	if source == "" {
		source = sourceName(resolved, options.SourceRoot)
	}
	documentID := StableID("doc", pythonCaseFold(source))
	title := titleOf(content.Text, fileStem(resolved))
	suffix := strings.ToLower(filepath.Ext(resolved))
	tokenizerModel := tokenizerModelOf(options)
	ranges, err := SplitContent(content, suffix, validated, budget)
	if err != nil {
		return nil, err
	}
	runes := []rune(content.Text)
	chunks := make([]Chunk, 0, len(ranges))
	chunkingMetadata := validated.Metadata(tokenizerModel)
	for _, span := range ranges {
		start, end := span.Start, span.End
		for start < end && unicode.IsSpace(runes[start]) {
			start++
		}
		for end > start && unicode.IsSpace(runes[end-1]) {
			end--
		}
		if start >= end {
			continue
		}
		piece := string(runes[start:end])
		index := len(chunks)
		metadata := map[string]any{
			"chunk_index":     index,
			"document_suffix": suffix,
			"chunking":        cloneMetadata(chunkingMetadata),
			"token_count":     budget.Count(piece),
		}
		for key, value := range SourceMetadata(content, start, end) {
			metadata[key] = value
		}
		chunks = append(chunks, Chunk{
			DocumentID: documentID,
			ChunkID:    StableID("chunk", fmt.Sprintf("%s\x00%d\x00%s", documentID, index, piece)),
			Text:       piece,
			Title:      title,
			Source:     source,
			Metadata:   metadata,
		})
	}
	return chunks, nil
}

// splitCharacterRanges ports rag_mcp.chunking._split_ranges.
func splitCharacterRanges(runes []rune, chunkSize, overlap int) []Range {
	chunks := make([]Range, 0, 8)
	start := 0
	for start < len(runes) {
		targetEnd := minInt(len(runes), start+chunkSize)
		end := targetEnd
		if targetEnd < len(runes) {
			minimumEnd := start + maxInt(1, chunkSize/2)
			for index := targetEnd - 1; index >= minimumEnd; index-- {
				if _, ok := characterBreaks[runes[index]]; ok {
					end = index + 1
					break
				}
			}
		}
		left := start
		for left < end && unicode.IsSpace(runes[left]) {
			left++
		}
		right := end
		for right > left && unicode.IsSpace(runes[right-1]) {
			right--
		}
		if left < right {
			chunks = append(chunks, Range{Start: left, End: right})
		}
		if end >= len(runes) {
			break
		}
		nextStart := maxInt(start+1, end-overlap)
		start = nextStart
	}
	return chunks
}

// relocateSpans ports the cursor-based span relocation at the end of
// chunk_document's character path.
func relocateSpans(spans []document.Span, text string) []document.Span {
	relocated := make([]document.Span, 0, len(spans))
	byteCursor := 0
	runeCursor := 0
	for _, span := range spans {
		normalized := normalizeText(span.Block.Text)
		if normalized == "" {
			continue
		}
		position := strings.Index(text[byteCursor:], normalized)
		if position < 0 {
			continue
		}
		bytePosition := byteCursor + position
		runePosition := runeCursor + utf8.RuneCountInString(text[byteCursor:bytePosition])
		length := utf8.RuneCountInString(normalized)
		relocated = append(relocated, document.Span{
			Start:     runePosition,
			End:       runePosition + length,
			Block:     span.Block,
			Paragraph: span.Paragraph,
		})
		byteCursor = bytePosition + len(normalized)
		runeCursor = runePosition + length
	}
	return relocated
}

// normalizeText ports rag_mcp.chunking._normalize_text.
func normalizeText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		line = horizontalSpace.ReplaceAllString(line, " ")
		lines[index] = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// sourceName ports rag_mcp.chunking._source_name.
func sourceName(resolved, root string) string {
	if root != "" {
		rootPath := resolvePath(root)
		if relative, err := filepath.Rel(rootPath, resolved); err == nil && relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
			return filepath.ToSlash(relative)
		}
	}
	return filepath.Base(resolved)
}

// titleOf ports rag_mcp.chunking._title.
func titleOf(text, fallback string) string {
	for _, line := range pythonSplitLines(text) {
		candidate := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if candidate != "" {
			return truncateRunes(candidate, 200)
		}
	}
	return truncateRunes(fallback, 200)
}

// StableID ports rag_mcp.chunking._stable_id.
func StableID(prefix, value string) string {
	digest := sha256.Sum256([]byte(value))
	return prefix + "-" + hex.EncodeToString(digest[:])[:32]
}

// pythonSplitLines splits like Python str.splitlines (the separators used by
// _title).
func pythonSplitLines(text string) []string {
	lines := make([]string, 0, 8)
	var builder strings.Builder
	for _, value := range text {
		switch value {
		case '\n', '\r', '\v', '\f', '\u2028', '\u2029', '\u0085':
			lines = append(lines, builder.String())
			builder.Reset()
		default:
			builder.WriteRune(value)
		}
	}
	lines = append(lines, builder.String())
	return lines
}

// pythonCaseFold approximates Python str.casefold with strings.ToLower plus the
// most common full-fold mappings. Parity note: exotic foldings (for example
// grapheme ligatures) are not covered, so document ids for such source names
// can differ from Python.
func pythonCaseFold(text string) string {
	folded := strings.ToLower(text)
	return strings.NewReplacer("ß", "ss", "ς", "σ", "ﬀ", "ff", "ﬁ", "fi", "ﬂ", "fl").Replace(folded)
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

func fileStem(resolved string) string {
	base := filepath.Base(resolved)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func readerFor(options ChunkOptions) *document.Reader {
	if options.Reader != nil {
		return options.Reader
	}
	return &document.Reader{}
}

func counterFor(options ChunkOptions) TokenCounter {
	if options.Counter != nil {
		return options.Counter
	}
	return DefaultCounter
}

func tokenizerModelOf(options ChunkOptions) string {
	if options.TokenizerModel != "" {
		return options.TokenizerModel
	}
	return defaultTokenizerModel
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx != nil {
		return ctx
	}
	return context.Background()
}

// resolvePath mirrors document_readers.read_document's expanduser().resolve()
// for the chunker (the document package keeps its own unexported helper).
func resolvePath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				path = home
			} else {
				path = filepath.Join(home, path[2:])
			}
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}

// cloneMetadata copies a metadata map so chunks do not share the nested
// "chunking" object.
func cloneMetadata(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
