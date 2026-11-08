package document

import (
	"strings"
	"testing"
)

func TestWordCaptionNoteAssociationAndInlineOrder(t *testing.T) {
	body := `<w:p><w:r><w:t xml:space="preserve">Before image</w:t></w:r>` +
		pictureRun("rId1", "Figure 1") +
		`<w:r><w:t xml:space="preserve">After image</w:t></w:r></w:p>` +
		paragraphXML("Figure 1: Red status lamp") +
		paragraphXML("Note: keep this lamp visible") +
		paragraphXML("Tail")
	path := wordPicturePackage(t, body)
	pkg, err := ParseWordPackage(path)
	if err != nil {
		t.Fatalf("ParseWordPackage() error = %v", err)
	}
	manifest := pkg.Manifest
	if len(manifest.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(manifest.Assets))
	}
	asset := manifest.Assets[0]
	if asset.Kind != "picture" || asset.MIMEType == nil || *asset.MIMEType != "image/jpeg" {
		t.Fatalf("asset = %+v", asset)
	}
	if asset.ImageFile == nil || *asset.ImageFile != "images/picture-1.jpeg" {
		t.Fatalf("image_file = %v", asset.ImageFile)
	}
	if asset.Caption != "Figure 1: Red status lamp" || asset.CaptionSource != "document" {
		t.Fatalf("caption = %q source = %q", asset.Caption, asset.CaptionSource)
	}
	if len(asset.Captions) != 1 || asset.Captions[0].Text != "Figure 1: Red status lamp" {
		t.Fatalf("captions = %+v", asset.Captions)
	}
	if len(asset.Footnotes) != 1 || asset.Footnotes[0].Text != "Note: keep this lamp visible" {
		t.Fatalf("footnotes = %+v", asset.Footnotes)
	}
	if len(asset.Labels) < 3 || asset.Labels[1] != "Figure 1" {
		t.Fatalf("labels = %v", asset.Labels)
	}
	if len(pkg.Images["picture-1"]) != len(testJPEGBytes) {
		t.Fatalf("image bytes = %d", len(pkg.Images["picture-1"]))
	}
	kinds := make([]string, 0, len(manifest.Blocks))
	texts := make([]string, 0, len(manifest.Blocks))
	for _, block := range manifest.Blocks {
		kinds = append(kinds, block.Kind)
		texts = append(texts, block.Text)
	}
	wantKinds := []string{"paragraph", "picture", "paragraph", "caption", "note", "paragraph"}
	if strings.Join(kinds, ",") != strings.Join(wantKinds, ",") {
		t.Fatalf("block kinds = %v", kinds)
	}
	if texts[0] != "Before image" || texts[2] != "After image" {
		t.Fatalf("inline order = %v", texts)
	}
	if manifest.SkippedObjectCount != 0 || len(manifest.Diagnostics) != 0 {
		t.Fatalf("skipped = %d diagnostics = %v", manifest.SkippedObjectCount, manifest.Diagnostics)
	}

	text, err := ReadText(path)
	if err != nil {
		t.Fatalf("ReadText() error = %v", err)
	}
	want := "Before image\n\n[Image: picture-1]\n\nAfter image\n\nFigure 1: Red status lamp\n\nNote: keep this lamp visible\n\nTail"
	if text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
}

func TestAmbiguousCaptionSkipsAssetsAndKeepsText(t *testing.T) {
	body := pictureParagraph("rId1", "") +
		paragraphXML("Figure 7: ambiguous") +
		pictureParagraph("rId1", "")
	path := wordPicturePackage(t, body)
	pkg, err := ParseWordPackage(path)
	if err != nil {
		t.Fatalf("ParseWordPackage() error = %v", err)
	}
	manifest := pkg.Manifest
	if len(manifest.Assets) != 0 {
		t.Fatalf("assets = %d, want 0", len(manifest.Assets))
	}
	if manifest.SkippedObjectCount != 2 {
		t.Fatalf("skipped = %d, want 2", manifest.SkippedObjectCount)
	}
	if len(manifest.Diagnostics) != 1 || manifest.Diagnostics[0].Kind != "unassociated_caption" {
		t.Fatalf("diagnostics = %+v", manifest.Diagnostics)
	}
	if len(manifest.Diagnostics[0].AssetIDs) != 2 {
		t.Fatalf("diagnostic asset ids = %v", manifest.Diagnostics[0].AssetIDs)
	}
	if len(pkg.Images) != 0 {
		t.Fatalf("skipped image bytes were not pruned: %d", len(pkg.Images))
	}
	if rendered := manifest.Render(); !strings.Contains(rendered, "Figure 7: ambiguous") {
		t.Fatalf("render = %q", rendered)
	}
	for _, block := range manifest.Blocks {
		if block.AssetID != "" {
			t.Fatalf("skipped asset block survived: %+v", block)
		}
	}
}

func TestNumberedCaptionsResolveGroupedPictures(t *testing.T) {
	body := pictureParagraph("rId1", "") + pictureParagraph("rId1", "") +
		paragraphXML("Figure 1: First") + paragraphXML("Figure 2: Second")
	path := wordPicturePackage(t, body)
	pkg, err := ParseWordPackage(path)
	if err != nil {
		t.Fatalf("ParseWordPackage() error = %v", err)
	}
	captions := make([]string, 0, len(pkg.Manifest.Assets))
	for _, asset := range pkg.Manifest.Assets {
		captions = append(captions, asset.Caption)
	}
	if strings.Join(captions, "|") != "Figure 1: First|Figure 2: Second" {
		t.Fatalf("captions = %v", captions)
	}
	if pkg.Manifest.SkippedObjectCount != 0 {
		t.Fatalf("skipped = %d", pkg.Manifest.SkippedObjectCount)
	}
}

func TestTableCaptionAndNoteAreAttached(t *testing.T) {
	body := paragraphXML("Table 1: Pump limits") + outerTableWithNested() + paragraphXML("Note: values in kPa")
	path := buildDOCX(t, basePackage(body)...)
	pkg, err := ParseWordPackage(path)
	if err != nil {
		t.Fatalf("ParseWordPackage() error = %v", err)
	}
	manifest := pkg.Manifest
	if len(manifest.Assets) != 2 {
		t.Fatalf("assets = %d, want 2 (outer + nested)", len(manifest.Assets))
	}
	outer := manifest.Assets[0]
	if outer.Kind != "table" || outer.Table == nil {
		t.Fatalf("outer asset = %+v", outer)
	}
	if outer.Caption != "Table 1: Pump limits" {
		t.Fatalf("caption = %q", outer.Caption)
	}
	if len(outer.Table.Footnotes) != 1 || outer.Table.Footnotes[0] != "Note: values in kPa" {
		t.Fatalf("table footnotes = %v", outer.Table.Footnotes)
	}
	if outer.Table.Rows[1][0] != "ZQ-25" || outer.Table.Rows[1][1] != "100" {
		t.Fatalf("outer rows = %v", outer.Table.Rows)
	}
	nested := manifest.Assets[1]
	if nested.Kind != "table" || nested.Table == nil || nested.Table.ParentCell == nil {
		t.Fatalf("nested asset = %+v", nested)
	}
	if nested.Table.ParentCell.AssetID != "table-1" || nested.Table.ParentCell.Row != 1 || nested.Table.ParentCell.Col != 1 {
		t.Fatalf("nested parent cell = %+v", nested.Table.ParentCell)
	}
	if nested.Table.Rows[0][0] != "Nested specification" {
		t.Fatalf("nested rows = %v", nested.Table.Rows)
	}
}

func TestCaptionDoesNotCrossSectionBoundary(t *testing.T) {
	body := headingParagraph("First section", 1) +
		pictureParagraph("rId1", "") +
		headingParagraph("Second section", 1) +
		paragraphXML("Figure 1: unrelated")
	path := wordPictureStylesPackage(t, body)
	pkg, err := ParseWordPackage(path)
	if err != nil {
		t.Fatalf("ParseWordPackage() error = %v", err)
	}
	manifest := pkg.Manifest
	if len(manifest.Assets) != 1 {
		t.Fatalf("assets = %d", len(manifest.Assets))
	}
	if manifest.Assets[0].Caption != "" {
		t.Fatalf("caption crossed a section boundary: %q", manifest.Assets[0].Caption)
	}
	if manifest.SkippedObjectCount != 0 {
		t.Fatalf("skipped = %d", manifest.SkippedObjectCount)
	}
	if len(manifest.Diagnostics) == 0 {
		t.Fatalf("expected an unassociated-caption diagnostic")
	}
}

func TestNoteBeforePictureIsNotAttached(t *testing.T) {
	body := paragraphXML("Note: appears before the object") + pictureParagraph("rId1", "")
	path := wordPicturePackage(t, body)
	pkg, err := ParseWordPackage(path)
	if err != nil {
		t.Fatalf("ParseWordPackage() error = %v", err)
	}
	if len(pkg.Manifest.Assets) != 1 {
		t.Fatalf("assets = %d", len(pkg.Manifest.Assets))
	}
	if len(pkg.Manifest.Assets[0].Footnotes) != 0 {
		t.Fatalf("note above the object was attached: %+v", pkg.Manifest.Assets[0].Footnotes)
	}
}

func TestCaptionReferenceFieldAssociatesByBookmark(t *testing.T) {
	picture := `<w:p><w:bookmarkStart w:id="0" w:name="_Ref123"/>` + pictureRun("rId1", "") + `</w:p>`
	caption := `<w:p><w:fldSimple w:instr=" REF _Ref123 \h "><w:r><w:t>Figure 9: linked caption</w:t></w:r></w:fldSimple></w:p>`
	body := picture + caption
	path := wordPicturePackage(t, body)
	pkg, err := ParseWordPackage(path)
	if err != nil {
		t.Fatalf("ParseWordPackage() error = %v", err)
	}
	manifest := pkg.Manifest
	if len(manifest.Assets) != 1 {
		t.Fatalf("assets = %d", len(manifest.Assets))
	}
	if manifest.Assets[0].Caption != "Figure 9: linked caption" {
		t.Fatalf("caption = %q", manifest.Assets[0].Caption)
	}
	if len(manifest.Blocks) < 2 || len(manifest.Blocks[1].References) == 0 ||
		manifest.Blocks[1].References[0] != "_Ref123" {
		t.Fatalf("caption references = %+v", manifest.Blocks)
	}
}

func TestPictureLabelsFromDocPr(t *testing.T) {
	body := pictureParagraph("rId1", "Figure 1")
	path := wordPicturePackage(t, body)
	pkg, err := ParseWordPackage(path)
	if err != nil {
		t.Fatalf("ParseWordPackage() error = %v", err)
	}
	labels := pkg.Manifest.Assets[0].Labels
	found := false
	for _, label := range labels {
		if label == "Figure 1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("labels = %v", labels)
	}
}
