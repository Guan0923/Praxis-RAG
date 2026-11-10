package document

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildNativeTextPDF writes a minimal one-page PDF containing the given lines.
func buildNativeTextPDF(t *testing.T, lines []string) []byte {
	t.Helper()
	var content strings.Builder
	content.WriteString("BT\n/F1 12 Tf\n72 720 Td\n")
	for index, line := range lines {
		if index > 0 {
			content.WriteString("0 -18 Td\n")
		}
		content.WriteString("(" + escapePDFString(line) + ") Tj\n")
	}
	content.WriteString("ET\n")
	stream := content.String()

	var buffer bytes.Buffer
	offsets := make([]int, 6)
	writeObject := func(number int, body string) {
		offsets[number] = buffer.Len()
		fmt.Fprintf(&buffer, "%d 0 obj\n%s\nendobj\n", number, body)
	}
	buffer.WriteString("%PDF-1.4\n")
	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObject(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObject(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
		"/Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>")
	writeObject(4, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream))
	writeObject(5, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	xrefOffset := buffer.Len()
	buffer.WriteString("xref\n0 6\n0000000000 65535 f \n")
	for number := 1; number <= 5; number++ {
		fmt.Fprintf(&buffer, "%010d 00000 n \n", offsets[number])
	}
	fmt.Fprintf(&buffer, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xrefOffset)
	return buffer.Bytes()
}

func escapePDFString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "(", `\(`)
	return strings.ReplaceAll(value, ")", `\)`)
}

func writeNativePDF(t *testing.T, name string, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, buildNativeTextPDF(t, lines), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func TestNativePDFParserParsesText(t *testing.T) {
	path := writeNativePDF(t, "sample.pdf", []string{
		"Hello native PDF parser",
		"Second paragraph line",
	})
	staging := t.TempDir()
	parser := &NativePDFParser{}
	manifest, err := parser.ParseInto(context.Background(), path, staging)
	if err != nil {
		t.Fatalf("ParseInto() error = %v", err)
	}
	if manifest.Version != PDFParserVersion {
		t.Errorf("version = %q, want %q", manifest.Version, PDFParserVersion)
	}
	if manifest.Parser["engine"] != PDFNativeEngine {
		t.Errorf("engine = %v, want %q", manifest.Parser["engine"], PDFNativeEngine)
	}
	if len(manifest.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1: %+v", len(manifest.Blocks), manifest.Blocks)
	}
	block := manifest.Blocks[0]
	if block.Kind != "text" || block.Page != 1 || block.Order != 0 || block.Ref == "" {
		t.Errorf("block = %+v", block)
	}
	if !strings.Contains(block.Text, "Hello native PDF parser") ||
		!strings.Contains(block.Text, "Second paragraph line") {
		t.Errorf("block text = %q", block.Text)
	}
	raw, err := os.ReadFile(filepath.Join(staging, "manifest.json"))
	if err != nil {
		t.Fatalf("manifest.json error = %v", err)
	}
	var written PDFManifest
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("manifest.json is not valid JSON: %v", err)
	}
	if written.Version != PDFParserVersion || len(written.Blocks) != 1 {
		t.Errorf("written manifest = %+v", written)
	}
	pages, err := parser.Pages(context.Background(), path)
	if err != nil {
		t.Fatalf("Pages() error = %v", err)
	}
	if len(pages) != 1 || !strings.Contains(pages[0].Text, "Hello native PDF parser") {
		t.Errorf("pages = %+v", pages)
	}
}

func TestNativePDFParserRejectsBrokenInput(t *testing.T) {
	parser := &NativePDFParser{}
	garbage := filepath.Join(t.TempDir(), "broken.pdf")
	if err := os.WriteFile(garbage, []byte("this is not a pdf"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, err := parser.ParseInto(context.Background(), garbage, t.TempDir())
	var parseErr *PDFParseError
	if !errors.As(err, &parseErr) || !strings.Contains(parseErr.Message, "PDF") {
		t.Fatalf("error = %v, want *PDFParseError", err)
	}
}

func TestNativePDFParserWithoutText(t *testing.T) {
	path := writeNativePDF(t, "empty.pdf", nil)
	parser := &NativePDFParser{}
	_, err := parser.ParseInto(context.Background(), path, t.TempDir())
	var parseErr *PDFParseError
	if !errors.As(err, &parseErr) || !strings.Contains(parseErr.Message, "没有可索引的内容") {
		t.Fatalf("error = %v, want no-indexable-content PDFParseError", err)
	}
}

func TestNativePDFTextHeuristics(t *testing.T) {
	if got := sanitizeNativePDFText("Of\ufb01ce\u00a0p\u00addf\ufffd"); got != "Office pdf" {
		t.Errorf("sanitizeNativePDFText() = %q", got)
	}
	pageNumbers := []string{"3", "- 12 -", "Page 4 of 20", "4 / 20", "p. 7"}
	for _, text := range pageNumbers {
		if !nativePageNumberLine.MatchString(text) {
			t.Errorf("page number line %q was not recognized", text)
		}
	}
	contents := []string{"1 Introduction", "Chapter 3", "2024 results", "表 1 说明"}
	for _, text := range contents {
		if nativePageNumberLine.MatchString(text) {
			t.Errorf("content line %q was treated as a page number", text)
		}
	}
	if got := joinParagraphLines("informa-", "tion"); got != "information" {
		t.Errorf("dehyphenation = %q", got)
	}
	if got := joinParagraphLines("中文", "内容"); got != "中文内容" {
		t.Errorf("CJK join = %q", got)
	}
	if got := joinParagraphLines("query-", "Focused"); got != "query- Focused" {
		t.Errorf("uppercase join = %q", got)
	}
	previous := pdfTextItem{x: 10, w: 20, s: "a"}
	next := pdfTextItem{x: 40, w: 5, s: "."}
	if nativeNeedsSpace(previous, next, 12) {
		t.Error("space inserted before closing punctuation")
	}
	if nativePositionsReliable([]pdfTextItem{{w: 5}, {w: 0}}) {
		t.Error("line with a zero-width run reported reliable positions")
	}
}
