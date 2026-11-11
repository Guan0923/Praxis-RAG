package document

import (
	"context"
	"strings"
	"testing"
)

const minimalPDF = "%PDF-1.7\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n"

func writeValidPDF(t *testing.T, name string) string {
	t.Helper()
	return writeTemp(t, name, []byte(minimalPDF))
}

type stubPDFPages struct {
	pages []Page
	err   error
	calls int
}

func (s *stubPDFPages) Pages(_ context.Context, _ string) ([]Page, error) {
	s.calls++
	return s.pages, s.err
}

func TestPDFPagesProduceBlocksAndSpans(t *testing.T) {
	path := writeValidPDF(t, "manual.pdf")
	stub := &stubPDFPages{pages: []Page{
		{Number: 1, Text: "First page paragraph.\n\nSecond paragraph."},
		{Number: 3, Text: "Third page paragraph."},
	}}
	reader := &Reader{PDF: stub}
	content, err := reader.ReadDocument(path)
	if err != nil {
		t.Fatalf("ReadDocument() error = %v", err)
	}
	wantText := "First page paragraph.\n\nSecond paragraph.\n\nThird page paragraph."
	if content.Text != wantText {
		t.Fatalf("text = %q, want %q", content.Text, wantText)
	}
	if len(content.Spans) != 3 {
		t.Fatalf("spans = %d, want 3", len(content.Spans))
	}
	pages := []int{1, 1, 3}
	for index, span := range content.Spans {
		if span.Block.Page != pages[index] {
			t.Fatalf("span %d page = %d, want %d", index, span.Block.Page, pages[index])
		}
		if span.Block.Kind != KindParagraph {
			t.Fatalf("span %d kind = %q", index, span.Block.Kind)
		}
	}
	if content.OriginalText != "First page paragraph.\n\nSecond paragraph.\n\nThird page paragraph." {
		t.Fatalf("original text = %q", content.OriginalText)
	}
	text, err := reader.ReadText(path)
	if err != nil {
		t.Fatalf("ReadText() error = %v", err)
	}
	if text != content.OriginalText {
		t.Fatalf("ReadText() = %q", text)
	}
}

func TestPDFBlankScanRequiresOCR(t *testing.T) {
	path := writeValidPDF(t, "scan.pdf")
	reader := &Reader{PDF: &stubPDFPages{pages: []Page{{Number: 1, Text: ""}, {Number: 2, Text: "   \n\t "}}}}
	_, err := reader.ReadText(path)
	if err == nil || err.Error() != "PDF 未提取到文本，扫描件需要先进行 OCR：scan.pdf" {
		t.Fatalf("error = %v", err)
	}
}

func TestPDFEncryptedAndDamaged(t *testing.T) {
	encrypted := writeTemp(t, "protected.pdf", []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R /Encrypt 5 0 R >>\n%%EOF\n"))
	_, err := ReadText(encrypted)
	if err == nil || err.Error() != "PDF 已加密，暂不支持需要密码的文件：protected.pdf" {
		t.Fatalf("encrypted error = %v", err)
	}

	damaged := writeTemp(t, "broken.pdf", []byte("this is not a pdf at all"))
	_, err = ReadText(damaged)
	if err == nil || err.Error() != "PDF 文件损坏或格式无效：broken.pdf" {
		t.Fatalf("damaged error = %v", err)
	}

	empty := writeTemp(t, "empty.pdf", nil)
	_, err = ReadText(empty)
	if err == nil || !strings.Contains(err.Error(), "PDF 文件损坏或格式无效") {
		t.Fatalf("empty error = %v", err)
	}
}

func TestPDFPageLimit(t *testing.T) {
	overrideInt(t, &MaxPDFPages, 2)
	path := writeValidPDF(t, "many.pdf")
	stub := &stubPDFPages{pages: []Page{
		{Number: 1, Text: "one"}, {Number: 2, Text: "two"}, {Number: 3, Text: "three"},
	}}
	_, err := (&Reader{PDF: stub}).ReadText(path)
	if err == nil || err.Error() != "PDF 页数不能超过 2 页：many.pdf" {
		t.Fatalf("error = %v", err)
	}
}

func TestPDFCharacterLimit(t *testing.T) {
	overrideInt(t, &MaxExtractedCharacters, 10)
	path := writeValidPDF(t, "long.pdf")
	_, err := (&Reader{PDF: &stubPDFPages{pages: []Page{{Number: 1, Text: strings.Repeat("x", 11)}}}}).ReadText(path)
	if err == nil || err.Error() != "文档提取文本过大：long.pdf" {
		t.Fatalf("error = %v", err)
	}
}

func TestPDFParserUnavailable(t *testing.T) {
	path := writeValidPDF(t, "manual.pdf")
	_, err := ReadText(path)
	if err == nil || !strings.Contains(err.Error(), "PDF 文本解析未配置解析器") {
		t.Fatalf("error = %v", err)
	}
}

func TestRenderContentAndTableRowText(t *testing.T) {
	manifest := PDFManifest{
		Version: PDFParserVersion,
		Blocks: []PDFBlock{
			{Ref: "left", Kind: "text", Page: 1, Text: "Left column."},
			{Ref: "picture-1", Kind: "picture", Page: 1, AssetID: "picture-1"},
			{Ref: "caption", Kind: "caption", Page: 1, Text: "Fig. 1"},
			{Ref: "table-1", Kind: "table", Page: 1, AssetID: "table-1"},
			{Ref: "right", Kind: "text", Page: 1, Text: "Right column."},
		},
		Assets: []PDFAsset{
			{AssetID: "picture-1", Kind: "picture", Caption: "Fig. 1"},
			{AssetID: "table-1", Kind: "table", Table: &PDFTable{
				Headers: []string{"A", "", "C"},
				Rows:    [][]string{{"1", "", "3"}},
			}},
		},
	}
	text, err := RenderContent(manifest)
	if err != nil {
		t.Fatalf("RenderContent() error = %v", err)
	}
	want := "Left column.\n\n[Image: picture-1]\n\nFig. 1\n\n1 |  | 3\n\nRight column."
	if text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	if TableRowText([]string{"a\nb", "c|d"}) != `a b | c\|d` {
		t.Fatalf("TableRowText() = %q", TableRowText([]string{"a\nb", "c|d"}))
	}

	unknown := PDFManifest{Blocks: []PDFBlock{{Ref: "x", AssetID: "missing"}}}
	if _, err := RenderContent(unknown); err == nil || !strings.Contains(err.Error(), "unknown asset") {
		t.Fatalf("unknown asset error = %v", err)
	}

	overrideInt(t, &MaxExtractedCharacters, 5)
	if _, err := RenderContent(manifest); err == nil || err.Error() != "Parsed PDF exceeds the extracted text limit" {
		t.Fatalf("size error = %v", err)
	}
}

func TestPagesFromManifestOrdersPagesAndFlattensTables(t *testing.T) {
	manifest := PDFManifest{
		Version: PDFParserVersion,
		Blocks: []PDFBlock{
			{Ref: "table-1", Kind: "table", Page: 1, AssetID: "table-1"},
			{Ref: "caption", Kind: "caption", Page: 1, Text: "Caption page one."},
			{Ref: "picture-1", Kind: "picture", Page: 1, AssetID: "picture-1"},
			{Ref: "text-2", Kind: "text", Page: 2, Text: "Second page."},
		},
		Assets: []PDFAsset{
			{AssetID: "table-1", Kind: "table", Table: &PDFTable{Rows: [][]string{{"A", "B"}}}},
			{AssetID: "picture-1", Kind: "picture"},
		},
	}
	pages := PagesFromManifest(manifest)
	if len(pages) != 2 {
		t.Fatalf("pages = %+v", pages)
	}
	if pages[0].Number != 1 || pages[0].Text != "A | B\n\nCaption page one." {
		t.Fatalf("page 1 = %+v", pages[0])
	}
	if pages[1].Number != 2 || pages[1].Text != "Second page." {
		t.Fatalf("page 2 = %+v", pages[1])
	}
}

func TestPDFParserVersionConstant(t *testing.T) {
	if PDFParserVersion != "go-native-v1" {
		t.Fatalf("PDFParserVersion = %q", PDFParserVersion)
	}
	if PDFParserVersionDocling != "docling-service-v1" {
		t.Fatalf("PDFParserVersionDocling = %q", PDFParserVersionDocling)
	}
}

func TestIsSupportedPDFParserVersion(t *testing.T) {
	for _, supported := range []string{
		PDFParserVersion, PDFParserVersionDocling, "docling-formula-remote-v3",
	} {
		if !IsSupportedPDFParserVersion(supported) {
			t.Fatalf("IsSupportedPDFParserVersion(%q) = false", supported)
		}
	}
	for _, unsupported := range []string{"", "old", "go-native-v2", "docling-service-v2"} {
		if IsSupportedPDFParserVersion(unsupported) {
			t.Fatalf("IsSupportedPDFParserVersion(%q) = true", unsupported)
		}
	}
}

func TestPDFParserVersionForEngine(t *testing.T) {
	cases := map[string]string{
		"docling": PDFParserVersionDocling,
		"DOCLING": PDFParserVersionDocling,
		"go":      PDFParserVersion,
		"":        PDFParserVersion,
		"other":   PDFParserVersion,
	}
	for engine, want := range cases {
		if got := PDFParserVersionForEngine(engine); got != want {
			t.Fatalf("PDFParserVersionForEngine(%q) = %q, want %q", engine, got, want)
		}
	}
}
