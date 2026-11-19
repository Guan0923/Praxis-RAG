package resources

import (
	"strings"
	"testing"
)

func TestDocumentURIRoundTrip(t *testing.T) {
	namespace := "user-0123456789abcdef"
	name := "X7 终端手册.md"
	uri := DocumentURI(namespace, name)
	if uri != "rag://knowledge/user-0123456789abcdef/X7%20%E7%BB%88%E7%AB%AF%E6%89%8B%E5%86%8C.md" {
		t.Fatalf("uri = %q", uri)
	}
	parsedName, page, version, err := ParseURI(namespace, uri)
	if err != nil {
		t.Fatalf("ParseURI() error = %v", err)
	}
	if parsedName != name || page != 1 || version != nil {
		t.Fatalf("parsed = %q, %d, %v", parsedName, page, version)
	}
}

func TestParseURIRejectsInvalidForms(t *testing.T) {
	namespace := "user-1"
	valid := DocumentURI(namespace, "guide.md")
	cases := []string{
		"http://knowledge/user-1/guide.md",
		"rag://knowledge/user-2/guide.md",
		"rag://knowledge/user-1/guide.md#fragment",
		"rag://knowledge/user-1/guide.md?page=2",
		"rag://knowledge/user-1/guide.md?page=0",
		"rag://knowledge/user-1/guide.md?page=02",
		"rag://knowledge/user-1/guide.md?page=1&page=2",
		"rag://knowledge/user-1/guide.md?page",
		"rag://knowledge/user-1/guide.md?other=1",
		"rag://knowledge/user-1/guide.md?page=1&version=",
		"rag://knowledge/user-1/%67uide.md",
	}
	for _, uri := range cases {
		if _, _, _, err := ParseURI(namespace, uri); err == nil {
			t.Fatalf("ParseURI(%q) accepted invalid URI", uri)
		}
	}
	if _, _, _, err := ParseURI(namespace, "rag://knowledge/user-1/../guide.md"); err == nil {
		t.Fatalf("path traversal accepted")
	}
	page2 := valid + "?page=2&version=v1"
	name, page, version, err := ParseURI(namespace, page2)
	if err != nil {
		t.Fatalf("versioned page rejected: %v", err)
	}
	if name != "guide.md" || page != 2 || version == nil || *version != "v1" {
		t.Fatalf("parsed = %q, %d, %v", name, page, version)
	}
}

func TestParagraphPages(t *testing.T) {
	if pages := ParagraphPages("", false); len(pages) != 1 || pages[0] != "" {
		t.Fatalf("empty pages = %q", pages)
	}
	if pages := ParagraphPages("short text", false); len(pages) != 1 || pages[0] != "short text" {
		t.Fatalf("short pages = %q", pages)
	}

	prefix := strings.Repeat("a", 5000)
	text := prefix + "\n\n" + strings.Repeat("b", 5000)
	pages := ParagraphPages(text, false)
	if len(pages) != 2 {
		t.Fatalf("page count = %d", len(pages))
	}
	if pages[0] != prefix+"\n\n" {
		t.Fatalf("first page does not end on the paragraph boundary")
	}
	if strings.Join(pages, "") != text {
		t.Fatalf("pages do not round-trip")
	}

	// Chinese text is split by code points, not bytes.
	chinese := strings.Repeat("中", 9000)
	pages = ParagraphPages(chinese, false)
	if len(pages) != 2 || len([]rune(pages[0])) != PageCharacters {
		t.Fatalf("chinese split wrong: %d pages, first has %d runes", len(pages), len([]rune(pages[0])))
	}
}

func TestParagraphPagesProtectsFormulaBlocks(t *testing.T) {
	// The formula spans the 8000-code-point page boundary, so the naive cut
	// would slice it in half and the protection has to move the cut.
	prefix := strings.Repeat("a", 7994) + "\n"
	formula := "$$\nE = mc^2\n$$\n"
	text := prefix + formula + strings.Repeat("b", 1000)
	pages := ParagraphPages(text, false)
	if len(pages) != 2 {
		t.Fatalf("page count = %d", len(pages))
	}
	if strings.Contains(pages[0], "$$") {
		t.Fatalf("formula leaked into the first page: %q", pages[0][len(pages[0])-40:])
	}
	if !strings.Contains(pages[1], formula) {
		t.Fatalf("formula block was split across pages: %q", pages[1][:40])
	}
	if strings.Join(pages, "") != text {
		t.Fatalf("pages do not round-trip")
	}
}

func TestParagraphPagesProtectsInlineMath(t *testing.T) {
	// The inline formula straddles the page boundary: with protection the cut
	// moves before it, without protection it is sliced mid-formula.
	prefix := strings.Repeat("a", 7997)
	text := prefix + "$x+y$" + strings.Repeat("b", 1000)
	pages := ParagraphPages(text, true)
	if len(pages) != 2 || strings.Contains(pages[0], "$") {
		t.Fatalf("protected inline math was split: %q", pages[0][len(pages[0])-20:])
	}
	if !strings.Contains(pages[1], "$x+y$") {
		t.Fatalf("inline math not kept whole on the second page: %q", pages[1][:20])
	}
	unprotected := ParagraphPages(text, false)
	if len(unprotected) < 2 || strings.Contains(unprotected[0], "$x+y$") {
		t.Fatalf("unprotected split must cut through the math: %q", unprotected[0][len(unprotected[0])-20:])
	}
}

func TestResourcePage(t *testing.T) {
	version := "v1"
	page, err := ResourcePage("user-1", "guide.md", "hello", version, 1, &version)
	if err != nil {
		t.Fatalf("ResourcePage() error = %v", err)
	}
	if page["text"] != "hello" || page["page"] != 1 || page["total_pages"] != 1 {
		t.Fatalf("page = %v", page)
	}
	if page["has_more"] != false || page["next_uri"] != nil {
		t.Fatalf("final page = %v", page)
	}
	stale := "v0"
	if _, err := ResourcePage("user-1", "guide.md", "hello", version, 1, &stale); err == nil || err.Error() != "Document changed; read its document URI again" {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ResourcePage("user-1", "guide.md", "hello", version, 2, nil); err == nil || err.Error() != "Page does not exist" {
		t.Fatalf("unexpected error: %v", err)
	}

	longText := strings.Repeat("a", 9000)
	multi, err := ResourcePage("user-1", "guide.md", longText, version, 1, nil)
	if err != nil {
		t.Fatalf("ResourcePage() error = %v", err)
	}
	next, ok := multi["next_uri"].(string)
	if !ok || next != DocumentURI("user-1", "guide.md")+"?page=2&version=v1" {
		t.Fatalf("next_uri = %v", multi["next_uri"])
	}
}
