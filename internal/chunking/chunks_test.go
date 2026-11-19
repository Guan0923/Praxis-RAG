package chunking

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/Guan0923/Praxis-RAG/internal/document"
)

func writeTextFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func mustConfig(t *testing.T, strategy string, size, overlap int) Config {
	t.Helper()
	config, err := NewConfig(strategy, size, overlap)
	if err != nil {
		t.Fatalf("NewConfig(%q,%d,%d) error = %v", strategy, size, overlap, err)
	}
	return config
}

// checkTokenChunks ports tests/test_chunk_strategies.py::check_chunks with an
// injected fixed counter instead of the real BGE-M3 tokenizer.
func checkTokenChunks(t *testing.T, path string, chunks []Chunk, counter TokenCounter, size int, reader *document.Reader) {
	t.Helper()
	if reader == nil {
		reader = &document.Reader{}
	}
	content, err := reader.ReadDocument(path)
	if err != nil {
		t.Fatalf("ReadDocument(%s) error = %v", filepath.Base(path), err)
	}
	runes := []rune(content.Text)
	covered := make([]bool, len(runes))
	previousEnd := 0
	for index, chunk := range chunks {
		start, ok := chunk.Metadata["start"].(int)
		if !ok {
			t.Fatalf("chunk %d metadata start = %v", index, chunk.Metadata["start"])
		}
		end, ok := chunk.Metadata["end"].(int)
		if !ok {
			t.Fatalf("chunk %d metadata end = %v", index, chunk.Metadata["end"])
		}
		if start < 0 || end > len(runes) || start >= end {
			t.Fatalf("chunk %d range = [%d,%d) over %d runes", index, start, end, len(runes))
		}
		if end <= previousEnd {
			t.Fatalf("chunk %d end %d does not advance past %d", index, end, previousEnd)
		}
		if got, want := chunk.Text, string(runes[start:end]); got != want {
			t.Fatalf("chunk %d text = %q, want %q", index, got, want)
		}
		count := counter.Count(chunk.Text)
		if count <= 0 || count > size {
			t.Fatalf("chunk %d token count = %d, want 1..%d", index, count, size)
		}
		if recorded := chunk.Metadata["token_count"]; recorded != count {
			t.Fatalf("chunk %d token_count = %v, want %d", index, recorded, count)
		}
		for position := start; position < end; position++ {
			covered[position] = true
		}
		previousEnd = end
	}
	for index, value := range runes {
		if !covered[index] && !unicode.IsSpace(value) {
			t.Fatalf("source character %d (%q) is not covered by any chunk", index, value)
		}
	}
}

func TestTokenStrategyLimitsAndSourceCoverage(t *testing.T) {
	texts := map[string]string{
		"chinese": strings.Repeat("测试设备工作正常。温度超过上限时应先断电！请检查传感器连接？\n\n", 45),
		"english": strings.Repeat("Dr. Smith checked version 3.14. It works! Another sentence?\n\n", 40),
		"mixed":   strings.Repeat("Mixed中文 English café 👩🏽‍💻。下一句话有内容！\n\n", 40),
		"nopunct": strings.Repeat("无标点非常长的连续文字", 200),
	}
	for strategy := range map[string]struct{}{"token": {}, "sentence": {}, "recursive": {}, "structure": {}} {
		for name, text := range texts {
			t.Run(strategy+"/"+name, func(t *testing.T) {
				path := writeTextFile(t, "sample.txt", text)
				config := mustConfig(t, strategy, 64, 12)
				chunks, err := ChunkDocument(path, ChunkOptions{Config: &config, Counter: runeCounter{}})
				if err != nil {
					t.Fatalf("ChunkDocument() error = %v", err)
				}
				if len(chunks) <= 1 {
					t.Fatalf("chunks = %d, want more than one", len(chunks))
				}
				checkTokenChunks(t, path, chunks, runeCounter{}, 64, nil)
			})
		}
	}
}

func TestMarkdownHeadingsAndCodeFences(t *testing.T) {
	source := "# 手册\n\n开头。\n\n## 维护\n\n```python\n# not a heading\nprint('ok')\n```\n\n清洁。\n\n第二节\n------\n\n结束。"
	path := writeTextFile(t, "guide.md", source)

	content, err := document.ReadDocument(path)
	if err != nil {
		t.Fatalf("ReadDocument() error = %v", err)
	}
	var headings [][]string
	for _, span := range content.Spans {
		if span.Block.Kind == document.KindHeading {
			headings = append(headings, span.Block.Headings)
		}
	}
	want := [][]string{{"手册"}, {"手册", "维护"}, {"手册", "第二节"}}
	if len(headings) != len(want) {
		t.Fatalf("headings = %v, want %v", headings, want)
	}
	for index := range want {
		if strings.Join(headings[index], "\x00") != strings.Join(want[index], "\x00") {
			t.Fatalf("heading[%d] = %v, want %v", index, headings[index], want[index])
		}
	}

	config := mustConfig(t, "structure", 512, 64)
	chunks, err := ChunkDocument(path, ChunkOptions{Config: &config, Counter: runeCounter{}})
	if err != nil {
		t.Fatalf("ChunkDocument() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	if !strings.Contains(chunks[0].Text, "# not a heading") {
		t.Fatalf("code fence text missing: %q", chunks[0].Text)
	}
	paths := chunks[0].Metadata["heading_paths"].([][]string)
	if !containsPath(paths, []string{"手册", "维护"}) {
		t.Fatalf("heading_paths = %v", paths)
	}
	checkTokenChunks(t, path, chunks, runeCounter{}, 512, nil)
}

func containsPath(paths [][]string, want []string) bool {
	for _, path := range paths {
		if len(path) != len(want) {
			continue
		}
		matches := true
		for index := range want {
			if path[index] != want[index] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

type stubPDFPages struct {
	pages []document.Page
}

func (s stubPDFPages) Pages(_ context.Context, _ string) ([]document.Page, error) {
	return s.pages, nil
}

func writeMinimalPDF(t *testing.T, name string) string {
	t.Helper()
	return writeTextFile(t, name, "%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n")
}

func TestPDFMergesAcrossPageNumbers(t *testing.T) {
	path := writeMinimalPDF(t, "pages.pdf")
	reader := &document.Reader{PDF: stubPDFPages{pages: []document.Page{
		{Number: 1, Text: "First page paragraph."},
		{Number: 3, Text: "Third page paragraph."},
	}}}
	config := mustConfig(t, "structure", 512, 64)
	chunks, err := ChunkDocument(path, ChunkOptions{Config: &config, Counter: runeCounter{}, Reader: reader})
	if err != nil {
		t.Fatalf("ChunkDocument() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	pages, ok := chunks[0].Metadata["pages"].([]int)
	if !ok || len(pages) != 2 || pages[0] != 1 || pages[1] != 3 {
		t.Fatalf("pages = %v", chunks[0].Metadata["pages"])
	}
	if chunks[0].Metadata["document_suffix"] != ".pdf" {
		t.Fatalf("document_suffix = %v", chunks[0].Metadata["document_suffix"])
	}
	if !strings.Contains(chunks[0].Text, "First page paragraph.") || !strings.Contains(chunks[0].Text, "Third page paragraph.") {
		t.Fatalf("chunk text = %q", chunks[0].Text)
	}
	checkTokenChunks(t, path, chunks, runeCounter{}, 512, reader)
}

func TestPDFChunkingSurfacesOCRRequirement(t *testing.T) {
	path := writeMinimalPDF(t, "scan.pdf")
	reader := &document.Reader{PDF: stubPDFPages{pages: []document.Page{{Number: 1, Text: ""}}}}
	config := mustConfig(t, "recursive", 64, 12)
	_, err := ChunkDocument(path, ChunkOptions{Config: &config, Counter: runeCounter{}, Reader: reader})
	if err == nil || !strings.Contains(err.Error(), "OCR") {
		t.Fatalf("error = %v", err)
	}
}

func TestWordSectionsTablesAndLongRows(t *testing.T) {
	// The in-process Word reader is gone (.docx is Docling-only), so the
	// structure-aware Word splitting is exercised on the document.Content
	// block view directly (the Docling manifest projects to the same Block
	// shape): a heading, body text, a second heading and long table rows.
	longCell := strings.Repeat("检查传感器然后重启。", 100)
	rows := [][]string{
		{"代码", "处理方法"},
		{"索引", longCell},
	}
	tableNumber := 1
	blocks := []document.Block{
		{Text: "设备手册", Kind: document.KindHeading, Headings: []string{"设备手册"}},
		{Text: "正常使用。", Kind: document.KindParagraph, Headings: []string{"设备手册"}},
		{Text: "故障处理", Kind: document.KindHeading, Headings: []string{"设备手册", "故障处理"}},
	}
	for _, row := range rows {
		cells := make([]string, len(row))
		for index, cell := range row {
			cells[index] = strings.ReplaceAll(cell, "|", `\|`)
		}
		blocks = append(blocks, document.Block{
			Text: strings.Join(cells, " | "), Kind: document.KindTableRow,
			Headings: []string{"设备手册", "故障处理"}, Table: tableNumber,
		})
	}
	blocks = append(blocks, document.Block{
		Text: "处理结束。", Kind: document.KindParagraph, Headings: []string{"设备手册", "故障处理"},
	})
	// Assemble the joined text and spans the way document.Content carries
	// them for the structure splitter.
	parts := make([]string, len(blocks))
	spans := make([]document.Span, 0, len(blocks))
	offset := 0
	for index, block := range blocks {
		if index > 0 {
			offset += 2
		}
		length := len([]rune(block.Text))
		spans = append(spans, document.Span{
			Start: offset, End: offset + length, Block: block, Paragraph: len(spans) + 1,
		})
		parts[index] = block.Text
		offset += length
	}
	content := document.Content{Text: strings.Join(parts, "\n\n"), Spans: spans}

	config := mustConfig(t, "structure", 64, 12)
	budget, err := NewTokenBudget(runeCounter{}, config)
	if err != nil {
		t.Fatalf("NewTokenBudget() error = %v", err)
	}
	ranges, err := SplitContent(content, document.SuffixDOCX, config, budget)
	if err != nil {
		t.Fatalf("SplitContent() error = %v", err)
	}
	if len(ranges) < 3 {
		t.Fatalf("ranges = %v", ranges)
	}
	runes := []rune(content.Text)
	tableRowLocation := false
	headingPath := false
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
		metadata := SourceMetadata(content, start, end)
		locations, ok := metadata["locations"].([]map[string]any)
		if !ok {
			t.Fatalf("locations = %v", metadata["locations"])
		}
		for _, location := range locations {
			if location["kind"] == document.KindTableRow {
				tableRowLocation = true
			}
		}
		if paths, ok := metadata["heading_paths"].([][]string); ok {
			if containsPath(paths, []string{"设备手册", "故障处理"}) {
				headingPath = true
			}
		}
	}
	if !tableRowLocation {
		t.Fatalf("no table_row location in split ranges")
	}
	if !headingPath {
		t.Fatalf("heading path 设备手册/故障处理 missing")
	}
	// Long rows are still split into bounded pieces.
	for _, span := range ranges {
		if length := span.End - span.Start; length > 64 {
			t.Fatalf("range length %d over limit", length)
		}
	}
}

func TestSentenceBoundariesAndCompleteOverlapThroughChunker(t *testing.T) {
	text := strings.Repeat("设备正常。请检查连接。随后重启。", 30)
	path := writeTextFile(t, "sentences.txt", text)
	config := mustConfig(t, "sentence", 32, 12)
	chunks, err := ChunkDocument(path, ChunkOptions{Config: &config, Counter: runeCounter{}})
	if err != nil {
		t.Fatalf("ChunkDocument() error = %v", err)
	}
	if len(chunks) <= 1 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	content, err := document.ReadDocument(path)
	if err != nil {
		t.Fatalf("ReadDocument() error = %v", err)
	}
	runes := []rune(content.Text)
	overlaps := 0
	for index := 0; index+1 < len(chunks); index++ {
		current := chunks[index]
		next := chunks[index+1]
		currentEnd := current.Metadata["end"].(int)
		nextStart := next.Metadata["start"].(int)
		if nextStart >= currentEnd {
			continue
		}
		overlap := string(runes[nextStart:currentEnd])
		overlaps++
		if !strings.HasSuffix(overlap, "。") {
			t.Fatalf("overlap %q does not end on a complete sentence", overlap)
		}
		counter := runeCounter{}
		if counter.Count(overlap) > 12 {
			t.Fatalf("overlap %q has %d tokens", overlap, counter.Count(overlap))
		}
	}
	if overlaps == 0 {
		t.Fatalf("expected complete-sentence overlaps")
	}
}

func TestCharacterChunkMetadataAndStableIDs(t *testing.T) {
	path := writeTextFile(t, "notes.txt", "标题行\n\n正文内容在这里。还有更多内容。")
	config := mustConfig(t, "character", 200, 20)
	options := ChunkOptions{Config: &config, TokenizerModel: "test-model"}
	chunks, err := ChunkDocument(path, options)
	if err != nil {
		t.Fatalf("ChunkDocument() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	chunk := chunks[0]
	if chunk.DocumentID != StableID("doc", "notes.txt") {
		t.Fatalf("document id = %q", chunk.DocumentID)
	}
	wantChunkID := StableID("chunk", chunk.DocumentID+"\x00"+strconv.Itoa(0)+"\x00"+chunk.Text)
	if chunk.ChunkID != wantChunkID {
		t.Fatalf("chunk id = %q, want %q", chunk.ChunkID, wantChunkID)
	}
	if chunk.Title != "标题行" {
		t.Fatalf("title = %q", chunk.Title)
	}
	if chunk.Source != "notes.txt" {
		t.Fatalf("source = %q", chunk.Source)
	}
	if chunk.Metadata["chunk_index"] != 0 || chunk.Metadata["document_suffix"] != ".txt" {
		t.Fatalf("metadata = %v", chunk.Metadata)
	}
	chunkingMetadata, ok := chunk.Metadata["chunking"].(map[string]any)
	if !ok {
		t.Fatalf("chunking metadata = %v", chunk.Metadata["chunking"])
	}
	if chunkingMetadata["strategy"] != "character" || chunkingMetadata["chunk_size"] != 200 ||
		chunkingMetadata["chunk_overlap"] != 20 || chunkingMetadata["tokenizer"] != "test-model" {
		t.Fatalf("chunking metadata = %v", chunkingMetadata)
	}
	if _, ok := chunk.Metadata["locations"].([]map[string]any); !ok {
		t.Fatalf("locations = %v", chunk.Metadata["locations"])
	}

	again, err := ChunkDocument(path, options)
	if err != nil {
		t.Fatalf("ChunkDocument() second run error = %v", err)
	}
	if again[0].ChunkID != chunk.ChunkID || again[0].DocumentID != chunk.DocumentID {
		t.Fatalf("ids are not stable: %q/%q", again[0].DocumentID, again[0].ChunkID)
	}
}

func TestCharacterChunkerValidation(t *testing.T) {
	path := writeTextFile(t, "data.txt", "content")
	if _, err := ChunkDocument(path, ChunkOptions{ChunkSize: 50, Overlap: 10}); err == nil ||
		err.Error() != "chunk_size must be at least 100 characters" {
		t.Fatalf("chunk size error = %v", err)
	}
	if _, err := ChunkDocument(path, ChunkOptions{ChunkSize: 120, Overlap: 120}); err == nil ||
		err.Error() != "overlap must be non-negative and smaller than chunk_size" {
		t.Fatalf("overlap error = %v", err)
	}
}

func TestChunkDocumentSourceRootTitleAndNormalization(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "docs")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(subdir, "guide.txt")
	if err := os.WriteFile(path, []byte("#   标题  \r\n\r\nbody\ttext  "), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	config := mustConfig(t, "character", 200, 20)
	chunks, err := ChunkDocument(path, ChunkOptions{Config: &config, SourceRoot: root, Counter: runeCounter{}})
	if err != nil {
		t.Fatalf("ChunkDocument() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	if chunks[0].Source != "docs/guide.txt" {
		t.Fatalf("source = %q", chunks[0].Source)
	}
	if chunks[0].Title != "标题" {
		t.Fatalf("title = %q", chunks[0].Title)
	}
	if strings.ContainsAny(chunks[0].Text, "\t\r") || strings.Contains(chunks[0].Text, "  ") {
		t.Fatalf("text was not normalized: %q", chunks[0].Text)
	}

	override, err := ChunkDocument(path, ChunkOptions{Config: &config, SourceName: "custom.md", Counter: runeCounter{}})
	if err != nil {
		t.Fatalf("ChunkDocument(SourceName) error = %v", err)
	}
	if override[0].Source != "custom.md" {
		t.Fatalf("overridden source = %q", override[0].Source)
	}
}

func TestChunkDocumentEmptyDocument(t *testing.T) {
	path := writeTextFile(t, "empty.txt", "   \n\n\t\n")
	chunks, err := ChunkDocument(path, ChunkOptions{})
	if err != nil {
		t.Fatalf("ChunkDocument() error = %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("chunks = %v", chunks)
	}
	config := mustConfig(t, "recursive", 64, 8)
	chunks, err = ChunkDocument(path, ChunkOptions{Config: &config, Counter: runeCounter{}})
	if err != nil {
		t.Fatalf("ChunkDocument(token) error = %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("token chunks = %v", chunks)
	}
}

func TestChunkDocumentModelLimitIsSurfaced(t *testing.T) {
	path := writeTextFile(t, "data.txt", "some content")
	config := mustConfig(t, "recursive", 8192, 64)
	_, err := ChunkDocument(path, ChunkOptions{Config: &config, Counter: DefaultCounter})
	if err == nil || !strings.Contains(err.Error(), "模型") {
		t.Fatalf("error = %v", err)
	}
}

func TestDiscoverDocuments(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.md", "b.pdf", "c.docx", "d.txt", "legacy.doc", "ignored.xyz"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "e.md"), []byte("fixture"), 0o644); err != nil {
		t.Fatalf("write nested: %v", err)
	}
	found, err := DiscoverDocuments(root)
	if err != nil {
		t.Fatalf("DiscoverDocuments() error = %v", err)
	}
	if len(found) != 6 {
		t.Fatalf("found = %v", found)
	}
	want := map[string]bool{"a.md": true, "b.pdf": true, "c.docx": true, "d.txt": true, "legacy.doc": true, "e.md": true}
	for _, path := range found {
		if !want[filepath.Base(path)] {
			t.Fatalf("unexpected discovery: %s", path)
		}
		delete(want, filepath.Base(path))
	}
	if len(want) != 0 {
		t.Fatalf("missing discoveries: %v", want)
	}
	for index := 1; index < len(found); index++ {
		if pythonCaseFold(filepath.ToSlash(found[index-1])) > pythonCaseFold(filepath.ToSlash(found[index])) {
			t.Fatalf("discovery is not sorted: %v", found)
		}
	}

	single, err := DiscoverDocuments(filepath.Join(root, "a.md"))
	if err != nil || len(single) != 1 || single[0] != filepath.Join(root, "a.md") {
		t.Fatalf("single discovery = %v, %v", single, err)
	}
	if _, err := DiscoverDocuments(filepath.Join(root, "ignored.xyz")); err == nil ||
		err.Error() != "Unsupported document type: .xyz" {
		t.Fatalf("unsupported error = %v", err)
	}
	if _, err := DiscoverDocuments(filepath.Join(root, "missing-dir")); err == nil ||
		!strings.Contains(err.Error(), "Document path does not exist") {
		t.Fatalf("missing error = %v", err)
	}
}

func TestStableIDShape(t *testing.T) {
	identifier := StableID("doc", "manual.pdf")
	if !strings.HasPrefix(identifier, "doc-") || len(identifier) != 4+32 {
		t.Fatalf("id = %q", identifier)
	}
	if StableID("doc", "manual.pdf") != identifier {
		t.Fatalf("id is not deterministic")
	}
	if StableID("chunk", "manual.pdf") == identifier {
		t.Fatalf("prefix is not part of the identity")
	}
}

func TestPythonCaseFold(t *testing.T) {
	if pythonCaseFold("ABC") != "abc" {
		t.Fatalf("casefold ASCII = %q", pythonCaseFold("ABC"))
	}
	if pythonCaseFold("Straße") != "strasse" {
		t.Fatalf("casefold ß = %q", pythonCaseFold("Straße"))
	}
}

func TestTitleOfTruncatesAndSkipsEmptyLines(t *testing.T) {
	long := strings.Repeat("标", 250)
	if got := titleOf("\n\n  ## "+long+"\nbody", "fallback"); len([]rune(got)) != 200 {
		t.Fatalf("title length = %d", len([]rune(got)))
	}
	if got := titleOf("\n \n", "fallback"); got != "fallback" {
		t.Fatalf("title = %q", got)
	}
	// Python strips the line before lstrip("#"), so leading spaces must not
	// hide the heading markers.
	if got := titleOf("  ## 标题  \nbody", "fallback"); got != "标题" {
		t.Fatalf("title = %q", got)
	}
}
