package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/document"
)

// fakeDoclingParser implements DoclingParser without HTTP. It mirrors the real
// client contract by writing the manifest and referenced images into staging.
type fakeDoclingParser struct {
	manifest document.WordManifest
	images   map[string][]byte
	err      error
	calls    int
	source   string
	name     string
}

func (f *fakeDoclingParser) ParseInto(_ context.Context, source, name, staging string) (*document.WordPackage, error) {
	f.calls++
	f.source = source
	f.name = name
	if f.err != nil {
		return nil, f.err
	}
	for assetID, data := range f.images {
		asset := doclingAssetByID(f.manifest, assetID)
		if asset == nil || asset.ImageFile == nil {
			continue
		}
		target := filepath.Join(staging, filepath.FromSlash(*asset.ImageFile))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return nil, err
		}
	}
	if err := f.manifest.WriteFile(filepath.Join(staging, "manifest.json")); err != nil {
		return nil, err
	}
	return &document.WordPackage{Manifest: f.manifest, Images: f.images}, nil
}

func doclingAssetByID(manifest document.WordManifest, assetID string) *document.WordManifestAsset {
	for _, asset := range manifest.Assets {
		if asset.AssetID == assetID {
			return asset
		}
	}
	return nil
}

func doclingFakeManifest(version string) document.WordManifest {
	imageFile := "images/picture-1.png"
	location := func(paragraph, section int, headings ...string) document.WordLocation {
		return document.WordLocation{Part: "body", Paragraph: paragraph, Headings: headings, Scope: "body", Section: section}
	}
	return document.WordManifest{
		Version: version,
		Blocks: []document.WordManifestBlock{
			{Ref: "#/texts/0", Order: 0, Kind: document.KindHeading, Text: "Galaxy X7 系统手册",
				Location: location(1, 1, "Galaxy X7 系统手册")},
			{Ref: "#/texts/1", Order: 1, Kind: document.KindParagraph, Text: "本文档描述安装流程。",
				Location: location(2, 1, "Galaxy X7 系统手册")},
			{Ref: "#/texts/2", Order: 2, Kind: document.KindParagraph, Text: "表 1 硬件参数表",
				Location: location(3, 1, "Galaxy X7 系统手册")},
			{Ref: "#/tables/0", Order: 3, Kind: "table", AssetID: "table-1",
				Location: location(4, 1, "Galaxy X7 系统手册")},
			{Ref: "#/texts/3", Order: 4, Kind: document.KindParagraph, Text: "图 1 系统架构图",
				Location: location(5, 1, "Galaxy X7 系统手册")},
			{Ref: "#/pictures/0", Order: 5, Kind: document.KindPicture, AssetID: "picture-1",
				Location: location(6, 1, "Galaxy X7 系统手册")},
		},
		Assets: []*document.WordManifestAsset{
			{AssetID: "table-1", Kind: "table",
				Table: &document.WordTableData{
					NumRows: 2, NumCols: 2,
					Rows:       [][]string{{"部件", "规格"}, {"处理器", "8 核"}},
					HeaderRows: [][]string{{"部件", "规格"}},
					Headers:    []string{"部件", "规格"},
				},
				Location: location(4, 1, "Galaxy X7 系统手册")},
			{AssetID: "picture-1", Kind: "picture", ImageFile: &imageFile,
				Location: location(6, 1, "Galaxy X7 系统手册")},
		},
		Diagnostics: []document.Diagnostic{},
	}
}

func doclingFakeParser(version string) *fakeDoclingParser {
	return &fakeDoclingParser{
		manifest: doclingFakeManifest(version),
		images:   map[string][]byte{"picture-1": []byte("PNGDATA")},
	}
}

func TestPrepareUsesDoclingForDOCX(t *testing.T) {
	source := plainDocxPackage(t, paragraphXML("正文"), false)
	staging := t.TempDir()
	parser := doclingFakeParser(document.DoclingWordParserVersion)
	result, err := Prepare(context.Background(), source, Options{
		SourceName: "manual.docx",
		Chunking:   chunking.Config{Strategy: "character", ChunkSize: 700, ChunkOverlap: 100},
		StagingDir: staging,
		Docling:    DoclingOptions{Parser: parser},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if parser.calls != 1 || parser.source != source || parser.name != "manual.docx" {
		t.Fatalf("parser call = %+v", parser)
	}
	if result.Metadata["parser_version"] != document.DoclingWordParserVersion {
		t.Fatalf("parser_version = %v", result.Metadata["parser_version"])
	}
	chunkingMetadata := result.Metadata["chunking"].(map[string]any)
	if chunkingMetadata["word_parser_version"] != document.DoclingWordParserVersion {
		t.Fatalf("chunking word_parser_version = %v", chunkingMetadata["word_parser_version"])
	}
	for _, chunk := range result.Chunks {
		if chunk.Metadata["word_parser_version"] != document.DoclingWordParserVersion {
			t.Fatalf("chunk parser version = %v", chunk.Metadata["word_parser_version"])
		}
	}
	if len(result.Assets) != 2 {
		t.Fatalf("assets = %d", len(result.Assets))
	}
	var picture *document.Asset
	for index := range result.Assets {
		if result.Assets[index].Kind == "picture" {
			picture = &result.Assets[index]
		}
	}
	if picture == nil || string(picture.Data) != "PNGDATA" || picture.Storage != "word" {
		t.Fatalf("picture asset = %+v", picture)
	}
	// DOCX caption paragraphs are associated to the adjacent object, so the
	// caption text travels with the table/picture chunk instead of a duplicate
	// text chunk.
	if !strings.Contains(strings.Join(chunkTexts(result.Chunks), "\n"), "表 1 硬件参数表") {
		t.Fatalf("table caption missing from chunks: %q", chunkTexts(result.Chunks))
	}
	if _, err := os.Stat(filepath.Join(staging, "manifest.json")); err != nil {
		t.Fatalf("staging manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "images", "picture-1.png")); err != nil {
		t.Fatalf("staging image missing: %v", err)
	}
}

func TestPrepareUsesDoclingForHTML(t *testing.T) {
	source := writeText(t, "manual.html", "<!doctype html><html><body><h1>标题</h1><p>正文</p></body></html>")
	staging := t.TempDir()
	parser := doclingFakeParser(document.HTMLParserVersion)
	result, err := Prepare(context.Background(), source, Options{
		SourceName: "manual.html",
		Chunking:   chunking.Config{Strategy: "character", ChunkSize: 700, ChunkOverlap: 100},
		StagingDir: staging,
		Docling:    DoclingOptions{Parser: parser},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result.Metadata["parser_version"] != document.HTMLParserVersion {
		t.Fatalf("parser_version = %v", result.Metadata["parser_version"])
	}
	if result.Metadata["document_suffix"] != ".html" {
		t.Fatalf("document_suffix = %v", result.Metadata["document_suffix"])
	}
	chunkingMetadata := result.Metadata["chunking"].(map[string]any)
	if chunkingMetadata["html_parser_version"] != document.HTMLParserVersion {
		t.Fatalf("html_parser_version = %v", chunkingMetadata["html_parser_version"])
	}
	for _, chunk := range result.Chunks {
		if _, exists := chunk.Metadata["word_parser_version"]; exists {
			t.Fatalf("HTML chunk marked as Word: %+v", chunk.Metadata)
		}
		if chunk.Metadata["html_parser_version"] != document.HTMLParserVersion {
			t.Fatalf("chunk html_parser_version = %v", chunk.Metadata["html_parser_version"])
		}
	}
	for _, asset := range result.Assets {
		if asset.Storage != "html" {
			t.Fatalf("asset storage = %q", asset.Storage)
		}
		if payload := asset.Payload("manual.html"); payload["asset_storage"] != "html" {
			t.Fatalf("asset payload storage = %v", payload["asset_storage"])
		}
	}
}

func TestPrepareConvertsDOCBeforeDocling(t *testing.T) {
	source := writeText(t, "legacy.doc", "legacy bytes")
	converted := plainDocxPackage(t, paragraphXML("转换后的正文"), false)
	staging := t.TempDir()
	parser := doclingFakeParser(document.DoclingWordParserVersion)
	result, err := Prepare(context.Background(), source, Options{
		SourceName: "legacy.doc",
		Chunking:   chunking.Config{Strategy: "character", ChunkSize: 700, ChunkOverlap: 100},
		StagingDir: staging,
		Docling:    DoclingOptions{Parser: parser},
		Word:       WordOptions{Converter: &fakeDOCConverter{target: converted}},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if parser.source != converted {
		t.Fatalf("Docling parsed %q, want the converted DOCX %q", parser.source, converted)
	}
	if result.Metadata["document_suffix"] != ".doc" {
		t.Fatalf("document_suffix = %v", result.Metadata["document_suffix"])
	}
	for _, chunk := range result.Chunks {
		if chunk.Source != "legacy.doc" {
			t.Fatalf("chunk source = %q", chunk.Source)
		}
	}
}

func TestPrepareHTMLRequiresDocling(t *testing.T) {
	source := writeText(t, "manual.html", "<!doctype html><html><body>正文</body></html>")
	_, err := Prepare(context.Background(), source, Options{
		SourceName: "manual.html",
		Chunking:   chunking.Config{Strategy: "character", ChunkSize: 700, ChunkOverlap: 100},
	})
	if !errors.Is(err, ErrDoclingNotConfigured) {
		t.Fatalf("error = %v, want ErrDoclingNotConfigured", err)
	}
}

func TestPrepareDoclingRejectsWrongManifestVersion(t *testing.T) {
	source := plainDocxPackage(t, paragraphXML("正文"), false)
	for _, version := range []string{document.HTMLParserVersion, "docling-future-v9"} {
		parser := doclingFakeParser(version)
		_, err := Prepare(context.Background(), source, Options{
			SourceName: "manual.docx",
			Chunking:   chunking.Config{Strategy: "character", ChunkSize: 700, ChunkOverlap: 100},
			StagingDir: t.TempDir(),
			Docling:    DoclingOptions{Parser: parser},
		})
		if err == nil || !strings.Contains(err.Error(), "Docling") {
			t.Fatalf("version %q error = %v", version, err)
		}
	}
}

func TestPrepareDoclingNoIndexableText(t *testing.T) {
	source := plainDocxPackage(t, paragraphXML("正文"), false)
	manifest := document.WordManifest{Version: document.DoclingWordParserVersion, Blocks: []document.WordManifestBlock{}, Assets: []*document.WordManifestAsset{}, Diagnostics: []document.Diagnostic{}}
	parser := &fakeDoclingParser{manifest: manifest, images: map[string][]byte{}}
	_, err := Prepare(context.Background(), source, Options{
		SourceName: "manual.docx",
		Chunking:   chunking.Config{Strategy: "character", ChunkSize: 700, ChunkOverlap: 100},
		StagingDir: t.TempDir(),
		Docling:    DoclingOptions{Parser: parser},
	})
	if !errors.Is(err, ErrNoIndexableText) {
		t.Fatalf("error = %v, want ErrNoIndexableText", err)
	}
}

func TestPrepareDoclingParserErrorFailsTheAttempt(t *testing.T) {
	source := plainDocxPackage(t, paragraphXML("正文"), false)
	parser := &fakeDoclingParser{err: errors.New("Docling 解析服务不可用：manual.docx")}
	staging := t.TempDir()
	_, err := Prepare(context.Background(), source, Options{
		SourceName: "manual.docx",
		Chunking:   chunking.Config{Strategy: "character", ChunkSize: 700, ChunkOverlap: 100},
		StagingDir: staging,
		Docling:    DoclingOptions{Parser: parser},
	})
	if err == nil || !strings.Contains(err.Error(), "不可用") {
		t.Fatalf("error = %v", err)
	}
	entries, readErr := os.ReadDir(staging)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("staging was populated after a failed parse: %v", entries)
	}
}

func chunkTexts(chunks []chunking.Chunk) []string {
	texts := make([]string, len(chunks))
	for index, chunk := range chunks {
		texts[index] = chunk.Text
	}
	return texts
}
