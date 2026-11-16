package feishu

import (
	"strings"
	"testing"
)

func TestDocumentFilename(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"项目周报", "项目周报.md"},
		{"项目/周报:2026", "项目_周报_2026.md"},
		{"  .hidden. ", "hidden.md"},
		{"already.md", "already.md"},
		{"CON", "_CON.md"},
		{"", "飞书文档.md"},
		{"...", "飞书文档.md"},
	}
	for _, testCase := range cases {
		if got := DocumentFilename(testCase.title); got != testCase.want {
			t.Fatalf("DocumentFilename(%q) = %q, want %q", testCase.title, got, testCase.want)
		}
	}
}

func TestDocumentFilenameTruncatesOnRuneBoundary(t *testing.T) {
	title := strings.Repeat("飞", 300)
	name := DocumentFilename(title)
	if len(name) > 200 {
		t.Fatalf("filename is %d bytes, want <= 200", len(name))
	}
	if !strings.HasSuffix(name, ".md") {
		t.Fatalf("filename %q does not end in .md", name)
	}
	if !strings.HasPrefix(name, "飞飞") {
		t.Fatalf("filename lost its leading runes: %q", name)
	}
}
