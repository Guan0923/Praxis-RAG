package document

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func wordAssetContent(t *testing.T) (*WordContent, *WordPackage) {
	t.Helper()
	body := paragraphXML("Before image") + pictureParagraph("rId1", "Figure 1") +
		paragraphXML("Figure 1: Red status lamp") + paragraphXML("Note: keep this lamp visible") +
		paragraphXML("Tail")
	path := wordPicturePackage(t, body)
	pkg, err := ParseWordPackage(path)
	if err != nil {
		t.Fatalf("ParseWordPackage() error = %v", err)
	}
	content, err := BuildWordContent("doc-abc", pkg)
	if err != nil {
		t.Fatalf("BuildWordContent() error = %v", err)
	}
	return content, pkg
}

func TestBuildWordContentAssetRecords(t *testing.T) {
	content, pkg := wordAssetContent(t)
	if len(content.Assets) != 1 {
		t.Fatalf("assets = %d", len(content.Assets))
	}
	asset := content.Assets[0]
	if asset.ID != "doc-abc:picture-1" || asset.SourceID != "picture-1" {
		t.Fatalf("ids = %q/%q", asset.ID, asset.SourceID)
	}
	if asset.Kind != "picture" || asset.Storage != "word" {
		t.Fatalf("kind/storage = %q/%q", asset.Kind, asset.Storage)
	}
	if asset.Name != "doc-abc/images/picture-1.jpeg" || asset.SourceName != "images/picture-1.jpeg" {
		t.Fatalf("names = %q/%q", asset.Name, asset.SourceName)
	}
	if asset.MIMEType != "image/jpeg" {
		t.Fatalf("mime = %q", asset.MIMEType)
	}
	if string(asset.Data) != string(testJPEGBytes) {
		t.Fatalf("data = %d bytes", len(asset.Data))
	}
	digest := sha256.Sum256(testJPEGBytes)
	if asset.SHA256 != hex.EncodeToString(digest[:]) || asset.Size != int64(len(testJPEGBytes)) {
		t.Fatalf("checksum/size = %q/%d", asset.SHA256, asset.Size)
	}
	if asset.Order != 0 || asset.Caption != "Figure 1: Red status lamp" {
		t.Fatalf("order/caption = %d/%q", asset.Order, asset.Caption)
	}
	if len(asset.Captions) != 1 || asset.Captions[0].Text != "Figure 1: Red status lamp" ||
		asset.Captions[0].Location == nil {
		t.Fatalf("captions = %+v", asset.Captions)
	}
	if len(asset.Footnotes) != 1 || asset.Footnotes[0].Text != "Note: keep this lamp visible" {
		t.Fatalf("footnotes = %+v", asset.Footnotes)
	}

	// BuildWordContent must not mutate the parser output retained by the caller.
	if pkg.Manifest.Assets[0].AssetID != "picture-1" {
		t.Fatalf("parser manifest was mutated: %q", pkg.Manifest.Assets[0].AssetID)
	}
	qualified := content.Manifest
	if qualified.Assets[0].AssetID != "doc-abc:picture-1" {
		t.Fatalf("qualified asset id = %q", qualified.Assets[0].AssetID)
	}
	if qualified.Assets[0].ImageFile == nil || *qualified.Assets[0].ImageFile != "doc-abc/images/picture-1.jpeg" {
		t.Fatalf("qualified image file = %v", qualified.Assets[0].ImageFile)
	}
	assetBlock := false
	for _, block := range qualified.Blocks {
		if block.AssetID == "doc-abc:picture-1" {
			assetBlock = true
		}
	}
	if !assetBlock {
		t.Fatalf("qualified blocks lost the asset id: %+v", qualified.Blocks)
	}
	if !strings.Contains(content.DocumentMarkdown, "[Image: doc-abc:picture-1]") ||
		!strings.Contains(content.DocumentMarkdown, "Figure 1: Red status lamp") ||
		!strings.Contains(content.DocumentMarkdown, "Note: keep this lamp visible") {
		t.Fatalf("document markdown = %q", content.DocumentMarkdown)
	}

	payload := asset.Payload("manual.docx")
	mustEqual := func(key string, want any) {
		t.Helper()
		if payload[key] != want {
			t.Fatalf("payload[%q] = %v, want %v", key, payload[key], want)
		}
	}
	mustEqual("record_type", "asset")
	mustEqual("document_id", "doc-abc")
	mustEqual("source", "manual.docx")
	mustEqual("asset_kind", "picture")
	mustEqual("asset_storage", "word")
	mustEqual("asset_id", "doc-abc:picture-1")
	mustEqual("image_file", "doc-abc/images/picture-1.jpeg")
	mustEqual("mime_type", "image/jpeg")
	mustEqual("caption", "Figure 1: Red status lamp")
	if payload["table"] != nil {
		t.Fatalf("picture table payload = %v", payload["table"])
	}
	locations := payload["location"].(map[string]any)
	if locations["part"] != "word/document.xml" || locations["scope"] != "body" {
		t.Fatalf("location payload = %v", locations)
	}
	if len(payload["captions"].([]map[string]any)) != 1 {
		t.Fatalf("caption payload = %v", payload["captions"])
	}
	if len(payload["labels"].([]string)) < 3 || len(payload["bookmarks"].([]string)) != 0 {
		t.Fatalf("labels/bookmarks = %v/%v", payload["labels"], payload["bookmarks"])
	}
}

func TestBuildWordContentNestedTableIdentity(t *testing.T) {
	body := paragraphXML("Table 1: Pump limits") + outerTableWithNested() + paragraphXML("Note: values in kPa")
	path := buildDOCX(t, basePackage(body)...)
	pkg, err := ParseWordPackage(path)
	if err != nil {
		t.Fatalf("ParseWordPackage() error = %v", err)
	}
	content, err := BuildWordContent("doc-t", pkg)
	if err != nil {
		t.Fatalf("BuildWordContent() error = %v", err)
	}
	if len(content.Assets) != 2 {
		t.Fatalf("assets = %d", len(content.Assets))
	}
	outer, nested := content.Assets[0], content.Assets[1]
	if outer.ID != "doc-t:table-1" || outer.Table == nil {
		t.Fatalf("outer = %+v", outer)
	}
	if outer.Table.Caption != "Table 1: Pump limits" {
		t.Fatalf("outer caption = %q", outer.Table.Caption)
	}
	if len(outer.Table.Footnotes) != 1 || outer.Table.Footnotes[0] != "Note: values in kPa" {
		t.Fatalf("outer footnotes = %v", outer.Table.Footnotes)
	}
	if outer.Table.NestedTables["doc-t:table-2"] == nil {
		t.Fatalf("outer nested tables = %v", outer.Table.NestedTables)
	}
	if nested.Table == nil || nested.Table.ParentCell == nil ||
		nested.Table.ParentCell.AssetID != "doc-t:table-1" {
		t.Fatalf("nested parent = %+v", nested.Table)
	}
	payload := outer.Payload("spec.docx")
	tablePayload := payload["table"].(map[string]any)
	if tablePayload["caption"] != "Table 1: Pump limits" {
		t.Fatalf("table payload caption = %v", tablePayload["caption"])
	}
	nestedPayload := tablePayload["nested_tables"].(map[string]any)
	if _, ok := nestedPayload["doc-t:table-2"]; !ok {
		t.Fatalf("table payload nested = %v", nestedPayload)
	}
	cells := tablePayload["cells"].([]map[string]any)
	if len(cells) != 4 {
		t.Fatalf("table payload cells = %v", cells)
	}
	nestedCell := cells[3]
	if nestedIDs, ok := nestedCell["nested_tables"].([]string); !ok || len(nestedIDs) != 1 ||
		nestedIDs[0] != "doc-t:table-2" {
		t.Fatalf("nested cell payload = %v", nestedCell)
	}
}

func TestBuildWordContentMissingPictureData(t *testing.T) {
	_, pkg := wordAssetContent(t)
	delete(pkg.Images, "picture-1")
	if _, err := BuildWordContent("doc-abc", pkg); err == nil ||
		!strings.Contains(err.Error(), "Word picture data is missing") {
		t.Fatalf("error = %v", err)
	}
}

func TestWordPackageStagingRoundTrip(t *testing.T) {
	body := pictureParagraph("rId1", "") + paragraphXML("Figure 1: captioned")
	path := wordPicturePackage(t, body)
	pkg, err := ParseWordPackage(path)
	if err != nil {
		t.Fatalf("ParseWordPackage() error = %v", err)
	}
	dir := t.TempDir()
	if err := pkg.WriteStaging(dir); err != nil {
		t.Fatalf("WriteStaging() error = %v", err)
	}
	imagePath := filepath.Join(dir, "images", "picture-1.jpeg")
	data, err := os.ReadFile(imagePath)
	if err != nil || string(data) != string(testJPEGBytes) {
		t.Fatalf("staged image = %d bytes, err = %v", len(data), err)
	}
	manifest, err := ReadWordManifestFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("ReadWordManifestFile() error = %v", err)
	}
	if len(manifest.Assets) != 1 || manifest.Assets[0].AssetID != "picture-1" {
		t.Fatalf("staged manifest = %+v", manifest.Assets)
	}
	manifest.Assets[0].GeneratedDescription = "A red indicator is lit."
	if err := manifest.WriteFile(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := pkg.ReloadFromStaging(dir); err != nil {
		t.Fatalf("ReloadFromStaging() error = %v", err)
	}
	if pkg.Manifest.Assets[0].GeneratedDescription != "A red indicator is lit." {
		t.Fatalf("reload = %q", pkg.Manifest.Assets[0].GeneratedDescription)
	}

	empty := &WordPackage{Manifest: WordManifest{Version: WordParserVersion, Assets: []*WordManifestAsset{
		{AssetID: "picture-9", Kind: "picture", ImageFile: stringPointer("images/picture-9.jpeg")},
	}}}
	if err := empty.WriteStaging(t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "Word picture data is missing") {
		t.Fatalf("error = %v", err)
	}
}

func stringPointer(value string) *string { return &value }

func PDFTableFixture() *PDFTable {
	return &PDFTable{
		NumRows:     2,
		NumCols:     2,
		Orientation: "horizontal",
		Headers:     []string{"Name", "Value"},
		HeaderRows:  [][]string{{"Name", "Value"}},
		Rows:        [][]string{{"Name", "Value"}, {"A", "1.25"}},
		Cells: []map[string]any{
			{"text": "Name", "row_span": 1, "col_span": 2},
		},
		Footnotes: []string{"Source note"},
	}
}

func TestBuildPDFContentAssetRecords(t *testing.T) {
	staging := t.TempDir()
	writeStagingFile(t, staging, "pictures/picture-0.png", []byte("\x89PNG\r\n\x1a\noriginal"))
	manifest := PDFManifest{
		Version: PDFParserVersion,
		Blocks: []PDFBlock{
			{Ref: "text-1", Kind: "text", Page: 1, Order: 0, Text: "Left column."},
			{Ref: "#/pictures/0", Kind: "picture", Page: 2, Order: 1, AssetID: "#/pictures/0"},
			{Ref: "caption-1", Kind: "caption", Page: 2, Order: 2, Text: "Fig. 1"},
			{Ref: "#/tables/0", Kind: "table", Page: 3, Order: 3, AssetID: "#/tables/0"},
		},
		Assets: []PDFAsset{
			{
				AssetID:       "#/pictures/0",
				Kind:          "picture",
				Page:          2,
				BBox:          map[string]any{"l": 1.0, "t": 2.0},
				Caption:       "Fig. 1",
				CaptionSource: "document",
				ImageFile:     "pictures/picture-0.png",
				Captions: []PDFAnnotation{
					{Ref: "#/texts/1", Text: "Fig. 1", Page: 2, BBox: map[string]any{"l": 1.0}},
				},
			},
			{
				AssetID:       "#/tables/0",
				Kind:          "table",
				Page:          3,
				Caption:       "Table 1. Values",
				CaptionSource: "document",
				Table:         PDFTableFixture(),
			},
		},
	}
	content, err := BuildPDFContent(manifest, "doc-pdf", staging)
	if err != nil {
		t.Fatalf("BuildPDFContent() error = %v", err)
	}
	if len(content.Assets) != 2 {
		t.Fatalf("assets = %d", len(content.Assets))
	}
	picture := content.Assets[0]
	if picture.ID != "doc-pdf:#/pictures/0" || picture.Kind != "picture" {
		t.Fatalf("picture = %+v", picture)
	}
	if picture.Name != "doc-pdf/pictures/picture-0.png" || picture.SourceName != "pictures/picture-0.png" {
		t.Fatalf("picture names = %q/%q", picture.Name, picture.SourceName)
	}
	if picture.MIMEType != "image/png" || picture.Size != int64(len("\x89PNG\r\n\x1a\noriginal")) {
		t.Fatalf("picture mime/size = %q/%d", picture.MIMEType, picture.Size)
	}
	if picture.SHA256 == "" || picture.Page != 2 || picture.Order != 0 {
		t.Fatalf("picture checksum/page/order = %q/%d/%d", picture.SHA256, picture.Page, picture.Order)
	}
	if len(picture.Captions) != 1 || picture.Captions[0].Page != 2 {
		t.Fatalf("picture captions = %+v", picture.Captions)
	}
	table := content.Assets[1]
	if table.Table == nil || table.Table.Orientation != "horizontal" ||
		len(table.Table.Cells) != 1 || table.Table.Cells[0]["col_span"] != 2 {
		t.Fatalf("table = %+v", table.Table)
	}
	if table.Data != nil || table.Name != "" {
		t.Fatalf("table data/name = %d/%q", len(table.Data), table.Name)
	}

	qualified := content.Manifest
	if qualified.Assets[0].AssetID != "doc-pdf:#/pictures/0" {
		t.Fatalf("qualified asset id = %q", qualified.Assets[0].AssetID)
	}
	if qualified.Assets[0].ImageFile != "doc-pdf/pictures/picture-0.png" {
		t.Fatalf("qualified image file = %q", qualified.Assets[0].ImageFile)
	}
	if qualified.Blocks[1].AssetID != "doc-pdf:#/pictures/0" {
		t.Fatalf("qualified block asset id = %q", qualified.Blocks[1].AssetID)
	}
	if !strings.Contains(content.DocumentMarkdown, "[Image: #/pictures/0]") ||
		!strings.Contains(content.DocumentMarkdown, "Name | Value\nA | 1.25") {
		t.Fatalf("document markdown = %q", content.DocumentMarkdown)
	}

	payload := picture.Payload("manual.pdf")
	if payload["asset_kind"] != "picture" || payload["image_file"] != "doc-pdf/pictures/picture-0.png" {
		t.Fatalf("pdf payload = %v", payload)
	}
	if _, exists := payload["asset_storage"]; exists {
		t.Fatalf("pdf payload must not claim asset_storage: %v", payload)
	}
	if payload["page"] != 2 || payload["table"] != nil {
		t.Fatalf("pdf payload page/table = %v/%v", payload["page"], payload["table"])
	}
	tablePayload := table.Payload("manual.pdf")
	pdfTable := tablePayload["table"].(map[string]any)
	if pdfTable["orientation"] != "horizontal" || len(pdfTable["footnotes"].([]string)) != 1 {
		t.Fatalf("pdf table payload = %v", pdfTable)
	}
}

func TestBuildPDFContentSafeImageErrors(t *testing.T) {
	staging := t.TempDir()
	manifest := PDFManifest{
		Version: PDFParserVersion,
		Assets: []PDFAsset{{
			AssetID: "#/pictures/0", Kind: "picture", Page: 1, ImageFile: "pictures/missing.png",
		}},
	}
	if _, err := BuildPDFContent(manifest, "doc-pdf", staging); err == nil ||
		!strings.Contains(err.Error(), "PDF asset image is missing: pictures/missing.png") {
		t.Fatalf("missing image error = %v", err)
	}
	writeStagingFile(t, staging, "pictures/empty.png", nil)
	manifest.Assets[0].ImageFile = "pictures/empty.png"
	if _, err := BuildPDFContent(manifest, "doc-pdf", staging); err == nil ||
		!strings.Contains(err.Error(), "PDF asset image is empty: pictures/empty.png") {
		t.Fatalf("empty image error = %v", err)
	}
}

func TestBuildPDFContentQualificationDoesNotMutateInput(t *testing.T) {
	staging := t.TempDir()
	writeStagingFile(t, staging, "pictures/picture-0.png", []byte("png"))
	manifest := PDFManifest{
		Version: PDFParserVersion,
		Assets:  []PDFAsset{{AssetID: "#/pictures/0", Kind: "picture", Page: 1, ImageFile: "pictures/picture-0.png"}},
	}
	if _, err := BuildPDFContent(manifest, "doc-pdf", staging); err != nil {
		t.Fatalf("BuildPDFContent() error = %v", err)
	}
	if manifest.Assets[0].AssetID != "#/pictures/0" || manifest.Assets[0].ImageFile != "pictures/picture-0.png" {
		t.Fatalf("input manifest was mutated: %+v", manifest.Assets[0])
	}
}

func TestWriteArtifacts(t *testing.T) {
	content, _ := wordAssetContent(t)
	dir := t.TempDir()
	if err := content.WriteArtifacts(dir); err != nil {
		t.Fatalf("WriteArtifacts() error = %v", err)
	}
	manifest, err := ReadWordManifestFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("ReadWordManifestFile() error = %v", err)
	}
	if len(manifest.Assets) != 1 || manifest.Assets[0].AssetID != "doc-abc:picture-1" {
		t.Fatalf("qualified manifest artifact = %+v", manifest.Assets)
	}
	documentMarkdown, err := os.ReadFile(filepath.Join(dir, "document.md"))
	if err != nil || string(documentMarkdown) != content.DocumentMarkdown {
		t.Fatalf("document.md = %d bytes, err = %v", len(documentMarkdown), err)
	}

	pdfDir := t.TempDir()
	pdfContent := &PDFContent{DocumentMarkdown: "PDF content"}
	if err := pdfContent.WriteDocumentMarkdown(pdfDir); err != nil {
		t.Fatalf("WriteDocumentMarkdown() error = %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(pdfDir, "document.md"))
	if err != nil || string(raw) != "PDF content" {
		t.Fatalf("pdf document.md = %q, err = %v", raw, err)
	}
}

func TestParserVersionGuards(t *testing.T) {
	if err := ValidateWordParserVersion(WordParserVersion); err != nil {
		t.Fatalf("word version guard error = %v", err)
	}
	if err := ValidateWordParserVersion("old"); err == nil ||
		err.Error() != "Word parser changed; reimport this document" {
		t.Fatalf("word version error = %v", err)
	}
	if err := ValidatePDFParserVersion(PDFParserVersion); err != nil {
		t.Fatalf("pdf version guard error = %v", err)
	}
	if err := ValidatePDFParserVersion(PDFParserVersionService); err != nil {
		t.Fatalf("docling pdf version guard error = %v", err)
	}
	if err := ValidatePDFParserVersion("old"); err == nil ||
		err.Error() != "PDF parser version changed; explicitly reimport this PDF" {
		t.Fatalf("pdf version error = %v", err)
	}
}
