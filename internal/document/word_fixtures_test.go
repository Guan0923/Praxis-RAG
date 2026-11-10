package document

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// testJPEGBytes is a real decodable JPEG used by the Word asset tests and the
// vision describer tests (the reader itself does not decode images).
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

func pictureRun(relID, title string) string {
	label := ""
	if title != "" {
		label = ` title="` + title + `"`
	}
	return `<w:r><w:drawing><wp:inline><a:graphic><a:graphicData uri="image">` +
		`<a:blip r:embed="` + relID + `"/></a:graphicData></a:graphic>` +
		`<wp:docPr id="1" name="Picture 1"` + label + `/>` +
		`</wp:inline></w:drawing></w:r>`
}

func pictureParagraph(relID, title string) string {
	return `<w:p>` + pictureRun(relID, title) + `</w:p>`
}

// wordPicturePackage builds a DOCX with one jpeg image (rId1) available.
func wordPicturePackage(t *testing.T, body string) string {
	t.Helper()
	entries := []docxEntry{
		{name: "[Content_Types].xml", body: contentTypesXML(docxEntry{name: "jpeg", body: "image/jpeg"})},
		{name: "word/document.xml", body: documentXML(body)},
		relationshipsXML(relationshipXML("rId1", "image", "media/image1.jpeg")),
		{name: "word/media/image1.jpeg", body: string(testJPEGBytes)},
	}
	return buildDOCX(t, entries...)
}

// wordPictureStylesPackage is wordPicturePackage plus heading styles.
func wordPictureStylesPackage(t *testing.T, body string) string {
	t.Helper()
	entries := []docxEntry{
		{name: "[Content_Types].xml", body: contentTypesXML(docxEntry{name: "jpeg", body: "image/jpeg"})},
		{name: "word/document.xml", body: documentXML(body)},
		stylesXML(),
		relationshipsXML(relationshipXML("rId1", "image", "media/image1.jpeg")),
		{name: "word/media/image1.jpeg", body: string(testJPEGBytes)},
	}
	return buildDOCX(t, entries...)
}

// outerTableWithNested returns a 2x2 table whose last cell contains a 1x1
// nested table, mirroring test_word_fidelity.
func outerTableWithNested() string {
	nested := tableXML([]string{"Nested specification"})
	return `<w:tbl><w:tblGrid><w:gridCol/><w:gridCol/></w:tblGrid>` +
		`<w:tr><w:tc><w:p><w:r><w:t>Model</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>Maximum</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:p><w:r><w:t>ZQ-25</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>100</w:t></w:r></w:p>` + nested + `</w:tc></w:tr></w:tbl>`
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

func writeStagingZip(t *testing.T, path string, entries []docxEntry) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
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
		t.Fatalf("close zip file: %v", err)
	}
}
