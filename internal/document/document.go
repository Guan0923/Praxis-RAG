// Package document ports supported-document text extraction from
// rag_mcp.document_readers and the OOXML reader from rag_mcp.word_reader.
//
// Markdown/TXT are read in process; DOCX is parsed from the OOXML zip package
// in process; PDF text extraction is delegated to the external Docling parser
// process through the protocol defined in pdf.go (Docling itself is never
// reimplemented here). Legacy .doc files additionally require an external
// LibreOffice based converter, see docconvert.go.
//
// Character offsets in Span are code-point (rune) offsets, matching Python's
// len()/index semantics for str, so chunking metadata stays comparable with
// the Python implementation.
package document

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Supported document suffixes, mirroring document_readers.SUPPORTED_SUFFIXES.
const (
	SuffixDOC  = ".doc"
	SuffixDOCX = ".docx"
	SuffixMD   = ".md"
	SuffixPDF  = ".pdf"
	SuffixTXT  = ".txt"
)

// Block kinds. "table_row" and "picture" are produced by the Word/PDF adapters
// the same way document_readers transforms parser manifests.
const (
	KindParagraph = "paragraph"
	KindHeading   = "heading"
	KindCaption   = "caption"
	KindNote      = "note"
	KindCode      = "code"
	KindTableRow  = "table_row"
	KindPicture   = "picture"
)

// Extraction limits mirror the module constants in
// rag_mcp.document_readers. They are variables (not constants) so tests can
// tighten them, mirroring the way tests/test_document_readers.py monkeypatches
// the Python module constants.
var (
	// MaxPDFPages is the maximum number of PDF pages.
	MaxPDFPages = 1_000
	// MaxDOCXMembers is the maximum number of zip entries in a Word package.
	MaxDOCXMembers = 10_000
	// MaxDOCXUncompressedBytes is the maximum combined uncompressed zip size.
	MaxDOCXUncompressedBytes int64 = 50 * 1024 * 1024
	// MaxExtractedCharacters is the maximum extracted text size in runes.
	MaxExtractedCharacters = 5_000_000
)

var supportedSuffixes = map[string]struct{}{
	SuffixDOC: {}, SuffixDOCX: {}, SuffixMD: {}, SuffixPDF: {}, SuffixTXT: {},
}

// IsSupportedSuffix reports whether suffix (with leading dot, case-insensitive)
// is a supported knowledge document type.
func IsSupportedSuffix(suffix string) bool {
	_, ok := supportedSuffixes[strings.ToLower(suffix)]
	return ok
}

// ReadError is raised when an uploaded document cannot be safely converted to
// text. It mirrors rag_mcp.document_readers.DocumentReadError; messages match
// the Python implementation (Chinese where Python exposes Chinese).
type ReadError struct{ Message string }

func (e *ReadError) Error() string { return e.Message }

// AsReadError extracts a ReadError from err, if any.
func AsReadError(err error) (*ReadError, bool) {
	typed, ok := err.(*ReadError)
	return typed, ok
}

func readErrf(format string, args ...any) *ReadError {
	return &ReadError{Message: fmt.Sprintf(format, args...)}
}

// Block is one ordered source block: a paragraph, heading, code fence, table
// row or picture placeholder.
type Block struct {
	Text     string
	Kind     string
	Page     int // 0 means "no page" (Python None); PDF pages are 1-based.
	Headings []string
	Table    int // 0 means "no table" (Python None); Word tables are 1-based.
}

// Span is one block placed inside Content.Text.
type Span struct {
	Start     int
	End       int
	Block     Block
	Paragraph int
}

// Content is extracted document text plus the block/span provenance needed for
// structure-aware chunking and source metadata.
type Content struct {
	// Text is the "\n\n" joined text of all non-blank blocks.
	Text string
	// Spans locate every non-blank block in Text.
	Spans []Span
	// OriginalText is the raw extracted text before block joining: the UTF-8
	// file text for .md/.txt, the Word block join for .docx, or the joined PDF
	// page text.
	OriginalText string
}

// Reader extracts document content. The optional PDF and DOC dependencies keep
// external parsers injectable; a nil dependency produces the same kind of safe
// read error that a missing Python parser would in practice.
type Reader struct {
	// PDF extracts per-page plain text for read_document/chunk_document
	// parity. When nil, PDF documents report that no PDF parser is configured.
	PDF PDFPages
	// DOC converts legacy .doc files to .docx through an external converter.
	// When nil, .doc files report the LibreOffice requirement.
	DOC DOCConverter
}

// ReadDocument extracts block-structured content from one supported document.
func ReadDocument(path string) (Content, error) {
	return (&Reader{}).ReadDocument(path)
}

// ReadDocument extracts block-structured content from one supported document.
func (r *Reader) ReadDocument(path string) (Content, error) {
	return r.ReadDocumentContext(context.Background(), path)
}

// ReadDocumentContext is ReadDocument with a caller supplied context, used for
// external parser processes.
func (r *Reader) ReadDocumentContext(ctx context.Context, path string) (Content, error) {
	resolved := resolvePath(path)
	name := filepath.Base(resolved)
	suffix := strings.ToLower(filepath.Ext(resolved))
	if !IsSupportedSuffix(suffix) {
		return Content{}, unsupportedSuffixError(suffix)
	}
	switch suffix {
	case SuffixPDF:
		pages, err := r.pdfPages(ctx, resolved, name)
		if err != nil {
			return Content{}, err
		}
		blocks := make([]Block, 0, len(pages))
		for _, page := range pages {
			for _, part := range splitParagraphs(page.Text) {
				blocks = append(blocks, Block{Text: part, Kind: KindParagraph, Page: page.Number})
			}
		}
		original := pagesText(pages)
		return assembleBlocks(blocks, original, name)
	case SuffixDOC, SuffixDOCX:
		blocks, original, err := r.docxDocument(ctx, resolved, name, suffix)
		if err != nil {
			return Content{}, err
		}
		return assembleBlocks(blocks, original, name)
	case SuffixMD:
		original, err := readUTF8(resolved, name)
		if err != nil {
			return Content{}, err
		}
		return assembleBlocks(markdownBlocks(original), original, name)
	default: // .txt
		original, err := readUTF8(resolved, name)
		if err != nil {
			return Content{}, err
		}
		blocks := make([]Block, 0, 16)
		for _, part := range splitParagraphs(original) {
			blocks = append(blocks, Block{Text: part, Kind: KindParagraph})
		}
		return assembleBlocks(blocks, original, name)
	}
}

// ReadText extracts bounded plain text from one supported document. It mirrors
// rag_mcp.document_readers.read_document_text: .md/.txt return the raw UTF-8
// text, PDF returns the joined page text and Word returns the joined block text.
func ReadText(path string) (string, error) {
	return (&Reader{}).ReadText(path)
}

// ReadText extracts bounded plain text from one supported document.
func (r *Reader) ReadText(path string) (string, error) {
	return r.ReadTextContext(context.Background(), path)
}

// ReadTextContext is ReadText with a caller supplied context.
func (r *Reader) ReadTextContext(ctx context.Context, path string) (string, error) {
	resolved := resolvePath(path)
	name := filepath.Base(resolved)
	suffix := strings.ToLower(filepath.Ext(resolved))
	if !IsSupportedSuffix(suffix) {
		return "", unsupportedSuffixError(suffix)
	}
	switch suffix {
	case SuffixMD, SuffixTXT:
		return readUTF8(resolved, name)
	case SuffixPDF:
		pages, err := r.pdfPages(ctx, resolved, name)
		if err != nil {
			return "", err
		}
		return pagesText(pages), nil
	default: // .doc, .docx
		_, original, err := r.docxDocument(ctx, resolved, name, suffix)
		if err != nil {
			return "", err
		}
		return original, nil
	}
}

func unsupportedSuffixError(suffix string) *ReadError {
	display := suffix
	if display == "" {
		display = "<none>"
	}
	return readErrf("暂不支持该文件类型：%s", display)
}

// assembleBlocks joins non-blank blocks with "\n\n" and records rune spans,
// mirroring document_readers.read_document.
func assembleBlocks(blocks []Block, original, name string) (Content, error) {
	spans := make([]Span, 0, len(blocks))
	parts := make([]string, 0, len(blocks))
	offset := 0
	for _, block := range blocks {
		if strings.TrimSpace(block.Text) == "" {
			continue
		}
		if len(parts) > 0 {
			offset += 2
		}
		length := utf8.RuneCountInString(block.Text)
		spans = append(spans, Span{
			Start:     offset,
			End:       offset + length,
			Block:     block,
			Paragraph: len(spans) + 1,
		})
		parts = append(parts, block.Text)
		offset += length
	}
	text, err := boundedText(strings.Join(parts, "\n\n"), name)
	if err != nil {
		return Content{}, err
	}
	return Content{Text: text, Spans: spans, OriginalText: original}, nil
}

// resolvePath expands a leading "~" and makes the path absolute. Python uses
// Path.resolve(); absolute resolution is sufficient for stable error names and
// suffix handling without failing on paths that do not exist yet.
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

func normalizeNewlines(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

var paragraphBreak = regexp.MustCompile(`\n[ \t]*\n+`)

// splitParagraphs mirrors document_readers._paragraphs: universal newlines,
// split on blank lines and strip each part, dropping empty parts.
func splitParagraphs(text string) []string {
	text = normalizeNewlines(text)
	parts := paragraphBreak.Split(text, -1)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// readUTF8 mirrors document_readers._read_utf8: UTF-8 (with optional BOM),
// universal newlines and the extracted text size cap.
func readUTF8(path, name string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", readErrf("知识文件无法读取：%s", name)
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	if !utf8.ValidString(text) {
		return "", readErrf("文本文件必须使用 UTF-8 编码：%s", name)
	}
	return boundedText(normalizeNewlines(text), name)
}

// boundedText mirrors document_readers._bounded_text.
func boundedText(text, name string) (string, error) {
	if utf8.RuneCountInString(text) > MaxExtractedCharacters {
		return "", readErrf("文档提取文本过大：%s", name)
	}
	return text, nil
}

// pagesText joins page texts with a blank line, mirroring
// document_readers._read_pdf.
func pagesText(pages []Page) string {
	parts := make([]string, len(pages))
	for index, page := range pages {
		parts[index] = page.Text
	}
	return strings.Join(parts, "\n\n")
}
