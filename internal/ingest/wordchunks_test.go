package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/chunking"
	"github.com/Guan0923/Praxis-RAG/internal/document"
)

func mustConfig(t *testing.T, strategy string, size, overlap int) chunking.Config {
	t.Helper()
	config, err := chunking.NewConfig(strategy, size, overlap)
	if err != nil {
		t.Fatalf("NewConfig(%q,%d,%d) error = %v", strategy, size, overlap, err)
	}
	return config
}

func chunksWithMetadata(chunks []chunking.Chunk, key string, value any) []chunking.Chunk {
	result := make([]chunking.Chunk, 0, len(chunks))
	for _, chunk := range chunks {
		if chunk.Metadata[key] == value {
			result = append(result, chunk)
		}
	}
	return result
}

// manifestBuilder assembles a Docling-shaped Word manifest for the Word chunk
// tests. It replaces the removed in-process OOXML fixtures: the chunk builder
// is format-agnostic, so the fake service output is the natural input.
type manifestBuilder struct {
	blocks []document.WordManifestBlock
	assets []*document.WordManifestAsset
}

func newManifestBuilder() *manifestBuilder {
	return &manifestBuilder{}
}

func (b *manifestBuilder) location(paragraph int, headings ...string) document.WordLocation {
	return document.WordLocation{Part: "body", Paragraph: paragraph, Headings: headings, Scope: "body", Section: 1}
}

func (b *manifestBuilder) heading(ref string, text string, headings []string) *manifestBuilder {
	b.blocks = append(b.blocks, document.WordManifestBlock{
		Ref: ref, Order: len(b.blocks), Kind: document.KindHeading, Text: text,
		Location: b.location(len(b.blocks)+1, headings...),
	})
	return b
}

func (b *manifestBuilder) paragraph(ref, text string, headings ...string) *manifestBuilder {
	b.blocks = append(b.blocks, document.WordManifestBlock{
		Ref: ref, Order: len(b.blocks), Kind: document.KindParagraph, Text: text,
		Location: b.location(len(b.blocks)+1, headings...),
	})
	return b
}

func (b *manifestBuilder) table(ref, assetID string, rows [][]string, headings ...string) *manifestBuilder {
	headers := rows[0]
	b.blocks = append(b.blocks, document.WordManifestBlock{
		Ref: ref, Order: len(b.blocks), Kind: "table", AssetID: assetID,
		Location: b.location(len(b.blocks)+1, headings...),
	})
	cells := make([]document.WordTableCellData, 0, len(rows)*len(headers))
	for rowIndex, row := range rows {
		for colIndex, cell := range row {
			cells = append(cells, document.WordTableCellData{
				Row: rowIndex, Col: colIndex, RowSpan: 1, ColSpan: 1, Text: cell,
			})
		}
	}
	b.assets = append(b.assets, &document.WordManifestAsset{
		AssetID: assetID, Kind: "table",
		Table: &document.WordTableData{
			NumRows: len(rows), NumCols: len(headers),
			Rows: rows, HeaderRows: [][]string{headers}, Headers: headers, Cells: cells,
		},
		Location: b.location(len(b.blocks)),
	})
	return b
}

func (b *manifestBuilder) picture(ref, assetID string, caption string, headings ...string) *manifestBuilder {
	b.blocks = append(b.blocks, document.WordManifestBlock{
		Ref: ref, Order: len(b.blocks), Kind: document.KindPicture, AssetID: assetID,
		Location: b.location(len(b.blocks)+1, headings...),
	})
	imageFile := "images/" + assetID + ".jpeg"
	asset := &document.WordManifestAsset{
		AssetID: assetID, Kind: "picture", ImageFile: &imageFile,
		MIMEType: strPtr("image/jpeg"), Location: b.location(len(b.blocks)),
	}
	if caption != "" {
		asset.Captions = []document.WordAnnotation{{Text: caption, Ref: ref}}
		asset.Caption = caption
		asset.CaptionSource = "document"
	}
	b.assets = append(b.assets, asset)
	return b
}

// build runs AssociateDoclingCaptions and returns the finished manifest.
func (b *manifestBuilder) build() document.WordManifest {
	return document.AssociateDoclingCaptions(document.WordManifest{
		Version:     document.DoclingWordParserVersion,
		Blocks:      b.blocks,
		Assets:      b.assets,
		Diagnostics: []document.Diagnostic{},
	})
}

func strPtr(value string) *string { return &value }

// wordPrepareOptions runs Prepare over one .docx source with a fake Docling
// parser backed by the given manifest. Picture assets are staged with real
// decodable JPEG bytes so the optional vision step can process them.
func wordPrepareOptions(t *testing.T, source string, staging string, manifest document.WordManifest,
	extra func(*Options)) Options {
	t.Helper()
	images := map[string][]byte{}
	for _, asset := range manifest.Assets {
		if asset.Kind == "picture" && asset.ImageFile != nil {
			images[asset.AssetID] = testJPEGBytes
		}
	}
	options := Options{
		SourceName: "spec.docx",
		Chunking:   mustConfig(t, "character", 700, 100),
		StagingDir: staging,
		Docling:    DoclingOptions{Parser: &fakeDoclingParser{manifest: manifest, images: images}},
	}
	if extra != nil {
		extra(&options)
	}
	return options
}

// writeAnyDocx writes a minimal valid .docx upload file; its content is
// irrelevant because the fake Docling parser supplies the manifest.
func writeAnyDocx(t *testing.T) string {
	t.Helper()
	return plainDocxPackage(t, paragraphXML("正文"), false)
}

func TestWordChunkStrategiesPreserveTableHeaders(t *testing.T) {
	// Port of tests/test_word_fidelity.py::test_word_chunk_strategies_preserve_table_headers.
	manifest := newManifestBuilder().
		heading("#/texts/0", "Specification", []string{"Specification"}).
		table("#/tables/0", "table-1", [][]string{{"Model", "Limit"}, {"ZX256", "90"}}, "Specification").
		build()
	source := writeAnyDocx(t)
	for _, strategy := range []string{"character", "token", "sentence", "recursive", "structure"} {
		t.Run(strategy, func(t *testing.T) {
			staging := t.TempDir()
			options := wordPrepareOptions(t, source, staging, manifest, func(o *Options) {
				o.Chunking = mustConfig(t, strategy, 256, 16)
			})
			result, err := Prepare(t.Context(), source, options)
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			rows := chunksWithMetadata(result.Chunks, "asset_kind", "table")
			if len(rows) != 2 {
				t.Fatalf("table chunks = %d, want 2 (%v)", len(rows), result.Chunks)
			}
			for _, row := range rows {
				if !strings.Contains(row.Text, "Model | Limit") {
					t.Fatalf("table header missing: %q", row.Text)
				}
			}
			if !strings.Contains(rows[len(rows)-1].Text, "ZX256 | 90") {
				t.Fatalf("table row missing: %q", rows[len(rows)-1].Text)
			}
			if result.Title != "spec" {
				t.Fatalf("title = %q, want source stem", result.Title)
			}
			for _, chunk := range result.Chunks {
				if chunk.DocumentID != chunking.DocumentID("spec.docx") {
					t.Fatalf("document id = %q", chunk.DocumentID)
				}
				if chunk.Source != "spec.docx" || chunk.Title != "spec" {
					t.Fatalf("chunk identity = %+v", chunk)
				}
			}
		})
	}
}

func TestWordPictureCaptionAndNoteChunk(t *testing.T) {
	manifest := newManifestBuilder().
		picture("#/pictures/0", "picture-1", "Figure 1: Red status lamp").
		paragraph("#/texts/0", "Note: keep this lamp visible").
		build()
	source := writeAnyDocx(t)
	staging := t.TempDir()
	result, err := Prepare(t.Context(), source, wordPrepareOptions(t, source, staging, manifest, nil))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	pictures := chunksWithMetadata(result.Chunks, "asset_kind", "picture")
	if len(pictures) != 1 {
		t.Fatalf("picture chunks = %d", len(pictures))
	}
	picture := pictures[0]
	if picture.Text != "Figure 1: Red status lamp\nNote: keep this lamp visible" {
		t.Fatalf("picture chunk text = %q", picture.Text)
	}
	if picture.Metadata["kind"] != "picture" {
		t.Fatalf("kind = %v", picture.Metadata["kind"])
	}
	assets := result.Assets
	if len(assets) != 1 {
		t.Fatalf("assets = %d", len(assets))
	}
	if !strings.HasPrefix(assets[0].ID, "doc-") || !strings.Contains(assets[0].ID, ":picture-1") {
		t.Fatalf("asset id = %q", assets[0].ID)
	}
	if picture.Metadata["asset_id"] != assets[0].ID {
		t.Fatalf("chunk asset_id = %v, asset = %v", picture.Metadata["asset_id"], assets[0].ID)
	}
	location, ok := picture.Metadata["location"].(map[string]any)
	if !ok || location["part"] != "body" || location["scope"] != "body" {
		t.Fatalf("location = %v", picture.Metadata["location"])
	}
	refs, ok := picture.Metadata["refs"].([]string)
	if !ok || len(refs) != 1 {
		t.Fatalf("refs = %v", picture.Metadata["refs"])
	}
}

func TestWordInlineEquationIsNotSplit(t *testing.T) {
	longPrefix := strings.Repeat("a", 90)
	longSuffix := strings.Repeat("b", 200)
	manifest := newManifestBuilder().
		paragraph("#/texts/0", longPrefix+"$\\frac{x}{2}$"+longSuffix).
		build()
	source := writeAnyDocx(t)
	staging := t.TempDir()
	options := wordPrepareOptions(t, source, staging, manifest, func(o *Options) {
		o.Chunking = mustConfig(t, "character", 100, 10)
	})
	result, err := Prepare(t.Context(), source, options)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	formula := `$\frac{x}{2}$`
	found := 0
	for _, chunk := range result.Chunks {
		if !strings.Contains(chunk.Text, "$") {
			continue
		}
		found++
		if strings.Count(chunk.Text, "$") != 2 || !strings.Contains(chunk.Text, formula) {
			t.Fatalf("inline formula was split: %q", chunk.Text)
		}
	}
	if found == 0 {
		t.Fatalf("no chunk contains the inline formula: %v", result.Chunks)
	}
}

func TestWordAmbiguousAssetsAreSkipped(t *testing.T) {
	// Two pictures share one caption between them. The Docling association
	// resolves such a caption to the adjacent preceding picture (a second
	// adjacent asset in the same direction would be ambiguous and keep the
	// caption as searchable text with a diagnostic), so the caption text
	// travels with the picture chunk instead of a duplicate text chunk. The
	// second picture keeps its asset record but produces no text chunk: its
	// caption list is empty and nothing else describes it.
	manifest := newManifestBuilder().
		picture("#/pictures/0", "picture-1", "").
		paragraph("#/texts/0", "Figure 7: ambiguous").
		picture("#/pictures/1", "picture-2", "").
		build()
	source := writeAnyDocx(t)
	staging := t.TempDir()
	result, err := Prepare(t.Context(), source, wordPrepareOptions(t, source, staging, manifest, nil))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	pictures := chunksWithMetadata(result.Chunks, "asset_kind", "picture")
	if len(pictures) != 1 {
		t.Fatalf("picture chunks = %d, want 1 (%v)", len(pictures), result.Chunks)
	}
	if !strings.Contains(pictures[0].Text, "Figure 7: ambiguous") {
		t.Fatalf("caption did not travel with the picture chunk: %q", pictures[0].Text)
	}
	if result.SkippedObjectCount != 0 {
		t.Fatalf("skipped = %d", result.SkippedObjectCount)
	}
	if len(result.Assets) != 2 {
		t.Fatalf("assets = %d, want both pictures retained", len(result.Assets))
	}
}

func TestWordChunkMetadataShape(t *testing.T) {
	manifest := newManifestBuilder().
		heading("#/texts/0", "Manual", []string{"Manual"}).
		paragraph("#/texts/1", "Body text under the heading.", "Manual").
		build()
	source := writeAnyDocx(t)
	staging := t.TempDir()
	options := wordPrepareOptions(t, source, staging, manifest, func(o *Options) {
		o.Chunking = mustConfig(t, "token", 64, 12)
		o.TokenizerModel = "test-model"
	})
	result, err := Prepare(t.Context(), source, options)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	textChunks := chunksWithMetadata(result.Chunks, "kind", "text")
	if len(textChunks) == 0 {
		t.Fatalf("no text chunks")
	}
	chunk := textChunks[0]
	if chunk.Metadata["document_suffix"] != ".docx" ||
		chunk.Metadata["word_parser_version"] != document.DoclingWordParserVersion {
		t.Fatalf("chunk metadata = %v", chunk.Metadata)
	}
	chunkingMetadata := chunk.Metadata["chunking"].(map[string]any)
	if chunkingMetadata["strategy"] != "token" || chunkingMetadata["tokenizer"] != "test-model" ||
		chunkingMetadata["chunk_size"] != 64 {
		t.Fatalf("chunking metadata = %v", chunkingMetadata)
	}
	if chunk.Metadata["chunk_index"] != 0 {
		t.Fatalf("chunk_index = %v", chunk.Metadata["chunk_index"])
	}
	count, ok := chunk.Metadata["token_count"].(int)
	if !ok || count <= 0 || count > 64 {
		t.Fatalf("token_count = %v", chunk.Metadata["token_count"])
	}
	if refs, ok := chunk.Metadata["refs"].([]string); !ok || len(refs) == 0 {
		t.Fatalf("refs = %v", chunk.Metadata["refs"])
	}
	locations, ok := chunk.Metadata["locations"].([]map[string]any)
	if !ok || len(locations) == 0 {
		t.Fatalf("locations = %v", chunk.Metadata["locations"])
	}
	headings, ok := locations[0]["headings"].([]string)
	if !ok || len(headings) != 1 || headings[0] != "Manual" {
		t.Fatalf("heading path = %v", locations[0]["headings"])
	}
	documentMetadata := result.Metadata
	if documentMetadata["document_suffix"] != ".docx" ||
		documentMetadata["parser_version"] != document.DoclingWordParserVersion {
		t.Fatalf("result metadata = %v", documentMetadata)
	}
	if result.Title != "spec" {
		t.Fatalf("title = %q", result.Title)
	}
}

func TestWordCharacterChunkTextLimit(t *testing.T) {
	manifest := newManifestBuilder().
		paragraph("#/texts/0", strings.Repeat("设备正常工作。", 60)).
		build()
	source := writeAnyDocx(t)
	staging := t.TempDir()
	options := wordPrepareOptions(t, source, staging, manifest, func(o *Options) {
		o.Chunking = mustConfig(t, "character", 100, 10)
	})
	result, err := Prepare(t.Context(), source, options)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(result.Chunks) < 2 {
		t.Fatalf("chunks = %d", len(result.Chunks))
	}
	for _, chunk := range result.Chunks {
		if stringLength := len([]rune(chunk.Text)); stringLength > 100 {
			t.Fatalf("character chunk over limit (%d): %q", stringLength, chunk.Text)
		}
	}
}

func TestWordEmptyDocumentHasClearError(t *testing.T) {
	manifest := document.WordManifest{
		Version: document.DoclingWordParserVersion,
		Blocks:  []document.WordManifestBlock{},
		Assets:  []*document.WordManifestAsset{},
		Diagnostics: []document.Diagnostic{},
	}
	source := writeAnyDocx(t)
	staging := t.TempDir()
	_, err := Prepare(t.Context(), source, wordPrepareOptions(t, source, staging, manifest, func(o *Options) {
		o.SourceName = "empty.docx"
	}))
	if err == nil || !strings.Contains(err.Error(), "Word document has no indexable content") {
		t.Fatalf("error = %v", err)
	}
	if !isNoIndexable(err) {
		t.Fatalf("error is not ErrNoIndexableText: %v", err)
	}
}

func TestWordPrepareWithoutDoclingIsRejected(t *testing.T) {
	// The in-process Word parser is gone: without a Docling parser every
	// DOCX upload fails with the configuration error.
	source := writeAnyDocx(t)
	_, err := Prepare(context.Background(), source, Options{
		SourceName: "spec.docx",
		Chunking:   mustConfig(t, "character", 700, 100),
	})
	if !isErrDoclingNotConfigured(err) {
		t.Fatalf("error = %v, want ErrDoclingNotConfigured", err)
	}
}

func isNoIndexable(err error) bool {
	for current := err; current != nil; {
		if current == ErrNoIndexableText {
			return true
		}
		unwrapper, ok := current.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		current = unwrapper.Unwrap()
	}
	return false
}

func isErrDoclingNotConfigured(err error) bool {
	for current := err; current != nil; {
		if current == ErrDoclingNotConfigured {
			return true
		}
		unwrapper, ok := current.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		current = unwrapper.Unwrap()
	}
	return false
}
