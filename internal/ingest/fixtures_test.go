package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/document"
)

const testWordNamespaces = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"` +
	` xmlns:m="http://schemas.openxmlformats.org/officeDocument/2006/math"` +
	` xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"` +
	` xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"` +
	` xmlns:v="urn:schemas-microsoft-com:vml"` +
	` xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing"` +
	` xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006"`

// testJPEGBytes is a real decodable JPEG embedded in the Word fixtures (the
// vision describer decodes it).
var testJPEGBytes = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.RGBA{R: 20, G: 160, B: 90, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, nil); err != nil {
		panic(err)
	}
	return buffer.Bytes()
}()

type docxEntry struct {
	name string
	body string
}

func buildDocx(t *testing.T, entries ...docxEntry) string {
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
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := handle.Write([]byte(entry.body)); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close docx: %v", err)
	}
	return path
}

func contentTypesXML(extra ...docxEntry) string {
	defaults := `<Default Extension="xml" ContentType="application/xml"/>` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="jpeg" ContentType="image/jpeg"/>` +
		`<Default Extension="png" ContentType="image/png"/>`
	for _, entry := range extra {
		defaults += `<Default Extension="` + entry.name + `" ContentType="` + entry.body + `"/>`
	}
	// OOXML consumers (including python-docx, which Docling's Word backend
	// uses) identify the main part by its Override, not by the file extension.
	overrides := `<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>` +
		`<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>`
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` + defaults + overrides + `</Types>`
}

func documentXML(body string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document ` + testWordNamespaces + `><w:body>` + body + `</w:body></w:document>`
}

func paragraphXML(text string) string {
	return `<w:p><w:r><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
}

func headingParagraphXML(text string, level int) string {
	return `<w:p><w:pPr><w:pStyle w:val="Heading` + strconv.Itoa(level) + `"/></w:pPr>` +
		`<w:r><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
}

func stylesEntry() docxEntry {
	return docxEntry{name: "word/styles.xml", body: `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:styles ` + testWordNamespaces + `>` +
		`<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:basedOn w:val="Heading1"/></w:style>` +
		`</w:styles>`}
}

// rootRelsXML points the package at word/document.xml. Real Office packages
// always carry it, and OOXML consumers such as python-docx (used by Docling's
// Word backend) refuse a package without it.
func rootRelsXML() docxEntry {
	return docxEntry{name: "_rels/.rels", body: `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>` +
		`</Relationships>`}
}

func pictureRun(title string) string {
	label := ""
	if title != "" {
		label = ` title="` + title + `"`
	}
	return `<w:r><w:drawing><wp:inline><a:graphic><a:graphicData uri="image">` +
		`<a:blip r:embed="rId1"/></a:graphicData></a:graphic>` +
		`<wp:docPr id="1" name="Picture 1"` + label + `/>` +
		`</wp:inline></w:drawing></w:r>`
}

func pictureParagraph(title string) string {
	return `<w:p>` + pictureRun(title) + `</w:p>`
}

// plainDocxPackage builds a DOCX with no images.
func plainDocxPackage(t *testing.T, body string, styles bool) string {
	t.Helper()
	entries := []docxEntry{
		{name: "[Content_Types].xml", body: contentTypesXML()},
		rootRelsXML(),
		{name: "word/document.xml", body: documentXML(body)},
	}
	if styles {
		entries = append(entries, stylesEntry())
	}
	return buildDocx(t, entries...)
}

// pictureDocxPackage builds a DOCX with one jpeg image referenced by rId1.
func pictureDocxPackage(t *testing.T, body string, styles bool) string {
	t.Helper()
	entries := []docxEntry{
		{name: "[Content_Types].xml", body: contentTypesXML()},
		rootRelsXML(),
		{name: "word/document.xml", body: documentXML(body)},
		{name: "word/_rels/document.xml.rels", body: `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="media/image1.jpeg"/>` +
			`</Relationships>`},
		{name: "word/media/image1.jpeg", body: string(testJPEGBytes)},
	}
	if styles {
		entries = append(entries, stylesEntry())
	}
	return buildDocx(t, entries...)
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

func writeText(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// fakePDFParser writes worker-style artifacts into the staging dir and returns
// the configured manifest.
type fakePDFParser struct {
	manifest document.PDFManifest
	err      error
	calls    int
}

func (f *fakePDFParser) ParseInto(_ context.Context, _ string, staging string) (document.PDFManifest, error) {
	f.calls++
	if f.err != nil {
		return document.PDFManifest{}, f.err
	}
	if err := os.MkdirAll(filepath.Join(staging, "pictures"), 0o755); err != nil {
		return document.PDFManifest{}, err
	}
	if err := os.WriteFile(filepath.Join(staging, "pictures", "picture-0.png"), []byte("PNGDATA"), 0o644); err != nil {
		return document.PDFManifest{}, err
	}
	raw, err := json.Marshal(f.manifest)
	if err != nil {
		return document.PDFManifest{}, err
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), raw, 0o644); err != nil {
		return document.PDFManifest{}, err
	}
	return f.manifest, nil
}

// pdfManifestFixture mirrors the minimal remote-service manifest used by the chunk tests.
func pdfManifestFixture() document.PDFManifest {
	blocks := []document.PDFBlock{
		{Ref: "text-1", Kind: "text", Page: 1, Order: 0, Text: "First page paragraph."},
		{Ref: "#/pictures/0", Kind: "picture", Page: 1, Order: 1, AssetID: "#/pictures/0"},
		{Ref: "caption-1", Kind: "caption", Page: 1, Order: 2, Text: "Fig. 1"},
		{Ref: "formula-1", Kind: "formula", Page: 2, Order: 3, Text: "$$\n\\frac{a}{b}\n$$", LaTeX: `\frac{a}{b}`},
		{Ref: "#/tables/0", Kind: "table", Page: 2, Order: 4, AssetID: "#/tables/0"},
	}
	for index := range blocks {
		blocks[index].Locations = []document.PDFLocation{{Page: blocks[index].Page}}
	}
	return document.PDFManifest{
		Version: document.PDFParserVersion,
		Blocks:  blocks,
		Assets: []document.PDFAsset{
			{AssetID: "#/pictures/0", Kind: "picture", Page: 1, Caption: "Fig. 1", CaptionSource: "document",
				ImageFile: "pictures/picture-0.png"},
			{AssetID: "#/tables/0", Kind: "table", Page: 2,
				Table: &document.PDFTable{
					NumRows: 2, NumCols: 2,
					Headers:    []string{"A", "B"},
					HeaderRows: [][]string{{"A", "B"}},
					Rows:       [][]string{{"A", "B"}, {"1", "2"}},
				}},
		},
	}
}

// fakeDOCConverter returns a prepared .docx path for a .doc source.
type fakeDOCConverter struct {
	target string
	called bool
}

func (f *fakeDOCConverter) Convert(_ context.Context, _ string) (string, func(), error) {
	f.called = true
	return f.target, func() {}, nil
}
