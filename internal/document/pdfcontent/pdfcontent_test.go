package pdfcontent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/document"
)

// runeCounter is the fixed counter for chunking tests: one token per rune.
type runeCounter struct{}

func (runeCounter) Count(text string) int { return utf8.RuneCountInString(text) }

func (runeCounter) TokenStartOffsets(text string) []int {
	runes := []rune(text)
	offsets := make([]int, len(runes))
	for index := range offsets {
		offsets[index] = index
	}
	return offsets
}

func (runeCounter) ModelMaxLength() int     { return 8192 }
func (runeCounter) SpecialTokensToAdd() int { return 0 }

// manifestFixture mirrors tests/test_pdf_fidelity.py::manifest_fixture.
func manifestFixture() document.PDFManifest {
	blocks := []document.PDFBlock{
		{Ref: "text-left", Kind: "text", Text: strings.Repeat("Left column sentence. ", 30)},
		{Ref: "picture", Kind: "picture", AssetID: "picture"},
		{Ref: "caption", Kind: "caption", Text: "Fig. 1"},
		{Ref: "formula", Kind: "formula", Text: "$$\n\\frac{a+b}{c}\n$$", LaTeX: `\frac{a+b}{c}`, OriginalText: "a+b c (2)"},
		{Ref: "text-right", Kind: "text", Text: strings.Repeat("Right column sentence. ", 30)},
		{Ref: "table", Kind: "table", AssetID: "table"},
		{Ref: "unlinked", Kind: "caption", Text: "Unlinked caption"},
	}
	for index := range blocks {
		blocks[index].Page = 1
		blocks[index].Order = index
		blocks[index].BBox = map[string]any{"l": 1.0, "t": 2.0, "r": 10.0, "b": 12.0}
		blocks[index].Locations = []document.PDFLocation{{
			Page: 1, BBox: map[string]any{"l": 1.0, "t": 2.0}, Charspan: []int{0, 0},
		}}
	}
	return document.PDFManifest{
		Version: document.PDFParserVersion,
		Blocks:  blocks,
		Assets: []document.PDFAsset{
			{AssetID: "picture", Kind: "picture", Caption: "Fig. 1", GeneratedDescription: "private"},
			{AssetID: "table", Kind: "table", Caption: "", GeneratedDescription: "private table description",
				Table: &document.PDFTable{
					Headers: []string{"A", "", "C"},
					Rows:    [][]string{{"1", "", "3"}},
				}},
		},
	}
}

func mustConfig(t *testing.T, strategy string, size, overlap int) chunking.Config {
	t.Helper()
	config, err := chunking.NewConfig(strategy, size, overlap)
	if err != nil {
		t.Fatalf("NewConfig(%q,%d,%d) error = %v", strategy, size, overlap, err)
	}
	return config
}

func contentOptions(t *testing.T, config chunking.Config) Options {
	t.Helper()
	return Options{
		DocumentID:    "doc",
		Source:        "paper.pdf",
		FallbackTitle: "Paper",
		Config:        config,
		Counter:       runeCounter{},
	}
}

func TestOrderTokenLimitsAndAtomicFormula(t *testing.T) {
	for _, strategy := range []string{"character", "token", "sentence", "recursive", "structure"} {
		t.Run(strategy, func(t *testing.T) {
			var config chunking.Config
			if strategy == "character" {
				config = mustConfig(t, "character", 100, 10)
			} else {
				config = mustConfig(t, strategy, 32, 8)
			}
			manifest := manifestFixture()
			chunks, err := ContentChunks(manifest, contentOptions(t, config))
			if err != nil {
				t.Fatalf("ContentChunks() error = %v", err)
			}
			orders := make([]int, len(chunks))
			for index, chunk := range chunks {
				order, ok := chunk.Metadata["order"].(int)
				if !ok {
					t.Fatalf("chunk %d has no order metadata: %v", index, chunk.Metadata)
				}
				orders[index] = order
			}
			for index := 1; index < len(orders); index++ {
				if orders[index] < orders[index-1] {
					t.Fatalf("orders not sorted: %v", orders)
				}
			}
			formulas := make([]chunking.Chunk, 0, 1)
			tableChunks := make([]chunking.Chunk, 0, 1)
			ordinary := make([]chunking.Chunk, 0, len(chunks))
			for _, chunk := range chunks {
				switch chunk.Metadata["kind"] {
				case "formula":
					formulas = append(formulas, chunk)
				case "table":
					tableChunks = append(tableChunks, chunk)
				case "picture":
				default:
					ordinary = append(ordinary, chunk)
				}
			}
			if len(formulas) != 1 || formulas[0].Text != manifest.Blocks[3].Text {
				t.Fatalf("formula chunks = %+v", formulas)
			}
			refs := formulas[0].Metadata["refs"].([]string)
			if len(refs) != 1 || refs[0] != "formula" {
				t.Fatalf("formula refs = %v", refs)
			}
			if len(ordinary) <= 2 {
				t.Fatalf("ordinary chunks = %d, want more than 2", len(ordinary))
			}
			for _, chunk := range ordinary {
				if strategy == "character" {
					if length := utf8.RuneCountInString(chunk.Text); length > 100 {
						t.Fatalf("character chunk too long (%d): %q", length, chunk.Text)
					}
					continue
				}
				count, ok := chunk.Metadata["token_count"].(int)
				if !ok || count <= 0 || count > 32 {
					t.Fatalf("token chunk count = %v: %q", chunk.Metadata["token_count"], chunk.Text)
				}
			}
			if len(tableChunks) == 0 {
				t.Fatalf("no table chunk")
			}
			joined := make([]string, 0, len(tableChunks))
			for _, chunk := range tableChunks {
				if chunk.Metadata["asset_kind"] != "table" {
					t.Fatalf("table chunk metadata = %v", chunk.Metadata)
				}
				joined = append(joined, chunk.Text)
			}
			if !strings.Contains(strings.Join(joined, "\n"), "A |  | C\n1 |  | 3") {
				t.Fatalf("table text = %q", joined)
			}

			text, err := document.RenderContent(manifest)
			if err != nil {
				t.Fatalf("RenderContent() error = %v", err)
			}
			imageIndex := strings.Index(text, "[Image:")
			captionIndex := strings.Index(text, "Fig. 1")
			formulaIndex := strings.Index(text, `\frac`)
			rightIndex := strings.Index(text, "Right column")
			if !(imageIndex < captionIndex && captionIndex < formulaIndex && formulaIndex < rightIndex) {
				t.Fatalf("render order = %d/%d/%d/%d: %q", imageIndex, captionIndex, formulaIndex, rightIndex, text)
			}
			if strings.Count(text, "Fig. 1") != 1 || !strings.Contains(text, "Unlinked caption") ||
				strings.Contains(text, "private") {
				t.Fatalf("render content = %q", text)
			}
		})
	}
}

func TestAtomicItemsOverModelLimitAreRejected(t *testing.T) {
	manifest := manifestFixture()
	manifest.Blocks[3].Text = "$$\n" + strings.Repeat("x + ", 9000) + "\n$$"
	config := mustConfig(t, "recursive", 512, 64)
	_, err := ContentChunks(manifest, contentOptions(t, config))
	if err == nil || !strings.Contains(err.Error(), "model input limit") {
		t.Fatalf("formula limit error = %v", err)
	}
	manifest = manifestFixture()
	manifest.Assets[1].Table.Rows = [][]string{{strings.Repeat("x ", 9000)}}
	_, err = ContentChunks(manifest, contentOptions(t, config))
	if err == nil || !strings.Contains(err.Error(), "model input limit") {
		t.Fatalf("table limit error = %v", err)
	}
}

func TestContentChunksUnknownAssetAndEmptyManifest(t *testing.T) {
	manifest := document.PDFManifest{
		Version: document.PDFParserVersion,
		Blocks:  []document.PDFBlock{{Ref: "x", Kind: "picture", Page: 1, AssetID: "missing"}},
	}
	config := mustConfig(t, "recursive", 512, 64)
	if _, err := ContentChunks(manifest, contentOptions(t, config)); err == nil ||
		!strings.Contains(err.Error(), "unknown asset: missing") {
		t.Fatalf("unknown asset error = %v", err)
	}
	empty := document.PDFManifest{Blocks: []document.PDFBlock{{Ref: "e", Kind: "text", Page: 1, Text: "  "}}}
	chunks, err := ContentChunks(empty, contentOptions(t, config))
	if err != nil {
		t.Fatalf("ContentChunks(empty) error = %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("empty chunks = %v", chunks)
	}
}

func TestContentChunksMetadataAndWarnings(t *testing.T) {
	manifest := manifestFixture()
	manifest.Warnings = []document.PDFWarning{{
		Page: 1, Kind: "low_confidence_ocr", Count: 2, Message: "verify",
	}}
	config := mustConfig(t, "recursive", 512, 64)
	chunks, err := ContentChunks(manifest, contentOptions(t, config))
	if err != nil {
		t.Fatalf("ContentChunks() error = %v", err)
	}
	if len(chunks) == 0 {
		t.Fatalf("no chunks")
	}
	first := chunks[0]
	if first.DocumentID != "doc" || first.Source != "paper.pdf" || first.Title == "" {
		t.Fatalf("chunk identity = %+v", first)
	}
	if first.Metadata["document_suffix"] != ".pdf" ||
		first.Metadata["pdf_parser_version"] != document.PDFParserVersion {
		t.Fatalf("chunk metadata = %v", first.Metadata)
	}
	chunkingMetadata, ok := first.Metadata["chunking"].(map[string]any)
	if !ok || chunkingMetadata["strategy"] != "recursive" || chunkingMetadata["tokenizer"] != "BAAI/bge-m3" {
		t.Fatalf("chunking metadata = %v", first.Metadata["chunking"])
	}
	if _, ok := first.Metadata["start"].(int); !ok {
		t.Fatalf("chunk metadata start = %v", first.Metadata["start"])
	}
	if locations, ok := first.Metadata["locations"].([]map[string]any); !ok || len(locations) == 0 {
		t.Fatalf("chunk metadata locations = %v", first.Metadata["locations"])
	}
	warnings, ok := first.Metadata["source_warnings"].([]document.PDFWarning)
	if !ok || len(warnings) != 1 {
		t.Fatalf("source warnings = %v", first.Metadata["source_warnings"])
	}
}

func TestContentChunksMetadataPagesAndOrder(t *testing.T) {
	manifest := manifestFixture()
	manifest.Blocks[4].Page = 2
	manifest.Blocks[4].Locations = []document.PDFLocation{{Page: 2}}
	config := mustConfig(t, "recursive", 512, 64)
	chunks, err := ContentChunks(manifest, contentOptions(t, config))
	if err != nil {
		t.Fatalf("ContentChunks() error = %v", err)
	}
	rightFound := false
	for _, chunk := range chunks {
		if strings.Contains(chunk.Text, "Right column") {
			rightFound = true
			pages := chunk.Metadata["pages"].([]int)
			if len(pages) != 1 || pages[0] != 2 {
				t.Fatalf("right chunk pages = %v", pages)
			}
			if chunk.Metadata["order"] != 4 {
				t.Fatalf("right chunk order = %v", chunk.Metadata["order"])
			}
			provenance := chunk.Metadata["provenance"].([]map[string]any)
			if len(provenance) != 1 || provenance[0]["ref"] != "text-right" {
				t.Fatalf("provenance = %v", provenance)
			}
		}
	}
	if !rightFound {
		t.Fatalf("right column chunk missing")
	}
}

func TestParagraphPagesPreserveCompleteFormula(t *testing.T) {
	formula := "$$\n" + strings.Repeat("x + ", 2100) + "\n$$"
	for _, prefix := range []string{strings.Repeat("a", 7900) + "\n\n", ""} {
		text := prefix + formula + "\n\nAfter formula"
		pages := ParagraphPages(text, false)
		if strings.Join(pages, "") != text {
			t.Fatalf("pages do not reassemble: %d pages", len(pages))
		}
		count := 0
		for _, page := range pages {
			if strings.Contains(page, formula) {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("formula split across pages: %d", count)
		}
	}
}

func TestParagraphPagesProtectInlineMath(t *testing.T) {
	equation := "$" + strings.Repeat("x+", 100) + "1$"
	text := strings.Repeat("a", 7950) + equation + strings.Repeat("z", 200)
	pages := ParagraphPages(text, true)
	if strings.Join(pages, "") != text {
		t.Fatalf("pages do not reassemble")
	}
	count := 0
	for _, page := range pages {
		if strings.Contains(page, equation) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("inline formula split across pages: %d", count)
	}
}

func TestParagraphPagesEdgeCases(t *testing.T) {
	if pages := ParagraphPages("", false); len(pages) != 1 || pages[0] != "" {
		t.Fatalf("empty pages = %v", pages)
	}
	short := "short document"
	if pages := ParagraphPages(short, false); len(pages) != 1 || pages[0] != short {
		t.Fatalf("short pages = %v", pages)
	}
	long := strings.Repeat("paragraph text\n\n", 700)
	pages := ParagraphPages(long, false)
	if strings.Join(pages, "") != long {
		t.Fatalf("long pages do not reassemble")
	}
	for _, page := range pages {
		if utf8.RuneCountInString(page) > PageCharacters {
			t.Fatalf("page over %d characters: %d", PageCharacters, utf8.RuneCountInString(page))
		}
	}
}

func TestInlineMathRanges(t *testing.T) {
	cases := []struct {
		text string
		want [][2]int
	}{
		{"$a$", [][2]int{{0, 3}}},
		{"$$a$$", nil},
		{"a $b$ c", [][2]int{{2, 5}}},
		{"$a$ $b$", [][2]int{{0, 3}, {4, 7}}},
		{"no math", nil},
		{"$", nil},
		{"$a$$b$", nil},
		{"中文$公式$后", [][2]int{{6, 14}}},
	}
	for _, testCase := range cases {
		got := inlineMathRanges(testCase.text)
		want := testCase.want
		if len(got) != len(want) {
			t.Fatalf("inlineMathRanges(%q) = %v, want %v", testCase.text, got, want)
		}
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("inlineMathRanges(%q) = %v, want %v", testCase.text, got, want)
			}
		}
	}
}

func TestChunkingHelperExports(t *testing.T) {
	if got := chunking.Title("\n\n  ## 标题  \nbody", "fallback"); got != "标题" {
		t.Fatalf("title = %q", got)
	}
	if got := chunking.Title("\n \n", "fallback"); got != "fallback" {
		t.Fatalf("fallback title = %q", got)
	}
	ranges := chunking.CharacterRanges("abc\ndef", 4, 1)
	want := []chunking.Range{{Start: 0, End: 3}, {Start: 4, End: 7}}
	if len(ranges) != len(want) {
		t.Fatalf("character ranges = %v", ranges)
	}
	for index := range want {
		if ranges[index] != want[index] {
			t.Fatalf("character ranges = %v, want %v", ranges, want)
		}
	}
	if chunking.DocumentID("Manual.PDF") != chunking.DocumentID("manual.pdf") {
		t.Fatalf("DocumentID() must case-fold like Python")
	}
	if !strings.HasPrefix(chunking.DocumentID("manual.pdf"), "doc-") {
		t.Fatalf("DocumentID() prefix = %q", chunking.DocumentID("manual.pdf"))
	}
}
