package chunking

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/document"
)

func rangeTexts(text string, ranges []Range) []string {
	runes := []rune(text)
	result := make([]string, len(ranges))
	for index, span := range ranges {
		result[index] = string(runes[span.Start:span.End])
	}
	return result
}

func TestSentenceRangesChineseEnglishAndAbbreviations(t *testing.T) {
	text := "Dr. Smith measured 3.14. Everything works! 中文句子结束。”下一句？"
	ranges := SentenceRanges(text, 0, len([]rune(text)))
	sentences := make([]string, len(ranges))
	for index, span := range ranges {
		runes := []rune(text)
		sentences[index] = strings.TrimSpace(string(runes[span.Start:span.End]))
	}
	want := []string{"Dr. Smith measured 3.14.", "Everything works!", "中文句子结束。”", "下一句？"}
	if !reflect.DeepEqual(sentences, want) {
		t.Fatalf("sentences = %#v, want %#v", sentences, want)
	}
}

func TestSentenceRangesTerminatorRunsAndQuotes(t *testing.T) {
	text := "什么！？下一句。他说“好的。”然后离开。"
	ranges := SentenceRanges(text, 0, len([]rune(text)))
	want := []string{"什么！？", "下一句。", "他说“好的。”", "然后离开。"}
	if got := rangeTexts(text, ranges); !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges = %#v, want %#v", got, want)
	}
}

func TestSentenceRangesSkipsWhitespaceOnlyRanges(t *testing.T) {
	text := "\n\n。\n\n"
	ranges := SentenceRanges(text, 0, len([]rune(text)))
	if got := rangeTexts(text, ranges); !reflect.DeepEqual(got, []string{"\n\n。"}) {
		t.Fatalf("ranges = %#v", got)
	}
}

func TestSentenceRangesEmptyText(t *testing.T) {
	if ranges := SentenceRanges("", 0, 0); len(ranges) != 0 {
		t.Fatalf("ranges = %v", ranges)
	}
}

func TestParagraphRangesBlankLineSeparators(t *testing.T) {
	text := "first\n\nsecond\n \n\nthird"
	ranges := ParagraphRanges(text, 0, len([]rune(text)))
	if got := rangeTexts(text, ranges); !reflect.DeepEqual(got, []string{"first", "second", "third"}) {
		t.Fatalf("ranges = %#v", got)
	}
	// Single newlines are not paragraph separators, and leading spaces are kept.
	text = "line one\nline two\n\n  leading"
	ranges = ParagraphRanges(text, 0, len([]rune(text)))
	if got := rangeTexts(text, ranges); !reflect.DeepEqual(got, []string{"line one\nline two", "  leading"}) {
		t.Fatalf("ranges = %#v", got)
	}
}

func TestStructureRangesMarkdownSections(t *testing.T) {
	spans := []document.Span{
		{Start: 0, End: 4, Block: document.Block{Text: "# H1", Kind: document.KindHeading, Headings: []string{"H1"}}, Paragraph: 1},
		{Start: 6, End: 10, Block: document.Block{Text: "para", Kind: document.KindParagraph, Headings: []string{"H1"}}, Paragraph: 2},
		{Start: 12, End: 18, Block: document.Block{Text: "## H2", Kind: document.KindHeading, Headings: []string{"H1", "H2"}}, Paragraph: 3},
		{Start: 20, End: 24, Block: document.Block{Text: "sub1", Kind: document.KindParagraph, Headings: []string{"H1", "H2"}}, Paragraph: 4},
		{Start: 26, End: 30, Block: document.Block{Text: "sub2", Kind: document.KindParagraph, Headings: []string{"H1", "H2"}}, Paragraph: 5},
		{Start: 32, End: 38, Block: document.Block{Text: "# Other", Kind: document.KindHeading, Headings: []string{"Other"}}, Paragraph: 6},
		{Start: 40, End: 44, Block: document.Block{Text: "last", Kind: document.KindParagraph, Headings: []string{"Other"}}, Paragraph: 7},
	}
	content := document.Content{Text: "0123456789012345678901234567890123456789012345", Spans: spans}
	groups := StructureRanges(content, document.SuffixMD)
	want := []Range{{Start: 0, End: 10}, {Start: 12, End: 30}, {Start: 32, End: 44}}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("groups = %v, want %v", groups, want)
	}
}

func TestStructureRangesTablesAndPages(t *testing.T) {
	tableSpans := []document.Span{
		{Start: 0, End: 5, Block: document.Block{Text: "a | b", Kind: document.KindTableRow, Table: 1}, Paragraph: 1},
		{Start: 7, End: 12, Block: document.Block{Text: "c | d", Kind: document.KindTableRow, Table: 1}, Paragraph: 2},
		{Start: 14, End: 20, Block: document.Block{Text: "plain", Kind: document.KindParagraph}, Paragraph: 3},
		{Start: 22, End: 27, Block: document.Block{Text: "e | f", Kind: document.KindTableRow, Table: 2}, Paragraph: 4},
	}
	content := document.Content{Text: strings.Repeat(" ", 27), Spans: tableSpans}
	groups := StructureRanges(content, document.SuffixDOCX)
	want := []Range{{Start: 0, End: 12}, {Start: 14, End: 20}, {Start: 22, End: 27}}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("docx groups = %v, want %v", groups, want)
	}

	pageSpans := []document.Span{
		{Start: 0, End: 5, Block: document.Block{Text: "p1", Kind: document.KindParagraph, Page: 1}, Paragraph: 1},
		{Start: 7, End: 12, Block: document.Block{Text: "p1b", Kind: document.KindParagraph, Page: 1}, Paragraph: 2},
		{Start: 14, End: 20, Block: document.Block{Text: "p2", Kind: document.KindParagraph, Page: 2}, Paragraph: 3},
	}
	content = document.Content{Text: strings.Repeat(" ", 20), Spans: pageSpans}
	groups = StructureRanges(content, document.SuffixPDF)
	want = []Range{{Start: 0, End: 12}, {Start: 14, End: 20}}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("pdf groups = %v, want %v", groups, want)
	}
}

func TestSplitContentTokenWindowsAndOverlap(t *testing.T) {
	text := strings.Repeat("a", 100)
	config, err := NewConfig("token", 10, 2)
	if err != nil {
		t.Fatalf("NewConfig() error = %v", err)
	}
	budget, err := NewTokenBudget(runeCounter{}, config)
	if err != nil {
		t.Fatalf("NewTokenBudget() error = %v", err)
	}
	content := document.Content{Text: text, Spans: []document.Span{{Start: 0, End: 100, Block: document.Block{Text: text, Kind: document.KindParagraph}}}}
	ranges, err := SplitContent(content, document.SuffixTXT, config, budget)
	if err != nil {
		t.Fatalf("SplitContent() error = %v", err)
	}
	if len(ranges) < 2 {
		t.Fatalf("ranges = %v", ranges)
	}
	if ranges[0] != (Range{Start: 0, End: 10}) {
		t.Fatalf("first range = %v", ranges[0])
	}
	if ranges[1] != (Range{Start: 8, End: 18}) {
		t.Fatalf("second range = %v", ranges[1])
	}
	if last := ranges[len(ranges)-1]; last.End != 100 {
		t.Fatalf("last range = %v", last)
	}
	for _, span := range ranges {
		if span.End-span.Start > 10 {
			t.Fatalf("range over budget: %v", span)
		}
	}
}

func TestSplitContentSentenceOverlapIsCompleteSentences(t *testing.T) {
	text := strings.Repeat("设备正常。请检查连接。随后重启。", 30)
	config, err := NewConfig("sentence", 32, 12)
	if err != nil {
		t.Fatalf("NewConfig() error = %v", err)
	}
	budget, err := NewTokenBudget(runeCounter{}, config)
	if err != nil {
		t.Fatalf("NewTokenBudget() error = %v", err)
	}
	content := document.Content{Text: text, Spans: []document.Span{{Start: 0, End: len([]rune(text)), Block: document.Block{Text: text, Kind: document.KindParagraph}}}}
	ranges, err := SplitContent(content, document.SuffixTXT, config, budget)
	if err != nil {
		t.Fatalf("SplitContent() error = %v", err)
	}
	if len(ranges) < 2 {
		t.Fatalf("ranges = %d", len(ranges))
	}
	runes := []rune(text)
	overlaps := 0
	for index := 1; index < len(ranges); index++ {
		previous, current := ranges[index-1], ranges[index]
		if current.Start >= previous.End {
			continue
		}
		overlap := string(runes[current.Start:previous.End])
		overlaps++
		if !strings.HasSuffix(overlap, "。") {
			t.Fatalf("overlap %q does not end on a complete sentence", overlap)
		}
		count := runeCounter{}
		if count.Count(overlap) > 12 {
			t.Fatalf("overlap %q has %d tokens", overlap, count.Count(overlap))
		}
	}
	if overlaps == 0 {
		t.Fatalf("expected sentence overlaps between chunks")
	}
}

func TestSplitContentOversizedSentenceFallsBackToWindows(t *testing.T) {
	text := strings.Repeat("无标点非常长的连续文字", 20)
	config, err := NewConfig("sentence", 16, 4)
	if err != nil {
		t.Fatalf("NewConfig() error = %v", err)
	}
	budget, err := NewTokenBudget(runeCounter{}, config)
	if err != nil {
		t.Fatalf("NewTokenBudget() error = %v", err)
	}
	content := document.Content{Text: text, Spans: []document.Span{{Start: 0, End: len([]rune(text)), Block: document.Block{Text: text, Kind: document.KindParagraph}}}}
	ranges, err := SplitContent(content, document.SuffixTXT, config, budget)
	if err != nil {
		t.Fatalf("SplitContent() error = %v", err)
	}
	if len(ranges) < 2 {
		t.Fatalf("ranges = %v", ranges)
	}
	for _, span := range ranges {
		if span.End-span.Start > 16 {
			t.Fatalf("window over budget: %v", span)
		}
	}
}

func TestSplitContentEmptyText(t *testing.T) {
	config, err := NewConfig("recursive", 32, 8)
	if err != nil {
		t.Fatalf("NewConfig() error = %v", err)
	}
	budget, err := NewTokenBudget(runeCounter{}, config)
	if err != nil {
		t.Fatalf("NewTokenBudget() error = %v", err)
	}
	ranges, err := SplitContent(document.Content{Text: "  \n\t "}, document.SuffixTXT, config, budget)
	if err != nil {
		t.Fatalf("SplitContent() error = %v", err)
	}
	if len(ranges) != 0 {
		t.Fatalf("ranges = %v", ranges)
	}
}

func TestSourceMetadataShape(t *testing.T) {
	spans := []document.Span{
		{Start: 0, End: 6, Block: document.Block{Text: "alpha", Kind: document.KindHeading, Headings: []string{"H1"}}, Paragraph: 1},
		{Start: 8, End: 14, Block: document.Block{Text: "beta", Kind: document.KindParagraph, Page: 2, Headings: []string{"H1", "H2"}}, Paragraph: 2},
		{Start: 16, End: 22, Block: document.Block{Text: "gamma", Kind: document.KindTableRow, Page: 2, Headings: []string{"H1", "H2"}, Table: 3}, Paragraph: 3},
	}
	content := document.Content{Text: "alpha\n\nbeta\n\ngamma", Spans: spans}
	metadata := SourceMetadata(content, 3, 20)

	if metadata["start"] != 3 || metadata["end"] != 20 {
		t.Fatalf("start/end = %v/%v", metadata["start"], metadata["end"])
	}
	if !reflect.DeepEqual(metadata["pages"], []int{2}) {
		t.Fatalf("pages = %v", metadata["pages"])
	}
	if !reflect.DeepEqual(metadata["heading_paths"], [][]string{{"H1"}, {"H1", "H2"}}) {
		t.Fatalf("heading_paths = %v", metadata["heading_paths"])
	}
	locations := metadata["locations"].([]map[string]any)
	if len(locations) != 3 {
		t.Fatalf("locations = %v", locations)
	}
	expected := []map[string]any{
		{"start": 3, "end": 6, "paragraph": 1, "page": nil, "heading_path": []string{"H1"}, "kind": "heading", "table": nil},
		{"start": 8, "end": 14, "paragraph": 2, "page": 2, "heading_path": []string{"H1", "H2"}, "kind": "paragraph", "table": nil},
		{"start": 16, "end": 20, "paragraph": 3, "page": 2, "heading_path": []string{"H1", "H2"}, "kind": "table_row", "table": 3},
	}
	if !reflect.DeepEqual(locations, expected) {
		t.Fatalf("locations = %#v, want %#v", locations, expected)
	}
}

func TestSourceMetadataEmptyRange(t *testing.T) {
	content := document.Content{Text: "alpha", Spans: []document.Span{
		{Start: 0, End: 5, Block: document.Block{Text: "alpha", Kind: document.KindParagraph}, Paragraph: 1},
	}}
	metadata := SourceMetadata(content, 100, 110)
	if !reflect.DeepEqual(metadata["pages"], []int{}) {
		t.Fatalf("pages = %#v", metadata["pages"])
	}
	if !reflect.DeepEqual(metadata["heading_paths"], [][]string{}) {
		t.Fatalf("heading_paths = %#v", metadata["heading_paths"])
	}
	if locations := metadata["locations"].([]map[string]any); len(locations) != 0 {
		t.Fatalf("locations = %v", locations)
	}
}

func TestSourceMetadataEmptyHeadingPathIsEmptyList(t *testing.T) {
	content := document.Content{Text: "alpha", Spans: []document.Span{
		{Start: 0, End: 5, Block: document.Block{Text: "alpha", Kind: document.KindParagraph}, Paragraph: 1},
	}}
	metadata := SourceMetadata(content, 0, 5)
	locations := metadata["locations"].([]map[string]any)
	headings, ok := locations[0]["heading_path"].([]string)
	if !ok || headings == nil || len(headings) != 0 {
		t.Fatalf("heading_path = %#v (want a non-nil empty list)", locations[0]["heading_path"])
	}
	if locations[0]["page"] != nil || locations[0]["table"] != nil {
		t.Fatalf("optional page/table should be nil: %v", locations[0])
	}
}
