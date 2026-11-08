package document

import (
	"errors"
	"strings"
	"testing"
)

// mathXML wraps an OMML fragment in an m:oMath element, mirroring
// tests/test_word_fidelity.py::math_xml.
func mathXML(inner string) []byte {
	return []byte(`<m:oMath xmlns:m="` + nsMath + `">` + inner + `</m:oMath>`)
}

func mustFormulaLatex(t *testing.T, inner string) string {
	t.Helper()
	latex, err := ConvertOMML(mathXML(inner))
	if err != nil {
		t.Fatalf("ConvertOMML(%s) error = %v", inner, err)
	}
	return latex
}

func TestFormulaCommonStructures(t *testing.T) {
	cases := []struct {
		name  string
		inner string
		want  string
	}{
		{
			name:  "fraction",
			inner: `<m:f><m:num><m:r><m:t>x</m:t></m:r></m:num><m:den><m:r><m:t>2</m:t></m:r></m:den></m:f>`,
			want:  `\frac{x}{2}`,
		},
		{
			name:  "superscript",
			inner: `<m:sSup><m:e><m:r><m:t>x</m:t></m:r></m:e><m:sup><m:r><m:t>2</m:t></m:r></m:sup></m:sSup>`,
			want:  `{x}^{2}`,
		},
		{
			name:  "subscript",
			inner: `<m:sSub><m:e><m:r><m:t>x</m:t></m:r></m:e><m:sub><m:r><m:t>i</m:t></m:r></m:sub></m:sSub>`,
			want:  `{x}_{i}`,
		},
		{
			name:  "subscript and superscript",
			inner: `<m:sSubSup><m:e><m:r><m:t>x</m:t></m:r></m:e><m:sub><m:r><m:t>i</m:t></m:r></m:sub><m:sup><m:r><m:t>2</m:t></m:r></m:sup></m:sSubSup>`,
			want:  `{x}_{i}^{2}`,
		},
		{
			name:  "radical",
			inner: `<m:rad><m:e><m:r><m:t>x</m:t></m:r></m:e></m:rad>`,
			want:  `\sqrt{x}`,
		},
		{
			name:  "matrix",
			inner: `<m:m><m:mr><m:e><m:r><m:t>1</m:t></m:r></m:e><m:e><m:r><m:t>2</m:t></m:r></m:e></m:mr></m:m>`,
			want:  `\begin{matrix} 1 & 2 \end{matrix}`,
		},
		{
			name:  "double struck run",
			inner: `<m:r><m:rPr><m:scr m:val="double-struck"/></m:rPr><m:t>R</m:t></m:r>`,
			want:  `\mathbb{R}`,
		},
		{
			name:  "bold run",
			inner: `<m:r><m:rPr><m:sty m:val="b"/></m:rPr><m:t>v</m:t></m:r>`,
			want:  `\mathbf{v}`,
		},
		{
			name:  "nary sum with limits",
			inner: `<m:nary><m:naryPr><m:chr m:val="∑"/></m:naryPr><m:sub><m:r><m:t>i</m:t></m:r></m:sub><m:sup><m:r><m:t>n</m:t></m:r></m:sup><m:e><m:r><m:t>x</m:t></m:r></m:e></m:nary>`,
			want:  `\sum_{i}^{n}{x}`,
		},
		{
			name:  "empty numerator placeholder",
			inner: `<m:f><m:num><m:r><m:t/></m:r></m:num><m:den><m:r><m:t>2</m:t></m:r></m:den></m:f>`,
			want:  `\frac{}{2}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := mustFormulaLatex(t, testCase.inner); got != testCase.want {
				t.Fatalf("latex = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestFormulaRejectsUnknownAndIncompleteStructures(t *testing.T) {
	_, err := ConvertOMML(mathXML(`<m:unknown><m:r><m:t>x</m:t></m:r></m:unknown>`))
	if err == nil || !strings.Contains(err.Error(), "Unsupported Word formula structure") {
		t.Fatalf("unknown structure error = %v", err)
	}
	_, err = ConvertOMML(mathXML(`<m:f><m:num><m:r><m:t>x</m:t></m:r></m:num></m:f>`))
	if err == nil || !strings.Contains(err.Error(), "Incomplete Word formula: f/den") {
		t.Fatalf("incomplete error = %v", err)
	}
	_, err = ConvertOMML(mathXML(`<m:nary><m:naryPr><m:chr m:val="?"/></m:naryPr><m:e><m:r><m:t>x</m:t></m:r></m:e></m:nary>`))
	if err == nil || !strings.Contains(err.Error(), "Unsupported Word formula operator: ?") {
		t.Fatalf("operator error = %v", err)
	}
	_, err = ConvertOMML(mathXML(`<m:r><m:rPr><m:scr m:val="bogus"/></m:rPr><m:t>R</m:t></m:r>`))
	if err == nil || !strings.Contains(err.Error(), "Unsupported styled Word formula alphabet") {
		t.Fatalf("alphabet error = %v", err)
	}
	if _, err := ConvertOMML([]byte("<not-omml/>")); err == nil ||
		!strings.Contains(err.Error(), "Word formula conversion failed") {
		t.Fatalf("non-OMML error = %v", err)
	}
	if _, err := ConvertOMML([]byte(`<m:oMath xmlns:m="` + nsMath + `"/>`)); err == nil ||
		!strings.Contains(err.Error(), "returned no text") {
		t.Fatalf("empty formula error = %v", err)
	}
}

// failingFormulaConverter exercises the injectable external-tool interface.
type failingFormulaConverter struct{ called bool }

func (f *failingFormulaConverter) ConvertOMML([]byte) (string, error) {
	f.called = true
	return "", errors.New("external formula tool unavailable")
}

func TestFormulaConverterIsInjectable(t *testing.T) {
	body := `<w:p><w:r><m:oMath><m:r><m:t>x</m:t></m:r></m:oMath></w:r></w:p>`
	path := buildDOCX(t, basePackage(body)...)
	converter := &failingFormulaConverter{}
	pkg, err := ParseWordPackage(path, ParseOptions{FormulaConverter: converter})
	if err == nil || !strings.Contains(err.Error(), "external formula tool unavailable") {
		t.Fatalf("ParseWordPackage() error = %v (pkg=%v)", err, pkg)
	}
	if !converter.called {
		t.Fatalf("custom formula converter was not used")
	}
}

func TestDOCXInlineFormulaIsRenderedInline(t *testing.T) {
	body := `<w:p><w:r><w:t>Equation </w:t></w:r><w:r><m:oMath><m:f>` +
		`<m:num><m:r><m:t>a</m:t></m:r></m:num><m:den><m:r><m:t>b</m:t></m:r></m:den>` +
		`</m:f></m:oMath></w:r><w:r><w:t> follows.</w:t></w:r></w:p>`
	path := buildDOCX(t, basePackage(body)...)
	text, err := ReadText(path)
	if err != nil {
		t.Fatalf("ReadText() error = %v", err)
	}
	if text != `Equation $\frac{a}{b}$ follows.` {
		t.Fatalf("text = %q", text)
	}
}

func TestSerializeOMMLRoundTrip(t *testing.T) {
	data := mathXML(`<m:f><m:num><m:r><m:rPr><m:scr m:val="double-struck"/></m:rPr><m:t>R</m:t></m:r></m:num><m:den><m:r><m:t>2</m:t></m:r></m:den></m:f>`)
	node, err := parseXMLNode(data)
	if err != nil {
		t.Fatalf("parseXMLNode() error = %v", err)
	}
	serialized, err := serializeOMML(node)
	if err != nil {
		t.Fatalf("serializeOMML() error = %v", err)
	}
	if !strings.Contains(string(serialized), `xmlns:m="`+nsMath+`"`) {
		t.Fatalf("serialized OMML = %s", serialized)
	}
	latex, err := ConvertOMML(serialized)
	if err != nil {
		t.Fatalf("ConvertOMML(round trip) error = %v", err)
	}
	if latex != `\frac{\mathbb{R}}{2}` {
		t.Fatalf("latex = %q", latex)
	}
}

func TestEscapeFormulaText(t *testing.T) {
	if got := escapeFormulaText("a_b%c"); got != `a\_b\%c` {
		t.Fatalf("escaped = %q", got)
	}
}
