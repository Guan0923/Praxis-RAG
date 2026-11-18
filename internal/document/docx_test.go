package document

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// wordNamespaces declares every prefix used by the test fixtures, matching
// word_reader.NS.
const wordNamespaces = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"` +
	` xmlns:m="http://schemas.openxmlformats.org/officeDocument/2006/math"` +
	` xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"` +
	` xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"` +
	` xmlns:v="urn:schemas-microsoft-com:vml"` +
	` xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing"` +
	` xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006"`

type docxEntry struct {
	name string
	body string
}

// buildDOCX writes an in-memory OOXML package, the Go equivalent of
// python-docx's Document().save() used by the Python tests.
func buildDOCX(t *testing.T, entries ...docxEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.docx")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create docx: %v", err)
	}
	writer := zip.NewWriter(file)
	for _, entry := range entries {
		handle, err := writer.Create(entry.name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", entry.name, err)
		}
		if _, err := handle.Write([]byte(entry.body)); err != nil {
			t.Fatalf("write zip entry %s: %v", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close docx: %v", err)
	}
	return path
}

func contentTypesXML(extraDefaults ...docxEntry) string {
	defaults := `<Default Extension="xml" ContentType="application/xml"/>` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="png" ContentType="image/png"/>`
	for _, entry := range extraDefaults {
		defaults += `<Default Extension="` + entry.name + `" ContentType="` + entry.body + `"/>`
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` + defaults + `</Types>`
}

func documentXML(body string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document ` + wordNamespaces + `><w:body>` + body + `</w:body></w:document>`
}

func basePackage(body string, extra ...docxEntry) []docxEntry {
	entries := []docxEntry{
		{name: "[Content_Types].xml", body: contentTypesXML()},
		{name: "word/document.xml", body: documentXML(body)},
	}
	return append(entries, extra...)
}

func stylesXML() docxEntry {
	return docxEntry{name: "word/styles.xml", body: `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:styles ` + wordNamespaces + `>` +
		`<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:basedOn w:val="Heading1"/></w:style>` +
		`</w:styles>`}
}

func headingParagraph(text string, level int) string {
	return `<w:p><w:pPr><w:pStyle w:val="Heading` + strconv.Itoa(level) + `"/></w:pPr>` +
		`<w:r><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
}

func paragraphXML(text string) string {
	return `<w:p><w:r><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
}

func tableXML(rows ...[]string) string {
	var builder strings.Builder
	builder.WriteString(`<w:tbl><w:tblGrid>`)
	if len(rows) > 0 {
		for index := 0; index < len(rows[0]); index++ {
			builder.WriteString(`<w:gridCol/>`)
		}
	}
	builder.WriteString(`</w:tblGrid>`)
	for _, row := range rows {
		builder.WriteString(`<w:tr>`)
		for _, cell := range row {
			builder.WriteString(`<w:tc><w:p><w:r><w:t xml:space="preserve">` + cell + `</w:t></w:r></w:p></w:tc>`)
		}
		builder.WriteString(`</w:tr>`)
	}
	builder.WriteString(`</w:tbl>`)
	return builder.String()
}

func relationshipsXML(relations ...string) docxEntry {
	var builder strings.Builder
	builder.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for _, relation := range relations {
		builder.WriteString(relation)
	}
	builder.WriteString(`</Relationships>`)
	return docxEntry{name: "word/_rels/document.xml.rels", body: builder.String()}
}

func relationshipXML(id, kind, target string) string {
	return `<Relationship Id="` + id + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/` +
		kind + `" Target="` + target + `"/>`
}

func TestDOCXParagraphsHeadingsAndTables(t *testing.T) {
	body := headingParagraph("X7 终端故障手册", 1) +
		paragraphXML("E03 表示温度传感器异常。") +
		tableXML([]string{"错误码", "处理方法"}, []string{"E03", "检查传感器连接"})
	path := buildDOCX(t, basePackage(body, stylesXML())...)

	text, err := ReadText(path)
	if err != nil {
		t.Fatalf("ReadText() error = %v", err)
	}
	want := "X7 终端故障手册\n\nE03 表示温度传感器异常。\n\n错误码 | 处理方法\nE03 | 检查传感器连接"
	if text != want {
		t.Fatalf("ReadText() = %q, want %q", text, want)
	}

	content := mustReadDocument(t, path)
	if len(content.Spans) != 4 {
		t.Fatalf("spans = %d, want 4: %+v", len(content.Spans), content.Spans)
	}
	if content.Spans[0].Block.Kind != KindHeading || len(content.Spans[0].Block.Headings) != 1 {
		t.Fatalf("heading span = %+v", content.Spans[0])
	}
	for _, index := range []int{2, 3} {
		if content.Spans[index].Block.Kind != KindTableRow {
			t.Fatalf("span %d kind = %q", index, content.Spans[index].Block.Kind)
		}
		if content.Spans[index].Block.Table != 1 {
			t.Fatalf("span %d table = %d", index, content.Spans[index].Block.Table)
		}
		if strings.Join(content.Spans[index].Block.Headings, "\x00") != "X7 终端故障手册" {
			t.Fatalf("span %d headings = %v", index, content.Spans[index].Block.Headings)
		}
	}
	if !strings.Contains(content.Text, "错误码 | 处理方法\n\nE03 | 检查传感器连接") {
		t.Fatalf("document text table separation = %q", content.Text)
	}
}

func TestDOCXContentControlsNestedTablesAndGridSpan(t *testing.T) {
	nested := tableXML([]string{"Nested value"})
	outer := `<w:tbl><w:tblGrid><w:gridCol/><w:gridCol/><w:gridCol/></w:tblGrid>` +
		`<w:tr><w:tc><w:p><w:r><w:t>Item</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>Minimum</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>Maximum</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:p><w:r><w:t>Pressure</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p/></w:tc>` +
		`<w:tc><w:p><w:r><w:t>100</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:tcPr><w:gridSpan w:val="2"/></w:tcPr><w:p><w:r><w:t>Merged</w:t></w:r></w:p></w:tc>` +
		`<w:tc>` + nested + `</w:tc></w:tr></w:tbl>`
	body := headingParagraph("Manual", 1) +
		`<w:sdt><w:sdtPr/><w:sdtContent>` + outer + `</w:sdtContent></w:sdt>`
	path := buildDOCX(t, basePackage(body, stylesXML())...)

	text, err := ReadText(path)
	if err != nil {
		t.Fatalf("ReadText() error = %v", err)
	}
	for _, expected := range []string{
		"Item | Minimum | Maximum",
		"Pressure |  | 100",
		"Merged |  | ",
		"Nested value",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("text missing %q: %q", expected, text)
		}
	}
	// The nested table is a separate asset, so _join_docx separates it from the
	// outer table rows with a blank line (same as Python).
	if !strings.Contains(text, "Merged |  | \n\nNested value") {
		t.Fatalf("nested table order = %q", text)
	}
}

func TestDOCXVerticalMergeKeepsAnchor(t *testing.T) {
	table := `<w:tbl><w:tblGrid><w:gridCol/><w:gridCol/></w:tblGrid>` +
		`<w:tr><w:tc><w:tcPr><w:vMerge w:val="restart"/></w:tcPr><w:p><w:r><w:t>Only once</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p/></w:tc></w:tr>` +
		`<w:tr><w:tc><w:tcPr><w:vMerge/></w:tcPr><w:p/></w:tc>` +
		`<w:tc><w:p/></w:tc></w:tr>` +
		`<w:tr><w:tc><w:tcPr><w:vMerge/></w:tcPr><w:p/></w:tc>` +
		`<w:tc><w:p/></w:tc></w:tr></w:tbl>`
	path := buildDOCX(t, basePackage(table)...)
	text, err := ReadText(path)
	if err != nil {
		t.Fatalf("ReadText() error = %v", err)
	}
	if !strings.Contains(text, "Only once | ") {
		t.Fatalf("text = %q", text)
	}
	if strings.Count(text, "Only once") != 1 {
		t.Fatalf("anchor text repeated: %q", text)
	}
}

func TestDOCXMalformedPackages(t *testing.T) {
	broken := writeTemp(t, "broken.docx", []byte("not a zip archive"))
	if _, err := ReadText(broken); err == nil || err.Error() != "Word 文件损坏或格式无效：broken.docx" {
		t.Fatalf("broken docx error = %v", err)
	}

	missingDocument := buildDOCX(t, docxEntry{name: "[Content_Types].xml", body: contentTypesXML()})
	_, err := ReadText(missingDocument)
	if err == nil || !strings.Contains(err.Error(), "Word 解析失败") {
		t.Fatalf("missing document.xml error = %v", err)
	}

	missingBody := buildDOCX(t,
		docxEntry{name: "[Content_Types].xml", body: contentTypesXML()},
		docxEntry{name: "word/document.xml", body: `<?xml version="1.0"?><w:document ` + wordNamespaces + `/>`},
	)
	_, err = ReadText(missingBody)
	if err == nil || !strings.Contains(err.Error(), "Word document has no body") {
		t.Fatalf("missing body error = %v", err)
	}

	missingTypes := buildDOCX(t, docxEntry{name: "word/document.xml", body: documentXML(paragraphXML("text"))})
	_, err = ReadText(missingTypes)
	if err == nil || !strings.Contains(err.Error(), "Word 解析失败") {
		t.Fatalf("missing content types error = %v", err)
	}

	invalidXML := buildDOCX(t,
		docxEntry{name: "[Content_Types].xml", body: contentTypesXML()},
		docxEntry{name: "word/document.xml", body: `<w:document`},
	)
	_, err = ReadText(invalidXML)
	if err == nil || !strings.Contains(err.Error(), "Word 解析失败") {
		t.Fatalf("invalid XML error = %v", err)
	}
}

func TestParseWordPackageStructuralErrors(t *testing.T) {
	missing := buildDOCX(t, docxEntry{name: "[Content_Types].xml", body: contentTypesXML()})
	_, err := ParseWordPackage(missing)
	want := "Word document is invalid or incomplete: fixture.docx: missing Word part: word/document.xml"
	if err == nil || err.Error() != want {
		t.Fatalf("missing part error = %v, want %q", err, want)
	}
	invalid := buildDOCX(t,
		docxEntry{name: "[Content_Types].xml", body: contentTypesXML()},
		docxEntry{name: "word/document.xml", body: `<w:document`},
	)
	_, err = ParseWordPackage(invalid)
	if err == nil || !strings.Contains(err.Error(), "Word document is invalid or incomplete: fixture.docx") {
		t.Fatalf("malformed XML error = %v", err)
	}
}

func TestDOCXRejectsUnsupportedStructures(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		message string
	}{
		{
			name:    "symbol font",
			body:    `<w:p><w:r><w:sym w:font="Wingdings" w:char="F0E0"/></w:r></w:p>`,
			message: "Unsupported legacy symbol font",
		},
		{
			name:    "chart",
			body:    `<w:p><w:r><w:drawing><wp:inline><a:graphic><a:graphicData uri="chart"><a:chart/></a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p>`,
			message: "Unsupported embedded chart or diagram",
		},
		{
			name:    "ole object",
			body:    `<w:p><w:r><w:object/></w:r></w:p>`,
			message: "Unsupported embedded OLE object",
		},
		{
			name:    "altChunk",
			body:    `<w:altChunk r:id="rId9"/>`,
			message: "Unsupported embedded Word content",
		},
		{
			name:    "unsupported formula structure",
			body:    `<w:p><w:r><m:oMath><m:unknown><m:r><m:t>x</m:t></m:r></m:unknown></m:oMath></w:r></w:p>`,
			message: "Unsupported Word formula structure",
		},
		{
			name: "horizontal merge",
			body: `<w:tbl><w:tblGrid><w:gridCol/></w:tblGrid><w:tr><w:tc>` +
				`<w:tcPr><w:hMerge/></w:tcPr><w:p><w:r><w:t>x</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`,
			message: "Unsupported legacy horizontal table merge",
		},
		{
			name: "broken vertical merge",
			body: `<w:tbl><w:tblGrid><w:gridCol/></w:tblGrid><w:tr><w:tc>` +
				`<w:tcPr><w:vMerge/></w:tcPr><w:p/></w:tc></w:tr></w:tbl>`,
			message: "Broken vertical table merge",
		},
		{
			name:    "empty table",
			body:    `<w:tbl><w:tblGrid><w:gridCol/></w:tblGrid></w:tbl>`,
			message: "Empty Word table",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := buildDOCX(t, basePackage(testCase.body)...)
			_, err := ReadText(path)
			if err == nil || !strings.Contains(err.Error(), testCase.message) {
				t.Fatalf("error = %v, want containing %q", err, testCase.message)
			}
		})
	}
}

func TestDOCXArchiveBounds(t *testing.T) {
	path := buildDOCX(t, basePackage(paragraphXML("bounded"))...)

	t.Run("member count", func(t *testing.T) {
		overrideInt(t, &MaxDOCXMembers, 1)
		_, err := ReadText(path)
		if err == nil || err.Error() != "Word 文件内部条目过多：fixture.docx" {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("uncompressed size", func(t *testing.T) {
		overrideInt64(t, &MaxDOCXUncompressedBytes, 1)
		_, err := ReadText(path)
		if err == nil || !strings.Contains(err.Error(), "Word 解压后不能超过 0 MiB") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("encrypted flag", func(t *testing.T) {
		encryptedPath := buildDOCX(t, basePackage(paragraphXML("bounded"))...)
		markZipEncrypted(t, encryptedPath)
		_, err := ReadText(encryptedPath)
		if err == nil || err.Error() != "Word 文件已加密，暂不支持需要密码的文件：fixture.docx" {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("extracted characters", func(t *testing.T) {
		overrideInt(t, &MaxExtractedCharacters, 5)
		_, err := ReadText(path)
		if err == nil || !strings.Contains(err.Error(), "Word extracted text exceeds the size limit") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestDOCXPicturePlaceholder(t *testing.T) {
	drawing := `<w:p><w:r><w:drawing><wp:inline><a:graphic>` +
		`<a:graphicData uri="image"><a:blip r:embed="rId1"/></a:graphicData>` +
		`</a:graphic></wp:inline></w:drawing></w:r></w:p>`
	packageEntries := []docxEntry{
		{name: "[Content_Types].xml", body: contentTypesXML()},
		{name: "word/document.xml", body: documentXML(drawing)},
		relationshipsXML(relationshipXML("rId1", "image", "media/image1.png")),
		{name: "word/media/image1.png", body: "PNGDATA"},
	}
	path := buildDOCX(t, packageEntries...)
	text, err := ReadText(path)
	if err != nil {
		t.Fatalf("ReadText() error = %v", err)
	}
	if !strings.Contains(text, "[Image: picture-1]") {
		t.Fatalf("text = %q", text)
	}

	t.Run("external image", func(t *testing.T) {
		external := []docxEntry{
			{name: "[Content_Types].xml", body: contentTypesXML()},
			{name: "word/document.xml", body: documentXML(drawing)},
			{name: "word/_rels/document.xml.rels", body: `<?xml version="1.0"?>` +
				`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
				`<Relationship Id="rId1" Type="image" Target="http://example.com/x.png" TargetMode="External"/>` +
				`</Relationships>`},
		}
		path := buildDOCX(t, external...)
		_, err := ReadText(path)
		if err == nil || !strings.Contains(err.Error(), "Missing or external Word image") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("unsupported content type", func(t *testing.T) {
		unsupported := []docxEntry{
			{name: "[Content_Types].xml", body: contentTypesXML(docxEntry{name: "bin", body: "application/octet-stream"})},
			{name: "word/document.xml", body: documentXML(drawing)},
			relationshipsXML(relationshipXML("rId1", "image", "media/data.bin")),
			{name: "word/media/data.bin", body: "BINARY"},
		}
		path := buildDOCX(t, unsupported...)
		_, err := ReadText(path)
		if err == nil || !strings.Contains(err.Error(), "Unsupported image content type") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("empty image", func(t *testing.T) {
		empty := []docxEntry{
			{name: "[Content_Types].xml", body: contentTypesXML()},
			{name: "word/document.xml", body: documentXML(drawing)},
			relationshipsXML(relationshipXML("rId1", "image", "media/image1.png")),
			{name: "word/media/image1.png", body: ""},
		}
		path := buildDOCX(t, empty...)
		_, err := ReadText(path)
		if err == nil || !strings.Contains(err.Error(), "Empty Word image") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestDOCXHeaderFooterAndFootnotes(t *testing.T) {
	body := paragraphXML("Body text") +
		`<w:p><w:r><w:t>Referenced</w:t></w:r><w:r><w:footnoteReference w:id="1"/></w:r></w:p>` +
		`<w:sectPr><w:headerReference w:type="default" r:id="rId2"/></w:sectPr>`
	header := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:hdr ` + wordNamespaces + `>` + paragraphXML("Header text") + `</w:hdr>`
	footnotes := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:footnotes ` + wordNamespaces + `>` +
		`<w:footnote w:id="1"><w:p><w:r><w:t>Note text</w:t></w:r></w:p></w:footnote>` +
		`</w:footnotes>`
	entries := []docxEntry{
		{name: "[Content_Types].xml", body: contentTypesXML()},
		{name: "word/document.xml", body: documentXML(body)},
		{name: "word/_rels/document.xml.rels", body: `<?xml version="1.0"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId2" Type="header" Target="header1.xml"/>` +
			`</Relationships>`},
		{name: "word/header1.xml", body: header},
		{name: "word/footnotes.xml", body: footnotes},
	}
	path := buildDOCX(t, entries...)
	text, err := ReadText(path)
	if err != nil {
		t.Fatalf("ReadText() error = %v", err)
	}
	for _, expected := range []string{"Body text", "Referenced [footnote 1]", "Note text", "Header text"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("text missing %q: %q", expected, text)
		}
	}
	if strings.Count(text, "Note text") != 1 {
		t.Fatalf("footnote visited twice: %q", text)
	}
	if !strings.Contains(text, "Referenced [footnote 1]\n\nNote text") {
		t.Fatalf("footnote order = %q", text)
	}
	if !strings.Contains(text, "Header text") {
		t.Fatalf("header text missing: %q", text)
	}
}

func TestDOCXTableHeaderRowsAndGridBeforeAfter(t *testing.T) {
	table := `<w:tbl><w:tblGrid><w:gridCol/><w:gridCol/></w:tblGrid>` +
		`<w:tr><w:trPr><w:tblHeader/></w:trPr><w:tc><w:p><w:r><w:t>A</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>B</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:trPr><w:gridBefore w:val="1"/><w:gridAfter w:val="1"/></w:trPr>` +
		`<w:tc><w:p><w:r><w:t>only</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`
	path := buildDOCX(t, basePackage(table)...)
	text, err := ReadText(path)
	if err != nil {
		t.Fatalf("ReadText() error = %v", err)
	}
	if !strings.Contains(text, "A | B") {
		t.Fatalf("header row = %q", text)
	}
	// gridBefore=1 shifts the single cell to column 1 and gridAfter pads to 3
	// columns; rows are padded to the widest row.
	if !strings.Contains(text, " | only | ") {
		t.Fatalf("grid offset row = %q", text)
	}
}

type fakeDOCConverter struct {
	target        string
	err           error
	called        bool
	cleanupCalled bool
}

func (f *fakeDOCConverter) Convert(_ context.Context, _ string) (string, func(), error) {
	f.called = true
	if f.err != nil {
		return "", nil, f.err
	}
	return f.target, func() { f.cleanupCalled = true }, nil
}

func TestLegacyDOCConversion(t *testing.T) {
	docxPath := buildDOCX(t, basePackage(paragraphXML("converted text"))...)
	docPath := writeTemp(t, "legacy.doc", []byte("dummy legacy bytes"))

	converter := &fakeDOCConverter{target: docxPath}
	reader := &Reader{DOC: converter}
	text, err := reader.ReadText(docPath)
	if err != nil {
		t.Fatalf("ReadText(.doc) error = %v", err)
	}
	if text != "converted text" {
		t.Fatalf("text = %q", text)
	}
	if !converter.called {
		t.Fatalf("converter was not called")
	}

	docPathAgain := writeTemp(t, "legacy2.doc", []byte("dummy"))
	converter = &fakeDOCConverter{target: docxPath}
	reader = &Reader{DOC: converter}
	content, err := reader.ReadDocument(docPathAgain)
	if err != nil {
		t.Fatalf("ReadDocument(.doc) error = %v", err)
	}
	if !converter.cleanupCalled {
		t.Fatalf("converter cleanup was not called")
	}
	if len(content.Spans) != 1 || content.Spans[0].Block.Text != "converted text" {
		t.Fatalf("content = %+v", content)
	}

	failing := &fakeDOCConverter{err: errors.New("conversion exploded")}
	_, err = (&Reader{DOC: failing}).ReadText(writeTemp(t, "legacy3.doc", []byte("dummy")))
	if err == nil || err.Error() != "Word 解析失败：legacy3.doc；conversion exploded" {
		t.Fatalf("conversion error = %v", err)
	}

	missingBinary := &SofficeConverter{Executable: "definitely-missing-soffice-binary"}
	_, err = (&Reader{DOC: missingBinary}).ReadText(writeTemp(t, "legacy4.doc", []byte("dummy")))
	if err == nil || !strings.Contains(err.Error(), "DOC converter could not start") {
		t.Fatalf("missing converter error = %v", err)
	}
}

// markZipEncrypted sets the encryption flag bit in every central directory
// header, which is what zip readers inspect.
func markZipEncrypted(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	patched := 0
	for index := 0; index+46 <= len(data); index++ {
		if binary.LittleEndian.Uint32(data[index:]) == 0x02014b50 {
			data[index+8] |= 0x1
			data[index+9] = 0
			patched++
			index += 45
		}
	}
	if patched == 0 {
		t.Fatalf("no central directory headers found")
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write encrypted zip: %v", err)
	}
}
