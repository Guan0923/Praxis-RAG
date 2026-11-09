package document

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if err == nil || !strings.Contains(err.Error(), "PDF 文本解析需要外部 Docling 解析器") {
		t.Fatalf("error = %v", err)
	}
}

type fakeCommandRunner struct {
	name             string
	args             []string
	env              map[string]string
	err              error
	manifest         string
	blockUntilCancel bool
}

func (r *fakeCommandRunner) Run(ctx context.Context, name string, args []string, env map[string]string) error {
	r.name, r.args, r.env = name, args, env
	if r.blockUntilCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	if r.err != nil {
		return r.err
	}
	if r.manifest != "" {
		manifestPath := argumentValue(args, "--manifest")
		if manifestPath == "" {
			return errors.New("no manifest argument")
		}
		return os.WriteFile(manifestPath, []byte(r.manifest), 0o644)
	}
	return nil
}

func argumentValue(args []string, flag string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag {
			return args[index+1]
		}
	}
	return ""
}

func TestExternalPDFParserProtocol(t *testing.T) {
	pdfPath := writeValidPDF(t, "sample.pdf")
	manifestJSON := `{"version":"` + PDFParserVersion + `","blocks":[` +
		`{"ref":"p1","kind":"text","page":1,"order":0,"text":"First page paragraph."},` +
		`{"ref":"t1","kind":"table","page":2,"order":1,"asset_id":"table-1","text":""}],` +
		`"assets":[{"asset_id":"table-1","kind":"table","page":2,"table":{"rows":[["A","B"],["1","2"]]}}]}`
	runner := &fakeCommandRunner{manifest: manifestJSON}
	parser := &ExternalPDFParser{
		Executable: "test-python",
		PythonPath: "/package/root",
		ModelsPath: "/models",
		VLM:        testVLM,
		Timeout:    5 * time.Second,
		Runner:     runner,
	}
	manifest, err := parser.Parse(context.Background(), pdfPath)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if manifest.Version != PDFParserVersion || len(manifest.Blocks) != 2 || manifest.Blocks[0].Text != "First page paragraph." {
		t.Fatalf("manifest = %+v", manifest)
	}
	if runner.name != "test-python" {
		t.Fatalf("executable = %q", runner.name)
	}
	if argumentValue(runner.args, "-m") != "rag_mcp.pdf_worker" {
		t.Fatalf("module args = %v", runner.args)
	}
	if argumentValue(runner.args, "--input") != pdfPath {
		t.Fatalf("input arg = %q", argumentValue(runner.args, "--input"))
	}
	if argumentValue(runner.args, "--models") != "/models" {
		t.Fatalf("models arg = %q", argumentValue(runner.args, "--models"))
	}
	staging := argumentValue(runner.args, "--output")
	if staging == "" || argumentValue(runner.args, "--manifest") != filepath.Join(staging, "manifest.json") {
		t.Fatalf("staging args = %v", runner.args)
	}
	if runner.env["PYTHONUTF8"] != "1" {
		t.Fatalf("env = %v", runner.env)
	}
	if runner.env["RAG_VLM_BASE_URL"] != "http://vlm.test/v1" || runner.env["RAG_VLM_MODEL"] != "test-vlm" ||
		runner.env["RAG_VLM_API_KEY"] != "test-key" || runner.env["RAG_VLM_CONCURRENCY"] != "2" ||
		runner.env["RAG_VLM_TIMEOUT"] != "30" {
		t.Fatalf("vlm env = %v", runner.env)
	}
	if !strings.HasPrefix(runner.env["PYTHONPATH"], "/package/root") {
		t.Fatalf("PYTHONPATH = %q", runner.env["PYTHONPATH"])
	}

	pages, err := parser.Pages(context.Background(), pdfPath)
	if err != nil {
		t.Fatalf("Pages() error = %v", err)
	}
	if len(pages) != 2 || pages[0].Text != "First page paragraph." || pages[1].Text != "A | B\n1 | 2" {
		t.Fatalf("pages = %+v", pages)
	}
}

func TestExternalPDFParserErrors(t *testing.T) {
	pdfPath := writeValidPDF(t, "sample.pdf")
	t.Run("process failure", func(t *testing.T) {
		parser := &ExternalPDFParser{Runner: &fakeCommandRunner{err: errors.New("boom")}}
		_, err := parser.Parse(context.Background(), pdfPath)
		if err == nil || err.Error() != "PDF 解析失败：sample.pdf" {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing manifest", func(t *testing.T) {
		parser := &ExternalPDFParser{Runner: &fakeCommandRunner{}}
		_, err := parser.Parse(context.Background(), pdfPath)
		if err == nil || err.Error() != "PDF 解析失败：sample.pdf" {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("invalid manifest json", func(t *testing.T) {
		parser := &ExternalPDFParser{Runner: &fakeCommandRunner{manifest: "{not json"}}
		_, err := parser.Parse(context.Background(), pdfPath)
		if err == nil || err.Error() != "PDF 解析失败：sample.pdf" {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("version mismatch", func(t *testing.T) {
		parser := &ExternalPDFParser{Runner: &fakeCommandRunner{manifest: `{"version":"old-parser"}`}}
		_, err := parser.Parse(context.Background(), pdfPath)
		if err == nil || err.Error() != "PDF parser version mismatch; reimport this document" {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("cannot start", func(t *testing.T) {
		parser := &ExternalPDFParser{Runner: &fakeCommandRunner{err: &exec.Error{Name: "python", Err: exec.ErrNotFound}}}
		_, err := parser.Parse(context.Background(), pdfPath)
		if err == nil || err.Error() != "PDF parser could not start: exec.Error" {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		parser := &ExternalPDFParser{Runner: &fakeCommandRunner{blockUntilCancel: true}, Timeout: 20 * time.Millisecond}
		_, err := parser.Parse(context.Background(), pdfPath)
		if err == nil || !strings.Contains(err.Error(), "PDF 解析超过") {
			t.Fatalf("error = %v", err)
		}
	})
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
	if PDFParserVersion != "docling-formula-remote-v3" {
		t.Fatalf("PDFParserVersion = %q", PDFParserVersion)
	}
}
