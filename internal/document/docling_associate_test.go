package document

import "testing"

func doclingAssociateFixture() WordManifest {
	imageFile := "images/picture-1.png"
	return WordManifest{
		Version: DoclingWordParserVersion,
		Blocks: []WordManifestBlock{
			{Ref: "b0", Order: 0, Kind: KindHeading, Text: "硬件参数",
				Location: WordLocation{Part: "body", Paragraph: 1, Headings: []string{"硬件参数"}, Scope: "body", Section: 1}},
			{Ref: "b1", Order: 1, Kind: KindParagraph, Text: "表 1 硬件参数表",
				Location: WordLocation{Part: "body", Paragraph: 2, Headings: []string{"硬件参数"}, Scope: "body", Section: 1}},
			{Ref: "b2", Order: 2, Kind: "table", AssetID: "table-1",
				Location: WordLocation{Part: "body", Paragraph: 3, Headings: []string{"硬件参数"}, Scope: "body", Section: 1}},
			{Ref: "b3", Order: 3, Kind: KindParagraph, Text: "图 1 系统架构图",
				Location: WordLocation{Part: "body", Paragraph: 4, Headings: []string{"硬件参数"}, Scope: "body", Section: 1}},
			{Ref: "b4", Order: 4, Kind: KindPicture, AssetID: "picture-1",
				Location: WordLocation{Part: "body", Paragraph: 5, Headings: []string{"硬件参数"}, Scope: "body", Section: 1}},
		},
		Assets: []*WordManifestAsset{
			{AssetID: "table-1", Kind: "table",
				Table:    &WordTableData{NumRows: 1, NumCols: 2, Rows: [][]string{{"部件", "规格"}}},
				Location: WordLocation{Part: "body", Paragraph: 3, Scope: "body", Section: 1}},
			{AssetID: "picture-1", Kind: "picture", ImageFile: &imageFile,
				Location: WordLocation{Part: "body", Paragraph: 5, Scope: "body", Section: 1}},
		},
	}
}

func TestAssociateDoclingCaptionsAttachesUniqueAdjacentObjects(t *testing.T) {
	manifest := AssociateDoclingCaptions(doclingAssociateFixture())
	table := manifest.Assets[0]
	picture := manifest.Assets[1]
	if table.Caption != "表 1 硬件参数表" || table.CaptionSource != "document" {
		t.Fatalf("table caption = %q source = %q", table.Caption, table.CaptionSource)
	}
	if picture.Caption != "图 1 系统架构图" || picture.CaptionSource != "document" {
		t.Fatalf("picture caption = %q source = %q", picture.Caption, picture.CaptionSource)
	}
	if manifest.Blocks[1].Kind != KindCaption || len(manifest.Blocks[1].AssociatedAssets) != 1 ||
		manifest.Blocks[1].AssociatedAssets[0] != "table-1" {
		t.Fatalf("table caption block = %+v", manifest.Blocks[1])
	}
	if manifest.Blocks[3].Kind != KindCaption || manifest.Blocks[3].AssociatedAssets[0] != "picture-1" {
		t.Fatalf("picture caption block = %+v", manifest.Blocks[3])
	}
}

func TestAssociateDoclingCaptionsKeepsAmbiguousCaptionsAsText(t *testing.T) {
	imageOne := "images/picture-1.png"
	imageTwo := "images/picture-2.png"
	manifest := WordManifest{
		Version: DoclingWordParserVersion,
		Blocks: []WordManifestBlock{
			{Ref: "b0", Order: 0, Kind: KindParagraph, Text: "图 1 架构图",
				Location: WordLocation{Part: "body", Paragraph: 1, Scope: "body", Section: 1}},
			{Ref: "b1", Order: 1, Kind: KindPicture, AssetID: "picture-1",
				Location: WordLocation{Part: "body", Paragraph: 2, Scope: "body", Section: 1}},
			{Ref: "b2", Order: 2, Kind: KindPicture, AssetID: "picture-2",
				Location: WordLocation{Part: "body", Paragraph: 3, Scope: "body", Section: 1}},
		},
		Assets: []*WordManifestAsset{
			{AssetID: "picture-1", Kind: "picture", ImageFile: &imageOne,
				Location: WordLocation{Part: "body", Paragraph: 2, Scope: "body", Section: 1}},
			{AssetID: "picture-2", Kind: "picture", ImageFile: &imageTwo,
				Location: WordLocation{Part: "body", Paragraph: 3, Scope: "body", Section: 1}},
		},
	}
	result := AssociateDoclingCaptions(manifest)
	if result.Blocks[0].Kind != KindCaption || len(result.Blocks[0].AssociatedAssets) != 0 {
		t.Fatalf("ambiguous caption was associated: %+v", result.Blocks[0])
	}
	if result.Assets[0].Caption != "" || result.Assets[1].Caption != "" {
		t.Fatalf("ambiguous caption leaked onto an asset: %+v", result.Assets)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Kind != "unassociated_caption" {
		t.Fatalf("missing unassociated_caption diagnostic: %+v", result.Diagnostics)
	}
}

func TestAssociateDoclingCaptionsDoesNotDuplicateExplicitCaptions(t *testing.T) {
	manifest := doclingAssociateFixture()
	// Simulate the HTML service path: the caption block is already explicit and
	// the asset already carries the text.
	manifest.Blocks[3].AssociatedAssets = []string{"picture-1"}
	manifest.Assets[1].Caption = "图 1 系统架构图"
	manifest.Assets[1].Captions = []WordAnnotation{{Text: "图 1 系统架构图", Ref: "c1"}}
	result := AssociateDoclingCaptions(manifest)
	if len(result.Assets[1].Captions) != 1 {
		t.Fatalf("explicit caption duplicated: %+v", result.Assets[1].Captions)
	}
	if len(result.Blocks[3].AssociatedAssets) != 1 {
		t.Fatalf("caption block association changed: %+v", result.Blocks[3])
	}
}

func TestAssociateDoclingCaptionsAttachesNotesToPrecedingAsset(t *testing.T) {
	imageFile := "images/picture-1.png"
	manifest := WordManifest{
		Version: DoclingWordParserVersion,
		Blocks: []WordManifestBlock{
			{Ref: "b0", Order: 0, Kind: KindPicture, AssetID: "picture-1",
				Location: WordLocation{Part: "body", Paragraph: 1, Scope: "body", Section: 1}},
			{Ref: "b1", Order: 1, Kind: KindParagraph, Text: "注：数据来自实验室测试。",
				Location: WordLocation{Part: "body", Paragraph: 2, Scope: "body", Section: 1}},
		},
		Assets: []*WordManifestAsset{
			{AssetID: "picture-1", Kind: "picture", ImageFile: &imageFile,
				Location: WordLocation{Part: "body", Paragraph: 1, Scope: "body", Section: 1}},
		},
	}
	result := AssociateDoclingCaptions(manifest)
	picture := result.Assets[0]
	if len(picture.Footnotes) != 1 || picture.Footnotes[0].Text != "注：数据来自实验室测试。" {
		t.Fatalf("picture footnotes = %+v", picture.Footnotes)
	}
	if result.Blocks[1].Kind != KindNote || result.Blocks[1].AssociatedAssets[0] != "picture-1" {
		t.Fatalf("note block = %+v", result.Blocks[1])
	}
}
