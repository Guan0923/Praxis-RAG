package document

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// overrideInt tightens one extraction limit for the duration of a test. The
// Python tests monkeypatch the same module constants.
func overrideInt(t *testing.T, target *int, value int) {
	t.Helper()
	previous := *target
	*target = value
	t.Cleanup(func() { *target = previous })
}

func overrideInt64(t *testing.T, target *int64, value int64) {
	t.Helper()
	previous := *target
	*target = value
	t.Cleanup(func() { *target = previous })
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func mustReadDocument(t *testing.T, path string) Content {
	t.Helper()
	content, err := ReadDocument(path)
	if err != nil {
		t.Fatalf("ReadDocument(%s) error = %v", filepath.Base(path), err)
	}
	return content
}

func TestTextFileUniversalNewlinesBOMAndSpans(t *testing.T) {
	path := writeTemp(t, "notes.txt", []byte("\ufefffirst line\r\nsecond line\rthird\n\nnew paragraph\r\n"))
	content := mustReadDocument(t, path)

	if got, want := content.Text, "first line\nsecond line\nthird\n\nnew paragraph"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
	if !strings.Contains(content.OriginalText, "new paragraph") {
		t.Fatalf("original text = %q", content.OriginalText)
	}
	if len(content.Spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(content.Spans))
	}
	for _, span := range content.Spans {
		text := []rune(content.Text)
		if got := string(text[span.Start:span.End]); got != span.Block.Text {
			t.Fatalf("span %d text = %q, block = %q", span.Paragraph, got, span.Block.Text)
		}
		if span.Block.Kind != KindParagraph {
			t.Fatalf("span kind = %q", span.Block.Kind)
		}
	}
	if content.Spans[1].Paragraph != 2 || content.Spans[1].Start != len([]rune(content.Spans[0].Block.Text))+2 {
		t.Fatalf("second span = %+v", content.Spans[1])
	}

	text, err := ReadText(path)
	if err != nil {
		t.Fatalf("ReadText() error = %v", err)
	}
	if text != "first line\nsecond line\nthird\n\nnew paragraph\n" {
		t.Fatalf("ReadText() = %q", text)
	}
}

func TestUnsupportedSuffix(t *testing.T) {
	path := writeTemp(t, "archive.zip", []byte("data"))
	_, err := ReadDocument(path)
	if err == nil || err.Error() != "暂不支持该文件类型：.zip" {
		t.Fatalf("ReadDocument() error = %v", err)
	}
	_, err = ReadText(filepath.Join(t.TempDir(), "noextension"))
	if err == nil || err.Error() != "暂不支持该文件类型：<none>" {
		t.Fatalf("ReadText() error = %v", err)
	}
}

func TestMissingFileErrors(t *testing.T) {
	missingTxt := filepath.Join(t.TempDir(), "missing.txt")
	_, err := ReadText(missingTxt)
	if err == nil || err.Error() != "知识文件无法读取：missing.txt" {
		t.Fatalf("missing txt error = %v", err)
	}
	missingDOCX := filepath.Join(t.TempDir(), "missing.docx")
	_, err = ReadText(missingDOCX)
	if err == nil || err.Error() != "Word 文档需要通过 Docling 解析服务导入：missing.docx" {
		t.Fatalf("missing docx error = %v", err)
	}
	missingPDF := filepath.Join(t.TempDir(), "missing.pdf")
	_, err = ReadText(missingPDF)
	if err == nil || err.Error() != "PDF 文件损坏或格式无效：missing.pdf" {
		t.Fatalf("missing pdf error = %v", err)
	}
}

func TestInvalidUTF8TextIsRejected(t *testing.T) {
	path := writeTemp(t, "latin1.txt", []byte{0x66, 0x6f, 0x6f, 0xff, 0xfe})
	_, err := ReadText(path)
	if err == nil || err.Error() != "文本文件必须使用 UTF-8 编码：latin1.txt" {
		t.Fatalf("error = %v", err)
	}
}

func TestBoundedText(t *testing.T) {
	overrideInt(t, &MaxExtractedCharacters, 5)
	path := writeTemp(t, "long.txt", []byte("0123456789"))
	_, err := ReadText(path)
	if err == nil || err.Error() != "文档提取文本过大：long.txt" {
		t.Fatalf("error = %v", err)
	}
}

func TestMarkdownHeadingsCodeFencesAndSetext(t *testing.T) {
	source := "# 手册\n\n开头。\n\n## 维护\n\n```python\n# not a heading\nprint('ok')\n```\n\n清洁。\n\n第二节\n------\n\n结束。"
	path := writeTemp(t, "guide.md", []byte(source))
	content := mustReadDocument(t, path)

	var headings [][]string
	var kinds []string
	for _, span := range content.Spans {
		kinds = append(kinds, span.Block.Kind)
		if span.Block.Kind == KindHeading {
			headings = append(headings, span.Block.Headings)
		}
	}
	wantHeadings := [][]string{{"手册"}, {"手册", "维护"}, {"手册", "第二节"}}
	if len(headings) != len(wantHeadings) {
		t.Fatalf("headings = %v, want %v", headings, wantHeadings)
	}
	for index := range wantHeadings {
		if strings.Join(headings[index], "\x00") != strings.Join(wantHeadings[index], "\x00") {
			t.Fatalf("heading[%d] = %v, want %v", index, headings[index], wantHeadings[index])
		}
	}
	codeIndex := -1
	for index, kind := range kinds {
		if kind == KindCode {
			codeIndex = index
			break
		}
	}
	if codeIndex < 0 {
		t.Fatalf("no code block in %v", kinds)
	}
	if !strings.Contains(content.Spans[codeIndex].Block.Text, "# not a heading") {
		t.Fatalf("code block = %q", content.Spans[codeIndex].Block.Text)
	}
	if strings.Contains(content.Text, "# 手册\n# not") {
		t.Fatalf("code fence area merged incorrectly: %q", content.Text)
	}
	text, err := ReadText(path)
	if err != nil {
		t.Fatalf("ReadText() error = %v", err)
	}
	if text != source {
		t.Fatalf("ReadText() changed Markdown source")
	}
}

func TestMarkdownTopLevelBlockKinds(t *testing.T) {
	source := "# Title\n\n- one\n- two\n\nafter list\n\n> quoted\n> text\n\n    indented code\n\nafter code\n\n---\n\nlast"
	path := writeTemp(t, "blocks.md", []byte(source))
	content := mustReadDocument(t, path)
	if len(content.Spans) < 6 {
		t.Fatalf("spans = %d: %+v", len(content.Spans), content.Spans)
	}
	foundQuote := false
	foundIndented := false
	for _, span := range content.Spans {
		if strings.Contains(span.Block.Text, "> quoted") {
			foundQuote = true
			if span.Block.Kind != KindParagraph {
				t.Fatalf("quote kind = %q", span.Block.Kind)
			}
		}
		if span.Block.Kind == KindCode && strings.Contains(span.Block.Text, "indented code") {
			foundIndented = true
		}
	}
	if !foundQuote {
		t.Fatalf("block quote missing: %+v", content.Spans)
	}
	if !foundIndented {
		t.Fatalf("indented code missing: %+v", content.Spans)
	}
}

func TestMarkdownParagraphsInsideListsDoNotBecomeHeadings(t *testing.T) {
	source := "# H\n\n- item with # hash\n- item two\n"
	path := writeTemp(t, "list.md", []byte(source))
	content := mustReadDocument(t, path)
	if len(content.Spans) != 2 {
		t.Fatalf("spans = %d, want 2: %+v", len(content.Spans), content.Spans)
	}
	if content.Spans[1].Block.Kind == KindHeading {
		t.Fatalf("list item parsed as heading: %+v", content.Spans[1])
	}
}

func TestReadDocumentUnsupportedSuffixHasNoDot(t *testing.T) {
	if _, err := ReadDocument(strings.ToUpper(SuffixTXT)); err == nil {
		// A bare ".TXT" path is absolute on Windows and unsupported; the point
		// is only that suffix handling is case-insensitive for real files.
		t.Fatalf("expected an error for a dot-only path")
	}
	path := writeTemp(t, "UPPER.TXT", []byte("hello"))
	if _, err := ReadDocument(path); err != nil {
		t.Fatalf("uppercase suffix rejected: %v", err)
	}
}

func TestResolvePathExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no user home directory")
	}
	resolved := resolvePath("~")
	if resolved != home {
		t.Fatalf("resolvePath(~) = %q, want %q", resolved, home)
	}
	joined := resolvePath("~/notes.txt")
	if !strings.HasPrefix(joined, home) || !strings.HasSuffix(joined, "notes.txt") {
		t.Fatalf("resolvePath(~/notes.txt) = %q", joined)
	}
}
