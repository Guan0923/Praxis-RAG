package document

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wordNamespaces is the minimal OOXML namespace set a well-formed package
// fixture carries. Docling-side parsers (python-docx) require the main part
// override and the package relationships, so the fixture mirrors a real
// package shape even though Go never parses it.
const wordNamespaces = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"` +
	` xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`

const docxContentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
	`<Default Extension="xml" ContentType="application/xml"/>` +
	`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
	`<Default Extension="jpeg" ContentType="image/jpeg"/>` +
	`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>` +
	`</Types>`

const docxRootRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>` +
	`</Relationships>`

func docxDocumentXML(body string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document ` + wordNamespaces + `><w:body>` + body + `</w:body></w:document>`
}

func docxParagraphXML(text string) string {
	return `<w:p><w:r><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
}

// testJPEGBytes is a real decodable JPEG used by the Word asset staging and
// vision describer tests. Neither the Docling client nor the asset pipeline
// decodes it in production; only the vision worker does at test time.
var testJPEGBytes = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 60, B: 60, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, nil); err != nil {
		panic(err)
	}
	return buffer.Bytes()
}()

// writeStagingZipPackage writes a minimal valid DOCX package used as the
// upload source for the Docling service client tests.
func writeStagingZipPackage(t *testing.T, path string) string {
	t.Helper()
	entries := []struct{ name, body string }{
		{"[Content_Types].xml", docxContentTypes},
		{"_rels/.rels", docxRootRels},
		{"word/document.xml", docxDocumentXML(docxParagraphXML("正文"))},
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir docx source: %v", err)
	}
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

// stagingWordPackage builds a Docling-style WordPackage manifest with one
// uncaptioned picture staged to dir, mirroring what a Docling parse returns.
func stagingWordPackage(t *testing.T, dir string) *WordPackage {
	t.Helper()
	imageFile := "images/picture-1.jpeg"
	location := WordLocation{Part: "body", Paragraph: 1, Scope: "body", Section: 1}
	manifest := WordManifest{
		Version: DoclingWordParserVersion,
		Blocks: []WordManifestBlock{
			{Ref: "#/pictures/0", Order: 0, Kind: KindPicture, AssetID: "picture-1", Location: location},
			{Ref: "#/texts/0", Order: 1, Kind: KindParagraph, Text: "Body text", Location: location},
		},
		Assets: []*WordManifestAsset{
			{AssetID: "picture-1", Kind: "picture", ImageFile: &imageFile,
				MIMEType: stringPtr("image/jpeg"), Location: location},
		},
		Diagnostics: []WordDiagnostic{},
	}
	pkg := &WordPackage{Manifest: manifest, Images: map[string][]byte{"picture-1": testJPEGBytes}}
	if err := pkg.WriteStaging(dir); err != nil {
		t.Fatalf("WriteStaging() error = %v", err)
	}
	return pkg
}

func writeStagingFile(t *testing.T, dir, relative string, data []byte) {
	t.Helper()
	target := filepath.Join(dir, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatalf("write staging file: %v", err)
	}
}

// assertContains fails with a readable diff message.
func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("text %q does not contain %q", haystack, needle)
	}
}
