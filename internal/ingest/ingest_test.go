package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/chunking"
	"github.com/Guan0923/Praxis-RAG/internal/document"
)

func TestPrepareTextTXTMatchesChunkDocument(t *testing.T) {
	path := writeText(t, "notes.txt", "Indexing Guide\n\nSplit documents into chunks before indexing.")
	config := mustConfig(t, "token", 512, 64)
	result, err := Prepare(t.Context(), path, Options{
		SourceName: "notes.txt",
		Chunking:   config,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(result.Chunks) == 0 {
		t.Fatalf("no chunks")
	}
	if result.Title != "Indexing Guide" {
		t.Fatalf("title = %q", result.Title)
	}
	if result.Metadata["document_suffix"] != ".txt" {
		t.Fatalf("document_suffix = %v", result.Metadata["document_suffix"])
	}
	chunkingMetadata := result.Metadata["chunking"].(map[string]any)
	if chunkingMetadata["strategy"] != "token" || chunkingMetadata["tokenizer"] != defaultTokenizerModel {
		t.Fatalf("chunking metadata = %v", chunkingMetadata)
	}
	if _, ok := chunkingMetadata["pdf_parser_version"]; ok {
		t.Fatalf("text metadata must not claim a parser version: %v", chunkingMetadata)
	}
	if result.StagingDir != "" || len(result.Assets) != 0 || result.SkippedObjectCount != 0 {
		t.Fatalf("unexpected text result: %+v", result)
	}

	// Chunk ids must be identical to the direct chunk_document port.
	direct, err := chunking.ChunkDocument(path, chunking.ChunkOptions{
		SourceName: "notes.txt",
		Config:     &config,
	})
	if err != nil {
		t.Fatalf("ChunkDocument() error = %v", err)
	}
	if len(direct) != len(result.Chunks) {
		t.Fatalf("chunk counts differ: %d vs %d", len(direct), len(result.Chunks))
	}
	for index := range direct {
		if direct[index].ChunkID != result.Chunks[index].ChunkID ||
			direct[index].DocumentID != result.Chunks[index].DocumentID {
			t.Fatalf("chunk %d differs: %q vs %q", index, direct[index].ChunkID, result.Chunks[index].ChunkID)
		}
	}
}

func TestPrepareMarkdownStructure(t *testing.T) {
	source := "# 手册\n\n开头。\n\n## 维护\n\n正文内容。"
	path := writeText(t, "guide.md", source)
	config := mustConfig(t, "structure", 512, 64)
	result, err := Prepare(t.Context(), path, Options{SourceName: "guide.md", Chunking: config})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(result.Chunks) != 1 {
		t.Fatalf("chunks = %d", len(result.Chunks))
	}
	if result.Metadata["document_suffix"] != ".md" {
		t.Fatalf("document_suffix = %v", result.Metadata["document_suffix"])
	}
	paths, ok := result.Chunks[0].Metadata["heading_paths"].([][]string)
	if !ok || len(paths) == 0 {
		t.Fatalf("heading_paths = %v", result.Chunks[0].Metadata["heading_paths"])
	}
	if result.Title != "手册" {
		t.Fatalf("title = %q", result.Title)
	}
}

func TestPrepareTextEmptyAndUnsupported(t *testing.T) {
	empty := writeText(t, "empty.txt", "   \n\n\t")
	_, err := Prepare(t.Context(), empty, Options{SourceName: "empty.txt"})
	if err == nil || !isNoIndexable(err) ||
		err.Error() != "知识文件没有可索引的文本：empty.txt" {
		t.Fatalf("empty error = %v", err)
	}
	unsupported := writeText(t, "archive.xyz", "data")
	_, err = Prepare(t.Context(), unsupported, Options{SourceName: "archive.xyz"})
	if err == nil || !strings.Contains(err.Error(), "暂不支持该文件类型：.xyz") {
		t.Fatalf("unsupported error = %v", err)
	}
	if isNoIndexable(err) {
		t.Fatalf("unsupported suffix must not be reported as empty")
	}
}

func TestPrepareSourceNameFallback(t *testing.T) {
	path := writeText(t, "notes.txt", "content")
	result, err := Prepare(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result.Chunks[0].Source != "notes.txt" || result.Chunks[0].DocumentID != chunking.DocumentID("notes.txt") {
		t.Fatalf("chunk identity = %+v", result.Chunks[0])
	}
	// The zero chunking.Config maps to Python's LEGACY_CONFIG (character 700/100).
	chunkingMetadata := result.Metadata["chunking"].(map[string]any)
	if chunkingMetadata["strategy"] != "character" || chunkingMetadata["chunk_size"] != 700 ||
		chunkingMetadata["chunk_overlap"] != 100 {
		t.Fatalf("legacy chunking metadata = %v", chunkingMetadata)
	}
}

func TestPrepareDOCXAssetsArtifactsAndMetadata(t *testing.T) {
	manifest := newManifestBuilder().
		heading("#/texts/0", "设备手册", []string{"设备手册"}).
		paragraph("#/texts/1", "正常使用。", "设备手册").
		table("#/tables/0", "table-1", [][]string{{"环节", "操作"}, {"索引", "写入片段向量"}}, "设备手册").
		picture("#/pictures/0", "picture-1", "图 1: 状态灯", "设备手册").
		build()
	source := plainDocxPackage(t, paragraphXML("正文"), false)
	staging := t.TempDir()
	options := wordPrepareOptions(t, source, staging, manifest, func(o *Options) {
		o.SourceName = "manual.docx"
	})
	result, err := Prepare(t.Context(), source, options)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(result.Chunks) == 0 {
		t.Fatalf("no chunks")
	}
	if result.Title != "manual" {
		t.Fatalf("title = %q", result.Title)
	}
	if result.Metadata["document_suffix"] != ".docx" ||
		result.Metadata["parser_version"] != document.DoclingWordParserVersion {
		t.Fatalf("result metadata = %v", result.Metadata)
	}
	chunkingMetadata := result.Metadata["chunking"].(map[string]any)
	if chunkingMetadata["word_parser_version"] != document.DoclingWordParserVersion {
		t.Fatalf("chunking metadata = %v", chunkingMetadata)
	}
	if result.Metadata["skipped_object_count"] != 0 {
		t.Fatalf("skipped = %v", result.Metadata["skipped_object_count"])
	}
	if _, ok := result.Metadata["diagnostics"].([]document.Diagnostic); !ok {
		t.Fatalf("diagnostics metadata type = %T", result.Metadata["diagnostics"])
	}
	if len(result.Assets) != 2 {
		t.Fatalf("assets = %d, want picture + table", len(result.Assets))
	}
	var asset, tableAsset document.Asset
	for _, candidate := range result.Assets {
		switch candidate.Kind {
		case "picture":
			asset = candidate
		case "table":
			tableAsset = candidate
		}
	}
	if asset.Kind != "picture" || len(asset.Data) == 0 || asset.SHA256 == "" {
		t.Fatalf("picture asset = %+v", asset)
	}
	if tableAsset.Kind != "table" || tableAsset.Table == nil {
		t.Fatalf("table asset = %+v", tableAsset)
	}
	if !strings.HasPrefix(asset.Name, "doc-") ||
		!strings.HasSuffix(asset.Name, "/images/"+asset.SourceID+".jpeg") {
		t.Fatalf("asset name = %q (source %q)", asset.Name, asset.SourceID)
	}

	if result.StagingDir == "" {
		t.Fatalf("missing staging dir")
	}
	t.Cleanup(func() { _ = os.RemoveAll(result.StagingDir) })
	manifestPath := filepath.Join(result.StagingDir, "manifest.json")
	stored, err := document.ReadWordManifestFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest artifact = %v", err)
	}
	qualifiedPicture := false
	for _, candidate := range stored.Assets {
		if candidate.AssetID == asset.ID {
			qualifiedPicture = true
		}
	}
	if len(stored.Assets) != 2 || !qualifiedPicture {
		t.Fatalf("qualified manifest artifact = %+v", stored.Assets)
	}
	markdown, err := os.ReadFile(filepath.Join(result.StagingDir, "document.md"))
	if err != nil || !strings.Contains(string(markdown), "设备手册") {
		t.Fatalf("document.md = %q, err = %v", markdown, err)
	}
}

func TestPrepareLegacyDOCConversion(t *testing.T) {
	manifest := newManifestBuilder().
		paragraph("#/texts/0", "legacy body text").
		build()
	docxPath := plainDocxPackage(t, paragraphXML("converted"), false)
	converter := &fakeDOCConverter{target: docxPath}
	docPath := writeText(t, "legacy.doc", "dummy legacy bytes")
	staging := t.TempDir()
	options := wordPrepareOptions(t, docPath, staging, manifest, func(o *Options) {
		o.SourceName = "legacy.doc"
		o.Word = WordOptions{Converter: converter}
	})
	result, err := Prepare(t.Context(), docPath, options)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !converter.called {
		t.Fatalf("DOC converter was not called")
	}
	if len(result.Chunks) == 0 {
		t.Fatalf("no chunks")
	}
	if result.Chunks[0].Metadata["document_suffix"] != ".doc" {
		t.Fatalf("chunk document_suffix = %v", result.Chunks[0].Metadata["document_suffix"])
	}
	if result.Metadata["document_suffix"] != ".doc" {
		t.Fatalf("result document_suffix = %v", result.Metadata["document_suffix"])
	}
	if !strings.Contains(result.Chunks[0].Text, "legacy body text") {
		t.Fatalf("chunk text = %q", result.Chunks[0].Text)
	}
}

func TestPreparePDFInjectedParserAssetsAndArtifacts(t *testing.T) {
	staging := t.TempDir()
	parser := &fakePDFParser{manifest: pdfManifestFixture()}
	result, err := Prepare(t.Context(), writeText(t, "paper.pdf", "%PDF-1.7"), Options{
		SourceName: "paper.pdf",
		StagingDir: staging,
		PDF:        PDFOptions{Parser: parser},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if parser.calls != 1 {
		t.Fatalf("parser calls = %d", parser.calls)
	}
	if result.StagingDir != staging {
		t.Fatalf("staging = %q, want caller directory", result.StagingDir)
	}
	if len(result.Chunks) == 0 {
		t.Fatalf("no chunks")
	}
	if result.Metadata["document_suffix"] != ".pdf" ||
		result.Metadata["parser_version"] != document.PDFParserVersion {
		t.Fatalf("result metadata = %v", result.Metadata)
	}
	chunkingMetadata := result.Metadata["chunking"].(map[string]any)
	if chunkingMetadata["pdf_parser_version"] != document.PDFParserVersion {
		t.Fatalf("chunking metadata = %v", chunkingMetadata)
	}
	if len(result.Assets) != 2 {
		t.Fatalf("assets = %d", len(result.Assets))
	}
	picture := result.Assets[0]
	if picture.Kind != "picture" || string(picture.Data) != "PNGDATA" || picture.MIMEType != "image/png" {
		t.Fatalf("picture asset = %+v", picture)
	}
	if picture.Name != chunking.DocumentID("paper.pdf")+"/pictures/picture-0.png" {
		t.Fatalf("picture name = %q", picture.Name)
	}
	if result.Assets[1].Kind != "table" || result.Assets[1].Table == nil {
		t.Fatalf("table asset = %+v", result.Assets[1])
	}

	formulas := chunksWithMetadata(result.Chunks, "kind", "formula")
	if len(formulas) != 1 || formulas[0].Text != "$$\n\\frac{a}{b}\n$$" {
		t.Fatalf("formula chunk = %+v", formulas)
	}
	tables := chunksWithMetadata(result.Chunks, "asset_kind", "table")
	if len(tables) == 0 {
		t.Fatalf("no table chunks")
	}
	for _, table := range tables {
		if !strings.Contains(table.Text, "A | B") {
			t.Fatalf("table chunk = %q", table.Text)
		}
	}

	markdown, err := os.ReadFile(filepath.Join(staging, "document.md"))
	if err != nil || !strings.Contains(string(markdown), "[Image:") {
		t.Fatalf("document.md = %q, err = %v", markdown, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "manifest.json")); err != nil {
		t.Fatalf("parser manifest artifact missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "pictures", "picture-0.png")); err != nil {
		t.Fatalf("parser image artifact missing: %v", err)
	}
}

func TestPreparePDFRecordsManifestParserVersion(t *testing.T) {
	for _, version := range []string{document.PDFParserVersionService, "docling-formula-remote-v3"} {
		t.Run(version, func(t *testing.T) {
			manifest := pdfManifestFixture()
			manifest.Version = version
			result, err := Prepare(t.Context(), writeText(t, "paper.pdf", "%PDF-1.7"), Options{
				SourceName: "paper.pdf",
				PDF:        PDFOptions{Parser: &fakePDFParser{manifest: manifest}},
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(result.StagingDir) })
			if result.Metadata["parser_version"] != version {
				t.Fatalf("parser_version = %v, want %q", result.Metadata["parser_version"], version)
			}
			chunkingMetadata := result.Metadata["chunking"].(map[string]any)
			if chunkingMetadata["pdf_parser_version"] != version {
				t.Fatalf("pdf_parser_version = %v, want %q", chunkingMetadata["pdf_parser_version"], version)
			}
		})
	}
}

func TestPreparePDFDefaultStagingIsReturned(t *testing.T) {
	parser := &fakePDFParser{manifest: pdfManifestFixture()}
	result, err := Prepare(t.Context(), writeText(t, "paper.pdf", "%PDF-1.7"), Options{
		SourceName: "paper.pdf",
		PDF:        PDFOptions{Parser: parser},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(result.StagingDir) })
	if result.StagingDir == "" {
		t.Fatalf("expected a staging directory")
	}
	if info, err := os.Stat(result.StagingDir); err != nil || !info.IsDir() {
		t.Fatalf("staging dir = %q, err = %v", result.StagingDir, err)
	}
}

func TestPreparePDFEmptyVersionMismatchAndParserError(t *testing.T) {
	empty := pdfManifestFixture()
	empty.Blocks = []document.PDFBlock{{Ref: "empty", Kind: "text", Page: 1, Order: 0, Text: "  "}}
	empty.Assets = nil
	_, err := Prepare(t.Context(), writeText(t, "scan.pdf", "%PDF-1.7"), Options{
		SourceName: "scan.pdf",
		PDF:        PDFOptions{Parser: &fakePDFParser{manifest: empty}},
	})
	if err == nil || !isNoIndexable(err) || !strings.Contains(err.Error(), "PDF 没有可索引的内容") {
		t.Fatalf("empty pdf error = %v", err)
	}
	mismatch := pdfManifestFixture()
	mismatch.Version = "old-parser"
	_, err = Prepare(t.Context(), writeText(t, "paper.pdf", "%PDF-1.7"), Options{
		SourceName: "paper.pdf",
		PDF:        PDFOptions{Parser: &fakePDFParser{manifest: mismatch}},
	})
	if err == nil || err.Error() != "PDF parser version mismatch; reimport this document" {
		t.Fatalf("version error = %v", err)
	}
	_, err = Prepare(t.Context(), writeText(t, "paper.pdf", "%PDF-1.7"), Options{
		SourceName: "paper.pdf",
		PDF:        PDFOptions{Parser: &fakePDFParser{err: errors.New("worker exploded")}},
	})
	if err == nil || !strings.Contains(err.Error(), "worker exploded") {
		t.Fatalf("parser error = %v", err)
	}
}

func TestPrepareWordVisionIntegration(t *testing.T) {
	manifest := newManifestBuilder().
		picture("#/pictures/0", "picture-1", "").
		paragraph("#/texts/0", "Body text").
		build()
	source := plainDocxPackage(t, paragraphXML("body"), false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{
				"content": "A green pressure indicator on the pump panel.",
			}}},
		})
	}))
	defer server.Close()
	worker := &document.WordVisionWorker{VLM: document.VLMSettings{
		BaseURL: server.URL + "/v1",
		Model:   "test-vlm",
	}}
	staging := t.TempDir()
	options := wordPrepareOptions(t, source, staging, manifest, func(o *Options) {
		o.SourceName = "vision.docx"
		o.Word = WordOptions{Vision: worker}
	})
	result, err := Prepare(t.Context(), source, options)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	pictures := chunksWithMetadata(result.Chunks, "asset_kind", "picture")
	if len(pictures) != 1 ||
		pictures[0].Text != "A green pressure indicator on the pump panel." {
		t.Fatalf("picture chunks = %+v", pictures)
	}
	if len(result.Assets) != 1 ||
		result.Assets[0].GeneratedDescription != "A green pressure indicator on the pump panel." {
		t.Fatalf("assets = %+v", result.Assets)
	}
	stored, err := document.ReadWordManifestFile(filepath.Join(result.StagingDir, "manifest.json"))
	if err != nil {
		t.Fatalf("manifest artifact = %v", err)
	}
	if stored.Assets[0].GeneratedDescription == "" {
		t.Fatalf("manifest artifact lost the description")
	}
}

func TestPrepareCancelledContextAndStagingCleanupOnFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Prepare(ctx, writeText(t, "paper.pdf", "%PDF-1.7"), Options{
		SourceName: "paper.pdf",
		PDF:        PDFOptions{Parser: &fakePDFParser{err: context.Canceled}},
	})
	if err == nil {
		t.Fatalf("cancelled context was ignored")
	}
}
